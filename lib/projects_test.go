package lib

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
