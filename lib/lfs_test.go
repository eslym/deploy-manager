package lib

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseLFSPointerRejectsMalformedAndExtensions(t *testing.T) {
	valid := "version https://git-lfs.github.com/spec/v1\noid sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nsize 7\n"
	p, err := parseLFSPointer(strings.NewReader(valid), int64(len(valid)))
	if err != nil || p.OID != strings.Repeat("a", 64) || p.Size != 7 {
		t.Fatalf("valid pointer: %#v, %v", p, err)
	}
	for _, input := range []string{
		"version https://git-lfs.github.com/spec/v1\noid sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nsize -1\n",
		"version https://git-lfs.github.com/spec/v1\next-1-filter sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\noid sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nsize 7\n",
	} {
		if _, err := parseLFSPointer(strings.NewReader(input), int64(len(input))); err == nil {
			t.Fatalf("accepted invalid/unsupported pointer %q", input)
		}
	}
}

func TestLFSSettingsRequireAuthenticatedIndependentApprovalAndPersist(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	projectID := "0123456789abcdef0123456789abcdef"
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "test", "https://git.example.test/repository.git", time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	unauth, err := server.Client().Get(server.URL + "/api/projects/" + projectID + "/lfs")
	if err != nil {
		t.Fatal(err)
	}
	unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated LFS settings status = %d", unauth.StatusCode)
	}
	token := []byte("0123456789abcdef0123456789abcdef")
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration, authenticatedAt: time.Now()}
	app.mu.Unlock()
	client := server.Client()
	getEmptyReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/projects/"+projectID+"/lfs", nil)
	getEmptyReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
	emptyResult, err := client.Do(getEmptyReq)
	if err != nil {
		t.Fatal(err)
	}
	emptyBody, readErr := io.ReadAll(emptyResult.Body)
	emptyResult.Body.Close()
	if readErr != nil || emptyResult.StatusCode != http.StatusOK {
		t.Fatalf("unconfigured settings GET status = %d, read error = %v", emptyResult.StatusCode, readErr)
	}
	var emptyShape map[string]json.RawMessage
	if err := json.Unmarshal(emptyBody, &emptyShape); err != nil {
		t.Fatalf("decode unconfigured settings response: %v", err)
	}
	var emptyData map[string]json.RawMessage
	if err := json.Unmarshal(emptyShape["data"], &emptyData); err != nil {
		t.Fatalf("decode unconfigured settings data: %v", err)
	}
	var emptySettings map[string]json.RawMessage
	if err := json.Unmarshal(emptyData["settings"], &emptySettings); err != nil {
		t.Fatalf("decode unconfigured settings: %v", err)
	}
	if got := string(emptySettings["objectOrigins"]); got != "[]" {
		t.Fatalf("unconfigured objectOrigins JSON = %s, want [] (raw response: %s)", got, emptyBody)
	}
	if got := string(emptySettings["endpoint"]); got != `""` {
		t.Fatalf("unconfigured endpoint JSON = %s, want empty string", got)
	}
	post := func(body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/projects/"+projectID+"/lfs", bytes.NewBufferString(body))
		req.Header.Set("Origin", server.URL)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
		response, requestErr := client.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		return response
	}
	missingInternalConfirmation := post(`{"endpoint":"https://127.0.0.1/info/lfs","objectOrigins":["https://127.0.0.1"],"allowInternal":true,"confirmation":"I independently verified these LFS destinations"}`)
	missingInternalConfirmation.Body.Close()
	if missingInternalConfirmation.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing internal confirmation status = %d", missingInternalConfirmation.StatusCode)
	}
	approved := post(`{"endpoint":"https://127.0.0.1/info/lfs","objectOrigins":["https://127.0.0.1"],"allowInternal":true,"confirmation":"I independently verified these LFS destinations","internalConfirmation":"I independently verified these LFS destinations"}`)
	if approved.StatusCode != http.StatusOK {
		t.Fatalf("approved settings status = %d", approved.StatusCode)
	}
	requireOutcome(t, approved, "lfs.settings_saved", "null")
	approved.Body.Close()
	getReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/projects/"+projectID+"/lfs", nil)
	getReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
	result, err := client.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	var response struct {
		Code string `json:"code"`
		Data struct {
			Settings  lfsProjectSettings `json:"settings"`
			Suggested string             `json:"suggestedEndpoint"`
		} `json:"data"`
	}
	if result.StatusCode != http.StatusOK || json.NewDecoder(result.Body).Decode(&response) != nil {
		t.Fatalf("settings GET status = %d", result.StatusCode)
	}
	if response.Code != "lfs.settings_loaded" || !response.Data.Settings.AllowInternal || len(response.Data.Settings.InternalAddresses["https://127.0.0.1"]) != 1 || response.Data.Settings.InternalAddresses["https://127.0.0.1"][0] != "127.0.0.1" {
		t.Fatalf("internal approval was not persisted: %#v", response.Data.Settings)
	}
	if want := "https://git.example.test/repository.git/info/lfs"; response.Data.Suggested != want {
		t.Fatalf("suggested endpoint = %q, want %q", response.Data.Suggested, want)
	}
	if _, err := url.Parse(response.Data.Settings.Endpoint); err != nil {
		t.Fatal(err)
	}
}

