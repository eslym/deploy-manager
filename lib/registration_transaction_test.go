package lib

import (
	"bytes"
	"crypto/tls"
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
