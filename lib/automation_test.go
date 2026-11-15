package lib

import (
	"encoding/json"
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
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
)

type automationFixture struct {
	app                                         *App
	server                                      *httptest.Server
	cookie, projectID, remoteDir, initialCommit string
	secret                                      []byte
	provider                                    string
	gitFetches                                  atomic.Int32
	fetchFailureStatus                          atomic.Int32
}

func newAutomationFixture(t *testing.T, providers ...string) *automationFixture {
	t.Helper()
	f := &automationFixture{remoteDir: filepath.Join(t.TempDir(), "repo")}
	f.provider = "github"
	if len(providers) > 0 {
		f.provider = providers[0]
	}
	f.initialCommit = createSmartFixture(t, f.remoteDir)
	repo, err := git.PlainOpen(f.remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	// An in-process smart HTTPS fixture, with the same protocol boundary as
	// production. No local Git/SSH processes are used by the application.
	var endpoint *transport.Endpoint
	var backend transport.Transport
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := int(f.fetchFailureStatus.Load()); status != 0 && r.Method == http.MethodGet {
			f.gitFetches.Add(1)
			http.Error(w, "controlled fetch failure Authorization: Bearer private-test-secret", status)
			return
		}
		session, err := backend.NewUploadPackSession(endpoint, nil)
		if err != nil {
			http.Error(w, "session unavailable", 500)
			return
		}
		defer session.Close()
		if r.Method == http.MethodGet {
			f.gitFetches.Add(1)
			adv, err := session.AdvertisedReferencesContext(r.Context())
			if err != nil {
				http.Error(w, "advertisement failed", 500)
				return
			}
			adv.Prefix = [][]byte{[]byte("# service=git-upload-pack"), pktline.Flush}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_ = adv.Encode(w)
			return
		}
		_, _ = session.AdvertisedReferencesContext(r.Context())
		request := packp.NewUploadPackRequest()
		scanner := pktline.NewScanner(r.Body)
		for scanner.Scan() {
			line := string(scanner.Bytes())
			if strings.HasPrefix(line, "want ") {
				request.Wants = append(request.Wants, plumbing.NewHash(strings.Fields(line)[1]))
			}
			if strings.HasPrefix(line, "have ") {
				request.Haves = append(request.Haves, plumbing.NewHash(strings.TrimSpace(strings.TrimPrefix(line, "have "))))
			}
		}
		response, err := session.UploadPack(r.Context(), request)
		if err != nil {
			http.Error(w, "upload failed", 500)
			return
		}
		defer response.Close()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		_ = response.Encode(w)
	}))
	t.Cleanup(remote.Close)
	endpoint, err = transport.NewEndpoint(remote.URL + "/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	backend = gitserver.NewServer(gitserver.MapLoader{endpoint.String(): repo.Storer})
	installTrustedSmartHTTPTestClient(t, remote)
	dir := t.TempDir()
	f.app, err = New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), FetchRetryAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.app.cfg.AuthRequestsPerMinute = 1000
	t.Cleanup(func() { _ = f.app.Close() })
	f.cookie = installAPITestSession(t, f.app)
	f.server = httptest.NewTLSServer(f.app.Handler())
	t.Cleanup(func() { f.server.Close() })
	response := projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/projects", `{"name":"fixture","url":"`+remote.URL+`/repo.git"}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("project: %d %s", response.StatusCode, responseText(response))
	}
	var project struct{ ID string }
	if err = json.NewDecoder(response.Body).Decode(&project); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	f.projectID = project.ID
	response = projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/projects/"+f.projectID+"/"+f.provider+"-webhook", `{"rotate":true}`)
	var setup struct{ Secret string }
	if err = json.NewDecoder(response.Body).Decode(&setup); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	f.secret = []byte(setup.Secret)
	return f
}

func (f *automationFixture) createBranch(t *testing.T, name string) string {
	t.Helper()
	response := projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/deployments", `{"projectId":"`+f.projectID+`","name":"`+name+`","targetKind":"branch","targetName":"feature/search"}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", response.StatusCode, responseText(response))
	}
	var created struct{ ID, JobID string }
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, created.ID, created.JobID, "succeeded")
	return created.ID
}