func TestLFSRejectsRedirectsAndCorruptPayloads(t *testing.T) {
	payload := []byte("payload to verify")
	digest := sha256.Sum256(payload)
	oid := hex.EncodeToString(digest[:])
	for _, mode := range []string{"redirect", "corrupt", "missing", "unsupported-transfer"} {
		t.Run(mode, func(t *testing.T) {
			var actionURL string
			targetRequests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/lfs/objects/batch":
					if mode == "missing" {
						_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":%d,"error":{"code":404,"message":"missing"}}]}`, oid, len(payload))
					} else if mode == "unsupported-transfer" {
						_, _ = fmt.Fprintf(w, `{"transfer":"ssh","objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":%q}}}]}`, oid, len(payload), actionURL)
					} else {
						_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":%q}}}]}`, oid, len(payload), actionURL)
					}
				case "/object":
					if mode == "redirect" {
						http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
						return
					}
					bad := append([]byte(nil), payload...)
					bad[0] ^= 0xff
					_, _ = w.Write(bad)
				case "/target":
					targetRequests++
					_, _ = w.Write(payload)
				}
			}))
			t.Cleanup(server.Close)
			actionURL = server.URL + "/object"
			pool := x509.NewCertPool()
			pool.AddCert(server.Certificate())
			oldRoots := lfsTestRootCAs
			lfsTestRootCAs = pool
			t.Cleanup(func() { lfsTestRootCAs = oldRoots })
			hydrator, err := newLFSHydrator("2123456789abcdef0123456789abcdef", lfsProjectSettings{Endpoint: server.URL + "/lfs", ObjectOrigins: []string{server.URL}, AllowInternal: true, InternalAddresses: map[string][]string{server.URL: {"127.0.0.1"}}}, t.TempDir(), 1<<20, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := hydrator.hydrate(context.Background(), lfsPointer{OID: oid, Size: int64(len(payload))}); err == nil {
				t.Fatal("accepted redirected or hash-mismatched payload")
			}
			if targetRequests != 0 {
				t.Fatal("HTTP client followed a redirect")
			}
			entries, err := os.ReadDir(hydrator.cacheRoot)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("failed transfer left cache entries: %#v", entries)
			}
		})
	}
}

