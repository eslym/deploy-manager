package lib

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	req.Header.Set("Accept", "text/html")
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "_app/immutable/entry/start.") {
		t.Fatalf("registration page: %d %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), grant) || !strings.Contains(res.Header().Get("Cache-Control"), "no-cache") {
		t.Fatal("SPA shell must be public, grant-free, and revalidated")
	}
	if !strings.Contains(res.Header().Get("Content-Security-Policy"), "sha256-") {
		t.Fatal("SPA shell must authorize only its exact inline boot script")
	}
	for _, route := range []string{"/register"} {
		deep := httptest.NewRequest(http.MethodGet, route, nil)
		deep.RemoteAddr, deep.Host = "127.0.0.1:43210", "127.0.0.1:8080"
		deep.Header.Set("Accept", "text/html")
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, deep)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "_app/immutable/entry/start.") {
			t.Fatalf("SPA route %s did not return shell: %d", route, response.Code)
		}
	}
	plainDashboard := httptest.NewRecorder()
	plainDashboardRequest := httptest.NewRequest(http.MethodGet, "/dashboard/projects/nested", nil)
	plainDashboardRequest.RemoteAddr, plainDashboardRequest.Host = "127.0.0.1:43210", "127.0.0.1:8080"
	plainDashboardRequest.Header.Set("Accept", "text/html")
	app.Handler().ServeHTTP(plainDashboard, plainDashboardRequest)
	if plainDashboard.Code != http.StatusUpgradeRequired {
		t.Fatalf("plain HTTP dashboard status = %d, want 426", plainDashboard.Code)
	}
	missing := httptest.NewRecorder()
	missingReq := httptest.NewRequest(http.MethodGet, "/_app/immutable/entry/missing.deadbeef.js", nil)
	missingReq.Header.Set("Accept", "text/html")
	app.Handler().ServeHTTP(missing, missingReq)
	if missing.Code != http.StatusNotFound || strings.Contains(missing.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("missing JS must remain a 404, got %d (%s)", missing.Code, missing.Header().Get("Content-Type"))
	}
	unsupported := httptest.NewRecorder()
	unsupportedReq := httptest.NewRequest(http.MethodPost, "/register", nil)
	unsupportedReq.Header.Set("Accept", "text/html")
	app.Handler().ServeHTTP(unsupported, unsupportedReq)
	if unsupported.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST frontend navigation status = %d, want 405", unsupported.Code)
	}
	protocolMethod := httptest.NewRecorder()
	app.Handler().ServeHTTP(protocolMethod, httptest.NewRequest(http.MethodGet, "/opaque/register/start", nil))
	if protocolMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET protocol endpoint status = %d, want 405", protocolMethod.Code)
	}
	assetURL := strings.Split(strings.Split(res.Body.String(), "href=\"")[1], "\"")[0]
	asset := httptest.NewRecorder()
	assetReq := httptest.NewRequest(http.MethodGet, assetURL, nil)
	app.Handler().ServeHTTP(asset, assetReq)
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Content-Type"), "javascript") || !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("hashed entry asset %q headers/status incorrect: %d %v", assetURL, asset.Code, asset.Header())
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
	req.Header.Set("Accept", "text/html")
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("unconsumed grant should survive restart: %d", res.Code)
	}
}

func TestCanonicalHTTPOriginRequiresOptIn(t *testing.T) {
	for _, tc := range []struct {
		name, origin string
		allowHTTP    bool
		valid        bool
	}{
		{"loopback denied by default", "http://localhost:5173", false, false},
		{"loopback allowed", "http://localhost:5173", true, true},
		{"IP loopback allowed", "http://127.0.0.1:5173", true, true},
		{"non-loopback HTTP allowed", "http://example.test", true, true},
		{"non-loopback HTTP denied by default", "http://example.test", false, false},
		{"other schemes remain forbidden", "ftp://example.test", true, false},
		{"HTTPS unaffected", "https://example.test", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			issuer, err := NewGrantIssuer(Config{
				DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"),
				CanonicalOrigin: tc.origin, AllowHTTPOrigin: tc.allowHTTP, InsecureLocalHTTP: true,
			})
			if (err == nil) != tc.valid {
				t.Fatalf("origin %q with allowHTTP=%t: err=%v, want valid=%t", tc.origin, tc.allowHTTP, err, tc.valid)
			}
			if err == nil {
				defer issuer.Close()
				token, err := issuer.IssueRegistrationGrant()
				if err != nil {
					t.Fatal(err)
				}
				link, err := issuer.RegistrationURL(tc.origin, token)
				if err != nil || !strings.HasPrefix(link, tc.origin+"/register#") {
					t.Fatalf("registration link using %s: link=%q err=%v", tc.origin, link, err)
				}
			}
		})
	}
}