func TestBranchDeploymentRetriesTransientFetchesButNotAuthenticationFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int32
		wantTries int32
	}{
		{name: "server unavailable", status: http.StatusServiceUnavailable, wantTries: 2},
		{name: "authentication rejected", status: http.StatusUnauthorized, wantTries: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAutomationFixture(t)
			id := f.createBranch(t, "retry-policy")
			before := f.gitFetches.Load()
			f.fetchFailureStatus.Store(tc.status)
			jobID := f.action(t, id, "deploy", `{}`)
			waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, jobID, "failed")
			if got := f.gitFetches.Load() - before; got != tc.wantTries {
				t.Fatalf("transient retry policy made %d fetch attempts, want %d", got, tc.wantTries)
			}
			jobs := f.waitJobs(t, id)
			if len(jobs) == 0 || jobs[0].ID != jobID || strings.Contains(jobs[0].Log, "controlled fetch failure") || strings.Contains(jobs[0].Log, "private-test-secret") {
				t.Fatalf("failed job details are missing or unsanitized: %+v", jobs)
			}
		})
	}
}

func TestJobHistoryRetentionExpiresTerminalWorkButPreservesActiveAndCleanupWork(t *testing.T) {
	f := newAutomationFixture(t)
	id := f.createBranch(t, "history-retention")
	retentionNow := time.Now().UTC()
	f.app.cfg.JobRetention = time.Hour
	f.app.cfg.Now = func() time.Time { return retentionNow }
	old := retentionNow.Add(-2 * time.Hour).UnixNano()
	ids := map[string]string{
		"expired": strings.Repeat("d", 32),
		"active":  strings.Repeat("e", 32),
		"cleanup": strings.Repeat("f", 32),
	}
	for key, status := range map[string]string{"expired": "succeeded", "active": "running", "cleanup": "succeeded"} {
		if _, err := f.app.db.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at) VALUES(?,?,?,1,?,?,?,?)`, ids[key], id, f.initialCommit, status, "Historical outcome", old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.app.db.Exec(`INSERT INTO publication_cleanup(deployment_id,job_id,path_name,dev,ino,not_before,last_error,attempts) VALUES(?,?,?,?,?,?,?,0)`, id, ids["cleanup"], "."+id+".stage-"+ids["cleanup"], 0, 0, retentionNow.Add(time.Hour).UnixNano(), "cleanup pending"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/deployments/"+id+"/jobs", nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.cookie})
		response, err := f.server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("job history response = %d: %s", response.StatusCode, responseText(response))
		}
		var jobs []deploymentJob
		err = json.NewDecoder(response.Body).Decode(&jobs)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		present := map[string]bool{}
		for _, job := range jobs {
			present[job.ID] = true
		}
		if !present[ids["expired"]] && present[ids["active"]] && present[ids["cleanup"]] {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("history retention did not expire terminal work while preserving active and cleanup-obligated jobs")
}

func (f *automationFixture) action(t *testing.T, id, action, body string) string {
	t.Helper()
	response := projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/deployments/"+id+"/"+action, body)
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d %s", action, response.StatusCode, responseText(response))
	}
	defer response.Body.Close()
	var result struct{ JobID string }
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result.JobID
}

func (f *automationFixture) push(t *testing.T, delivery, commit string) {
	t.Helper()
	body := []byte(`{"ref":"refs/heads/feature/search","after":"` + commit + `"}`)
	status := http.StatusNoContent
	if f.provider == "gitea" {
		status = sendGiteaWebhook(f.server.Client(), f.server.URL+"/webhooks/gitea/"+f.projectID, delivery, "push", body, f.secret)
	} else {
		status = sendGitHubWebhook(f.server.Client(), f.server.URL+"/webhooks/github/"+f.projectID, delivery, "push", body, f.secret)
	}
	if status != http.StatusNoContent {
		t.Fatalf("push status=%d", status)
	}
}

func (f *automationFixture) waitJobs(t *testing.T, id string) []deploymentJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/deployments/"+id+"/jobs", nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.cookie})
		response, err := f.server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var jobs []deploymentJob
		err = json.NewDecoder(response.Body).Decode(&jobs)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		settled := len(jobs) > 0
		for _, job := range jobs {
			if job.Status == "queued" || job.Status == "running" {
				settled = false
			}
		}
		if settled {
			return jobs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("jobs did not settle")
	return nil
}

func TestPausedWebhookFetchesOnlyWhileOtherDeploymentPublishes(t *testing.T) {
	forAutomationProviders(t, testPausedWebhookFetchesOnlyWhileOtherDeploymentPublishes)
}

func forAutomationProviders(t *testing.T, run func(*testing.T, string)) {
	t.Helper()
	for _, provider := range []string{"github", "gitea"} {
		t.Run(provider, func(t *testing.T) { run(t, provider) })
	}
}

func testPausedWebhookFetchesOnlyWhileOtherDeploymentPublishes(t *testing.T, provider string) {
	f := newAutomationFixture(t, provider)
	paused := f.createBranch(t, "paused")
	other := f.createBranch(t, "other")
	f.action(t, paused, "pause", `{}`)
	before, err := identity(f.app.deploymentPath(paused))
	if err != nil {
		t.Fatal(err)
	}
	latest := addSmartFixtureCommit(t, f.remoteDir, "new branch tip")
	f.push(t, "paused-delivery", latest)
	jobs := f.waitJobs(t, paused)
	if jobs[0].Status != "noop" {
		t.Fatalf("paused push=%s: %s", jobs[0].Status, jobs[0].Log)
	}
	f.waitJobs(t, other)
	refs := projectRefsFromAPI(t, f.server.Client(), f.server.URL, f.cookie, f.projectID)
	if got := findProjectRef(refs, "branch", "feature/search").Commit; got != latest {
		t.Fatalf("paused webhook did not fetch latest branch: %s", got)
	}
	if after, err := identity(f.app.deploymentPath(paused)); err != nil || after != before {
		t.Fatalf("paused push changed live directory: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(f.app.deploymentPath(other), "README.md")); err != nil || string(contents) != "new branch tip\n" {
		t.Fatalf("other deployment=%q: %v", contents, err)
	}
}

func TestPausedWebhookDoesNotExportOrHydrateUnpublishableTree(t *testing.T) {
	forAutomationProviders(t, testPausedWebhookDoesNotExportOrHydrateUnpublishableTree)
}

func testPausedWebhookDoesNotExportOrHydrateUnpublishableTree(t *testing.T, provider string) {
	f := newAutomationFixture(t, provider)
	id := f.createBranch(t, "paused")
	f.action(t, id, "pause", `{}`)
	before, err := identity(f.app.deploymentPath(id))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainOpen(f.remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err = wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("feature/search")}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.remoteDir, ".gitattributes"), []byte("README.md filter=lfs\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = wt.Add(".gitattributes"); err != nil {
		t.Fatal(err)
	}
	// An LFS-selected malformed pointer fails manual publication. Paused push
	// must still succeed as fetch-only, with no stage or pointer reaching live.
	latest := commitFixtureFile(t, f.remoteDir, wt, "malformed LFS", "version https://git-lfs.github.com/spec/v1\noid sha256:invalid").String()
	f.push(t, "unpublishable", latest)
	jobs := f.waitJobs(t, id)
	if jobs[0].Status != "noop" {
		t.Fatalf("paused delivery prepared invalid LFS: %s %s", jobs[0].Status, jobs[0].Log)
	}
	refs := projectRefsFromAPI(t, f.server.Client(), f.server.URL, f.cookie, f.projectID)
	if got := findProjectRef(refs, "branch", "feature/search").Commit; got != latest {
		t.Fatalf("fetch-only cached commit=%s", got)
	}
	job := f.action(t, id, "deploy", `{}`)
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, job, "failed")
	assertAutomationPaused(t, f.server, f.cookie, id, true)
	if after, err := identity(f.app.deploymentPath(id)); err != nil || after != before {
		t.Fatalf("invalid LFS changed publication: %v", err)
	}
	stages, err := filepath.Glob(filepath.Join(f.app.cfg.DeploymentRoot, "."+id+".stage-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("unpublished stages=%v: %v", stages, err)
	}
	// Both the pause and the failed one-off remain visible after a service
	// restart; a later fetch-only delivery must not erase that failure.
	cfg := f.app.cfg
	f.server.Close()
	if err = f.app.Close(); err != nil {
		t.Fatal(err)
	}
	f.app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.cookie = installAPITestSession(t, f.app)
	f.server = httptest.NewTLSServer(f.app.Handler())
	assertAutomationPaused(t, f.server, f.cookie, id, true)
	f.push(t, "after-restart", latest)
	jobs = f.waitJobs(t, id)
	if jobs[0].Status != "noop" {
		t.Fatalf("restarted pause not respected: %s", jobs[0].Status)
	}
	request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/api/deployments", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.cookie})
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var deployments []deployment
	err = json.NewDecoder(response.Body).Decode(&deployments)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 1 || deployments[0].Error == "" {
		t.Fatal("paused webhook erased failed manual publication warning")
	}
	if after, err := identity(f.app.deploymentPath(id)); err != nil || after != before {
		t.Fatalf("restart or paused fetch changed live publication: %v", err)
	}
}

func TestManualDeployWhilePausedAndResumeCatchUp(t *testing.T) {
	f := newAutomationFixture(t)
	id := f.createBranch(t, "paused")
	f.action(t, id, "pause", `{}`)
	addSmartFixtureCommit(t, f.remoteDir, "manual update")
	job := f.action(t, id, "deploy", `{}`)
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, job, "succeeded")
	assertAutomationPaused(t, f.server, f.cookie, id, true)
	if contents, err := os.ReadFile(filepath.Join(f.app.deploymentPath(id), "README.md")); err != nil || string(contents) != "manual update\n" {
		t.Fatalf("manual publication=%q: %v", contents, err)
	}
	addSmartFixtureCommit(t, f.remoteDir, "resume catches up without a push")
	job = f.action(t, id, "resume", `{}`)
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, job, "succeeded")
	assertAutomationPaused(t, f.server, f.cookie, id, false)
	if contents, err := os.ReadFile(filepath.Join(f.app.deploymentPath(id), "README.md")); err != nil || string(contents) != "resume catches up without a push\n" {
		t.Fatalf("resume publication=%q: %v", contents, err)
	}
}

func TestPauseInvalidatesAutomaticWorkAtPublicationBoundary(t *testing.T) {
	forAutomationProviders(t, testPauseInvalidatesAutomaticWorkAtPublicationBoundary)
}

func testPauseInvalidatesAutomaticWorkAtPublicationBoundary(t *testing.T, provider string) {
	for _, point := range []string{"after-stage-fsync", "after-intent"} {
		t.Run(point, func(t *testing.T) {
			f := newAutomationFixture(t, provider)
			id := f.createBranch(t, "production")
			before, err := identity(f.app.deploymentPath(id))
			if err != nil {
				t.Fatal(err)
			}
			latest := addSmartFixtureCommit(t, f.remoteDir, "must not publish automatically")
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var unblock sync.Once
			defer unblock.Do(func() { close(release) })
			previous := setPublicationFaultPoint(func(at string) error {
				if at == point {
					once.Do(func() { close(started); <-release })
				}
				return nil
			})
			defer setPublicationFaultPoint(previous)
			f.push(t, "in-flight", latest)
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("publication never reached boundary")
			}
			f.action(t, id, "pause", `{}`)
			unblock.Do(func() { close(release) })
			jobs := f.waitJobs(t, id)
			if jobs[0].Status != "superseded" {
				t.Fatalf("automatic publication after pause=%s: %s", jobs[0].Status, jobs[0].Log)
			}
			if after, err := identity(f.app.deploymentPath(id)); err != nil || after != before {
				t.Fatalf("pause failed to protect live publication: %v", err)
			}
			// A cancelled pre-switch intent must not block the next explicit
			// one-off or leave an orphan stage requiring a service restart.
			job := f.action(t, id, "deploy", `{}`)
			waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, job, "succeeded")
			assertAutomationPaused(t, f.server, f.cookie, id, true)
		})
	}
}

func TestPausedTargetChangesPublishOnceAndClearBranchOnlyState(t *testing.T) {
	for _, kind := range []string{"tag", "commit"} {
		t.Run(kind, func(t *testing.T) {
			f := newAutomationFixture(t)
			id := f.createBranch(t, "paused")
			f.action(t, id, "pause", `{}`)
			job := f.action(t, id, "target", `{"targetKind":"branch","targetName":"main"}`)
			waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, job, "succeeded")
			assertAutomationPaused(t, f.server, f.cookie, id, true)
			if contents, err := os.ReadFile(filepath.Join(f.app.deploymentPath(id), "README.md")); err != nil || string(contents) != "main content\n" {
				t.Fatalf("target-change publication=%q: %v", contents, err)
			}
			body := `{"targetKind":"tag","targetName":"v1.0"}`
			if kind == "commit" {
				body = `{"targetKind":"commit","commit":"` + f.initialCommit + `"}`
			}
			job = f.action(t, id, "target", body)
			waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, job, "succeeded")
			assertAutomationPaused(t, f.server, f.cookie, id, false)
			// Returning to a branch must not resurrect obsolete branch-only state.
			job = f.action(t, id, "target", `{"targetKind":"branch","targetName":"main"}`)
			waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, job, "succeeded")
			assertAutomationPaused(t, f.server, f.cookie, id, false)
		})
	}
}

func TestPauseAndResumeDoNotReviveAcceptedAutomaticWork(t *testing.T) {
	forAutomationProviders(t, testPauseAndResumeDoNotReviveAcceptedAutomaticWork)
}

func testPauseAndResumeDoNotReviveAcceptedAutomaticWork(t *testing.T, provider string) {
	f := newAutomationFixture(t, provider)
	id := f.createBranch(t, "production")
	initial := f.waitJobs(t, id)[0].ID
	latest := addSmartFixtureCommit(t, f.remoteDir, "latest after controls")
	started, release := make(chan struct{}), make(chan struct{})
	var once, unblock sync.Once
	defer unblock.Do(func() { close(release) })
	previous := setPublicationFaultPoint(func(at string) error {
		if at == "after-stage-fsync" {
			once.Do(func() { close(started); <-release })
		}
		return nil
	})
	defer setPublicationFaultPoint(previous)
	f.push(t, "before-pause", latest)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("publication never started")
	}
	f.action(t, id, "pause", `{}`)
	f.push(t, "while-paused", latest)
	resume := f.action(t, id, "resume", `{}`)
	unblock.Do(func() { close(release) })
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, id, resume, "succeeded")
	for _, job := range f.waitJobs(t, id) {
		if job.ID != initial && job.ID != resume && job.Status != "superseded" {
			t.Fatalf("control changes revived automatic work: %s %s", job.Status, job.Log)
		}
	}
}

// Configuration fixtures are arranged directly; all controls and observations
// go through the authenticated application interface.
func TestPauseIsProtectedBranchOnlyAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key")}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	projectID, branchID, commitID := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	now := time.Now().UnixNano()
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "fixture", "https://example.test/repo.git", now); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct{ id, kind string }{{branchID, "branch"}, {commitID, "commit"}} {
		if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,target_kind,target_name,created_at,updated_at) VALUES(?,?,?,?,?,'main',?,?)`, target.id, projectID, target.kind, strings.Repeat("d", 40), target.kind, now, now); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewTLSServer(app.Handler())
	cookie := installAPITestSession(t, app)
	for _, action := range []string{"pause", "resume"} {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/deployments/"+branchID+"/"+action, strings.NewReader(`{}`))
		request.Header.Set("Origin", server.URL)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unprotected %s status=%d", action, response.StatusCode)
		}
		request, _ = http.NewRequest(http.MethodPost, server.URL+"/api/deployments/"+branchID+"/"+action, strings.NewReader(`{}`))
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		request.Header.Set("Origin", "https://attacker.test")
		request.Header.Set("Content-Type", "application/json")
		response, err = server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-origin %s status=%d", action, response.StatusCode)
		}
		response = projectPost(t, server.Client(), server.URL, cookie, "/api/deployments/"+commitID+"/"+action, `{}`)
		response.Body.Close()
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("commit %s status=%d", action, response.StatusCode)
		}
	}
	response := projectPost(t, server.Client(), server.URL, cookie, "/api/deployments/"+branchID+"/pause", `{}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("pause: %d %s", response.StatusCode, responseText(response))
	}
	response.Body.Close()
	assertAutomationPaused(t, server, cookie, branchID, true)
	server.Close()
	if err = app.Close(); err != nil {
		t.Fatal(err)
	}
	app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server = httptest.NewTLSServer(app.Handler())
	defer server.Close()
	cookie = installAPITestSession(t, app)
	assertAutomationPaused(t, server, cookie, branchID, true)
}

func assertAutomationPaused(t *testing.T, server *httptest.Server, cookie, id string, want bool) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/deployments", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var deployments []struct {
		ID     string `json:"id"`
		Paused bool   `json:"automationPaused"`
	}
	if err = json.NewDecoder(response.Body).Decode(&deployments); err != nil {
		t.Fatal(err)
	}
	for _, d := range deployments {
		if d.ID == id {
			if d.Paused != want {
				t.Fatalf("paused=%t, want %t", d.Paused, want)
			}
			return
		}
	}
	t.Fatal("deployment missing from dashboard")
}
