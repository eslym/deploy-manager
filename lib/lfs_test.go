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
	approved.Body.Close()
	if approved.StatusCode != http.StatusNoContent {
		t.Fatalf("approved settings status = %d", approved.StatusCode)
	}
	getReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/projects/"+projectID+"/lfs", nil)
	getReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token)})
	result, err := client.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	var response struct {
		Settings  lfsProjectSettings `json:"settings"`
		Suggested string             `json:"suggestedEndpoint"`
	}
	if result.StatusCode != http.StatusOK || json.NewDecoder(result.Body).Decode(&response) != nil {
		t.Fatalf("settings GET status = %d", result.StatusCode)
	}
	if !response.Settings.AllowInternal || len(response.Settings.InternalAddresses["https://127.0.0.1"]) != 1 || response.Settings.InternalAddresses["https://127.0.0.1"][0] != "127.0.0.1" {
		t.Fatalf("internal approval was not persisted: %#v", response.Settings)
	}
	if want := "https://git.example.test/repository.git/info/lfs"; response.Suggested != want {
		t.Fatalf("suggested endpoint = %q, want %q", response.Suggested, want)
	}
	if _, err := url.Parse(response.Settings.Endpoint); err != nil {
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
		{"attribute value override", map[string]string{".gitattributes": "*.bin filter=lfs\n*.bin filter=other\n"}, "a.bin", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := effectiveLFSAttribute(tc.files, tc.path)
			if err != nil || got != tc.want {
				t.Fatalf("effectiveLFSAttribute() = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
