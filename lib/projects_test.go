package lib

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestProjectHTTPRejectsUnauthenticatedAndUnsupportedURLs(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	client := server.Client()

	unauth, err := client.Get(server.URL + "/api/projects")
	if err != nil {
		t.Fatal(err)
	}
	unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d", unauth.StatusCode)
	}

	token := []byte("0123456789abcdef0123456789abcdef")
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration, authenticatedAt: time.Now()}
	app.mu.Unlock()
	body := `{"name":"private","url":"https://user:secret@example.test/repo.git"}`
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/projects", strings.NewReader(body))
	req.Header.Set("Origin", server.URL)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("credential URL status = %d", res.StatusCode)
	}
	res.Body.Close()
	// Even an HTTPS URL is not allowed to use a loopback destination. The
	// production Git transport verifies the connected peer address itself.
	localBody := `{"name":"local","url":"https://127.0.0.1:18439/repo.git"}`
	localReq, _ := http.NewRequest(http.MethodPost, server.URL+"/api/projects", strings.NewReader(localBody))
	localReq.Header.Set("Origin", server.URL)
	localReq.Header.Set("Content-Type", "application/json")
	localReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
	localRes, err := client.Do(localReq)
	if err != nil {
		t.Fatal(err)
	}
	defer localRes.Body.Close()
	if localRes.StatusCode != http.StatusBadRequest {
		t.Fatalf("loopback HTTPS status = %d", localRes.StatusCode)
	}
	var projects []project
	getReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/projects", nil)
	getReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
	get, err := client.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	if err := json.NewDecoder(get.Body).Decode(&projects); err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("unsupported URL created %d projects", len(projects))
	}
}

func TestListProjectsIncludesAssignedAndUnassignedKeysInProjectOrder(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	for _, row := range []struct {
		id, name, key string
		created       int64
	}{
		{"project-a", "A", "key-shared", 1},
		{"project-b", "B", "key-other", 2},
		{"project-c", "C", "", 3},
		{"project-d", "D", "key-shared", 4},
	} {
		if _, err := app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, row.id, row.name, "https://example.invalid/"+row.name, row.created); err != nil {
			t.Fatal(err)
		}
		if row.key != "" {
			if _, err := app.db.Exec(`INSERT OR IGNORE INTO repository_keys(id,name,algorithm,bits,fingerprint,public_key,ciphertext,created_at) VALUES(?,?,?,?,?,?,?,?)`, row.key, row.key, "ed25519", 256, "fingerprint", "public", []byte("ciphertext"), row.created); err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.Exec(`INSERT INTO project_ssh_keys(project_id,key_id) VALUES(?,?)`, row.id, row.key); err != nil {
				t.Fatal(err)
			}
		}
	}
	token := []byte("0123456789abcdef0123456789abcdef")
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration, authenticatedAt: time.Now()}
	app.mu.Unlock()
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("project list status = %d, want 200", res.StatusCode)
	}
	var got []project
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d projects, want 4", len(got))
	}
	wantIDs := []string{"project-a", "project-b", "project-c", "project-d"}
	wantKeys := []string{"key-shared", "key-other", "", "key-shared"}
	for i := range got {
		if got[i].ID != wantIDs[i] || got[i].KeyID != wantKeys[i] {
			t.Errorf("project[%d] = (%q, %q), want (%q, %q)", i, got[i].ID, got[i].KeyID, wantIDs[i], wantKeys[i])
		}
	}
}

