package lib

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSetupGrantPersistsAndIsSingleUse(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	grant, err := app.IssueRegistrationGrant()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.DatabasePath, cfg.DatabasePath + "-wal"} {
		stored, err := os.ReadFile(path)
		if err != nil && path == cfg.DatabasePath {
			t.Fatal(err)
		}
		if err == nil && bytes.Contains(stored, []byte(grant)) {
			t.Fatalf("registration grant persisted in plaintext in %s", filepath.Base(path))
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/register", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	req.Host = "127.0.0.1:8080"
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Set up administrator") {
		t.Fatalf("registration page: %d %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `src="/assets/register.js"`) {
		t.Fatal("registration must use the local OPAQUE client")
	}
	asset := httptest.NewRecorder()
	app.Handler().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/assets/register.js", nil))
	if strings.Contains(asset.Body.String(), `JSON.stringify({ password`) || !strings.Contains(asset.Body.String(), `start.registrationRequest`) {
		t.Fatal("browser registration must send only OPAQUE protocol messages")
	}
	if _, err := os.Stat(cfg.OPAQUEKeyPath); err != nil {
		t.Fatalf("external key file not created: %v", err)
	}
	keyInfo, err := os.Stat(cfg.OPAQUEKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if keyInfo.Mode().Perm() != 0600 {
		t.Fatalf("OPAQUE key permissions = %v, want 0600 (err %v)", keyInfo.Mode().Perm(), err)
	}
	dbInfo, err := os.Stat(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if dbInfo.Mode().Perm() != 0600 {
		t.Fatalf("SQLite permissions = %v, want 0600 (err %v)", dbInfo.Mode().Perm(), err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(cfg.OPAQUEKeyPath)
	if err != nil || len(key) == 0 {
		t.Fatalf("missing saved key bytes (%d): %v", len(key), err)
	}
	app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	req = httptest.NewRequest(http.MethodGet, "/register", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	req.Host = "127.0.0.1:8080"
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("unconsumed grant should survive restart: %d", res.Code)
	}
}

func TestExpiredGrantAndNonLoopbackHTTPFailClosed(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), GrantTTL: time.Minute, InsecureLocalHTTP: true, Now: func() time.Time { return now }}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	token, err := app.IssueRegistrationGrant()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/register", nil)
	req.RemoteAddr, req.Host = "127.0.0.1:4321", "127.0.0.1:8080"
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("fresh local grant rejected: %d", res.Code)
	}
	now = now.Add(2 * time.Minute)
	expired := httptest.NewRequest(http.MethodPost, "/opaque/register/start", strings.NewReader(`{"grant":"`+token+`","value":"AA"}`))
	expired.RemoteAddr, expired.Host = "127.0.0.1:4321", "127.0.0.1:8080"
	expired.Header.Set("Origin", "http://127.0.0.1:8080")
	expired.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, expired)
	if res.Code != http.StatusGone {
		t.Fatalf("expired grant status = %d, want 410", res.Code)
	}
	req.RemoteAddr = "192.0.2.1:4321"
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusUpgradeRequired {
		t.Fatalf("non-loopback HTTP status = %d, want 426", res.Code)
	}
	now = now.Add(-2 * time.Minute)
	proxyGrant, err := app.IssueRegistrationGrant()
	if err != nil {
		t.Fatal(err)
	}
	proxyReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	proxyReq.RemoteAddr, proxyReq.Host = "127.0.0.1:4321", "manager.example:443"
	proxyReq.Header.Set("X-Forwarded-Proto", "https")
	proxyRes := httptest.NewRecorder()
	app.Handler().ServeHTTP(proxyRes, proxyReq)
	if proxyRes.Code != http.StatusOK {
		t.Fatalf("trusted HTTPS reverse proxy registration status = %d", proxyRes.Code)
	}
	csrf := httptest.NewRequest(http.MethodPost, "/opaque/register/start", strings.NewReader(`{"grant":"`+proxyGrant+`","value":"AA"}`))
	csrf.RemoteAddr, csrf.Host = "127.0.0.1:4321", "manager.example:443"
	csrf.Header.Set("X-Forwarded-Proto", "https")
	csrf.Header.Set("Origin", "https://attacker.example")
	csrfRes := httptest.NewRecorder()
	app.Handler().ServeHTTP(csrfRes, csrf)
	if csrfRes.Code != http.StatusForbidden {
		t.Fatalf("cross-origin registration POST status = %d, want 403", csrfRes.Code)
	}
}

func TestRegistrationURLKeepsGrantInFragment(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	grant, err := app.IssueRegistrationGrant()
	if err != nil {
		t.Fatal(err)
	}
	link, err := app.RegistrationURL("http://localhost:8080", grant)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.RawQuery != "" || u.Fragment != grant {
		t.Fatalf("registration URL = %q; grant must be fragment-only", link)
	}
}

func TestOPAQUEKeyGenerationMustMatchSQLiteProfile(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	cfg1 := Config{DatabasePath: filepath.Join(first, "state.db"), OPAQUEKeyPath: filepath.Join(first, "opaque.key")}
	cfg2 := Config{DatabasePath: filepath.Join(second, "state.db"), OPAQUEKeyPath: filepath.Join(second, "opaque.key")}
	app, err := New(cfg1)
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
	other, err := New(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	key, err := os.ReadFile(cfg2.OPAQUEKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfg1.OPAQUEKeyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = New(cfg1); err == nil {
		t.Fatal("replacement OPAQUE key generation must fail closed")
	}
}

func TestOpaqueKeyMissingFailsClosed(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	app.Close()
	if err := os.Remove(cfg.OPAQUEKeyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("missing OPAQUE key must fail closed")
	}
}

func TestEmbeddedOpaqueAssetIsPinnedArtifact(t *testing.T) {
	digest := sha256.Sum256(opaqueAsset)
	if got := hex.EncodeToString(digest[:]); got != "1e8d56383a77948e453085b6d4f52dcb9645f8bfff32395738b63d3a2e2fcb52" {
		t.Fatalf("embedded OPAQUE asset digest = %s; pinned v1.1.0 artifact changed", got)
	}
}
