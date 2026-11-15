package lib

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bytemare/opaque"
)

func TestRegistrationAndRecoveryRequireOriginBoundOneUseTransaction(t *testing.T) {
	now := time.Now().UTC()
	var nowMu sync.RWMutex
	dir := t.TempDir()
	app, err := New(Config{
		DatabasePath: filepath.Join(dir, "state.sqlite"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"),
		InsecureLocalHTTP: true, Now: func() time.Time { nowMu.RLock(); defer nowMu.RUnlock(); return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	origin := "https://manager.example"

	setupGrant, err := app.IssueRegistrationGrant()
	if err != nil {
		t.Fatal(err)
	}
	setupHandle, setupRecord := newRegistrationFlow(t, app, setupGrant, origin, "initial secret")
	if got := finishTestRegistration(app, "", setupHandle, setupRecord, origin); got != http.StatusNoContent {
		t.Fatalf("initial registration status %d", got)
	}

	expiredGrant, err := app.IssueRecoveryGrant()
	if err != nil {
		t.Fatal(err)
	}
	expiredHandle, expiredRecord := newRegistrationFlow(t, app, expiredGrant, origin, "expired secret")
	nowMu.Lock()
	now = now.Add(6 * time.Minute)
	nowMu.Unlock()
	if got := finishTestRegistration(app, "", expiredHandle, expiredRecord, origin); got != http.StatusGone {
		t.Fatalf("expired transaction finish status %d, want 410", got)
	}
	if app.credentialGeneration != 1 {
		t.Fatal("expired transaction changed the credential")
	}

	recoveryGrant, err := app.IssueRecoveryGrant()
	if err != nil {
		t.Fatal(err)
	}
	recoveryHandle, recoveryRecord := newRegistrationFlow(t, app, recoveryGrant, origin, "replacement secret")
	if got := finishTestRegistration(app, recoveryGrant, "", recoveryRecord, origin); got != http.StatusBadRequest {
		t.Fatalf("grant-only recovery finish status %d, want 400", got)
	}
	if app.credentialGeneration != 1 {
		t.Fatal("grant-only recovery finish changed the credential")
	}
	if got := finishTestRegistration(app, "", recoveryHandle, recoveryRecord, "https://attacker.example"); got != http.StatusForbidden {
		t.Fatalf("cross-origin finish status %d, want 403", got)
	}
	if app.credentialGeneration != 1 {
		t.Fatal("cross-origin recovery finish changed the credential")
	}
	if got := finishTestRegistration(app, "", recoveryHandle, recoveryRecord, origin); got != http.StatusNoContent {
		t.Fatalf("valid recovery finish status %d, want 204", got)
	}
	if got := finishTestRegistration(app, "", recoveryHandle, recoveryRecord, origin); got != http.StatusBadRequest && got != http.StatusGone {
		t.Fatalf("replayed recovery finish status %d, want rejection", got)
	}
	if app.credentialGeneration != 2 {
		t.Fatalf("credential generation = %d, want 2", app.credentialGeneration)
	}
}

func TestRestoredInstallationGrantPurgeAndCLIRecovery(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.sqlite"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	setupGrant, err := app.IssueRegistrationGrant()
	if err != nil {
		t.Fatal(err)
	}
	handle, record := newRegistrationFlow(t, app, setupGrant, "https://manager.example", "restore-test password")
	if got := finishTestRegistration(app, "", handle, record, "https://manager.example"); got != http.StatusNoContent {
		t.Fatalf("initial registration status=%d", got)
	}
	oldSession := completeFixtureLogin(t, app, "restore-test password")
	pendingLoginFinish := pendingFixtureLoginFinish(t, app, "restore-test password")
	resurrected, err := app.IssueRecoveryGrant()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	// This is the documented offline restore action: delete all possibly
	// resurrected grants while no application process has the database open.
	db, err := sql.Open("sqlite3", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(context.Background(), "DELETE FROM registration_grants"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart the application as a restore would. It must invalidate in-memory
	// sessions/login handshakes, and the offline purge must reject old grants.
	app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	request.RemoteAddr, request.Host = "127.0.0.1:12345", "manager.example"
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Origin", "https://manager.example")
	request.AddCookie(oldSession)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("pre-restore session status=%d, want 401", response.Code)
	}
	if got := registrationRequest(app, http.MethodPost, "/opaque/login/finish", pendingLoginFinish, "https://manager.example").Code; got != http.StatusUnauthorized {
		t.Fatalf("pre-restore pending login finish status=%d, want 401", got)
	}
	client, err := opaque.NewClient(opaque.DefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	requestInit, err := client.RegistrationInit([]byte("must not be accepted"))
	if err != nil {
		t.Fatal(err)
	}
	oldBody, err := json.Marshal(protocolMessage{Grant: resurrected, Value: base64.RawURLEncoding.EncodeToString(requestInit.Serialize())})
	if err != nil {
		t.Fatal(err)
	}
	if got := registrationRequest(app, http.MethodPost, "/opaque/register/start", oldBody, "https://manager.example").Code; got != http.StatusGone {
		t.Fatalf("restored consumed/revoked grant status=%d, want 410", got)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	// The real CLI uses this compatible grant-issuer path; it must not migrate
	// the restored store and can issue a fresh one-use recovery grant.
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	newGrant, err := issuer.IssueRecoveryGrant()
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.Close(); err != nil {
		t.Fatal(err)
	}

	app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	newHandle, newRecord := newRegistrationFlow(t, app, newGrant, "https://manager.example", "replacement restore password")
	if got := finishTestRegistration(app, "", newHandle, newRecord, "https://manager.example"); got != http.StatusNoContent {
		t.Fatalf("new CLI-issued recovery grant status=%d, want 204", got)
	}
}

func pendingFixtureLoginFinish(t *testing.T, app *App, password string) []byte {
	t.Helper()
	client, err := opaque.NewClient(opaque.DefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	defer client.ClearState()
	ke1, err := client.GenerateKE1([]byte(password))
	if err != nil {
		t.Fatal(err)
	}
	startBody, err := json.Marshal(protocolMessage{Value: base64.RawURLEncoding.EncodeToString(ke1.Serialize())})
	if err != nil {
		t.Fatal(err)
	}
	start := registrationRequest(app, http.MethodPost, "/opaque/login/start", startBody, "https://manager.example")
	if start.Code != http.StatusOK {
		t.Fatalf("pre-restore OPAQUE login start status=%d: %s", start.Code, start.Body.String())
	}
	var response struct {
		Transaction string `json:"transaction"`
		Value       string `json:"value"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	ke2Bytes, err := base64.RawURLEncoding.DecodeString(response.Value)
	if err != nil {
		t.Fatal(err)
	}
	ke2, err := client.Deserialize.KE2(ke2Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ke3, _, _, err := client.GenerateKE3(ke2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	finishBody, err := json.Marshal(protocolMessage{Transaction: &response.Transaction, Value: base64.RawURLEncoding.EncodeToString(ke3.Serialize())})
	if err != nil {
		t.Fatal(err)
	}
	return finishBody
}

func newRegistrationFlow(t *testing.T, app *App, grant, origin, password string) (string, []byte) {
	return newRegistrationFlowWithIdentities(t, app, grant, origin, password, []byte("administrator"), nil)
}

func newRegistrationFlowWithIdentities(t *testing.T, app *App, grant, origin, password string, clientIdentity, serverIdentity []byte) (string, []byte) {
	t.Helper()
	client, err := opaque.NewClient(opaque.DefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.RegistrationInit([]byte(password))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(protocolMessage{Grant: grant, Value: base64.RawURLEncoding.EncodeToString(request.Serialize())})
	recorder := registrationRequest(app, http.MethodPost, "/opaque/register/start", body, origin)
	if recorder.Code != http.StatusOK {
		t.Fatalf("registration start status %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Value       string `json:"value"`
		Transaction string `json:"transaction"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Value == "" || response.Transaction == "" {
		t.Fatalf("registration start returned incomplete challenge: %#v (%v)", response, err)
	}
	responseBytes, err := base64.RawURLEncoding.DecodeString(response.Value)
	if err != nil {
		t.Fatal(err)
	}
	registrationResponse, err := client.Deserialize.RegistrationResponse(responseBytes)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := client.RegistrationFinalize(registrationResponse, clientIdentity, serverIdentity)
	if err != nil {
		t.Fatal(err)
	}
	return response.Transaction, record.Serialize()
}

func finishTestRegistration(app *App, grant, handle string, record []byte, origin string) int {
	transaction := (*string)(nil)
	if handle != "" {
		transaction = &handle
	}
	body, _ := json.Marshal(protocolMessage{Grant: grant, Transaction: transaction, Value: base64.RawURLEncoding.EncodeToString(record)})
	return registrationRequest(app, http.MethodPost, "/opaque/register/finish", body, origin).Code
}

func registrationRequest(app *App, method, path string, body []byte, origin string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Host = "manager.example"
	request.RemoteAddr = "127.0.0.1:12345"
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Origin", origin)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	return recorder
}
