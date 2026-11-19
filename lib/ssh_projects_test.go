package lib

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
	"golang.org/x/crypto/ssh"
)

func TestSSHRemoteURLValidation(t *testing.T) {
	for _, input := range []string{
		"https://example.test/repo", "ssh://u:secret@example.test/repo", "ssh://example.test/repo?token=x",
		"ssh://example.test/repo#fragment", "ssh://example.test:0/repo", "ssh://example.test/-upload-pack",
		"ssh://example.test/a/../repo", "ssh://example.test/repo%0d%0aexec",
	} {
		if _, _, _, _, err := safeSSHRemote(input); err == nil {
			t.Errorf("accepted unsafe SSH URL %q", input)
		}
	}
	u, host, port, canonical, err := safeSSHRemote("ssh://Deploy@Git.Example.test:2222/team/site.git")
	if err != nil || host != "git.example.test" || port != 2222 || u.User.Username() != "Deploy" || canonical != "ssh://Deploy@git.example.test:2222/team/site.git" {
		t.Fatalf("valid URL parsed as u=%v host=%q port=%d canonical=%q err=%v", u, host, port, canonical, err)
	}
}

func TestSCPSSHRemoteNormalization(t *testing.T) {
	for _, tc := range []struct {
		input, want, path string
	}{
		{"git@github.com:owner/repo.git", "git@github.com:owner/repo.git", "owner/repo.git"},
		{"Deploy@Git.Example.test:team/site.git", "Deploy@git.example.test:team/site.git", "team/site.git"},
	} {
		parsed, host, port, canonical, err := safeSSHRemote(tc.input)
		if err != nil || parsed == nil || host == "" || port != 22 || canonical != tc.want || parsed.Path != tc.path {
			t.Errorf("safeSSHRemote(%q) = (%v, %q, %d, %q, %v), want canonical %q", tc.input, parsed, host, port, canonical, err, tc.want)
		}
	}
	for _, input := range []string{
		"git@github.com:", "git@:owner/repo", "git@github.com:../repo",
		"git@github.com:-upload-pack", "git@github.com:owner/repo\nexec",
		"git:password@github.com:owner/repo", "https://github.com/owner/repo.git",
	} {
		if _, _, _, _, err := safeSSHRemote(input); err == nil {
			t.Errorf("safeSSHRemote accepted unsafe input %q", input)
		}
	}
}

func TestSSHRemoteDetectionPreservesHTTPSAtPaths(t *testing.T) {
	for _, remote := range []string{
		"https://example.com/owner/repo@name.git",
		"https://example.com/@scope/repo.git",
		"https://example.com/owner/repo.git",
	} {
		if isSSHRemote(remote) {
			t.Errorf("isSSHRemote(%q) = true, want public HTTPS", remote)
		}
		if _, err := safeRemote(remote); err != nil {
			t.Errorf("safeRemote(%q): %v", remote, err)
		}
	}
	for _, remote := range []string{
		"ssh://git@example.com/owner/repo.git",
		"git@example.com:owner/repo.git",
	} {
		if !isSSHRemote(remote) {
			t.Errorf("isSSHRemote(%q) = false, want SSH", remote)
		}
	}
}

