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
		if failRequests.Load() {
			http.Error(w, "fixture deliberately unavailable", http.StatusServiceUnavailable)
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
