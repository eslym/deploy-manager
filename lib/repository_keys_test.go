package lib

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestRepositoryKeyHTTPPersistsOnlyCiphertextAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.sqlite"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), RepositoryKeyPath: filepath.Join(dir, "repository-master.key")}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(app.Handler())
	client := server.Client()
	defer server.Close()
	unauthenticated, err := client.Get(server.URL + "/api/repository-keys")
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated.Body.Close()
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list returned %d", unauthenticated.StatusCode)
	}
	token := []byte("0123456789abcdef0123456789abcdef")
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := ssh.MarshalPrivateKey(private, "test")
	if err != nil {
		t.Fatal(err)
	}
	secret := string(pem.EncodeToMemory(encoded))
	post := func(body string, origin string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/repository-keys", strings.NewReader(body))
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		return res
	}
	resp := post(`{"name":"primary","privateKey":`+mustJSON(secret)+`}`, server.URL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("import status %d: %s", resp.StatusCode, b)
	}
	var created struct {
		Code    string        `json:"code"`
		Message string        `json:"message"`
		Data    repositoryKey `json:"data"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	key := created.Data
	if created.Code != "repository_key.created" || created.Message == "" {
		t.Fatalf("unexpected creation outcome: %+v", created)
	}
	if key.PublicKey == "" || key.Fingerprint == "" || key.Algorithm != "Ed25519" {
		t.Fatalf("incomplete public metadata: %+v", key)
	}
	responseBody, _ := json.Marshal(key)
	if strings.Contains(string(responseBody), secret) || strings.Contains(string(responseBody), string(private)) {
		t.Fatal("private key appeared in response")
	}
	invalid := post(`{"name":"encrypted","privateKey":"-----BEGIN ENCRYPTED PRIVATE KEY-----\nsecret\n-----END ENCRYPTED PRIVATE KEY-----"}`, server.URL)
	invalidBody, _ := io.ReadAll(invalid.Body)
	invalid.Body.Close()
	if invalid.StatusCode != 400 || strings.Contains(string(invalidBody), "secret") {
		t.Fatalf("invalid import response leaked parser input (%d): %s", invalid.StatusCode, string(invalidBody))
	}
	badOrigin := post(`{"name":"x","generate":true}`, "https://attacker.example")
	badOrigin.Body.Close()
	if badOrigin.StatusCode != 403 {
		t.Fatalf("cross-origin mutation returned %d", badOrigin.StatusCode)
	}
	missingOrigin := post(`{"name":"x","generate":true}`, "")
	missingOrigin.Body.Close()
	if missingOrigin.StatusCode != http.StatusForbidden {
		t.Fatalf("mutation without Origin returned %d", missingOrigin.StatusCode)
	}
	oversized := post(`{"name":"large","privateKey":"`+strings.Repeat("A", maxRepositoryPrivateKeyBytes+1)+`"}`, server.URL)
	oversized.Body.Close()
	if oversized.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized import returned %d", oversized.StatusCode)
	}
	listReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/repository-keys", nil)
	listReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
	listed, e := client.Do(listReq)
	if e != nil {
		t.Fatal(e)
	}
	var listedOutcome struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Data    []repositoryKey `json:"data"`
	}
	e = json.NewDecoder(listed.Body).Decode(&listedOutcome)
	listed.Body.Close()
	if e != nil || listedOutcome.Code != "repository_key.listed" || len(listedOutcome.Data) != 1 || listedOutcome.Data[0].PublicKey != key.PublicKey {
		t.Fatalf("public list mismatch: %+v (%v)", listedOutcome, e)
	}
	if listedOutcome.Data[0].ProjectCount != 0 {
		t.Fatalf("unused key has project count %d, want 0", listedOutcome.Data[0].ProjectCount)
	}
	for _, projectID := range []string{"project-one", "project-two"} {
		if _, e = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, projectID, "git@github.com:owner/"+projectID+".git", time.Now().UnixNano()); e != nil {
			t.Fatal(e)
		}
		if _, e = app.db.Exec(`INSERT INTO project_ssh_keys(project_id,key_id) VALUES(?,?)`, projectID, key.ID); e != nil {
			t.Fatal(e)
		}
	}
	listReq, _ = http.NewRequest(http.MethodGet, server.URL+"/api/repository-keys", nil)
	listReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
	listed, e = client.Do(listReq)
	if e != nil {
		t.Fatal(e)
	}
	if e = json.NewDecoder(listed.Body).Decode(&listedOutcome); e != nil {
		listed.Body.Close()
		t.Fatal(e)
	}
	listed.Body.Close()
	if got := listedOutcome.Data[0].ProjectCount; got != 2 {
		t.Fatalf("shared key has project count %d, want 2", got)
	}
	if _, e = app.db.Exec(`DELETE FROM projects WHERE id=?`, "project-two"); e != nil {
		t.Fatal(e)
	}
	listReq, _ = http.NewRequest(http.MethodGet, server.URL+"/api/repository-keys", nil)
	listReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
	listed, e = client.Do(listReq)
	if e != nil {
		t.Fatal(e)
	}
	if e = json.NewDecoder(listed.Body).Decode(&listedOutcome); e != nil {
		listed.Body.Close()
		t.Fatal(e)
	}
	listed.Body.Close()
	if got := listedOutcome.Data[0].ProjectCount; got != 1 {
		t.Fatalf("key after project deletion has project count %d, want 1", got)
	}
	challenge := []byte("repository key remains usable")
	if !app.repositoryKeyCanSign(key.ID, challenge) {
		t.Fatal("imported key could not sign in process")
	}
	var ciphertext []byte
	if e = app.db.QueryRow(`SELECT ciphertext FROM repository_keys WHERE id=?`, key.ID).Scan(&ciphertext); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(ciphertext), secret) || strings.Contains(string(ciphertext), string(private)) {
		t.Fatal("plaintext private key found in SQLite ciphertext")
	}
	masterBefore, e := os.ReadFile(cfg.RepositoryKeyPath)
	if e != nil {
		t.Fatal(e)
	}
	if e = app.Close(); e != nil {
		t.Fatal(e)
	}
	app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !app.repositoryKeyCanSign(key.ID, challenge) {
		t.Fatal("key unusable after restart")
	}
	app.Close()
	masterAfter, _ := os.ReadFile(cfg.RepositoryKeyPath)
	if string(masterBefore) != string(masterAfter) {
		t.Fatal("repository master key changed across restart")
	}
	if err = os.Remove(cfg.RepositoryKeyPath); err != nil {
		t.Fatal(err)
	}
	if _, err = New(cfg); err == nil {
		t.Fatal("missing repository master key did not fail closed")
	}
	if _, err = os.Stat(cfg.RepositoryKeyPath); !os.IsNotExist(err) {
		t.Fatal("missing master key was silently regenerated")
	}
	if err = os.WriteFile(cfg.RepositoryKeyPath, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = New(cfg); err == nil {
		t.Fatal("replacement master key did not fail closed")
	}
	if err = os.Chmod(cfg.RepositoryKeyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = New(cfg); err == nil {
		t.Fatal("wrong-permission master key did not fail closed")
	}
}

func TestRepositoryKeyWritesAreRateLimited(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.sqlite"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), AuthRequestsPerMinute: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	token := []byte("abcdef0123456789abcdef0123456789")
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	request := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/repository-keys", strings.NewReader(`{"name":"rate test","generate":true}`))
		req.Header.Set("Origin", server.URL)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
		res, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		return res
	}
	first := request()
	first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first key request returned %d", first.StatusCode)
	}
	second := request()
	second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate-limited key request returned %d", second.StatusCode)
	}
	var count int
	if err = app.db.QueryRow(`SELECT count(*) FROM repository_keys`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rate-limited request persisted %d keys: %v", count, err)
	}
}

func TestRepositoryPrivateKeyAlgorithmAllowlist(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	rsaPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})
	_, _, algorithm, bits, err := parseRepositoryPrivateKey(rsaPEM)
	if err != nil || algorithm != "RSA" || bits != 3072 {
		t.Fatalf("strong RSA rejected: algorithm=%q bits=%d error=%v", algorithm, bits, err)
	}
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaDER, err := x509.MarshalECPrivateKey(ecdsaKey)
	if err != nil {
		t.Fatal(err)
	}
	_, _, algorithm, bits, err = parseRepositoryPrivateKey(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecdsaDER}))
	if err != nil || algorithm != "ECDSA-P-256" || bits != 256 {
		t.Fatalf("ECDSA P-256 rejected: algorithm=%q bits=%d error=%v", algorithm, bits, err)
	}
	weakRSA, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err = parseRepositoryPrivateKey(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(weakRSA)})); err == nil {
		t.Fatal("weak RSA key was accepted")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