func TestProjectCacheSizesAndExplicitRecreationOverHTTP(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	projectID := "0123456789abcdef0123456789abcdef"
	if _, err := app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "cache fixture", "https://example.invalid/repo.git", 1); err != nil {
		t.Fatal(err)
	}
	gitCache := app.projects.repoPath(projectID)
	oldCommit := createSmartFixture(t, gitCache)
	lfsCache := filepath.Join(app.projects.root, "lfs-cache", projectID)
	if err := os.MkdirAll(lfsCache, 0700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("payload")
	payloadOID := fmt.Sprintf("%x", sha256.Sum256(payload))
	if err := os.WriteFile(filepath.Join(lfsCache, payloadOID), payload, 0600); err != nil {
		t.Fatal(err)
	}
	deploymentID := "abcdef0123456789abcdef0123456789"
	if _, err := app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,generation,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, deploymentID, projectID, "existing", oldCommit, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	publication := app.deploymentPath(deploymentID)
	if err := os.MkdirAll(publication, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publication, "index.html"), []byte("published"), 0600); err != nil {
		t.Fatal(err)
	}
	token := []byte("0123456789abcdef0123456789abcdef")
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration, authenticatedAt: time.Now()}
	app.mu.Unlock()
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	client := server.Client()
	request := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
		if method == http.MethodPost {
			req.Header.Set("Origin", server.URL)
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	listed := request(http.MethodGet, "/api/projects", "")
	var projects []project
	if listed.StatusCode != http.StatusOK || json.NewDecoder(listed.Body).Decode(&projects) != nil {
		listed.Body.Close()
		t.Fatalf("project listing status = %d; want a cache size response", listed.StatusCode)
	}
	listed.Body.Close()
	if len(projects) != 1 || projects[0].GitCacheBytes == 0 || projects[0].LFSCacheBytes != int64(len(payload)) {
		t.Fatalf("reported project cache sizes = %+v", projects)
	}
	missingConfirmation := request(http.MethodPost, "/api/projects/"+projectID+"/cache/recreate", `{}`)
	missingConfirmation.Body.Close()
	if missingConfirmation.StatusCode != http.StatusBadRequest {
		t.Fatalf("recreation without explicit confirmation status = %d, want 400", missingConfirmation.StatusCode)
	}
	confirmed := request(http.MethodPost, "/api/projects/"+projectID+"/cache/recreate", `{"confirm":true}`)
	confirmed.Body.Close()
	if confirmed.StatusCode != http.StatusNoContent {
		t.Fatalf("confirmed cache recreation status = %d, want 204", confirmed.StatusCode)
	}
	listed = request(http.MethodGet, "/api/projects", "")
	projects = nil
	if listed.StatusCode != http.StatusOK || json.NewDecoder(listed.Body).Decode(&projects) != nil {
		listed.Body.Close()
		t.Fatalf("project listing after recreation status = %d", listed.StatusCode)
	}
	listed.Body.Close()
	if len(projects) != 1 || projects[0].GitCacheBytes != 0 || projects[0].LFSCacheBytes != 0 {
		t.Fatalf("cache sizes after recreation = %+v, want project retained with empty caches", projects)
	}

	type concurrentResult struct {
		status int
		body   []byte
	}
	results := make(chan concurrentResult, 4)
	for _, operation := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/projects", ""},
		{http.MethodPost, "/api/projects/" + projectID + "/cache/recreate", `{"confirm":true}`},
		{http.MethodGet, "/api/projects", ""},
		{http.MethodPost, "/api/projects/" + projectID + "/cache/recreate", `{"confirm":true}`},
	} {
		go func(operation struct{ method, path, body string }) {
			response := request(operation.method, operation.path, operation.body)
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			results <- concurrentResult{status: response.StatusCode, body: body}
		}(operation)
	}
	for range 4 {
		result := <-results
		if result.status != http.StatusOK && result.status != http.StatusNoContent {
			t.Fatalf("concurrent cache operation status=%d body=%q", result.status, result.body)
		}
	}
	if _, err := app.store.Project.Get(context.Background(), projectID); err != nil {
		t.Fatalf("recreation removed project configuration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(publication, "index.html")); err != nil {
		t.Fatalf("recreation changed published content: %v", err)
	}
	if _, err := app.store.Deployment.Get(context.Background(), deploymentID); err != nil {
		t.Fatalf("recreation removed deployment configuration: %v", err)
	}
	missingCommit := request(http.MethodPost, "/api/deployments", fmt.Sprintf(`{"projectId":%q,"name":"old commit","commit":%q}`, projectID, oldCommit))
	missingBody, _ := io.ReadAll(missingCommit.Body)
	missingCommit.Body.Close()
	if missingCommit.StatusCode != http.StatusBadRequest || !strings.Contains(string(missingBody), "commit is not available") {
		t.Fatalf("deployment using a commit removed by recreation got status=%d body=%q", missingCommit.StatusCode, missingBody)
	}
}

