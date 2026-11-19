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
	requireOpaqueSuccess(t, finishTestRegistrationResponse(app, "", setupHandle, setupRecord, origin), http.StatusOK, "auth.registration_completed", "null")

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
	requireOpaqueSuccess(t, finishTestRegistrationResponse(app, "", recoveryHandle, recoveryRecord, origin), http.StatusOK, "auth.registration_completed", "null")
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
	requireOpaqueSuccess(t, finishTestRegistrationResponse(app, "", handle, record, "https://manager.example"), http.StatusOK, "auth.registration_completed", "null")
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
	requireOpaqueSuccess(t, finishTestRegistrationResponse(app, "", newHandle, newRecord, "https://manager.example"), http.StatusOK, "auth.registration_completed", "null")
}

func TestPasswordChangeReturnsOutcomeAndRevokesSession(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{
		DatabasePath:  filepath.Join(dir, "state.sqlite"),
		OPAQUEKeyPath: filepath.Join(dir, "opaque.key"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	setupGrant, err := app.IssueRegistrationGrant()
	if err != nil {
		t.Fatal(err)
	}
	handle, record := newRegistrationFlow(t, app, setupGrant, "https://manager.example", "original password")
	if status := finishTestRegistration(app, "", handle, record, "https://manager.example"); status != http.StatusOK {
		t.Fatalf("registration status = %d, want 200", status)
	}
	cookie := completeFixtureLogin(t, app, "original password")

	client, err := opaque.NewClient(opaque.DefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	defer client.ClearState()
	request, err := client.RegistrationInit([]byte("replacement password"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(protocolMessage{Value: base64.RawURLEncoding.EncodeToString(request.Serialize())})
	startRequest := httptest.NewRequest(http.MethodPost, "/opaque/password/change/start", bytes.NewReader(body))
	startRequest.Host = "manager.example"
	startRequest.RemoteAddr = "127.0.0.1:12345"
	startRequest.TLS = &tls.ConnectionState{}
	startRequest.Header.Set("Origin", "https://manager.example")
	startRequest.Header.Set("Content-Type", "application/json")
	startRequest.AddCookie(cookie)
	start := httptest.NewRecorder()
	app.Handler().ServeHTTP(start, startRequest)
	requireOpaqueSuccess(t, start, http.StatusOK, "auth.password_change_started", "")
	var startOutcome struct {
		Code string `json:"code"`
		Data struct {
			Value string `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &startOutcome); err != nil || startOutcome.Code != "auth.password_change_started" || startOutcome.Data.Value == "" {
		t.Fatalf("invalid password change outcome: %+v (%v)", startOutcome, err)
	}
	responseBytes, err := base64.RawURLEncoding.DecodeString(startOutcome.Data.Value)
	if err != nil {
		t.Fatal(err)
	}
	registrationResponse, err := client.Deserialize.RegistrationResponse(responseBytes)
	if err != nil {
		t.Fatal(err)
	}
	newRecord, _, err := client.RegistrationFinalize(registrationResponse, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(protocolMessage{Value: base64.RawURLEncoding.EncodeToString(newRecord.Serialize())})
	finishRequest := httptest.NewRequest(http.MethodPost, "/opaque/password/change/finish", bytes.NewReader(body))
	finishRequest.Host = "manager.example"
	finishRequest.RemoteAddr = "127.0.0.1:12345"
	finishRequest.TLS = &tls.ConnectionState{}
	finishRequest.Header.Set("Origin", "https://manager.example")
	finishRequest.Header.Set("Content-Type", "application/json")
	finishRequest.AddCookie(cookie)
	finish := httptest.NewRecorder()
	app.Handler().ServeHTTP(finish, finishRequest)

	var finishOutcome struct {
		Code string `json:"code"`
		Data any    `json:"data"`
	}
	requireOpaqueSuccess(t, finish, http.StatusOK, "auth.password_changed", "null")
	if json.Unmarshal(finish.Body.Bytes(), &finishOutcome) != nil || finishOutcome.Code != "auth.password_changed" || finishOutcome.Data != nil {
		t.Fatalf("unexpected password change outcome: status=%d body=%s", finish.Code, finish.Body.String())
	}
	if got := finish.Result().Cookies(); len(got) != 1 || got[0].Value != "" {
		t.Fatalf("password change did not clear the session cookie: %+v", got)
	}
	sessionRequest := httptest.NewRequest(http.MethodGet, "https://manager.example/api/session", nil)
	sessionRequest.Host = "manager.example"
	sessionRequest.RemoteAddr = "127.0.0.1:12345"
	sessionRequest.TLS = &tls.ConnectionState{}
	sessionRequest.AddCookie(cookie)
	session := httptest.NewRecorder()
	app.Handler().ServeHTTP(session, sessionRequest)
	if session.Code != http.StatusUnauthorized || !bytes.Contains(session.Body.Bytes(), []byte(`"auth.session_required"`)) {
		t.Fatalf("replaced session remained valid: status=%d body=%s", session.Code, session.Body.String())
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
	requireOpaqueSuccess(t, start, http.StatusOK, "auth.login_started", "")
	var response struct {
		Data struct {
			Transaction string `json:"transaction"`
			Value       string `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &response); err != nil || response.Data.Transaction == "" {
		t.Fatal(err)
	}
	ke2Bytes, err := base64.RawURLEncoding.DecodeString(response.Data.Value)
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
	finishBody, err := json.Marshal(protocolMessage{Transaction: &response.Data.Transaction, Value: base64.RawURLEncoding.EncodeToString(ke3.Serialize())})
	if err != nil {
		t.Fatal(err)
	}
	return finishBody
}

func newRegistrationFlow(t *testing.T, app *App, grant, origin, password string) (string, []byte) {
	return newRegistrationFlowWithIdentities(t, app, grant, origin, password, nil, nil)
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
	requireOpaqueSuccess(t, recorder, http.StatusOK, "auth.registration_started", "")
	var response struct {
		Data struct {
			Value       string `json:"value"`
			Transaction string `json:"transaction"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Data.Value == "" || response.Data.Transaction == "" {
		t.Fatalf("registration start returned incomplete challenge: %#v (%v)", response, err)
	}
	responseBytes, err := base64.RawURLEncoding.DecodeString(response.Data.Value)
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
	return response.Data.Transaction, record.Serialize()
}

func finishTestRegistration(app *App, grant, handle string, record []byte, origin string) int {
	return finishTestRegistrationResponse(app, grant, handle, record, origin).Code
}

func finishTestRegistrationResponse(app *App, grant, handle string, record []byte, origin string) *httptest.ResponseRecorder {
	transaction := (*string)(nil)
	if handle != "" {
		transaction = &handle
	}
	body, _ := json.Marshal(protocolMessage{Grant: grant, Transaction: transaction, Value: base64.RawURLEncoding.EncodeToString(record)})
	return registrationRequest(app, http.MethodPost, "/opaque/register/finish", body, origin)
}

func requireOpaqueSuccess(t *testing.T, response *httptest.ResponseRecorder, status int, code, expectedData string) {
	t.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != "application/json" ||
		response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("OPAQUE success contract: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode OPAQUE outcome: %v (%s)", err, response.Body.String())
	}
	var gotCode, message string
	if len(envelope) != 3 || json.Unmarshal(envelope["code"], &gotCode) != nil ||
		json.Unmarshal(envelope["message"], &message) != nil || gotCode != code || message == "" {
		t.Fatalf("invalid OPAQUE outcome envelope: %s", response.Body.String())
	}
	if expectedData != "" {
		if string(envelope["data"]) != expectedData {
			t.Fatalf("OPAQUE outcome data=%s, want %s", envelope["data"], expectedData)
		}
	} else {
		var data map[string]json.RawMessage
		if err := json.Unmarshal(envelope["data"], &data); err != nil || len(data) == 0 {
			t.Fatalf("OPAQUE success data missing: %s", envelope["data"])
		}
	}
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