func TestLFSRejectsUntrustedTLSBeforeMakingAnHTTPRequest(t *testing.T) {
	requests := atomic.Int64{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	oldRoots := lfsTestRootCAs
	lfsTestRootCAs = nil
	t.Cleanup(func() { lfsTestRootCAs = oldRoots })
	settings := lfsProjectSettings{Endpoint: server.URL + "/lfs", ObjectOrigins: []string{server.URL}, AllowInternal: true, InternalAddresses: map[string][]string{server.URL: {"127.0.0.1"}}}
	hydrator, err := newLFSHydrator("6123456789abcdef0123456789abcdef", settings, t.TempDir(), 1<<20, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("must not be downloaded over untrusted TLS")
	digest := sha256.Sum256(payload)
	_, err = hydrator.hydrate(context.Background(), lfsPointer{OID: hex.EncodeToString(digest[:]), Size: int64(len(payload))})
	if err == nil || requests.Load() != 0 {
		t.Fatalf("untrusted TLS was accepted or reached HTTP handler: err=%v requests=%d", err, requests.Load())
	}
	entries, readErr := os.ReadDir(hydrator.cacheRoot)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("untrusted TLS left cache files: entries=%v err=%v", entries, readErr)
	}
}

func TestLFSAddressPolicyAndActionOriginCredentialScope(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1"} {
		ip := netip.MustParseAddr(raw)
		if lfsAddressAllowed(ip, false, nil) {
			t.Errorf("private/special address %s was allowed without internal approval", raw)
		}
		if !lfsAddressAllowed(ip, true, map[string]bool{ip.String(): true}) {
			t.Errorf("explicit internal approval did not allow %s", raw)
		}
	}
	for _, raw := range []string{"169.254.169.254", "100.64.0.1", "2001:db8::1", "64:ff9b:1::a00:1", "255.255.255.255"} {
		ip := netip.MustParseAddr(raw)
		if lfsAddressAllowed(ip, false, nil) || lfsAddressAllowed(ip, true, map[string]bool{ip.String(): true}) {
			t.Errorf("special address %s was allowed", raw)
		}
	}
	if lfsAddressAllowed(netip.MustParseAddr("::"), true, map[string]bool{"::": true}) || lfsAddressAllowed(netip.MustParseAddr("224.0.0.1"), true, map[string]bool{"224.0.0.1": true}) {
		t.Fatal("unspecified or multicast destination was allowed")
	}
	settings := lfsProjectSettings{Endpoint: "https://lfs.example.test/repo/info/lfs", ObjectOrigins: []string{"https://lfs.example.test", "https://cdn.example.test"}}
	if _, err := validateLFSActionURL("https://cdn.example.test/object?signature=sensitive", settings); err != nil {
		t.Fatalf("approved signed object URL rejected: %v", err)
	}
	if _, err := validateLFSActionURL("https://unapproved.example.test/object", settings); err == nil {
		t.Fatal("unapproved object origin accepted")
	}
}