// This end-to-end application test uses an in-process SSH server and go-git's
// upload-pack implementation; it never starts a local Git/SSH executable.
func TestAuthenticatedSSHProjectRequiresApprovedHostKey(t *testing.T) {
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if err := os.MkdirAll(filepath.Dir(remoteDir), 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainInit(remoteDir, true)
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "work")
	work, err := git.PlainInit(fixture, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := work.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "index.html"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = wt.Add("index.html"); err != nil {
		t.Fatal(err)
	}
	commit, err := wt.Commit("fixture", &git.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.test", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := work.Storer.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		t.Fatal(err)
	}
	if err = objects.ForEach(func(object plumbing.EncodedObject) error {
		_, copyErr := repo.Storer.SetEncodedObject(object)
		return copyErr
	}); err != nil {
		t.Fatal(err)
	}
	if err = repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), commit)); err != nil {
		t.Fatal(err)
	}
	if err = repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}
	server := newControlledSSHGitServer(t, remoteDir)
	defer server.Close()
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), GitTimeout: time.Minute, GitMaxTransferBytes: 4 << 20, GitMaxCacheBytes: 32 << 20, GitMaxRefs: 100}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cookie := installAPITestSession(t, app)
	appServer := httptest.NewTLSServer(app.Handler())
	client := appServer.Client()
	defer appServer.Close()
	key, err := app.createRepositoryKey("test", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("ssh://git@127.0.0.1:%d/repo.git", server.port)
	create := func(name string) *http.Response {
		return projectPost(t, client, appServer.URL, cookie, "/api/projects", fmt.Sprintf(`{"name":%q,"url":%q,"keyId":%q}`, name, url, key.ID))
	}
	first := create("unknown host")
	if first.StatusCode != http.StatusConflict {
		t.Fatalf("unknown SSH host status=%d body=%s", first.StatusCode, responseText(first))
	}
	first.Body.Close()
	warnings := readSSHWarnings(t, client, appServer.URL, cookie)
	if len(warnings) != 1 || warnings[0].Fingerprint == "" || !strings.Contains(warnings[0].Warning, "not approved") {
		t.Fatalf("unknown host warning not persisted: %#v", warnings)
	}
	approveSSHHost(t, client, appServer.URL, cookie, warnings[0])
	trustedHosts := readSSHWarnings(t, client, appServer.URL, cookie)
	if len(trustedHosts) != 1 || !trustedHosts[0].Trusted || trustedHosts[0].Fingerprint != warnings[0].Fingerprint {
		t.Fatalf("trusted SSH host missing from host list: %#v", trustedHosts)
	}
	createdResponse := create("approved host")
	if createdResponse.StatusCode != http.StatusCreated {
		t.Fatalf("approved SSH create status=%d body=%s", createdResponse.StatusCode, responseText(createdResponse))
	}
	created := decodeProjectOutcome(t, createdResponse.Body)
	createdResponse.Body.Close()
	refs := projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID)
	if len(refs) != 1 || refs[0].Name != "main" {
		t.Fatalf("SSH refs: %#v", refs)
	}
	unconfirmedRevoke := projectPost(t, client, appServer.URL, cookie, "/api/ssh-hosts/revoke", fmt.Sprintf(`{"host":%q,"port":%d,"fingerprint":%q}`, trustedHosts[0].Host, trustedHosts[0].Port, trustedHosts[0].Fingerprint))
	if unconfirmedRevoke.StatusCode != http.StatusBadRequest {
		t.Fatalf("unconfirmed SSH host revocation status=%d body=%s", unconfirmedRevoke.StatusCode, responseText(unconfirmedRevoke))
	}
	unconfirmedRevoke.Body.Close()
	revokeSSHHost(t, client, appServer.URL, cookie, trustedHosts[0])
	revokedHosts := readSSHWarnings(t, client, appServer.URL, cookie)
	if len(revokedHosts) != 1 || revokedHosts[0].Trusted || revokedHosts[0].Fingerprint != trustedHosts[0].Fingerprint {
		t.Fatalf("revoked SSH host did not remain listed as untrusted: %#v", revokedHosts)
	}
	revokedRefresh := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	revokedMessage := responseText(revokedRefresh)
	var revokedOutcome struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(revokedMessage), &revokedOutcome); err != nil {
		t.Fatalf("SSH refresh returned a non-outcome response: %s", revokedMessage)
	}
	if revokedRefresh.StatusCode != http.StatusBadGateway || revokedOutcome.Code != "ssh_host.approval_required" {
		t.Fatalf("SSH connection after revocation was not blocked: status=%d body=%s", revokedRefresh.StatusCode, revokedMessage)
	}
	revokedRefresh.Body.Close()
	revokedHosts = readSSHWarnings(t, client, appServer.URL, cookie)
	if len(revokedHosts) != 1 || revokedHosts[0].Trusted || !strings.Contains(revokedHosts[0].Warning, "not approved") {
		t.Fatalf("revoked SSH host did not return to the unapproved warning state: %#v", revokedHosts)
	}
	approveSSHHost(t, client, appServer.URL, cookie, revokedHosts[0])
	if err = app.Close(); err != nil {
		t.Fatal(err)
	}
	appServer.Close()
	app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	cookie = installAPITestSession(t, app)
	appServer = httptest.NewTLSServer(app.Handler())
	client = appServer.Client()
	defer appServer.Close()
	if len(projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID)) != 1 {
		t.Fatal("SSH refs did not survive restart")
	}
	cacheBeforeHostChange, err := digestProjectCache(app.projects.repoPath(created.ID))
	if err != nil {
		t.Fatal(err)
	}
	server.Close()
	server = newControlledSSHGitServerAtPort(t, remoteDir, server.port)
	refsBeforeChange := projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID)
	changed := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	if changed.StatusCode != http.StatusBadGateway {
		t.Fatalf("changed SSH host key status=%d body=%s", changed.StatusCode, responseText(changed))
	}
	changed.Body.Close()
	changedWarnings := readSSHWarnings(t, client, appServer.URL, cookie)
	if len(changedWarnings) != 1 || changedWarnings[0].Trusted || !strings.Contains(changedWarnings[0].Warning, "changed") {
		t.Fatalf("changed SSH key warning missing: %#v", changedWarnings)
	}
	if refs := projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID); !equalProjectRefs(refsBeforeChange, refs) {
		t.Fatal("changed SSH key modified cached refs")
	}
	cacheAfterHostChange, err := digestProjectCache(app.projects.repoPath(created.ID))
	if err != nil {
		t.Fatal(err)
	}
	if cacheAfterHostChange != cacheBeforeHostChange {
		t.Fatal("changed SSH key modified the live project cache")
	}
	server.Close()
	server = newControlledSSHGitServerAtPort(t, remoteDir, server.port)
	changedAgain := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	if changedAgain.StatusCode != http.StatusBadGateway {
		t.Fatalf("second SSH host-key change status=%d", changedAgain.StatusCode)
	}
	changedAgain.Body.Close()
	staleApproval := projectPost(t, client, appServer.URL, cookie, "/api/ssh-hosts/approve", fmt.Sprintf(`{"host":%q,"port":%d,"fingerprint":%q,"confirm":true}`, changedWarnings[0].Host, changedWarnings[0].Port, changedWarnings[0].Fingerprint))
	if staleApproval.StatusCode != http.StatusConflict {
		t.Fatalf("stale SSH host-key approval status=%d", staleApproval.StatusCode)
	}
	requireOutcome(t, staleApproval, "ssh_host.observation_changed", "null")
	staleApproval.Body.Close()
	changedWarnings = readSSHWarnings(t, client, appServer.URL, cookie)
	if len(changedWarnings) != 1 || changedWarnings[0].Fingerprint == "" {
		t.Fatalf("latest SSH host key observation missing: %#v", changedWarnings)
	}
	approveSSHHost(t, client, appServer.URL, cookie, changedWarnings[0])
	recovered := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	if recovered.StatusCode != http.StatusOK {
		t.Fatalf("refresh after host-key approval status=%d body=%s", recovered.StatusCode, responseText(recovered))
	}
	recovered.Body.Close()
	// Trust is bound to both the canonical hostname and port, not merely the
	// address that happened to resolve for this connection.
	hostAliasURL := fmt.Sprintf("ssh://git@localhost:%d/repo.git", server.port)
	alias := projectPost(t, client, appServer.URL, cookie, "/api/projects", fmt.Sprintf(`{"name":"host isolation","url":%q,"keyId":%q}`, hostAliasURL, key.ID))
	if alias.StatusCode != http.StatusConflict {
		t.Fatalf("different hostname reused trust: status=%d", alias.StatusCode)
	}
	alias.Body.Close()
	otherPortServer := newControlledSSHGitServer(t, remoteDir)
	otherPortURL := fmt.Sprintf("ssh://git@127.0.0.1:%d/repo.git", otherPortServer.port)
	portIsolation := projectPost(t, client, appServer.URL, cookie, "/api/projects", fmt.Sprintf(`{"name":"port isolation","url":%q,"keyId":%q}`, otherPortURL, key.ID))
	if portIsolation.StatusCode != http.StatusConflict {
		t.Fatalf("different port reused trust: status=%d", portIsolation.StatusCode)
	}
	portIsolation.Body.Close()
	otherPortServer.Close()
	second := create("shared key")
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("second SSH project status=%d body=%s", second.StatusCode, responseText(second))
	}
	secondProject := decodeProjectOutcome(t, second.Body)
	second.Body.Close()
	blockedDelete := projectPost(t, client, appServer.URL, cookie, "/api/repository-keys/"+key.ID+"/delete", `{}`)
	if blockedDelete.StatusCode != http.StatusConflict {
		t.Fatalf("assigned shared key delete status=%d", blockedDelete.StatusCode)
	}
	requireOutcome(t, blockedDelete, "repository_key.in_use", "null")
	blockedDelete.Body.Close()
	replacement, err := app.createRepositoryKey("replacement", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, projectID := range []string{created.ID, secondProject.ID} {
		assigned := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+projectID+"/key", fmt.Sprintf(`{"keyId":%q}`, replacement.ID))
		var assignmentOutcome struct {
			Code string `json:"code"`
			Data any    `json:"data"`
		}
		if assigned.StatusCode != http.StatusOK || json.NewDecoder(assigned.Body).Decode(&assignmentOutcome) != nil || assignmentOutcome.Code != "project.key_assigned" || assignmentOutcome.Data != nil {
			t.Fatalf("reassign key status=%d body=%s", assigned.StatusCode, responseText(assigned))
		}
		assigned.Body.Close()
	}
	deleteResponse := projectPost(t, client, appServer.URL, cookie, "/api/repository-keys/"+key.ID+"/delete", `{}`)
	if deleteResponse.StatusCode != http.StatusOK {
		t.Fatalf("assigned shared key delete status=%d", deleteResponse.StatusCode)
	}
	requireOutcome(t, deleteResponse, "repository_key.deleted", "null")
	deleteResponse.Body.Close()
	// A wrong managed signer is a clear authorization failure and never changes
	// the project cache or cached ref list.
	server.rejectAuth.Store(true)
	before := projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID)
	failed := projectPost(t, client, appServer.URL, cookie, "/api/projects/"+created.ID+"/refresh", `{}`)
	if failed.StatusCode != http.StatusBadGateway {
		t.Fatalf("SSH auth failure status=%d body=%s", failed.StatusCode, responseText(failed))
	}
	failedMessage := responseText(failed)
	failed.Body.Close()
	var failedOutcome struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(failedMessage), &failedOutcome); err != nil || failedOutcome.Code != "git.authentication_failed" {
		t.Fatalf("SSH authentication failure was not actionable: %q", failedMessage)
	}
	failedAssignedDelete := projectPost(t, client, appServer.URL, cookie, "/api/repository-keys/"+replacement.ID+"/delete", `{}`)
	if failedAssignedDelete.StatusCode != http.StatusConflict {
		t.Fatalf("key assigned to a failed-refresh project was deletable: status=%d", failedAssignedDelete.StatusCode)
	}
	failedAssignedDelete.Body.Close()
	if got := projectRefsFromAPI(t, client, appServer.URL, cookie, created.ID); !equalProjectRefs(before, got) {
		t.Fatal("SSH auth failure changed cached refs")
	}
}

