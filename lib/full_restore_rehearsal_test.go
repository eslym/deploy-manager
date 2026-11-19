package lib

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bytemare/opaque"
	"golang.org/x/sys/unix"
)

func unixRenameExchange(first, second string) error {
	return unix.Renameat2(unix.AT_FDCWD, first, unix.AT_FDCWD, second, unix.RENAME_EXCHANGE)
}

// TestDocumentedFullInstallationRestoreRehearsal exercises an offline,
// identity-preserving installation snapshot/restore through application seams.
// A directory rename stands in for the storage provider's atomic snapshot
// restore; the focused crash tests separately exercise real process death.
func TestDocumentedFullInstallationRestoreRehearsal(t *testing.T) {
	parent := t.TempDir()
	installation := filepath.Join(parent, "installation")
	if err := os.MkdirAll(installation, 0700); err != nil {
		t.Fatal(err)
	}
	config := func(root string) Config {
		return Config{
			DatabasePath:      filepath.Join(root, "data", "state.sqlite"),
			OPAQUEKeyPath:     filepath.Join(root, "keys", "opaque.key"),
			RepositoryKeyPath: filepath.Join(root, "keys", "repository.key"),
			DeploymentRoot:    filepath.Join(root, "publications"),
			InsecureLocalHTTP: true,
			DeployGracePeriod: 24 * time.Hour,
		}
	}
	cfg := config(installation)
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const origin = "https://manager.example"
	const initialPassword = "full-restore initial password"
	setup, err := app.IssueRegistrationGrant()
	if err != nil {
		t.Fatal(err)
	}
	registrationHandle, registrationRecord := newRegistrationFlow(t, app, setup, origin, initialPassword)
	if status := finishTestRegistration(app, "", registrationHandle, registrationRecord, origin); status != http.StatusOK {
		t.Fatalf("initial OPAQUE registration status=%d", status)
	}
	resurrectableGrant, err := app.IssueRecoveryGrant()
	if err != nil {
		t.Fatal(err)
	}
	oldSession := completeFixtureLogin(t, app, initialPassword)
	pendingLogin := pendingFixtureLoginFinish(t, app, initialPassword)
	repositoryKey, err := app.createRepositoryKey("restore witness", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !app.repositoryKeyCanSign(repositoryKey.ID, []byte("repository key restore witness")) {
		t.Fatal("repository key did not decrypt before snapshot")
	}
	remote := filepath.Join(t.TempDir(), "remote")
	_ = createSmartFixture(t, remote)
	sshServer := newControlledSSHGitServer(t, remote)
	defer sshServer.Close()
	adminCookie := completeFixtureLogin(t, app, initialPassword).Value
	appServer := httptest.NewTLSServer(app.Handler())
	defer appServer.Close()
	client := appServer.Client()
	remoteURL := fmt.Sprintf("ssh://git@127.0.0.1:%d/repo.git", sshServer.port)
	createProject := func() *http.Response {
		return projectPost(t, client, appServer.URL, adminCookie, "/api/projects", fmt.Sprintf(`{"name":"restore-witness","url":%q,"keyId":%q}`, remoteURL, repositoryKey.ID))
	}
	unknownHost := createProject()
	if unknownHost.StatusCode != http.StatusConflict {
		t.Fatalf("unapproved SSH host status=%d body=%s", unknownHost.StatusCode, responseText(unknownHost))
	}
	unknownHost.Body.Close()
	warnings := readSSHWarnings(t, client, appServer.URL, adminCookie)
	if len(warnings) != 1 || warnings[0].Fingerprint == "" {
		t.Fatalf("missing independently approvable SSH host observation: %#v", warnings)
	}
	approveSSHHost(t, client, appServer.URL, adminCookie, warnings[0])
	created := createProject()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("approved SSH project create status=%d body=%s", created.StatusCode, responseText(created))
	}
	var projectRecord project
	if err := json.NewDecoder(created.Body).Decode(&projectRecord); err != nil {
		created.Body.Close()
		t.Fatal(err)
	}
	created.Body.Close()
	if refs := projectRefsFromAPI(t, client, appServer.URL, adminCookie, projectRecord.ID); len(refs) != 3 {
		t.Fatalf("SSH refs before snapshot: %#v", refs)
	}

	deploymentID, projectID, newJob := strings.Repeat("a", 32), projectRecord.ID, strings.Repeat("d", 32)
	now := time.Now().UTC().UnixNano()
	publication := app.deploymentPath(deploymentID)
	stage := app.stagePath(deploymentID, newJob)
	if err := os.MkdirAll(publication, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publication, "index.html"), []byte("old live publication"), 0644); err != nil {
		t.Fatal(err)
	}
	oldIdentity, err := identity(publication)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stage, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "index.html"), []byte("restored interrupted publication"), 0644); err != nil {
		t.Fatal(err)
	}
	newIdentity, err := identity(stage)
	if err != nil {
		t.Fatal(err)
	}
	// Model the on-disk state after an atomic exchange but before the database
	// commit. Startup must resolve identity, never repeat the exchange blindly.
	if err := unixRenameExchange(publication, stage); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,generation,live_commit,live_dev,live_ino,status,created_at,updated_at,target_kind,target_name) VALUES(?,?,?, ?,1,?,?,?,'live',?,?,'branch','main')`, deploymentID, projectID, "restored", strings.Repeat("e", 40), strings.Repeat("0", 40), oldIdentity.Dev, oldIdentity.Ino, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at,durable,automatic) VALUES(?,?,?,1,'running','interrupted at exchange',?,?,1,0)`, newJob, deploymentID, strings.Repeat("e", 40), now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO publication_intents(deployment_id,job_id,stage_name,had_old,old_dev,old_ino,new_dev,new_ino,phase,created_at) VALUES(?,?,?,1,?,?,?,?,'intent',?)`, deploymentID, newJob, filepath.Base(stage), oldIdentity.Dev, oldIdentity.Ino, newIdentity.Dev, newIdentity.Ino, now); err != nil {
		t.Fatal(err)
	}
	// A second cleanup obligation is already due. Both the delayed old
	// publication and this expendable directory must be reconciled safely.
	cleanupJob := strings.Repeat("f", 32)
	cleanupPath := app.stagePath(deploymentID, cleanupJob)
	if err := os.MkdirAll(cleanupPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cleanupPath, "discard.html"), []byte("cleanup witness"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupIdentity, err := identity(cleanupPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO publication_cleanup(deployment_id,job_id,path_name,dev,ino,not_before,last_error,attempts) VALUES(?,?,?,?,?,?,?,0)`, deploymentID, cleanupJob, filepath.Base(cleanupPath), cleanupIdentity.Dev, cleanupIdentity.Ino, now-1, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	opaqueKeyBytes, err := os.ReadFile(cfg.OPAQUEKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	repositoryMasterBytes, err := os.ReadFile(cfg.RepositoryKeyPath)
	if err != nil {
		t.Fatal(err)
	}

	// Take and restore the whole installation as a single rename generation.
	// This preserves the directory identities which ordinary copies cannot.
	snapshot := filepath.Join(parent, "snapshot")
	if err := os.Rename(installation, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(snapshot, installation); err != nil {
		t.Fatal(err)
	}
	// Apply the documented offline sanitization before allowing startup.
	db, err := sql.Open("sqlite3", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(context.Background(), "DELETE FROM registration_grants; PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Inventory mismatch is a stop condition, not an invitation to generate a
	// replacement key. Keep the exact restored bytes and verify failed startup
	// neither silently replaces nor repairs either external secret.
	for _, key := range []struct {
		path  string
		good  []byte
		index int
	}{
		{path: cfg.OPAQUEKeyPath, good: opaqueKeyBytes, index: len(opaqueKeyMagic)},
		{path: cfg.RepositoryKeyPath, good: repositoryMasterBytes, index: len(repositoryMasterBytes) - 1},
	} {
		bad := append([]byte(nil), key.good...)
		bad[key.index] ^= 0xff
		if err := os.WriteFile(key.path, bad, 0600); err != nil {
			t.Fatal(err)
		}
		if rejected, err := New(cfg); err == nil {
			_ = rejected.Close()
			t.Fatalf("startup accepted mismatched external key %s", key.path)
		}
		observed, err := os.ReadFile(key.path)
		if err != nil || string(observed) != string(bad) {
			t.Fatalf("failed startup modified mismatched key %s: %v", key.path, err)
		}
		if err := os.WriteFile(key.path, key.good, 0600); err != nil {
			t.Fatal(err)
		}
	}

	app, err = New(cfg)
	if err != nil {
		t.Fatalf("restored installation failed startup reconciliation: %v", err)
	}
	defer app.Close()
	if b, err := os.ReadFile(filepath.Join(publication, "index.html")); err != nil || string(b) != "restored interrupted publication" {
		t.Fatalf("restored live content=%q err=%v", b, err)
	}
	if got, err := identity(publication); err != nil || got != newIdentity {
		t.Fatalf("restore changed publication directory identity: got=%+v want=%+v err=%v", got, newIdentity, err)
	}
	if got, err := identity(stage); err != nil || got != oldIdentity {
		t.Fatalf("reconciliation lost or changed the replaced publication: got=%+v want=%+v err=%v", got, oldIdentity, err)
	}
	if b, err := os.ReadFile(filepath.Join(stage, "index.html")); err != nil || string(b) != "old live publication" {
		t.Fatalf("replaced publication content=%q err=%v", b, err)
	}
	if _, err := os.Lstat(cleanupPath); !os.IsNotExist(err) {
		t.Fatalf("due cleanup obligation did not resume safely: %v", err)
	}
	var intentCount, cleanupCount int
	if err := app.db.QueryRow(`SELECT count(*) FROM publication_intents WHERE deployment_id=?`, deploymentID).Scan(&intentCount); err != nil || intentCount != 0 {
		t.Fatalf("publication intent remains=%d err=%v", intentCount, err)
	}
	if err := app.db.QueryRow(`SELECT count(*) FROM publication_cleanup WHERE deployment_id=? AND job_id=?`, deploymentID, cleanupJob).Scan(&cleanupCount); err != nil || cleanupCount != 0 {
		t.Fatalf("cleanup obligation remains=%d err=%v", cleanupCount, err)
	}
	if err := app.db.QueryRow(`SELECT count(*) FROM publication_cleanup WHERE deployment_id=? AND job_id=?`, deploymentID, newJob).Scan(&cleanupCount); err != nil || cleanupCount != 1 {
		t.Fatalf("reconciled old-publication cleanup obligation=%d err=%v, want delayed cleanup retained", cleanupCount, err)
	}
	if !app.repositoryKeyCanSign(repositoryKey.ID, []byte("repository key restore witness")) {
		t.Fatal("restored repository key could not be decrypted")
	}
	if status := registrationRequest(app, http.MethodPost, "/opaque/login/finish", pendingLogin, origin).Code; status != http.StatusUnauthorized {
		t.Fatalf("pre-restore pending OPAQUE login status=%d, want 401", status)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	request.RemoteAddr, request.Host = "127.0.0.1:12345", "manager.example"
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Origin", origin)
	request.AddCookie(oldSession)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("pre-restore session status=%d, want 401", response.Code)
	}
	if status := tryOldRecoveryGrant(t, app, resurrectableGrant, origin); status != http.StatusGone {
		t.Fatalf("restored old recovery grant status=%d, want 410", status)
	}
	cookie := completeFixtureLogin(t, app, initialPassword).Value
	restoredServer := httptest.NewTLSServer(app.Handler())
	defer restoredServer.Close()
	refresh := projectPost(t, restoredServer.Client(), restoredServer.URL, cookie, "/api/projects/"+projectID+"/refresh", `{}`)
	if refresh.StatusCode != http.StatusOK {
		t.Fatalf("restored managed-key/host-trust repository refresh status=%d body=%s", refresh.StatusCode, responseText(refresh))
	}
	refresh.Body.Close()
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	newGrant, err := issuer.IssueRecoveryGrant()
	if err != nil {
		_ = issuer.Close()
		t.Fatal(err)
	}
	if err := issuer.Close(); err != nil {
		t.Fatal(err)
	}
	app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	recoveryHandle, recoveryRecord := newRegistrationFlow(t, app, newGrant, origin, "restored recovery password")
	if status := finishTestRegistration(app, "", recoveryHandle, recoveryRecord, origin); status != http.StatusOK {
		t.Fatalf("restored OPAQUE recovery status=%d", status)
	}
	if !app.repositoryKeyCanSign(repositoryKey.ID, []byte("repository key restore witness")) {
		t.Fatal("OPAQUE recovery changed repository-key access")
	}
	if cookie := completeFixtureLogin(t, app, "restored recovery password"); cookie == nil {
		t.Fatal("recovered OPAQUE credential did not permit login")
	}
	_ = app.Close()
}

func tryOldRecoveryGrant(t *testing.T, app *App, grant, origin string) int {
	t.Helper()
	client, err := opaque.NewClient(opaque.DefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	defer client.ClearState()
	init, err := client.RegistrationInit([]byte("must not be accepted"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(protocolMessage{Grant: grant, Value: base64.RawURLEncoding.EncodeToString(init.Serialize())})
	if err != nil {
		t.Fatal(err)
	}
	return registrationRequest(app, http.MethodPost, "/opaque/register/start", body, origin).Code
}
