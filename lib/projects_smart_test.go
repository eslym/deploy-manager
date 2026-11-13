package lib

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
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
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
)

func TestAuthenticatedProjectLifecycleOverSmartHTTPS(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "repo.git")
	featureAtCreate := createSmartFixture(t, remoteDir)
	var failRequests atomic.Bool
	source, err := git.PlainOpen(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	var endpoint *transport.Endpoint
	var backend transport.Transport
	var remoteURL string
	var redirectRequests atomic.Bool
	var redirectedHits atomic.Int32
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failRequests.Load() {
			http.Error(w, "fixture deliberately unavailable", http.StatusServiceUnavailable)
			return
		}
		if endpoint == nil || backend == nil {
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
		session, err := backend.NewUploadPackSession(endpoint, nil)
		if err != nil {
			http.Error(w, "session unavailable", 500)
			return
		}
		defer session.Close()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repo.git/info/refs" && r.URL.Query().Get("service") == "git-upload-pack":
			adv, err := session.AdvertisedReferencesContext(r.Context())
			if err != nil {
				http.Error(w, "advertisement failed", 500)
				return
			}
			adv.Prefix = [][]byte{[]byte("# service=git-upload-pack"), pktline.Flush}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			if err := adv.Encode(w); err != nil {
				t.Errorf("encode refs: %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/repo.git/git-upload-pack":
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
			if err := response.Encode(w); err != nil {
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
	installTrustedSmartHTTPTestClient(t, remote)

	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.sqlite"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), GitTimeout: time.Minute}
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

	featureAfter := addSmartFixtureCommit(t, remoteDir, "after refresh")
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
	gitclient.InstallProtocol("https", githttp.NewClientWithOptions(client, &githttp.ClientOptions{RedirectPolicy: githttp.NoFollowRedirects}))
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
