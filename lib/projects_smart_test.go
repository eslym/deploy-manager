package lib

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
)

func TestAuthenticatedProjectLifecycleOverSmartHTTPS(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "repo.git")
	featureAtCreate := createSmartFixture(t, remoteDir)
	partialDir := filepath.Join(t.TempDir(), "partial.git")
	partialFeature := createSmartFixture(t, partialDir)
	partialRepo, err := git.PlainOpen(partialDir)
	if err != nil {
		t.Fatal(err)
	}
	partialCommit, err := partialRepo.CommitObject(plumbing.NewHash(partialFeature))
	if err != nil {
		t.Fatal(err)
	}
	partialTree, err := partialCommit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	partialFile, err := partialTree.File("README.md")
	if err != nil {
		t.Fatal(err)
	}
	partialStore := missingObjectStorer{Storer: partialRepo.Storer, missing: partialFile.Hash}
	var failRequests atomic.Bool
	var failStatus atomic.Int32
	var transientFailures atomic.Int32
	var pauseBranchFetch atomic.Bool
	branchFetchStarted := make(chan struct{})
	var pauseWebhookFetch atomic.Bool
	webhookFetchStarted := make(chan struct{})
	resumeWebhookFetch := make(chan struct{})
	var resumeWebhookOnce sync.Once
	releaseWebhookFetch := func() { resumeWebhookOnce.Do(func() { close(resumeWebhookFetch) }) }
	defer releaseWebhookFetch()
	source, err := git.PlainOpen(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	var endpoint *transport.Endpoint
	var backend transport.Transport
	var partialEndpoint *transport.Endpoint
	var partialBackend transport.Transport
	var remoteURL string
	var redirectRequests atomic.Bool
	var redirectedHits atomic.Int32
	var gitRequests atomic.Int32
	var measurePack atomic.Bool
	var measuredPackBytes atomic.Int64
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gitRequests.Add(1)
		if pauseWebhookFetch.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") && pauseWebhookFetch.CompareAndSwap(true, false) {
			close(webhookFetchStarted)
			<-resumeWebhookFetch
		}
		if pauseBranchFetch.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") {
			if pauseBranchFetch.CompareAndSwap(true, false) {
				close(branchFetchStarted)
				<-r.Context().Done()
				return
			}
		}
		if failRequests.Load() {
			status := int(failStatus.Load())
			if status == 0 {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, "fixture deliberately unavailable", status)
			return
		}
		if remaining := transientFailures.Load(); remaining > 0 && transientFailures.CompareAndSwap(remaining, remaining-1) {
			http.Error(w, "fixture temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		requestEndpoint, requestBackend := endpoint, backend
		if strings.HasPrefix(r.URL.Path, "/partial.git/") {
			requestEndpoint, requestBackend = partialEndpoint, partialBackend
		}
		if requestEndpoint == nil || requestBackend == nil {
			http.Error(w, "fixture not ready", 500)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/redirected/") {
			redirectedHits.Add(1)
			http.NotFound(w, r)
			return
		}
		if redirectRequests.Load() && r.Method == http.MethodGet && r.URL.Path == "/repo.git/info/refs" {
			http.Redirect(w, r, remoteURL+"/redirected/repo.git/info/refs?service=git-upload-pack", http.StatusFound)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/dumb.git/info/refs" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, strings.Repeat("0", 40)+" refs/heads/main\n")
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/sha256.git/info/refs" {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			encoder := pktline.NewEncoder(w)
			_ = encoder.Encode([]byte("# service=git-upload-pack\n"))
			_ = encoder.Flush()
			_ = encoder.Encode([]byte(strings.Repeat("0", 40) + " refs/heads/main\x00object-format=sha256\n"))
			_ = encoder.Flush()
			return
		}
		session, err := requestBackend.NewUploadPackSession(requestEndpoint, nil)
		if err != nil {
			http.Error(w, "session unavailable", 500)
			return
		}
		defer session.Close()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == "git-upload-pack":
			adv, err := session.AdvertisedReferencesContext(r.Context())
			if err != nil {
				http.Error(w, "advertisement failed", 500)
				return
			}
			adv.Prefix = [][]byte{[]byte("# service=git-upload-pack"), pktline.Flush}
			if err := adv.Capabilities.Set(capability.Filter); err != nil {
				t.Errorf("enable server filter capability: %v", err)
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			if err := adv.Encode(w); err != nil {
				t.Errorf("encode refs: %v", err)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
			if _, err := session.AdvertisedReferencesContext(r.Context()); err != nil {
				http.Error(w, "advertisement failed", 500)
				return
			}
			request := packp.NewUploadPackRequest()
			scanner := pktline.NewScanner(r.Body)
			for scanner.Scan() {
				line := string(scanner.Bytes())
				if strings.HasPrefix(line, "want ") {
					want := strings.SplitN(strings.TrimPrefix(line, "want "), "\x00", 2)[0]
					request.Wants = append(request.Wants, plumbing.NewHash(strings.TrimSpace(want)))
				} else if strings.HasPrefix(line, "have ") {
					request.Haves = append(request.Haves, plumbing.NewHash(strings.TrimSpace(strings.TrimPrefix(line, "have "))))
				}
			}
			if err := scanner.Err(); err != nil {
				http.Error(w, "invalid upload request", 400)
				return
			}
			response, err := session.UploadPack(r.Context(), request)
			if err != nil {
				http.Error(w, "upload-pack failed: "+err.Error(), 500)
				return
			}
			defer response.Close()
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			writer := http.ResponseWriter(w)
			if measurePack.Load() {
				writer = &countingHTTPWriter{ResponseWriter: w, count: &measuredPackBytes}
			}
			if err := response.Encode(writer); err != nil && r.Context().Err() == nil && !strings.HasPrefix(r.URL.Path, "/partial.git/") {
				t.Errorf("encode pack: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	remoteURL = remote.URL
	endpoint, err = transport.NewEndpoint(remote.URL + "/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	backend = gitserver.NewServer(gitserver.MapLoader{endpoint.String(): source.Storer})
	partialEndpoint, err = transport.NewEndpoint(remote.URL + "/partial.git")
	if err != nil {
		t.Fatal(err)
	}
	partialBackend = gitserver.NewServer(gitserver.MapLoader{partialEndpoint.String(): partialStore})
	installTrustedSmartHTTPTestClient(t, remote)

	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.sqlite"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), GitTimeout: time.Minute, GitMaxTransferBytes: 64 << 10, GitMaxCacheBytes: 32 << 20, GitMaxRefs: 10}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// This application-boundary scenario exercises many protected actions from
	// one test client; keep the limiter itself out of the scenario's concern.
	app.cfg.AuthRequestsPerMinute = 1000
	cookie := installAPITestSession(t, app)
	appServer := httptest.NewTLSServer(app.Handler())
	client := appServer.Client()

	createdRes := projectPost(t, client, appServer.URL, cookie, "/api/projects", `{"name":"fixture","url":"`+remote.URL+`/repo.git"}`)
	if createdRes.StatusCode != http.StatusCreated {
		t.Fatalf("project creation status %d: %s", createdRes.StatusCode, responseText(createdRes))
	}
	var created project
	if err := json.NewDecoder(createdRes.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	createdRes.Body.Close()
	if created.LastFetched == nil {
		t.Fatal("successful initial clone did not record last fetch")
	}
	initialRefs := projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID)
	requireProjectRefs(t, initialRefs, featureAtCreate)
	tagDeployment := createAndTestTagDeployment(t, client, appServer.URL, cookie, app, created.ID, initialRefs)
	deploymentResponse := projectPost(t, client, appServer.URL, cookie, "/api/deployments", `{"projectId":"`+created.ID+`","name":"production","commit":"`+featureAtCreate+`"}`)
	if deploymentResponse.StatusCode != http.StatusCreated {
		t.Fatalf("deployment creation status %d: %s", deploymentResponse.StatusCode, responseText(deploymentResponse))
	}
	var deploymentCreated struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(deploymentResponse.Body).Decode(&deploymentCreated); err != nil {
		t.Fatal(err)
	}
	deploymentResponse.Body.Close()
	// Creation fetch must survive a one-shot smart-HTTP 503 and publish normally.
	transientFailures.Store(1)
	branchDeploymentResponse := projectPost(t, client, appServer.URL, cookie, "/api/deployments", `{"projectId":"`+created.ID+`","name":"branch-production","targetKind":"branch","targetName":"feature/search","commit":"`+featureAtCreate+`"}`)
	if branchDeploymentResponse.StatusCode != http.StatusCreated {
		t.Fatalf("branch deployment creation status %d: %s", branchDeploymentResponse.StatusCode, responseText(branchDeploymentResponse))
	}
	var branchDeployment struct {
		ID    string `json:"id"`
		JobID string `json:"jobId"`
	}
	if err := json.NewDecoder(branchDeploymentResponse.Body).Decode(&branchDeployment); err != nil {
		t.Fatal(err)
	}
	branchDeploymentResponse.Body.Close()
	targetRequest, _ := http.NewRequest(http.MethodGet, appServer.URL+"/api/deployments", nil)
	targetRequest.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	targetResponse, err := client.Do(targetRequest)
	if err != nil {
		t.Fatal(err)
	}
	var listed []deployment
	if err = json.NewDecoder(targetResponse.Body).Decode(&listed); err != nil {
		targetResponse.Body.Close()
		t.Fatal(err)
	}
	targetResponse.Body.Close()
	foundTarget := false
	for _, item := range listed {
		if item.ID == branchDeployment.ID && item.TargetKind == "branch" && item.TargetName == "feature/search" {
			foundTarget = true
		}
	}
	if !foundTarget {
		t.Fatal("branch target was not returned by the deployments API")
	}
	branchPath := filepath.Join(app.cfg.DeploymentRoot, branchDeployment.ID)
	branchDeadline := time.Now().Add(5 * time.Second)
	for {
		if content, readErr := os.ReadFile(filepath.Join(branchPath, "README.md")); readErr == nil && string(content) == "searchable feature\n" {
			break
		}
		if time.Now().After(branchDeadline) {
			t.Fatal("branch-target creation did not publish the cached branch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	branchIdentity, err := identity(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	noopResponse := projectPost(t, client, appServer.URL, cookie, "/api/deployments/"+branchDeployment.ID+"/deploy", `{}`)
	if noopResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("branch no-op deploy status %d: %s", noopResponse.StatusCode, responseText(noopResponse))
	}
	var noopJob struct {
		JobID string `json:"jobId"`
	}
	if err := json.NewDecoder(noopResponse.Body).Decode(&noopJob); err != nil {
		t.Fatal(err)
	}
	noopResponse.Body.Close()
	noopDeadline := time.Now().Add(5 * time.Second)
	for {
		jobsRequest, _ := http.NewRequest(http.MethodGet, appServer.URL+"/api/deployments/"+branchDeployment.ID+"/jobs", nil)
		jobsRequest.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		jobsResponse, requestErr := client.Do(jobsRequest)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		var recorded []deploymentJob
		decodeErr := json.NewDecoder(jobsResponse.Body).Decode(&recorded)
		jobsResponse.Body.Close()
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		finished := false
		for _, job := range recorded {
			if job.ID == noopJob.JobID && job.Status == "noop" {
				finished = true
			}
		}
		if finished {
			break
		}
		if time.Now().After(noopDeadline) {
			t.Fatal("branch Deploy did not record a no-op for the already-live commit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after, identityErr := identity(branchPath); identityErr != nil || after != branchIdentity {
		t.Fatalf("no-op recreated the live directory: before=%+v after=%+v err=%v", branchIdentity, after, identityErr)
	}
	hardRedeployAndWait(t, client, appServer.URL, cookie, branchDeployment.ID, branchPath, nil)
	if transientFailures.Load() != 0 {
		t.Fatal("creation fetch did not reach the controlled transient failure")
	}
	// A second branch target shares the repository cache while retaining its own
	// destination; publishing it must not replace the feature deployment.
	app.mu.Lock()
	clear(app.rate)
	app.mu.Unlock()
	mainCommit := findProjectRef(initialRefs, "branch", "main").Commit
	independentResponse := projectPost(t, client, appServer.URL, cookie, "/api/deployments", `{"projectId":"`+created.ID+`","name":"main-production","targetKind":"branch","targetName":"main","commit":"`+mainCommit+`"}`)
	if independentResponse.StatusCode != http.StatusCreated {
		t.Fatalf("independent branch deployment status %d: %s", independentResponse.StatusCode, responseText(independentResponse))
	}
	var independentDeployment struct {
		ID    string `json:"id"`
		JobID string `json:"jobId"`
	}
	if err := json.NewDecoder(independentResponse.Body).Decode(&independentDeployment); err != nil {
		t.Fatal(err)
	}
	independentResponse.Body.Close()
	waitForDeploymentJob(t, client, appServer.URL, cookie, independentDeployment.ID, independentDeployment.JobID, "succeeded")
	independentPath := filepath.Join(app.cfg.DeploymentRoot, independentDeployment.ID)
	independentDeadline := time.Now().Add(5 * time.Second)
	for {
		if content, readErr := os.ReadFile(filepath.Join(independentPath, "README.md")); readErr == nil && string(content) == "main content\n" {
			break
		}
		if time.Now().After(independentDeadline) {
			t.Fatal("independent main deployment did not publish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if content, readErr := os.ReadFile(filepath.Join(branchPath, "README.md")); readErr != nil || string(content) != "searchable feature\n" {
		t.Fatalf("publishing main target changed feature destination: %q, %v", content, readErr)
	}
	// Pause a real branch publication after export, change its target through
	// the protected HTTP API, then verify the stale prepared tree never becomes
	// live and the new target is published instead.
	addSmartFixtureCommit(t, remoteDir, "race branch tip")
	waitForDeploymentWorkersIdle(t, app)
	stageReady, resumeStage := make(chan struct{}), make(chan struct{})
	var pauseOnce sync.Once
	var resumeOnce sync.Once
	resume := func() { resumeOnce.Do(func() { close(resumeStage) }) }
	defer resume()
	previousFault := setPublicationFaultPoint(func(point string) error {
		if point == "after-stage-fsync" {
			pauseOnce.Do(func() { close(stageReady); <-resumeStage })
		}
		return nil
	})
	defer func() { setPublicationFaultPoint(previousFault) }()
	staleRequest := projectPost(t, client, appServer.URL, cookie, "/api/deployments/"+branchDeployment.ID+"/deploy", `{}`)
	if staleRequest.StatusCode != http.StatusAccepted {
		t.Fatalf("racing branch deploy status %d: %s", staleRequest.StatusCode, responseText(staleRequest))
	}
	var staleJob struct {
		JobID string `json:"jobId"`
	}
	if err := json.NewDecoder(staleRequest.Body).Decode(&staleJob); err != nil {
		t.Fatal(err)
	}
	staleRequest.Body.Close()
	select {
	case <-stageReady:
	case <-time.After(5 * time.Second):
		var jobStatus, jobLog, targetKind, targetName, live, branchTip string
		var queued int
		_ = app.db.QueryRow(`SELECT status,log FROM deployment_jobs WHERE id=?`, staleJob.JobID).Scan(&jobStatus, &jobLog)
		_ = app.db.QueryRow(`SELECT target_kind,target_name,live_commit FROM deployments WHERE id=?`, branchDeployment.ID).Scan(&targetKind, &targetName, &live)
		_ = app.db.QueryRow(`SELECT commit_id FROM project_refs WHERE project_id=? AND kind='branch' AND name='feature/search'`, created.ID).Scan(&branchTip)
		_ = app.db.QueryRow(`SELECT count(*) FROM deployment_jobs WHERE status IN ('queued','running')`).Scan(&queued)
		t.Fatalf("branch publication did not reach staged-content boundary: job=%s %q target=%s/%s live=%s cached=%s active=%d", jobStatus, jobLog, targetKind, targetName, live, branchTip, queued)
	}
	mainCommit = findProjectRef(initialRefs, "branch", "main").Commit
	changed := projectPost(t, client, appServer.URL, cookie, "/api/deployments/"+branchDeployment.ID+"/target", `{"targetKind":"branch","targetName":"main","commit":"`+mainCommit+`"}`)
	if changed.StatusCode != http.StatusAccepted {
		resume()
		t.Fatalf("target change during preparation status %d: %s", changed.StatusCode, responseText(changed))
	}
	changed.Body.Close()
	resume()
	waitForDeploymentJob(t, client, appServer.URL, cookie, branchDeployment.ID, staleJob.JobID, "superseded")
	mainDeadline := time.Now().Add(5 * time.Second)
	for {
		content, readErr := os.ReadFile(filepath.Join(branchPath, "README.md"))
		if readErr == nil && string(content) == "main content\n" {
			break
		}
		if time.Now().After(mainDeadline) {
			t.Fatalf("new target was not published after stale preparation: content=%q err=%v", content, readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	latestBranchRef := findProjectRef(projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID), "branch", "feature/search")
	restoreBranch := projectPost(t, client, appServer.URL, cookie, "/api/deployments/"+branchDeployment.ID+"/target", `{"targetKind":"branch","targetName":"feature/search","commit":"`+latestBranchRef.Commit+`"}`)
	if restoreBranch.StatusCode != http.StatusAccepted {
		t.Fatalf("restoring branch target status %d: %s", restoreBranch.StatusCode, responseText(restoreBranch))
	}
	var restoreJob struct {
		JobID string `json:"jobId"`
	}
	if err := json.NewDecoder(restoreBranch.Body).Decode(&restoreJob); err != nil {
		t.Fatal(err)
	}
	restoreBranch.Body.Close()
	waitForDeploymentJob(t, client, appServer.URL, cookie, branchDeployment.ID, restoreJob.JobID, "succeeded")
	if content, readErr := os.ReadFile(filepath.Join(branchPath, "README.md")); readErr != nil || string(content) != "race branch tip\n" {
		t.Fatalf("restored branch target content=%q err=%v", content, readErr)
	}
	testTemporaryRollback(t, client, appServer.URL, cookie, branchDeployment.ID, featureAtCreate, branchPath)
	testRollbackGraphAndRaces(t, appServer, cookie, created.ID, branchDeployment.ID, featureAtCreate, branchPath, source)
	publicationPath := filepath.Join(app.cfg.DeploymentRoot, deploymentCreated.ID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, statErr := os.Stat(publicationPath)
		if statErr == nil && info.IsDir() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment was not published: %v", statErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if content, readErr := os.ReadFile(filepath.Join(publicationPath, "README.md")); readErr != nil || string(content) != "searchable feature\n" {
		t.Fatalf("published tracked file = %q, err=%v", content, readErr)
	}
	noOp := projectPost(t, client, appServer.URL, cookie, "/api/deployments/"+deploymentCreated.ID+"/deploy", `{}`)
	if noOp.StatusCode != http.StatusAccepted {
		t.Fatalf("ordinary deploy status %d: %s", noOp.StatusCode, responseText(noOp))
	}
	noOp.Body.Close()
	hardRedeployAndWait(t, client, appServer.URL, cookie, deploymentCreated.ID, publicationPath, &gitRequests)
	for _, unsupported := range []struct {
		name string
		url  string
	}{
		{"dumb HTTP", remote.URL + "/dumb.git"},
		{"SHA-256 Git", remote.URL + "/sha256.git"},
		{"partial clone", remote.URL + "/partial.git"},
	} {
		response := projectPost(t, client, appServer.URL, cookie, "/api/projects", `{"name":"`+unsupported.name+`","url":"`+unsupported.url+`"}`)
		message := responseText(response)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || strings.Contains(message, unsupported.url) {
			t.Fatalf("unsupported %s repository was not safely rejected: status %d, %s", unsupported.name, response.StatusCode, message)
		}
		if unsupported.name == "SHA-256 Git" && !strings.Contains(strings.ToLower(message), "sha-256") {
			t.Fatalf("SHA-256 rejection is not actionable: %s", message)
		}
		if unsupported.name == "dumb HTTP" && !strings.Contains(strings.ToLower(message), "smart https") {
			t.Fatalf("dumb HTTP rejection is not actionable: %s", message)
		}
		if unsupported.name == "partial clone" && !strings.Contains(strings.ToLower(message), "partial") && !strings.Contains(strings.ToLower(message), "incomplete") {
			t.Fatalf("incomplete repository rejection is not actionable: %s", message)
		}
	}

	largePayload := make([]byte, 256<<10)
	if _, err := rand.Read(largePayload); err != nil {
		t.Fatal(err)
	}
	fixtureRepo, err := git.PlainOpen(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	fixtureWorktree, err := fixtureRepo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixtureWorktree.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("feature/search")}); err != nil {
		t.Fatal(err)
	}
	featureAfter := commitFixtureFile(t, remoteDir, fixtureWorktree, "feature", string(largePayload)).String()
	lastSuccessfulBeforeOversizedRefresh := listProjectsFromAPI(t, client, appServer.URL, cookie)[0].LastFetched
	failStatus.Store(http.StatusUnauthorized)
	failRequests.Store(true)
	requestsBeforeAuthenticationFailure := gitRequests.Load()
	failedFetch := projectPost(t, client, appServer.URL, cookie, "/api/deployments/"+branchDeployment.ID+"/deploy", `{}`)
	if failedFetch.StatusCode != http.StatusAccepted {
		t.Fatalf("branch deploy after remote update status %d: %s", failedFetch.StatusCode, responseText(failedFetch))
	}
	var failedFetchJob struct {
		JobID string `json:"jobId"`
	}
	if err := json.NewDecoder(failedFetch.Body).Decode(&failedFetchJob); err != nil {
		t.Fatal(err)
	}
	failedFetch.Body.Close()
	waitForDeploymentJob(t, client, appServer.URL, cookie, branchDeployment.ID, failedFetchJob.JobID, "failed")
	failRequests.Store(false)
	failStatus.Store(0)
	if delta := gitRequests.Load() - requestsBeforeAuthenticationFailure; delta != 1 {
		t.Fatalf("permanent authentication failure made %d smart-HTTP requests, want exactly one", delta)
	}
	failStatus.Store(http.StatusUnauthorized)
	failRequests.Store(true)
	testRejectedRollbackKeepsLive(t, client, appServer.URL, cookie, branchDeployment.ID, featureAtCreate, branchPath)
	failRequests.Store(false)
	failStatus.Store(0)
	if content, readErr := os.ReadFile(filepath.Join(branchPath, "README.md")); readErr != nil || string(content) != "race branch tip\n" {
		t.Fatalf("failed branch fetch changed live content: %q, err=%v", content, readErr)
	}
	oversized := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	if oversized.StatusCode != http.StatusBadGateway {
		t.Fatalf("oversized pack refresh status %d: %s", oversized.StatusCode, responseText(oversized))
	}
	oversizedMessage := responseText(oversized)
	if !strings.Contains(strings.ToLower(oversizedMessage), "limit") {
		t.Fatalf("oversized pack error is not actionable: %s", oversizedMessage)
	}
	refsAfterOversizedRefresh := projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID)
	if len(refsAfterOversizedRefresh) != len(initialRefs) || listProjectsFromAPI(t, client, appServer.URL, cookie)[0].LastFetched == nil || !listProjectsFromAPI(t, client, appServer.URL, cookie)[0].LastFetched.Equal(*lastSuccessfulBeforeOversizedRefresh) {
		t.Fatal("oversized transfer changed cached refs or last successful fetch time")
	}
	// Increase the configured transfer ceiling for the remainder of the fixture
	// so the same valid repository update can be exercised after the rejection.
	app.projects.maxTransferBytes = 1 << 20
	refreshRes := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	if refreshRes.StatusCode != http.StatusOK {
		t.Fatalf("refresh status %d: %s", refreshRes.StatusCode, responseText(refreshRes))
	}
	refreshRes.Body.Close()
	refreshedRefs := projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID)
	featureRef := findProjectRef(refreshedRefs, "branch", "feature/search")
	if featureRef.Commit != featureAfter || featureRef.Commit == featureAtCreate {
		t.Fatalf("refresh returned branch %q, initial %q, want %q", featureRef.Commit, featureAtCreate, featureAfter)
	}
	branchDeployResponse := projectPost(t, client, appServer.URL, cookie, "/api/deployments/"+branchDeployment.ID+"/deploy", `{}`)
	if branchDeployResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("branch deploy status %d: %s", branchDeployResponse.StatusCode, responseText(branchDeployResponse))
	}
	branchDeployResponse.Body.Close()
	updatedContent := string(largePayload) + "\n"
	updateDeadline := time.Now().Add(5 * time.Second)
	for {
		published, readErr := os.ReadFile(filepath.Join(branchPath, "README.md"))
		if readErr == nil && string(published) == updatedContent {
			break
		}
		if time.Now().After(updateDeadline) {
			t.Fatal("ordinary Deploy did not publish the latest fetched branch tip")
		}
		time.Sleep(10 * time.Millisecond)
	}
	lastSuccessful := listProjectsFromAPI(t, client, appServer.URL, cookie)[0].LastFetched
	if lastSuccessful == nil {
		t.Fatal("successful refresh has no timestamp")
	}
	unchangedRefresh := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	if unchangedRefresh.StatusCode != http.StatusOK {
		t.Fatalf("unchanged refresh status %d: %s", unchangedRefresh.StatusCode, responseText(unchangedRefresh))
	}
	unchangedRefresh.Body.Close()
	lastSuccessful = listProjectsFromAPI(t, client, appServer.URL, cookie)[0].LastFetched
	// This highly compressible blob is much larger unpacked than the smart-HTTPS
	// pack carrying it. Keep refs under their limit so the cache byte ceiling is
	// the only gate that can reject this fetch.
	compressibleCommit := commitFixturePayload(t, remoteDir, fixtureWorktree, strings.Repeat("z", 8<<20)).String()
	liveCacheBeforeRejectedRefresh, err := digestProjectCache(app.projects.repoPath(created.ID))
	if err != nil {
		t.Fatal(err)
	}
	lastFetchedBeforeRejectedRefresh := *lastSuccessful
	oldCacheLimit := app.projects.maxCacheBytes
	liveBytes, err := directorySize(app.projects.repoPath(created.ID), oldCacheLimit)
	if err != nil {
		t.Fatal(err)
	}
	// Leave only 1 KiB of staged-write headroom. The packed blob is far smaller
	// than its expanded content but still exceeds this exact storage allowance.
	app.projects.maxCacheBytes = liveBytes + 1024
	measuredPackBytes.Store(0)
	measurePack.Store(true)
	cacheLimitedRefresh := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	measurePack.Store(false)
	cacheLimitMessage := responseText(cacheLimitedRefresh)
	cacheLimitedRefresh.Body.Close()
	app.projects.maxCacheBytes = oldCacheLimit
	if measuredPackBytes.Load() <= 1024 || measuredPackBytes.Load() >= 1<<20 {
		t.Fatalf("high-compressibility smart-HTTPS pack bytes = %d, want >1 KiB storage headroom and <1 MiB transfer ceiling", measuredPackBytes.Load())
	}
	if cacheLimitedRefresh.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(strings.ToLower(cacheLimitMessage), "cache limit") {
		t.Fatalf("compressed-object cache limit status/message = %d, %q; want 413 with cache-limit class", cacheLimitedRefresh.StatusCode, cacheLimitMessage)
	}
	assertPreviousCache := func(reason string) {
		t.Helper()
		if got := projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID); !equalProjectRefs(refreshedRefs, got) {
			t.Fatalf("%s changed cached refs: %#v", reason, got)
		}
		projectAfter := listProjectsFromAPI(t, client, appServer.URL, cookie)[0]
		if projectAfter.LastFetched == nil || !projectAfter.LastFetched.Equal(lastFetchedBeforeRejectedRefresh) {
			t.Fatalf("%s changed persistent last-fetched metadata", reason)
		}
		digest, err := digestProjectCache(app.projects.repoPath(created.ID))
		if err != nil {
			t.Fatal(err)
		}
		if digest != liveCacheBeforeRejectedRefresh {
			t.Fatalf("%s changed on-disk project objects or refs", reason)
		}
		cachedRepo, err := git.PlainOpen(app.projects.repoPath(created.ID))
		if err != nil {
			t.Fatal(err)
		}
		cachedCommit, err := cachedRepo.CommitObject(plumbing.NewHash(featureRef.Commit))
		if err != nil {
			t.Fatalf("%s lost previous cached commit: %v", reason, err)
		}
		cachedTree, err := cachedCommit.Tree()
		if err != nil {
			t.Fatalf("%s lost previous cached tree: %v", reason, err)
		}
		if _, err := cachedTree.File("README.md"); err != nil {
			t.Fatalf("%s lost previous cached blob: %v", reason, err)
		}
		if persistentBytes, sizeErr := directorySize(app.projects.root, oldCacheLimit); sizeErr != nil || persistentBytes > oldCacheLimit {
			t.Fatalf("%s exceeded persistent cache limit: bytes=%d err=%v", reason, persistentBytes, sizeErr)
		}
	}
	assertPreviousCache("compressed-object cache-limit rejection")

	for i := 0; i < 12; i++ {
		name := plumbing.NewBranchReferenceName(fmt.Sprintf("refs-test/%02d", i))
		if err := fixtureRepo.Storer.SetReference(plumbing.NewHashReference(name, plumbing.NewHash(compressibleCommit))); err != nil {
			t.Fatal(err)
		}
	}
	measuredPackBytes.Store(0)
	measurePack.Store(true)
	tooManyRefs := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	measurePack.Store(false)
	if tooManyRefs.StatusCode != http.StatusBadGateway {
		t.Fatalf("excessive refs refresh status %d: %s", tooManyRefs.StatusCode, responseText(tooManyRefs))
	}
	refLimitMessage := responseText(tooManyRefs)
	tooManyRefs.Body.Close()
	if !strings.Contains(strings.ToLower(refLimitMessage), "ref") {
		t.Fatalf("excessive refs error is not actionable: %s", refLimitMessage)
	}
	if measuredPackBytes.Load() <= 0 || measuredPackBytes.Load() >= 1<<20 {
		t.Fatalf("ref-limit control pack bytes = %d, want positive and below 1 MiB", measuredPackBytes.Load())
	}
	assertPreviousCache("ref-limit rejection")
	for i := 0; i < 12; i++ {
		name := plumbing.NewBranchReferenceName(fmt.Sprintf("refs-test/%02d", i))
		if err := fixtureRepo.Storer.RemoveReference(name); err != nil {
			t.Fatal(err)
		}
	}

	hardMovedTagJobID := moveAnnotatedTagAndTestHardRefresh(t, client, appServer.URL, cookie, app, fixtureRepo, &failRequests, tagDeployment, featureAfter, largePayload)
	refreshedRefs = projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID)
	lastSuccessful = listProjectsFromAPI(t, client, appServer.URL, cookie)[0].LastFetched

	// A genuinely failed refresh must keep refs and the timestamp from the last
	// successful smart-HTTPS exchange, while making the error visible.
	failRequests.Store(true)
	failedRes := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	if failedRes.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed refresh status %d: %s", failedRes.StatusCode, responseText(failedRes))
	}
	failedRes.Body.Close()
	afterFailure := listProjectsFromAPI(t, client, appServer.URL, cookie)[0]
	if afterFailure.Warning == "" {
		t.Fatal("failed refresh did not persist a warning")
	}
	if afterFailure.LastFetched == nil || !afterFailure.LastFetched.Equal(*lastSuccessful) {
		t.Fatalf("failed refresh changed last successful timestamp from %v to %v", lastSuccessful, afterFailure.LastFetched)
	}
	if got := projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID); !equalProjectRefs(refreshedRefs, got) {
		t.Fatalf("failed refresh altered cached refs: before %#v after %#v", refreshedRefs, got)
	}

	// Deleting a deployment while its branch fetch is in flight must cancel the
	// fetch and prevent the worker from reaching publication.
	failRequests.Store(false)
	refreshedRefs = projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID)
	mainRef := findProjectRef(refreshedRefs, "branch", "main")
	cancelledCreate := projectPost(t, client, appServer.URL, cookie, "/api/deployments", `{"projectId":"`+created.ID+`","name":"cancelled branch fetch","targetKind":"branch","targetName":"main","commit":"`+mainRef.Commit+`"}`)
	if cancelledCreate.StatusCode != http.StatusCreated {
		t.Fatalf("cancellable branch deployment status %d: %s", cancelledCreate.StatusCode, responseText(cancelledCreate))
	}
	var cancelledDeployment struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(cancelledCreate.Body).Decode(&cancelledDeployment); err != nil {
		t.Fatal(err)
	}
	cancelledCreate.Body.Close()
	initialDeadline := time.Now().Add(15 * time.Second)
	for {
		var status string
		_ = app.db.QueryRow(`SELECT status FROM deployments WHERE id=?`, cancelledDeployment.ID).Scan(&status)
		if status == "live" {
			break
		}
		if time.Now().After(initialDeadline) {
			t.Fatalf("cancellable branch deployment did not become live, status=%q", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	refreshedRefs = projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID)
	cacheBeforeCancellation, err := digestProjectCache(app.projects.repoPath(created.ID))
	if err != nil {
		t.Fatal(err)
	}
	pauseBranchFetch.Store(true)
	var cancelledGeneration int64
	if err := app.db.QueryRow(`SELECT generation FROM deployments WHERE id=?`, cancelledDeployment.ID).Scan(&cancelledGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := app.enqueueDeployment(cancelledDeployment.ID, mainRef.Commit, cancelledGeneration); err != nil {
		t.Fatalf("queue branch fetch before deletion: %v", err)
	}
	select {
	case <-branchFetchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("branch fetch did not enter the cancellable remote request")
	}
	app.mu.Lock()
	clear(app.rate)
	app.mu.Unlock()
	deleted := projectPost(t, client, appServer.URL, cookie, "/api/deployments/"+cancelledDeployment.ID+"/delete", `{"confirm":true}`)
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("delete during branch fetch status %d: %s", deleted.StatusCode, responseText(deleted))
	}
	deleted.Body.Close()
	if _, err := os.Stat(app.deploymentPath(cancelledDeployment.ID)); !os.IsNotExist(err) {
		t.Fatalf("cancelled branch fetch published or retained files: %v", err)
	}
	cacheAfterCancellation, err := digestProjectCache(app.projects.repoPath(created.ID))
	if err != nil || cacheAfterCancellation != cacheBeforeCancellation {
		t.Fatalf("cancelled branch fetch promoted its staged cache: before=%q after=%q err=%v", cacheBeforeCancellation, cacheAfterCancellation, err)
	}
	failRequests.Store(true)
	app.mu.Lock()
	clear(app.rate)
	app.mu.Unlock()
	restoreWarning := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	if restoreWarning.StatusCode != http.StatusBadGateway {
		t.Fatalf("restore failed-refresh status after cancellation test %d: %s", restoreWarning.StatusCode, responseText(restoreWarning))
	}
	restoreWarning.Body.Close()
	// Model an abrupt shutdown with a durable hard re-deploy in progress. Startup
	// must terminalize the job without losing its persisted force intent.
	if _, err := app.db.Exec(`UPDATE deployment_jobs SET status='running' WHERE id=?`, hardMovedTagJobID); err != nil {
		t.Fatal(err)
	}
	tagIdentityBeforeRestart, err := identity(tagDeployment.Path)
	if err != nil {
		t.Fatal(err)
	}

	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	appServer.Close()
	app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	cookie = installAPITestSession(t, app)
	restarted := httptest.NewTLSServer(app.Handler())
	defer restarted.Close()
	projects := listProjectsFromAPI(t, restarted.Client(), restarted.URL, cookie)
	if len(projects) != 1 || projects[0].ID != created.ID || projects[0].Warning == "" || projects[0].LastFetched == nil {
		t.Fatalf("project config/status did not survive restart: %#v", projects)
	}
	deploymentsRequest, _ := http.NewRequest(http.MethodGet, restarted.URL+"/api/deployments", nil)
	deploymentsRequest.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	deploymentsResponse, err := restarted.Client().Do(deploymentsRequest)
	if err != nil {
		t.Fatal(err)
	}
	var persistedDeployments []deployment
	if err = json.NewDecoder(deploymentsResponse.Body).Decode(&persistedDeployments); err != nil {
		deploymentsResponse.Body.Close()
		t.Fatal(err)
	}
	deploymentsResponse.Body.Close()
	persistedBranchTarget := false
	for _, item := range persistedDeployments {
		if item.ID == branchDeployment.ID && item.TargetKind == "branch" && item.TargetName == "feature/search" {
			persistedBranchTarget = true
		}
	}
	if !persistedBranchTarget {
		t.Fatal("configured branch target did not survive application restart")
	}
	assertTagDeploymentRestarted(t, restarted.Client(), restarted.URL, cookie, persistedDeployments, tagDeployment, hardMovedTagJobID, featureAfter, tagIdentityBeforeRestart)
	if got := projectRefsFromAPI(t, restarted.Client(), restarted.URL, cookie, created.ID); !equalProjectRefs(refreshedRefs, got) {
		t.Fatalf("cached refs did not survive restart: %#v", got)
	}
	redirectRequests.Store(true)
	redirectedCreate := projectPost(t, restarted.Client(), restarted.URL, cookie, "/api/projects", `{"name":"redirect","url":"`+remote.URL+`/repo.git"}`)
	if redirectedCreate.StatusCode != http.StatusBadRequest {
		t.Fatalf("redirected smart HTTPS creation status %d: %s", redirectedCreate.StatusCode, responseText(redirectedCreate))
	}
	redirectedCreate.Body.Close()
	if redirectedHits.Load() != 0 {
		t.Fatalf("Git client followed a repository redirect %d times", redirectedHits.Load())
	}
	if got := listProjectsFromAPI(t, restarted.Client(), restarted.URL, cookie); len(got) != 1 {
		t.Fatalf("redirected repository was saved: %#v", got)
	}
	usedCache, err := directorySize(app.projects.root, app.projects.maxCacheBytes)
	if err != nil {
		t.Fatal(err)
	}
	oldPersistentCacheLimit := app.projects.maxCacheBytes
	app.projects.maxCacheBytes = usedCache
	requestsBeforeCacheLimit := gitRequests.Load()
	cacheLimitedCreate := projectPost(t, restarted.Client(), restarted.URL, cookie, "/api/projects", `{"name":"budget limited","url":"`+remote.URL+`/repo.git"}`)
	if cacheLimitedCreate.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("cache-limited project creation status %d: %s", cacheLimitedCreate.StatusCode, responseText(cacheLimitedCreate))
	}
	cacheLimitedCreate.Body.Close()
	if gitRequests.Load() != requestsBeforeCacheLimit || len(listProjectsFromAPI(t, restarted.Client(), restarted.URL, cookie)) != 1 {
		t.Fatal("cache budget was checked only after starting another repository transfer")
	}
	app.projects.maxCacheBytes = oldPersistentCacheLimit
	failRequests.Store(false)
	redirectRequests.Store(false)
	cacheBeforeRollbackFailure, err := digestProjectCache(app.projects.repoPath(created.ID))
	if err != nil {
		t.Fatal(err)
	}
	app.projects.cacheFS = &faultProjectCacheFS{
		projectCacheFS:   osProjectCacheFS{},
		failRenameTarget: app.projects.repoPath(created.ID),
		failRenameCount:  2, // promotion fails, then restoration fails deterministically
	}
	requestsBeforeRollbackFailure := gitRequests.Load()
	rollbackFailure := projectPost(t, restarted.Client(), restarted.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	rollbackMessage := responseText(rollbackFailure)
	rollbackFailure.Body.Close()
	if rollbackFailure.StatusCode != http.StatusServiceUnavailable || !strings.Contains(strings.ToLower(rollbackMessage), "restoration failed") {
		t.Fatalf("rollback failure was not surfaced/blocked: status %d message %q", rollbackFailure.StatusCode, rollbackMessage)
	}
	requestsAfterRollbackFailure := gitRequests.Load()
	if requestsAfterRollbackFailure <= requestsBeforeRollbackFailure {
		t.Fatal("rollback-failure fixture did not reach smart HTTPS")
	}
	blockedRetry := projectPost(t, restarted.Client(), restarted.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	blockedMessage := responseText(blockedRetry)
	blockedRetry.Body.Close()
	if blockedRetry.StatusCode != http.StatusServiceUnavailable || !strings.Contains(strings.ToLower(blockedMessage), "blocked") || gitRequests.Load() != requestsAfterRollbackFailure {
		t.Fatalf("cache recovery block was not enforced: status=%d message=%q requests=%d", blockedRetry.StatusCode, blockedMessage, gitRequests.Load())
	}
	if _, err := os.Stat(app.projects.repoPath(created.ID)); !os.IsNotExist(err) {
		t.Fatalf("injected restoration failure should leave live path missing for recovery, stat error=%v", err)
	}
	if _, err := os.Stat(app.projects.refreshBackupPath(created.ID)); err != nil {
		t.Fatalf("previous cache backup was not retained for recovery: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	restarted.Close()
	app, err = New(cfg)
	if err != nil {
		t.Fatalf("startup did not recover interrupted promotion: %v", err)
	}
	cookie = installAPITestSession(t, app)
	recoveredServer := httptest.NewTLSServer(app.Handler())
	defer recoveredServer.Close()
	recoveredDigest, err := digestProjectCache(app.projects.repoPath(created.ID))
	if err != nil {
		t.Fatal(err)
	}
	if recoveredDigest != cacheBeforeRollbackFailure {
		t.Fatal("startup recovery did not restore previous cache contents")
	}
	if exists, err := app.projects.refreshIntentExists(created.ID); err != nil || exists {
		t.Fatalf("startup left refresh journal pending: exists=%v err=%v", exists, err)
	}

	// Cleanup failures block further cache work; a restart removes the owned
	// orphan stage before the application accepts requests again.
	app.projects.cacheFS = &faultProjectCacheFS{projectCacheFS: osProjectCacheFS{}, failRemovePrefix: ".git-stage-refresh-"}
	failRequests.Store(true)
	requestsBeforeCleanupFailure := gitRequests.Load()
	cleanupFailure := projectPost(t, recoveredServer.Client(), recoveredServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	cleanupMessage := responseText(cleanupFailure)
	cleanupFailure.Body.Close()
	if cleanupFailure.StatusCode != http.StatusServiceUnavailable || !strings.Contains(strings.ToLower(cleanupMessage), "staging cleanup failed") {
		t.Fatalf("staging cleanup failure was hidden: status=%d message=%q", cleanupFailure.StatusCode, cleanupMessage)
	}
	if gitRequests.Load() <= requestsBeforeCleanupFailure {
		t.Fatal("cleanup failure fixture did not reach the smart-HTTPS boundary")
	}
	blockedRetry = projectPost(t, recoveredServer.Client(), recoveredServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	blockedRetry.Body.Close()
	if blockedRetry.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("cleanup-failed cache accepted retry: %d", blockedRetry.StatusCode)
	}
	if err := app.projects.reconcileCacheFiles(); err == nil {
		t.Fatal("startup reconciliation accepted an orphan stage it could not remove")
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	recoveredServer.Close()
	app, err = New(cfg)
	if err != nil {
		t.Fatalf("startup did not remove orphan staged cache: %v", err)
	}
	defer app.Close()
	entries, err := os.ReadDir(app.projects.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".git-stage-") {
			t.Fatalf("orphan cache stage survived startup reconciliation: %s", entry.Name())
		}
	}
	cacheBeforeLoweredLimit, err := digestProjectCache(app.projects.repoPath(created.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	recoveredServer.Close()
	loweredLimitConfig := cfg
	loweredLimitConfig.GitMaxCacheBytes = 1
	if overLimitApp, startupErr := New(loweredLimitConfig); startupErr == nil {
		_ = overLimitApp.Close()
		t.Fatal("restart accepted committed project caches over the lowered cache limit")
	} else if !strings.Contains(strings.ToLower(startupErr.Error()), "cache") || !strings.Contains(strings.ToLower(startupErr.Error()), "limit") {
		t.Fatalf("lowered cache limit startup error is not actionable: %v", startupErr)
	}
	cacheAfterLoweredLimit, err := digestProjectCache(filepath.Join(cfg.DatabasePath+".projects", created.ID))
	if err != nil {
		t.Fatal(err)
	}
	if cacheAfterLoweredLimit != cacheBeforeLoweredLimit {
		t.Fatal("lowered cache limit startup deleted or modified the committed project cache")
	}
	if _, err := os.Lstat(filepath.Join(cfg.DatabasePath+".projects", ".git-stage-refresh-"+created.ID)); !os.IsNotExist(err) {
		t.Fatalf("lowered cache limit startup allocated a refresh stage: %v", err)
	}
	app, err = New(cfg)
	if err != nil {
		t.Fatalf("original cache limit could not reopen preserved project cache: %v", err)
	}
	defer app.Close()
	cookie = installAPITestSession(t, app)
	branchServer := httptest.NewTLSServer(app.Handler())
	defer branchServer.Close()
	if err := fixtureRepo.Storer.RemoveReference(plumbing.NewBranchReferenceName("feature/search")); err != nil {
		t.Fatal(err)
	}
	beforeMissingBranch, err := identity(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	missingBranchResponse := projectPost(t, branchServer.Client(), branchServer.URL, cookie, "/api/deployments/"+branchDeployment.ID+"/deploy", `{}`)
	if missingBranchResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("missing-branch deploy status %d: %s", missingBranchResponse.StatusCode, responseText(missingBranchResponse))
	}
	var missingBranchJob struct {
		JobID string `json:"jobId"`
	}
	if err := json.NewDecoder(missingBranchResponse.Body).Decode(&missingBranchJob); err != nil {
		t.Fatal(err)
	}
	missingBranchResponse.Body.Close()
	waitForDeploymentJob(t, branchServer.Client(), branchServer.URL, cookie, branchDeployment.ID, missingBranchJob.JobID, "failed")
	testRejectedRollbackKeepsLive(t, branchServer.Client(), branchServer.URL, cookie, branchDeployment.ID, featureAtCreate, branchPath)
	if afterMissingBranch, identityErr := identity(branchPath); identityErr != nil || afterMissingBranch != beforeMissingBranch {
		t.Fatalf("deleted branch changed the live publication: before=%+v after=%+v err=%v", beforeMissingBranch, afterMissingBranch, identityErr)
	}
	// GitHub push automation follows the fetched branch tip, not a possibly stale
	// event SHA, and independently updates every deployment following that branch.
	featureBranch := plumbing.NewBranchReferenceName("feature/search")
	if err := fixtureRepo.Storer.SetReference(plumbing.NewHashReference(featureBranch, plumbing.NewHash(featureAfter))); err != nil {
		t.Fatal(err)
	}
	if err := source.Storer.SetReference(plumbing.NewHashReference(featureBranch, plumbing.NewHash(featureAfter))); err != nil {
		t.Fatal(err)
	}
	// A prior cache-cleanup scenario deliberately held the controlled Git
	// server unavailable. Restore the fixture, then compare an ordinary manual
	// branch deploy with the webhook-triggered fetch against this same remote.
	failRequests.Store(false)
	manualFetch := projectPost(t, branchServer.Client(), branchServer.URL, cookie, "/api/deployments/"+branchDeployment.ID+"/deploy", `{}`)
	if manualFetch.StatusCode != http.StatusAccepted {
		t.Fatalf("manual branch deploy status %d: %s", manualFetch.StatusCode, responseText(manualFetch))
	}
	var manualJob struct {
		JobID string `json:"jobId"`
	}
	if err := json.NewDecoder(manualFetch.Body).Decode(&manualJob); err != nil {
		t.Fatal(err)
	}
	manualFetch.Body.Close()
	waitForDeploymentJob(t, branchServer.Client(), branchServer.URL, cookie, branchDeployment.ID, manualJob.JobID, "noop")
	mirrorID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,target_kind,target_name,generation,status,created_at,updated_at) VALUES(?,?,?,?,'branch','feature/search',1,'live',?,?)`, mirrorID, created.ID, "feature-mirror", featureAfter, time.Now().UnixNano(), time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	clear(app.rate)
	app.mu.Unlock()
	setupResponse := projectPost(t, branchServer.Client(), branchServer.URL, cookie, "/api/projects/"+created.ID+"/github-webhook", `{"rotate":true}`)
	var webhookSetup struct{ URL, Secret string }
	if setupResponse.StatusCode != http.StatusCreated || json.NewDecoder(setupResponse.Body).Decode(&webhookSetup) != nil || len(webhookSetup.Secret) != 64 {
		t.Fatalf("GitHub webhook setup status %d: %s", setupResponse.StatusCode, responseText(setupResponse))
	}
	setupResponse.Body.Close()
	webhookSecret := []byte(webhookSetup.Secret)
	rollbackBeforePush := projectPost(t, branchServer.Client(), branchServer.URL, cookie, "/api/deployments/"+branchDeployment.ID+"/rollback", `{"commit":"`+featureAtCreate+`"}`)
	var rollbackBeforePushJob struct{ JobID string }
	if rollbackBeforePush.StatusCode != http.StatusAccepted || json.NewDecoder(rollbackBeforePush.Body).Decode(&rollbackBeforePushJob) != nil {
		t.Fatalf("rollback before webhook status %d", rollbackBeforePush.StatusCode)
	}
	rollbackBeforePush.Body.Close()
	waitForDeploymentJob(t, branchServer.Client(), branchServer.URL, cookie, branchDeployment.ID, rollbackBeforePushJob.JobID, "succeeded")
	if content, err := os.ReadFile(filepath.Join(branchPath, "README.md")); err != nil || string(content) != "searchable feature\n" {
		t.Fatalf("rollback before matching webhook content=%q err=%v", content, err)
	}
	webhookRepo, err := git.PlainOpen(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	webhookWorktree, err := webhookRepo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := webhookWorktree.Checkout(&git.CheckoutOptions{Branch: featureBranch, Force: true}); err != nil {
		t.Fatal(err)
	}
	latestTip := commitFixtureFile(t, remoteDir, webhookWorktree, "feature", "webhook latest tip").String()
	if err := source.Storer.SetReference(plumbing.NewHashReference(featureBranch, plumbing.NewHash(latestTip))); err != nil {
		t.Fatal(err)
	}
	stalePush := []byte(`{"ref":"refs/heads/feature/search","after":"` + featureAfter + `","deleted":false}`)
	webhookURL := branchServer.URL + webhookSetup.URL
	pauseWebhookFetch.Store(true)
	if status := sendGitHubWebhook(branchServer.Client(), webhookURL, "github-delivery-1", "push", stalePush, webhookSecret); status != http.StatusNoContent {
		t.Fatalf("signed push status = %d, want 204", status)
	}
	select {
	case <-webhookFetchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("webhook-triggered branch fetch did not reach the controlled pause")
	}
	for i := 2; i <= 9; i++ {
		if status := sendGitHubWebhook(branchServer.Client(), webhookURL, fmt.Sprintf("github-burst-%d", i), "push", stalePush, webhookSecret); status != http.StatusNoContent {
			t.Fatalf("burst delivery %d status = %d, want 204", i, status)
		}
	}
	for _, deploymentID := range []string{branchDeployment.ID, mirrorID} {
		var pending int
		if err := app.db.QueryRow(`SELECT count(*) FROM deployment_jobs WHERE deployment_id=? AND durable=1 AND status IN ('queued','running')`, deploymentID).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending > 2 {
			t.Fatalf("webhook burst queued %d jobs for deployment %s, want at most one active plus one coalesced pending", pending, deploymentID)
		}
	}
	releaseWebhookFetch()
	for _, deploymentID := range []string{branchDeployment.ID, mirrorID} {
		waitForLiveCommit(t, app, deploymentID, latestTip)
		if content, readErr := os.ReadFile(filepath.Join(app.cfg.DeploymentRoot, deploymentID, "README.md")); readErr != nil || string(content) != "webhook latest tip\n" {
			t.Fatalf("deployment %s did not publish current branch tip: %q err=%v", deploymentID, content, readErr)
		}
	}
	mainBefore, err := identity(independentPath)
	if err != nil {
		t.Fatal(err)
	}
	if status := sendGitHubWebhook(branchServer.Client(), webhookURL, "github-delivery-1", "push", stalePush, webhookSecret); status != http.StatusNoContent {
		t.Fatalf("duplicate push status = %d, want 204", status)
	}
	if status := sendGitHubWebhook(branchServer.Client(), webhookURL, "github-tag-event", "create", []byte(`{"ref":"v2.0","ref_type":"tag"}`), webhookSecret); status != http.StatusNoContent {
		t.Fatalf("tag event status = %d, want 204", status)
	}
	deletePush := []byte(`{"ref":"refs/heads/feature/search","after":"` + latestTip + `","deleted":true}`)
	if status := sendGitHubWebhook(branchServer.Client(), webhookURL, "github-branch-deleted", "push", deletePush, webhookSecret); status != http.StatusNoContent {
		t.Fatalf("branch deletion status = %d, want 204", status)
	}
	if after, identityErr := identity(independentPath); identityErr != nil || after != mainBefore {
		t.Fatalf("unmatched branch/tag activity affected independent deployment: before=%+v after=%+v err=%v", mainBefore, after, identityErr)
	}
	if content, readErr := os.ReadFile(filepath.Join(branchPath, "README.md")); readErr != nil || string(content) != "webhook latest tip\n" {
		t.Fatalf("branch deletion changed the live publication: %q err=%v", content, readErr)
	}

	// Gitea shares the durable latest-tip pipeline, but never GitHub's secret or
	// delivery identity. Observe publication and protected job listings only.
	giteaSecret := setupProviderWebhook(t, branchServer, cookie, created.ID, "gitea")
	giteaURL := branchServer.URL + "/webhooks/gitea/" + created.ID
	rollbackBeforeGitea := projectPost(t, branchServer.Client(), branchServer.URL, cookie, "/api/deployments/"+branchDeployment.ID+"/rollback", `{"commit":"`+featureAtCreate+`"}`)
	var rollbackBeforeGiteaJob struct{ JobID string }
	if rollbackBeforeGitea.StatusCode != http.StatusAccepted || json.NewDecoder(rollbackBeforeGitea.Body).Decode(&rollbackBeforeGiteaJob) != nil {
		t.Fatalf("rollback before Gitea webhook status %d", rollbackBeforeGitea.StatusCode)
	}
	rollbackBeforeGitea.Body.Close()
	waitForDeploymentJob(t, branchServer.Client(), branchServer.URL, cookie, branchDeployment.ID, rollbackBeforeGiteaJob.JobID, "succeeded")
	if content, err := os.ReadFile(filepath.Join(branchPath, "README.md")); err != nil || string(content) != "searchable feature\n" {
		t.Fatalf("rollback before Gitea webhook content=%q err=%v", content, err)
	}
	giteaTip := commitFixtureFile(t, remoteDir, webhookWorktree, "feature", "Gitea latest tip").String()
	if err := source.Storer.SetReference(plumbing.NewHashReference(featureBranch, plumbing.NewHash(giteaTip))); err != nil {
		t.Fatal(err)
	}
	// This delivery ID already exists for GitHub in this project.
	if status := sendGiteaWebhook(branchServer.Client(), giteaURL, "github-delivery-1", "push", stalePush, giteaSecret); status != 204 {
		t.Fatalf("cross-provider push = %d", status)
	}
	for _, deploymentID := range []string{branchDeployment.ID, mirrorID} {
		waitForPublishedFile(t, filepath.Join(app.cfg.DeploymentRoot, deploymentID, "README.md"), "Gitea latest tip\n")
		waitForPublicJobsSettled(t, branchServer, cookie, deploymentID)
	}
	beforeIgnored := publicDeploymentJobs(t, branchServer, cookie, branchDeployment.ID)
	for _, event := range []struct{ id, event, body string }{
		{"github-delivery-1", "push", string(stalePush)},
		{"gitea-delete", "push", `{"ref":"refs/heads/feature/search","after":"` + strings.Repeat("0", 40) + `"}`},
		{"gitea-delete-event", "delete", `{"ref":"feature/search","ref_type":"branch"}`},
		{"gitea-tag", "push", `{"ref":"refs/tags/v2","after":"` + giteaTip + `"}`},
		{"gitea-unsupported", "issues", `{"action":"opened"}`},
	} {
		if status := sendGiteaWebhook(branchServer.Client(), giteaURL, event.id, event.event, []byte(event.body), giteaSecret); status != 204 {
			t.Fatalf("%s status = %d", event.id, status)
		}
	}
	if afterIgnored := publicDeploymentJobs(t, branchServer, cookie, branchDeployment.ID); len(afterIgnored) != len(beforeIgnored) {
		t.Fatalf("duplicate/deletion/non-branch activity queued jobs: before=%d after=%d", len(beforeIgnored), len(afterIgnored))
	}
	// The same ID in another project must also cause publication.
	// Earlier limit scenarios added large reachable fixture objects; this slice
	// tests identity isolation rather than the deliberately tiny transfer cap.
	app.projects.maxTransferBytes = 32 << 20
	second := projectPost(t, branchServer.Client(), branchServer.URL, cookie, "/api/projects", `{"name":"second Gitea project","url":"`+remote.URL+`/repo.git"}`)
	var secondProject project
	if second.StatusCode != 201 || json.NewDecoder(second.Body).Decode(&secondProject) != nil {
		t.Fatalf("second project = %d: %s", second.StatusCode, responseText(second))
	}
	second.Body.Close()
	secondSecret := setupProviderWebhook(t, branchServer, cookie, secondProject.ID, "gitea")
	deployment := projectPost(t, branchServer.Client(), branchServer.URL, cookie, "/api/deployments", `{"projectId":"`+secondProject.ID+`","name":"second Gitea site","targetKind":"branch","targetName":"feature/search"}`)
	var secondDeployment struct{ ID string }
	if deployment.StatusCode != 201 || json.NewDecoder(deployment.Body).Decode(&secondDeployment) != nil {
		t.Fatalf("second deployment = %d: %s", deployment.StatusCode, responseText(deployment))
	}
	deployment.Body.Close()
	waitForPublishedFile(t, filepath.Join(app.cfg.DeploymentRoot, secondDeployment.ID, "README.md"), "Gitea latest tip\n")
	waitForPublicJobsSettled(t, branchServer, cookie, secondDeployment.ID)
	secondTip := commitFixtureFile(t, remoteDir, webhookWorktree, "feature", "second Gitea project tip").String()
	if err := source.Storer.SetReference(plumbing.NewHashReference(featureBranch, plumbing.NewHash(secondTip))); err != nil {
		t.Fatal(err)
	}
	if status := sendGiteaWebhook(branchServer.Client(), branchServer.URL+"/webhooks/gitea/"+secondProject.ID, "github-delivery-1", "push", stalePush, secondSecret); status != 204 {
		t.Fatalf("cross-project push = %d", status)
	}
	waitForPublishedFile(t, filepath.Join(app.cfg.DeploymentRoot, secondDeployment.ID, "README.md"), "second Gitea project tip\n")
	if content, err := os.ReadFile(filepath.Join(branchPath, "README.md")); err != nil || string(content) != "Gitea latest tip\n" {
		t.Fatalf("other project affected first publication: %q %v", content, err)
	}
}

func publicDeploymentJobs(t *testing.T, server *httptest.Server, cookie, id string) []deploymentJob {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/deployments/"+id+"/jobs", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var jobs []deploymentJob
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&jobs) != nil {
		t.Fatalf("job list = %d", res.StatusCode)
	}
	return jobs
}

func waitForPublicJobsSettled(t *testing.T, server *httptest.Server, cookie, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		active := false
		for _, job := range publicDeploymentJobs(t, server, cookie, id) {
			active = active || job.Status == "queued" || job.Status == "running"
		}
		if !active {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("jobs did not settle")
}

func waitForPublishedFile(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if content, err := os.ReadFile(path); err == nil && string(content) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	content, err := os.ReadFile(path)
	t.Fatalf("publication = %q (%v), want %q", content, err, want)
}

func waitForDeploymentWorkersIdle(t *testing.T, app *App) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var active int
		if err := app.db.QueryRow(`SELECT count(*) FROM deployment_jobs WHERE status IN ('queued','running')`).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("deployment jobs did not settle before fault injection")
}

func waitForDeploymentJob(t *testing.T, client *http.Client, origin, cookie, deploymentID, jobID, want string) {
	t.Helper()
	// Fetch, export, hydration and fsync are asynchronous work, not an HTTP
	// response-time assertion. Allow controlled slow publications to finish;
	// fail immediately on an unexpected terminal outcome rather than retrying it.
	deadline := time.Now().Add(30 * time.Second)
	last := "not listed"
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, origin+"/api/deployments/"+deploymentID+"/jobs", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var jobs []deploymentJob
		err = json.NewDecoder(response.Body).Decode(&jobs)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range jobs {
			if job.ID == jobID {
				last = job.Status + ": " + job.Log
			}
			if job.ID == jobID && job.Status == want {
				return
			}
			if job.ID == jobID && (job.Status == "succeeded" || job.Status == "failed" || job.Status == "superseded" || job.Status == "noop") {
				t.Fatalf("deployment job %s reached unexpected terminal outcome: %s; want %s", jobID, last, want)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("deployment job %s did not reach status %s (last=%s)", jobID, want, last)
}

func waitForLiveCommit(t *testing.T, app *App, deploymentID, commit string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var live string
		if err := app.db.QueryRow(`SELECT live_commit FROM deployments WHERE id=?`, deploymentID).Scan(&live); err == nil && live == commit {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	var live string
	_ = app.db.QueryRow(`SELECT live_commit FROM deployments WHERE id=?`, deploymentID).Scan(&live)
	var status, message, jobStatus, jobLog, jobCommit string
	_ = app.db.QueryRow(`SELECT status,error FROM deployments WHERE id=?`, deploymentID).Scan(&status, &message)
	_ = app.db.QueryRow(`SELECT status,log,commit_id FROM deployment_jobs WHERE deployment_id=? ORDER BY created_at DESC LIMIT 1`, deploymentID).Scan(&jobStatus, &jobLog, &jobCommit)
	t.Fatalf("deployment %s live commit=%s status=%s error=%q latest-job=%s commit=%s %q, want latest branch tip %s", deploymentID, live, status, message, jobStatus, jobCommit, jobLog, commit)
}

type countingHTTPWriter struct {
	http.ResponseWriter
	count *atomic.Int64
}

func (w *countingHTTPWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.count.Add(int64(n))
	return n, err
}

type faultProjectCacheFS struct {
	projectCacheFS
	failRenameTarget string
	failRenameCount  int
	failRemovePrefix string
	renameAttempts   int
}

func (f *faultProjectCacheFS) Rename(oldPath, newPath string) error {
	if newPath == f.failRenameTarget && f.renameAttempts < f.failRenameCount {
		f.renameAttempts++
		return errors.New("injected cache rename failure")
	}
	return f.projectCacheFS.Rename(oldPath, newPath)
}

func (f *faultProjectCacheFS) RemoveAll(path string) error {
	if f.failRemovePrefix != "" && strings.HasPrefix(filepath.Base(path), f.failRemovePrefix) {
		return errors.New("injected cache cleanup failure")
	}
	return f.projectCacheFS.RemoveAll(path)
}

func TestBoundedGitFilesystemRejectsExpandedObjectWriteAtStorageBoundary(t *testing.T) {
	root := t.TempDir()
	fs := newBoundedGitFS(root, 64<<10)
	file, err := fs.Create("objects/expanded")
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1<<20)
	if _, err := file.Write(payload); !errors.Is(err, errGitCacheLimit) {
		t.Fatalf("expanded object write error = %v, want cache limit", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	used, err := directorySize(root, 64<<10)
	if err != nil || used > 64<<10 {
		t.Fatalf("storage boundary exceeded 64 KiB: bytes=%d err=%v", used, err)
	}
}

func digestProjectCache(root string) ([32]byte, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = io.WriteString(h, rel+"\x00")
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		_, _ = io.WriteString(h, info.Mode().String()+"\x00")
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(h, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest, err
}

func installTrustedSmartHTTPTestClient(t *testing.T, server *httptest.Server) {
	t.Helper()
	previous := gitclient.Protocols["https"]
	leaf := server.TLS.Certificates[0].Leaf
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	installProjectHTTPSClient(client)
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		gitclient.InstallProtocol("https", previous)
	})
}

func createSmartFixture(t *testing.T, dir string) string {
	t.Helper()
	main := plumbing.NewBranchReferenceName("main")
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{InitOptions: git.InitOptions{DefaultBranch: main}})
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	commitFixtureFile(t, dir, worktree, "main", "main content")
	feature := plumbing.NewBranchReferenceName("feature/search")
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: feature, Create: true}); err != nil {
		t.Fatal(err)
	}
	featureHash := commitFixtureFile(t, dir, worktree, "feature", "searchable feature")
	if _, err := repo.CreateTag("v1.0", featureHash, nil); err != nil {
		t.Fatal(err)
	}
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: main}); err != nil {
		t.Fatal(err)
	}
	return featureHash.String()
}

func addSmartFixtureCommit(t *testing.T, dir, content string) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("feature/search")}); err != nil {
		t.Fatal(err)
	}
	return commitFixtureFile(t, dir, worktree, "feature", content).String()
}

func commitFixtureFile(t *testing.T, dir string, worktree *git.Worktree, author, contents string) plumbing.Hash {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(contents+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	returned, err := worktree.Commit(contents, &git.CommitOptions{Author: &object.Signature{Name: author, Email: author + "@example.test", When: time.Now().UTC()}})
	if err != nil {
		t.Fatal(err)
	}
	return returned
}

func commitFixturePayload(t *testing.T, dir string, worktree *git.Worktree, contents string) plumbing.Hash {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	hash, err := worktree.Commit("compressible fixture payload", &git.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.test", When: time.Now().UTC()}})
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func installAPITestSession(t *testing.T, app *App) string {
	t.Helper()
	token := []byte("0123456789abcdef0123456789abcdef")
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration, authenticatedAt: time.Now()}
	app.mu.Unlock()
	return base64.RawURLEncoding.EncodeToString(token)
}

func projectPost(t *testing.T, client *http.Client, origin, cookie, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, origin+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func listProjectsFromAPI(t *testing.T, client *http.Client, origin, cookie string) []project {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, origin+"/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list projects status %d: %s", response.StatusCode, responseText(response))
	}
	var result []project
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func projectRefsFromAPI(t *testing.T, client *http.Client, origin, cookie, id string) []projectRef {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, origin+"/api/projects/"+id+"/refs", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("project refs status %d: %s", response.StatusCode, responseText(response))
	}
	var result []projectRef
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func requireProjectRefs(t *testing.T, refs []projectRef, featureID string) {
	t.Helper()
	for _, want := range []struct{ kind, name string }{{"branch", "main"}, {"branch", "feature/search"}, {"tag", "v1.0"}} {
		found := false
		for _, ref := range refs {
			if ref.Kind == want.kind && ref.Name == want.name {
				found = true
				if len(ref.Commit) != 40 {
					t.Errorf("ref %s has a non-full commit ID %q", ref.Name, ref.Commit)
				}
				if want.name == "feature/search" && ref.Commit != featureID {
					t.Errorf("feature ref commit %s, want %s", ref.Commit, featureID)
				}
			}
		}
		if !found {
			t.Errorf("missing %s %s in refs %#v", want.kind, want.name, refs)
		}
	}
}

func findProjectRef(refs []projectRef, kind, name string) projectRef {
	for _, ref := range refs {
		if ref.Kind == kind && ref.Name == name {
			return ref
		}
	}
	return projectRef{}
}

func equalProjectRefs(a, b []projectRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func responseText(response *http.Response) string {
	data, _ := io.ReadAll(response.Body)
	return string(data)
}

type missingObjectStorer struct {
	storer.Storer
	missing plumbing.Hash
}

func (s missingObjectStorer) EncodedObject(kind plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	if hash == s.missing {
		return nil, plumbing.ErrObjectNotFound
	}
	return s.Storer.EncodedObject(kind, hash)
}

func (s missingObjectStorer) HasEncodedObject(hash plumbing.Hash) error {
	if hash == s.missing {
		return plumbing.ErrObjectNotFound
	}
	return s.Storer.HasEncodedObject(hash)
}