func TestAnonymousLFSHTTPSDownloadCachesVerifiedContent(t *testing.T) {
	payload := []byte("actual hydrated asset")
	digest := sha256.Sum256(payload)
	oid := hex.EncodeToString(digest[:])
	var actionURL string
	batchRequests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lfs/objects/batch" {
			batchRequests++
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
				t.Errorf("batch request method/auth = %s/%q", r.Method, r.Header.Get("Authorization"))
			}
			var request lfsBatchRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Operation != "download" || len(request.Objects) != 1 || request.Objects[0].OID != oid {
				t.Errorf("unexpected batch request: %#v, %v", request, err)
			}
			w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
			_, _ = fmt.Fprintf(w, `{"transfer":"basic","objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":%q,"header":{"X-Action":"approved"}}}}]}`, oid, len(payload), actionURL)
			return
		}
		if r.URL.Path == "/object" {
			if r.Header.Get("X-Action") != "approved" {
				t.Errorf("action header missing: %q", r.Header.Get("X-Action"))
			}
			_, _ = w.Write(payload)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	actionURL = server.URL + "/object"
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	oldRoots := lfsTestRootCAs
	lfsTestRootCAs = pool
	t.Cleanup(func() { lfsTestRootCAs = oldRoots })
	root := t.TempDir()
	settings := lfsProjectSettings{Endpoint: server.URL + "/lfs", ObjectOrigins: []string{server.URL}, AllowInternal: true, InternalAddresses: map[string][]string{server.URL: {"127.0.0.1"}}}
	hydrator, err := newLFSHydrator("0123456789abcdef0123456789abcdef", settings, root, 1<<20, time.Second*5)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := hydrator.hydrate(context.Background(), lfsPointer{OID: oid, Size: int64(len(payload))})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cache)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("cached payload %q, %v", got, err)
	}
	if !validCachedLFS(cache, lfsPointer{OID: oid, Size: int64(len(payload))}) {
		t.Fatal("new cache entry did not validate")
	}
	requestsBeforeRestart := batchRequests
	restarted, err := newLFSHydrator("0123456789abcdef0123456789abcdef", settings, root, 1<<20, time.Second*5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.hydrate(context.Background(), lfsPointer{OID: oid, Size: int64(len(payload))}); err != nil {
		t.Fatalf("cache reuse: %v", err)
	}
	if batchRequests != requestsBeforeRestart {
		t.Fatal("restart did not reuse the verified disk cache")
	}
	if err = os.WriteFile(cache, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.hydrate(context.Background(), lfsPointer{OID: oid, Size: int64(len(payload))}); err != nil {
		t.Fatalf("corrupt cache redownload: %v", err)
	}
	if batchRequests != requestsBeforeRestart+1 || !validCachedLFS(cache, lfsPointer{OID: oid, Size: int64(len(payload))}) {
		t.Fatal("corrupt cache entry was trusted instead of redownloaded and verified")
	}
	if filepath.Base(cache) != oid {
		t.Fatalf("cache key = %q, want digest", filepath.Base(cache))
	}
	limited, err := newLFSHydratorWithBudget("1123456789abcdef0123456789abcdef", settings, t.TempDir(), 1<<20, int64(len(payload)-1), time.Second*5, &sync.Mutex{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = limited.hydrate(context.Background(), lfsPointer{OID: oid, Size: int64(len(payload))}); err == nil {
		t.Fatal("payload was written beyond the configured cache budget")
	}
}

func TestPrivateLFSCredentialsAreScopedToBatchAndVerifiedCacheIsReusable(t *testing.T) {
	payload := []byte("private repository payload")
	digest := sha256.Sum256(payload)
	oid := hex.EncodeToString(digest[:])
	objectServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer object-only" {
			t.Errorf("approved action credential missing or batch credential leaked: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write(payload)
	}))
	defer objectServer.Close()
	var actionURL string
	batchRequests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/lfs/objects/batch":
			batchRequests++
			if r.Header.Get("Authorization") != "Bearer ephemeral-test-secret" {
				t.Errorf("private LFS batch authorization missing: %q", r.Header.Get("Authorization"))
			}
			_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":%q,"header":{"Authorization":"Bearer object-only"}}}}]}`, oid, len(payload), actionURL)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	actionURL = objectServer.URL + "/object?signature=private-test-signature"
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	pool.AddCert(objectServer.Certificate())
	oldRoots := lfsTestRootCAs
	lfsTestRootCAs = pool
	t.Cleanup(func() { lfsTestRootCAs = oldRoots })
	settings := lfsProjectSettings{Endpoint: server.URL + "/lfs", ObjectOrigins: []string{server.URL, objectServer.URL}, AllowInternal: true, InternalAddresses: map[string][]string{server.URL: {"127.0.0.1"}, objectServer.URL: {"127.0.0.1"}}}
	root := t.TempDir()
	hydrator, err := newLFSHydrator("3123456789abcdef0123456789abcdef", settings, root, 1<<20, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	authCalls := 0
	hydrator.sshAuth = func(context.Context) (lfsAuthCredentials, error) {
		authCalls++
		return lfsAuthCredentials{endpoint: mustLFSURL(t, settings.Endpoint), headers: map[string]string{"Authorization": "Bearer ephemeral-test-secret"}, expires: time.Now().Add(time.Minute)}, nil
	}
	pointer := lfsPointer{OID: oid, Size: int64(len(payload))}
	cache, err := hydrator.hydrate(context.Background(), pointer)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(cache); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("private payload = %q, %v", got, err)
	}
	restarted, err := newLFSHydrator("3123456789abcdef0123456789abcdef", settings, root, 1<<20, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	restarted.sshAuth = hydrator.sshAuth
	if _, err = restarted.hydrate(context.Background(), pointer); err != nil || batchRequests != 1 || authCalls != 1 {
		t.Fatalf("verified cache reuse made an auth/network request: batches=%d auth=%d err=%v", batchRequests, authCalls, err)
	}
}

func TestPrivateLFSCredentialExpiryDuringBatchStopsWithoutRetryOrCacheWrite(t *testing.T) {
	payload := []byte("expiring private object")
	digest := sha256.Sum256(payload)
	oid := hex.EncodeToString(digest[:])
	base := time.Now().UTC()
	clock := atomic.Int64{}
	clock.Store(base.UnixNano())
	var actionURL string
	objectRequests := atomic.Int64{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lfs/objects/batch" {
			clock.Store(base.Add(2 * time.Second).UnixNano())
			_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":%q}}}]}`, oid, len(payload), actionURL)
			return
		}
		objectRequests.Add(1)
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	actionURL = server.URL + "/object"
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	oldRoots := lfsTestRootCAs
	lfsTestRootCAs = pool
	t.Cleanup(func() { lfsTestRootCAs = oldRoots })
	settings := lfsProjectSettings{Endpoint: server.URL + "/lfs", ObjectOrigins: []string{server.URL}, AllowInternal: true, InternalAddresses: map[string][]string{server.URL: {"127.0.0.1"}}}
	hydrator, err := newLFSHydrator("4123456789abcdef0123456789abcdef", settings, t.TempDir(), 1<<20, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hydrator.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	authCalls := 0
	hydrator.sshAuth = func(context.Context) (lfsAuthCredentials, error) {
		authCalls++
		return lfsAuthCredentials{endpoint: mustLFSURL(t, settings.Endpoint), expires: base.Add(time.Second)}, nil
	}
	if _, err = hydrator.hydrate(context.Background(), lfsPointer{OID: oid, Size: int64(len(payload))}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired in-flight credentials returned %v", err)
	}
	entries, err := os.ReadDir(hydrator.cacheRoot)
	if err != nil || len(entries) != 0 || authCalls != 1 || objectRequests.Load() != 0 {
		t.Fatalf("expiry was retried or left cached data: entries=%v auth=%d object=%d err=%v", entries, authCalls, objectRequests.Load(), err)
	}
}

func TestPrivateLFSCredentialExpiryDuringObjectRequestDoesNotCommitCache(t *testing.T) {
	payload := []byte("slow private object")
	digest := sha256.Sum256(payload)
	oid := hex.EncodeToString(digest[:])
	base := time.Now().UTC()
	clock := atomic.Int64{}
	clock.Store(base.UnixNano())
	var actionURL string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lfs/objects/batch" {
			_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":%q}}}]}`, oid, len(payload), actionURL)
			return
		}
		clock.Store(base.Add(2 * time.Second).UnixNano())
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	actionURL = server.URL + "/object"
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	oldRoots := lfsTestRootCAs
	lfsTestRootCAs = pool
	t.Cleanup(func() { lfsTestRootCAs = oldRoots })
	settings := lfsProjectSettings{Endpoint: server.URL + "/lfs", ObjectOrigins: []string{server.URL}, AllowInternal: true, InternalAddresses: map[string][]string{server.URL: {"127.0.0.1"}}}
	hydrator, err := newLFSHydrator("5123456789abcdef0123456789abcdef", settings, t.TempDir(), 1<<20, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hydrator.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	authCalls := 0
	hydrator.sshAuth = func(context.Context) (lfsAuthCredentials, error) {
		authCalls++
		return lfsAuthCredentials{endpoint: mustLFSURL(t, settings.Endpoint), expires: base.Add(time.Second)}, nil
	}
	if _, err = hydrator.hydrate(context.Background(), lfsPointer{OID: oid, Size: int64(len(payload))}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("credentials expiring during object download returned %v", err)
	}
	entries, err := os.ReadDir(hydrator.cacheRoot)
	if err != nil || len(entries) != 0 || authCalls != 1 {
		t.Fatalf("expired object request committed cache or retried auth: entries=%v auth=%d err=%v", entries, authCalls, err)
	}
}

func mustLFSURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestEffectiveLFSAttributesNestedPrecedenceAndMacros(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		path  string
		want  bool
	}{
		{"root wildcard", map[string]string{".gitattributes": "*.bin filter=lfs\n"}, "nested/a.bin", true},
		{"nested override", map[string]string{".gitattributes": "*.bin filter=lfs\n", "nested/.gitattributes": "a.bin -filter\n"}, "nested/a.bin", false},
		{"slash pattern", map[string]string{".gitattributes": "nested/*.bin filter=lfs\n"}, "nested/a.bin", true},
		{"macro unset", map[string]string{".gitattributes": "[attr]large filter=lfs -diff\n*.bin large !filter\n"}, "a.bin", false},
		{"recursive macro", map[string]string{".gitattributes": "[attr]one two\n[attr]two filter=lfs\n*.bin one\n"}, "a.bin", true},
		{"nested recursive macro", map[string]string{".gitattributes": "[attr]one two\n[attr]two filter=lfs\n", "nested/.gitattributes": "a.bin one\n"}, "nested/a.bin", true},
		{"nested macro definition ignored", map[string]string{".gitattributes": "*.bin filter=other\n", "nested/.gitattributes": "[attr]local filter=lfs\na.bin local\n"}, "nested/a.bin", false},
		{"attribute value override", map[string]string{".gitattributes": "*.bin filter=lfs\n*.bin filter=other\n"}, "a.bin", false},
		{"unrelated assignment preserves filter", map[string]string{".gitattributes": "*.bin filter=lfs\nx.bin text\n"}, "x.bin", true},
		{"explicit unset overrides ancestor", map[string]string{".gitattributes": "*.bin filter=lfs\n", "nested/.gitattributes": "x.bin -filter\n"}, "nested/x.bin", false},
		{"explicit unspecified overrides ancestor", map[string]string{".gitattributes": "*.bin filter=lfs\n", "nested/.gitattributes": "x.bin !filter\n"}, "nested/x.bin", false},
		{"double star directory match", map[string]string{".gitattributes": "**/deep/*.bin filter=lfs\n"}, "a/b/deep/file.bin", true},
		{"quoted space pattern", map[string]string{".gitattributes": "\"a b.bin\" filter=lfs\n"}, "a b.bin", true},
		{"trailing slash directory pattern is not recursive", map[string]string{".gitattributes": "nested/ filter=lfs\n"}, "nested/file.bin", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := prepareAttributes(tc.files).effective(tc.path)
			if err != nil || got != tc.want {
				t.Fatalf("prepared attributes effective() = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestEffectiveLFSAttributesRejectsForbiddenNegativePatterns(t *testing.T) {
	if _, err := prepareAttributes(map[string]string{".gitattributes": "!private.bin filter=lfs\n"}).effective("private.bin"); err == nil {
		t.Fatal("forbidden negative attribute pattern was accepted")
	}
}

func TestEffectiveLFSAttributesRejectsMacroCycles(t *testing.T) {
	if _, err := prepareAttributes(map[string]string{".gitattributes": "[attr]one two\n[attr]two one\n*.bin one\n"}).effective("a.bin"); err == nil {
		t.Fatal("cyclic macro expansion was accepted")
	}
}

func TestPreparedAttributesParseApplicableFilesLazilyAndReuseThem(t *testing.T) {
	prepared := prepareAttributes(map[string]string{
		".gitattributes":        "*.bin filter=lfs\n",
		"nested/.gitattributes": "a.bin -filter\n",
		"unused/.gitattributes": "this is deliberately malformed \"\n",
	})
	if got, err := prepared.effective("plain.txt"); err != nil || got {
		t.Fatalf("non-matching file selection = %v, %v", got, err)
	}
	if _, ok := prepared.parsed["unused/.gitattributes"]; ok {
		t.Fatal("unrelated tracked attributes were parsed eagerly")
	}
	for _, file := range []string{"nested/a.bin", "nested/b.bin"} {
		if _, err := prepared.effective(file); err != nil {
			t.Fatal(err)
		}
	}
	if len(prepared.parsed) != 2 {
		t.Fatalf("parsed %d files, want the two applicable sources", len(prepared.parsed))
	}
}

func TestPreparedAttributesRejectInvalidApplicableRule(t *testing.T) {
	prepared := prepareAttributes(map[string]string{".gitattributes": "\"unterminated filter=lfs\n"})
	if _, err := prepared.effective("file.bin"); err == nil {
		t.Fatal("malformed applicable attributes were accepted")
	}
}

func TestPreparedAttributesExpansionLimitIsPerFile(t *testing.T) {
	var rules strings.Builder
	for i := 0; i < maxAttributeExpansions/2+1; i++ {
		fmt.Fprintf(&rules, "* attr%d\n", i)
	}
	prepared := prepareAttributes(map[string]string{".gitattributes": rules.String()})
	for _, file := range []string{"one", "two"} {
		if selected, err := prepared.effective(file); err != nil || selected {
			t.Fatalf("effective(%q) = %v, %v; want non-LFS without error", file, selected, err)
		}
	}
}

func TestPreparedAttributesRejectsExpansionLimitWithinFile(t *testing.T) {
	var rules strings.Builder
	for i := 0; i <= maxAttributeExpansions; i++ {
		fmt.Fprintf(&rules, "* attr%d\n", i)
	}
	if _, err := prepareAttributes(map[string]string{".gitattributes": rules.String()}).effective("one"); err == nil {
		t.Fatal("per-file attribute expansion limit was not enforced")
	}
}

func TestPreparedAttributesKeepsCumulativeRuleWorkLimit(t *testing.T) {
	const rulesPerFile = maxLFSAttributeWork/2 + 1
	var rules strings.Builder
	for i := 0; i < rulesPerFile; i++ {
		fmt.Fprintf(&rules, "never%d attr\n", i)
	}
	prepared := prepareAttributes(map[string]string{".gitattributes": rules.String()})
	if _, err := prepared.effective("first"); err != nil {
		t.Fatalf("first file unexpectedly failed: %v", err)
	}
	if _, err := prepared.effective("second"); err == nil || !strings.Contains(err.Error(), "work limit") {
		t.Fatalf("cumulative rule-check limit error = %v", err)
	}
}