func TestOptedInHTTPOriginPassesRequestSecurityChecks(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), CanonicalOrigin: "http://Manager.Example:80", AllowHTTPOrigin: true, InsecureLocalHTTP: false})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	for _, tc := range []struct {
		host, origin string
		want         bool
	}{
		{"manager.example", "http://manager.example", true},
		{"manager.example", "http://manager.example:80", true},
		{"attacker.example", "http://manager.example", false},
		{"manager.example", "https://manager.example", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/", nil)
		r.RemoteAddr = "192.0.2.9:1234"
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		if got := app.secure(r) && app.originOK(r); got != tc.want {
			t.Errorf("host=%q origin=%q got %t want %t", tc.host, tc.origin, got, tc.want)
		}
	}
}

func TestOptedInHTTPSessionCookieAuthenticatesDashboardRequest(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), CanonicalOrigin: "http://manager.example", AllowHTTPOrigin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	token := []byte("01234567890123456789012345678901")
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: app.cfg.Now().Add(time.Hour), generation: app.credentialGeneration, authenticatedAt: app.cfg.Now()}
	app.mu.Unlock()
	r := httptest.NewRequest(http.MethodGet, "http://manager.example/api/session", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	r.AddCookie(&http.Cookie{Name: app.cookieName(), Value: base64.RawURLEncoding.EncodeToString(token)})
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated HTTP session status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"auth.session_valid"`)) ||
		!bytes.Contains(w.Body.Bytes(), []byte(`"authenticated":true`)) {
		t.Fatalf("authenticated HTTP session outcome is incorrect: %s", w.Body.String())
	}
	if got := app.cookieName(); got != insecureSessionCookie {
		t.Fatalf("HTTP cookie name = %q", got)
	}
	projects := httptest.NewRequest(http.MethodGet, "http://manager.example/api/projects", nil)
	projects.RemoteAddr = "192.0.2.10:1234"
	projects.AddCookie(&http.Cookie{Name: app.cookieName(), Value: base64.RawURLEncoding.EncodeToString(token)})
	listed := httptest.NewRecorder()
	app.Handler().ServeHTTP(listed, projects)
	if listed.Code != http.StatusOK {
		t.Fatalf("authenticated HTTP project listing status = %d, want 200: %s", listed.Code, listed.Body.String())
	}
	logout := httptest.NewRequest(http.MethodPost, "http://manager.example/api/logout", nil)
	logout.RemoteAddr = "192.0.2.10:1234"
	logout.Header.Set("Origin", "http://manager.example")
	logout.AddCookie(&http.Cookie{Name: app.cookieName(), Value: base64.RawURLEncoding.EncodeToString(token)})
	loggedOut := httptest.NewRecorder()
	app.Handler().ServeHTTP(loggedOut, logout)
	if loggedOut.Code != http.StatusOK {
		t.Fatalf("HTTP logout status = %d, want 200: %s", loggedOut.Code, loggedOut.Body.String())
	}
	if !bytes.Contains(loggedOut.Body.Bytes(), []byte(`"auth.logged_out"`)) ||
		!bytes.Contains(loggedOut.Body.Bytes(), []byte(`"data":null`)) {
		t.Fatalf("HTTP logout outcome is incorrect: %s", loggedOut.Body.String())
	}
	cookies := loggedOut.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != insecureSessionCookie || cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("HTTP session cookie clearing = %+v", cookies)
	}
}

func TestSessionFailureUsesAuthenticationOutcomeEnvelope(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	request := httptest.NewRequest(http.MethodGet, "https://manager.example/api/session", nil)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	var outcome struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &outcome); err != nil {
		t.Fatalf("response is not an outcome envelope: %v (%q)", err, response.Body.String())
	}
	if outcome.Code != "auth.session_required" || outcome.Message == "" || outcome.Data != nil {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected response headers: %v", response.Header())
	}
}

func TestProjectResourceLimitsRequirePositiveBoundedValues(t *testing.T) {
	for name, override := range map[string]func(*Config){
		"transfer":      func(cfg *Config) { cfg.GitMaxTransferBytes = -1 },
		"cache":         func(cfg *Config) { cfg.GitMaxCacheBytes = -1 },
		"refs":          func(cfg *Config) { cfg.GitMaxRefs = -1 },
		"advertisement": func(cfg *Config) { cfg.GitMaxAdvertisementBytes = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := Config{DatabasePath: filepath.Join(dir, "state.sqlite"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key")}
			override(&cfg)
			if app, err := New(cfg); err == nil {
				app.Close()
				t.Fatal("invalid resource limits were accepted")
			}
		})
	}
}

func TestProjectCacheLimitCanBeSmallerThanTransferLimit(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), GitMaxTransferBytes: 32 << 20, GitMaxCacheBytes: 1024}
	app, err := New(cfg)
	if err != nil {
		t.Fatalf("positive independently enforced cache limit rejected: %v", err)
	}
	app.Close()
}

func TestExpiredGrantAndNonLoopbackHTTPFailClosed(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	var nowMu sync.RWMutex
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), GrantTTL: time.Minute, InsecureLocalHTTP: true, Now: func() time.Time { nowMu.RLock(); defer nowMu.RUnlock(); return now }}
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
	req.Header.Set("Accept", "text/html")
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("fresh local grant rejected: %d", res.Code)
	}
	plainLogin := httptest.NewRequest(http.MethodGet, "/login", nil)
	plainLogin.RemoteAddr, plainLogin.Host = "127.0.0.1:4321", "127.0.0.1:8080"
	plainLogin.Header.Set("Accept", "text/html")
	plainLoginResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(plainLoginResponse, plainLogin)
	if plainLoginResponse.Code != http.StatusUpgradeRequired {
		t.Fatalf("plain HTTP login page status = %d, want 426", plainLoginResponse.Code)
	}
	nowMu.Lock()
	now = now.Add(2 * time.Minute)
	nowMu.Unlock()
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
	nowMu.Lock()
	now = now.Add(-2 * time.Minute)
	nowMu.Unlock()
	proxyGrant, err := app.IssueRegistrationGrant()
	if err != nil {
		t.Fatal(err)
	}
	proxyReq := httptest.NewRequest(http.MethodGet, "/register", nil)
	proxyReq.RemoteAddr, proxyReq.Host = "127.0.0.1:4321", "manager.example:443"
	proxyReq.Header.Set("X-Forwarded-Proto", "https")
	proxyReq.Header.Set("Accept", "text/html")
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

func TestTrustedProxyHTTPSAndOrigin(t *testing.T) {
	app := &App{cfg: Config{TrustedProxies: []netip.Prefix{
		netip.MustParsePrefix("172.18.0.0/16"), netip.MustParsePrefix("2001:db8::/32"),
	}}}
	for _, tc := range []struct {
		peer, forwarded string
		secure          bool
	}{
		{"172.18.4.2:5000", "https", true},
		{"[2001:db8::1]:5000", "https", true},
		{"127.0.0.1:5000", "https", true},
		{"192.0.2.1:5000", "https", false},
		{"172.19.4.2:5000", "https", false},
		{"172.18.4.2:5000", "http", false},
		{"172.18.4.2:5000", "https, http", false},
	} {
		t.Run(tc.peer+"/"+tc.forwarded, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://manager.example/opaque/login/start", nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("X-Forwarded-Proto", tc.forwarded)
			r.Header.Set("Origin", "https://manager.example")
			if got := app.https(r); got != tc.secure {
				t.Fatalf("HTTPS = %t, want %t", got, tc.secure)
			}
			if got := app.originOK(r); got != tc.secure {
				t.Fatalf("origin OK = %t, want %t", got, tc.secure)
			}
		})
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

func TestDashboardEndpointsRequireSessionAndSameOrigin(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	plainStatus := httptest.NewRecorder()
	plainRequest := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	plainRequest.RemoteAddr, plainRequest.Host = "127.0.0.1:43210", "127.0.0.1:8080"
	app.Handler().ServeHTTP(plainStatus, plainRequest)
	if plainStatus.Code != http.StatusUpgradeRequired {
		t.Fatalf("plain HTTP session status = %d, want 426", plainStatus.Code)
	}
	status := httptest.NewRecorder()
	statusRequest := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	statusRequest.RemoteAddr, statusRequest.Host, statusRequest.TLS = "127.0.0.1:43210", "127.0.0.1:8080", &tls.ConnectionState{}
	app.Handler().ServeHTTP(status, statusRequest)
	if status.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated HTTPS session status = %d, want 401", status.Code)
	}

	logout := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	logout.RemoteAddr, logout.Host = "127.0.0.1:43210", "127.0.0.1:8080"
	logout.TLS = &tls.ConnectionState{}
	logout.Header.Set("Origin", "https://attacker.example")
	logoutResponse := httptest.NewRecorder()
	app.Handler().ServeHTTP(logoutResponse, logout)
	if logoutResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-origin logout = %d, want 403", logoutResponse.Code)
	}

	protected := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/private/projects", nil)
	request.Header.Set("Accept", "application/json")
	app.Handler().ServeHTTP(protected, request)
	if protected.Code == http.StatusOK || strings.Contains(protected.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("private endpoint must not return a successful SPA shell: %d %q", protected.Code, protected.Header().Get("Content-Type"))
	}
}
