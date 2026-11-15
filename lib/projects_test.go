package lib

import (
	"context"
	"crypto/sha256"
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
	"testing"
	"time"
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