type controlledSSHGitServer struct {
	listener   net.Listener
	port       int
	done       chan struct{}
	rejectAuth atomic.Bool
}

func newControlledSSHGitServer(t *testing.T, dir string) *controlledSSHGitServer {
	return newControlledSSHGitServerAtPort(t, dir, 0)
}

func newControlledSSHGitServerAtPort(t *testing.T, dir string, port int) *controlledSSHGitServer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := transport.NewEndpoint(fmt.Sprintf("ssh://git@127.0.0.1:%d/repo.git", listener.Addr().(*net.TCPAddr).Port))
	if err != nil {
		t.Fatal(err)
	}
	backend := gitserver.NewServer(gitserver.MapLoader{ep.String(): fixture.Storer})
	s := &controlledSSHGitServer{listener: listener, port: listener.Addr().(*net.TCPAddr).Port, done: make(chan struct{})}
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if s.rejectAuth.Load() {
			return nil, errors.New("fixture rejects repository key")
		}
		return &ssh.Permissions{}, nil
	}}
	serverConfig.AddHostKey(signer)
	go func() {
		defer close(s.done)
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			go func(raw net.Conn) {
				defer raw.Close()
				sshConn, chans, reqs, e := ssh.NewServerConn(raw, serverConfig)
				if e != nil {
					return
				}
				defer sshConn.Close()
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					if ch.ChannelType() != "session" {
						_ = ch.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					channel, requests, e := ch.Accept()
					if e != nil {
						continue
					}
					go func(c ssh.Channel, requests <-chan *ssh.Request) {
						defer c.Close()
						for req := range requests {
							if req.Type != "exec" {
								_ = req.Reply(false, nil)
								continue
							}
							var command struct{ Value string }
							if ssh.Unmarshal(req.Payload, &command) != nil || !strings.HasPrefix(command.Value, "git-upload-pack ") {
								_ = req.Reply(false, nil)
								return
							}
							_ = req.Reply(true, nil)
							session, e := backend.NewUploadPackSession(ep, nil)
							if e != nil {
								return
							}
							adv, e := session.AdvertisedReferencesContext(context.Background())
							if e != nil {
								return
							}
							if e = adv.Encode(c); e != nil {
								return
							}
							request := packp.NewUploadPackRequest()
							if e = request.Decode(c); e != nil {
								return
							}
							response, e := session.UploadPack(context.Background(), request)
							if e != nil {
								return
							}
							_ = response.Encode(c)
							return
						}
					}(channel, requests)
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { s.Close() })
	return s
}
func (s *controlledSSHGitServer) Close() { _ = s.listener.Close(); <-s.done }
func readSSHWarnings(t *testing.T, c *http.Client, origin, cookie string) []sshHostWarning {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, origin+"/api/ssh-hosts", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var envelope struct {
		Code string           `json:"code"`
		Data []sshHostWarning `json:"data"`
	}
	if err = json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != "ssh_host.listed" {
		t.Fatalf("SSH host list outcome = %q", envelope.Code)
	}
	return envelope.Data
}
func approveSSHHost(t *testing.T, c *http.Client, origin, cookie string, host sshHostWarning) {
	t.Helper()
	res := projectPost(t, c, origin, cookie, "/api/ssh-hosts/approve", fmt.Sprintf(`{"host":%q,"port":%d,"fingerprint":%q,"confirm":true}`, host.Host, host.Port, host.Fingerprint))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("approve SSH host status=%d body=%s", res.StatusCode, responseText(res))
	}
	requireOutcome(t, res, "ssh_host.approved", "null")
	res.Body.Close()
}

func revokeSSHHost(t *testing.T, c *http.Client, origin, cookie string, host sshHostWarning) {
	t.Helper()
	res := projectPost(t, c, origin, cookie, "/api/ssh-hosts/revoke", fmt.Sprintf(`{"host":%q,"port":%d,"fingerprint":%q,"confirm":true}`, host.Host, host.Port, host.Fingerprint))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("revoke SSH host status=%d body=%s", res.StatusCode, responseText(res))
	}
	requireOutcome(t, res, "ssh_host.revoked", "null")
	res.Body.Close()
}