func TestRecreatedLFSCacheFailureIsVisibleThroughDeploymentApplication(t *testing.T) {
	f := newAutomationFixture(t)
	payload := []byte("the live LFS payload")
	payloadOID := fmt.Sprintf("%x", sha256.Sum256(payload))
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", payloadOID, len(payload))

	remoteRepo, err := git.PlainOpen(f.remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := remoteRepo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err = worktree.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("feature/search")}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.remoteDir, ".gitattributes"), []byte("asset.dat filter=lfs\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.remoteDir, "asset.dat"), []byte(pointer), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".gitattributes", "asset.dat"} {
		if _, err = worktree.Add(path); err != nil {
			t.Fatal(err)
		}
	}
	commit, err := worktree.Commit("track LFS payload", &git.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.test", When: time.Now().UTC()}})
	if err != nil {
		t.Fatal(err)
	}

	var lfsServerURL string
	var payloadAvailable atomic.Bool
	var unavailablePayloadRequested atomic.Bool
	requests := make(chan string, 8)
	payloadAvailable.Store(true)
	lfsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.Method + " " + r.URL.Path:
		default:
		}
		if strings.HasSuffix(r.URL.Path, "/objects/batch") {
			w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
			_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":%q}}}]}`, payloadOID, len(payload), lfsServerURL+"/payload")
			return
		}
		if r.URL.Path == "/payload" && payloadAvailable.Load() {
			_, _ = w.Write(payload)
			return
		}
		if r.URL.Path == "/payload" {
			unavailablePayloadRequested.Store(true)
		}
		http.NotFound(w, r)
	}))
	defer lfsServer.Close()
	lfsServerURL = lfsServer.URL
	pool := x509.NewCertPool()
	pool.AddCert(lfsServer.Certificate())
	oldRoots := lfsTestRootCAs
	lfsTestRootCAs = pool
	t.Cleanup(func() { lfsTestRootCAs = oldRoots })
	origins, err := json.Marshal([]string{lfsServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	internal, err := json.Marshal(map[string][]string{lfsServer.URL: {"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.app.db.Exec(`INSERT INTO project_lfs_settings(project_id,endpoint,object_origins,allow_internal,internal_addresses,updated_at) VALUES(?,?,?,?,?,?)`, f.projectID, lfsServer.URL+"/info/lfs", string(origins), 1, string(internal), time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	refresh := projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/projects/"+f.projectID+"/refresh", `{}`)
	if refresh.StatusCode != http.StatusOK {
		t.Fatalf("refresh LFS branch: %d %s", refresh.StatusCode, responseText(refresh))
	}
	refresh.Body.Close()
	if got := findProjectRef(projectRefsFromAPI(t, f.server.Client(), f.server.URL, f.cookie, f.projectID), "branch", "feature/search").Commit; got != commit.String() {
		t.Fatalf("refreshed LFS branch=%s, want %s", got, commit)
	}
	gitBackup := filepath.Join(t.TempDir(), "git-cache")
	if err = copyGitCache(f.app.projects.repoPath(f.projectID), gitBackup, f.app.projects.maxCacheBytes); err != nil {
		t.Fatal(err)
	}
	created := projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/deployments", fmt.Sprintf(`{"projectId":%q,"name":"unavailable-lfs-after-cache-recreation","commit":%q}`, f.projectID, f.initialCommit))
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create LFS deployment: %d %s", created.StatusCode, responseText(created))
	}
	var deployment struct{ ID, JobID string }
	if err = json.NewDecoder(created.Body).Decode(&deployment); err != nil {
		created.Body.Close()
		t.Fatal(err)
	}
	created.Body.Close()
	deploymentID := deployment.ID
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, deploymentID, deployment.JobID, "succeeded")
	livePath := f.app.deploymentPath(deploymentID)
	func() {
		for {
			select {
			case <-requests:
			default:
				return
			}
		}
	}()
	liveBefore, err := identity(livePath)
	if err != nil {
		t.Fatal(err)
	}

	recreated := projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/projects/"+f.projectID+"/cache/recreate", `{"confirm":true}`)
	if recreated.StatusCode != http.StatusNoContent {
		t.Fatalf("cache recreation: %d %s", recreated.StatusCode, responseText(recreated))
	}
	recreated.Body.Close()
	if size, sizeErr := projectLFSCacheSize(f.app.projects.root, f.projectID); sizeErr != nil || size != 0 {
		t.Fatalf("LFS cache after protected recreation=%d: %v", size, sizeErr)
	}
	payloadAvailable.Store(false)
	// Keep the known Git objects available while exercising LFS-specific loss;
	// the real cache-recreation request has already removed both cache trees.
	if err = copyGitCache(gitBackup, f.app.projects.repoPath(f.projectID), f.app.projects.maxCacheBytes); err != nil {
		t.Fatalf("restore isolated Git fixture after cache recreation: %v", err)
	}
	target := projectPost(t, f.server.Client(), f.server.URL, f.cookie, "/api/deployments/"+deploymentID+"/target", fmt.Sprintf(`{"commit":%q}`, commit.String()))
	if target.StatusCode != http.StatusAccepted {
		t.Fatalf("set recovery deployment target: %d %s", target.StatusCode, responseText(target))
	}
	var targetJob struct{ JobID string }
	if err = json.NewDecoder(target.Body).Decode(&targetJob); err != nil {
		target.Body.Close()
		t.Fatal(err)
	}
	target.Body.Close()
	jobID := targetJob.JobID
	waitForDeploymentJob(t, f.server.Client(), f.server.URL, f.cookie, deploymentID, jobID, "failed")
	jobs := f.waitJobs(t, deploymentID)
	var failed *deploymentJob
	for i := range jobs {
		if jobs[i].ID == jobID {
			failed = &jobs[i]
			break
		}
	}
	if !unavailablePayloadRequested.Load() {
		var seen []string
		for {
			select {
			case path := <-requests:
				seen = append(seen, path)
			default:
				t.Fatalf("deployment did not reach the LFS object after cache recreation: requests=%v job=%+v", seen, failed)
			}
		}
	}
	if failed == nil || failed.Status != "failed" || !strings.Contains(failed.Log, "current live files were kept") {
		t.Fatalf("missing-LFS deployment result is not visible in job history: %+v", failed)
	}
	if after, err := identity(livePath); err != nil || after != liveBefore {
		t.Fatalf("failed LFS recovery changed the live publication: %v", err)
	}
	if _, err := os.Stat(filepath.Join(livePath, "asset.dat")); !os.IsNotExist(err) {
		t.Fatalf("failed LFS deployment unexpectedly changed live asset: %v", err)
	}

	// The protected job-history endpoint reads durable state, not a worker-local result.
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
	jobs = f.waitJobs(t, deploymentID)
	for _, job := range jobs {
		if job.ID == jobID {
			if job.Status != "failed" || !strings.Contains(job.Log, "current live files were kept") {
				t.Fatalf("persisted missing-LFS failure changed after restart: %+v", job)
			}
			return
		}
	}
	t.Fatal("failed LFS deployment was not present in durable job history after restart")
}

func TestInterruptedProjectCacheRecreationFinishesBeforeRestartServesWork(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key")}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	projectID := "0123456789abcdef0123456789abcdef"
	if _, err := app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "restart fixture", "https://example.invalid/repo.git", 1); err != nil {
		t.Fatal(err)
	}
	cache := app.projects.repoPath(projectID)
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "partial"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	lfsCache := filepath.Join(app.projects.root, "lfs-cache", projectID)
	if err := os.MkdirAll(lfsCache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lfsCache, strings.Repeat("b", 64)), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.projects.markCacheRecreation(projectID); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("interrupted Git cache remains after startup recovery: %v", err)
	}
	if _, err := os.Stat(lfsCache); !os.IsNotExist(err) {
		t.Fatalf("interrupted LFS cache remains after startup recovery: %v", err)
	}
	if _, err := restarted.store.Project.Get(context.Background(), projectID); err != nil {
		t.Fatalf("restart recovery removed project configuration: %v", err)
	}
	if _, err := os.Lstat(app.projects.cacheRecreationPath(projectID)); !os.IsNotExist(err) {
		t.Fatalf("restart left recreation journal marker: %v", err)
	}
}

func TestSafeRemoteAllowsOnlyAnonymousHTTPS(t *testing.T) {
	for _, input := range []string{
		"file:///tmp/repo", "http://example.test/repo.git", "https://user:pass@example.test/repo.git", "https://example.test/repo.git?token=secret", "https://example.test/repo.git#fragment",
	} {
		if _, err := safeRemote(input); err == nil {
			t.Errorf("safeRemote(%q) accepted unsupported URL", input)
		}
	}
	if _, err := safeRemote("https://example.test/owner/site.git"); err != nil {
		t.Fatal(err)
	}
}

func TestPublicGitIPRejectsSpecialPurposeRanges(t *testing.T) {
	for _, address := range []string{"100.64.0.1", "192.0.2.1", "198.18.0.1", "2001:db8::1", "64:ff9b:1::1", "::ffff:192.0.2.1"} {
		ip, _ := netip.ParseAddr(address)
		if isPublicGitIP(ip) {
			t.Errorf("special-purpose address %s was classified public", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		ip, _ := netip.ParseAddr(address)
		if !isPublicGitIP(ip) {
			t.Errorf("public address %s was classified non-public", address)
		}
	}
}
