package lib

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/bytemare/opaque"
)

const (
	sessionCookie = "__Host-deploy-manager-session"
)

type pendingLogin struct {
	clientMAC []byte
	expires   time.Time
	origin    string
	remoteIP  string
}

func (a *App) loginStart(w http.ResponseWriter, r *http.Request) {
	if !a.https(r) {
		http.Error(w, "HTTPS is required", http.StatusUpgradeRequired)
		return
	}
	m, ok := a.decode(w, r)
	if !ok {
		return
	}
	if m.Transaction != nil || m.Grant != "" {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	input, err := decodeValue(m.Value)
	if err != nil {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	ke1, err := a.des.KE1(input)
	if err != nil {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	var registration []byte
	if err = a.db.QueryRow(`SELECT registration_record FROM administrator WHERE id=1`).Scan(&registration); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "login unavailable", http.StatusUnauthorized)
		} else {
			http.Error(w, "login temporarily unavailable", http.StatusInternalServerError)
		}
		return
	}
	record, err := a.des.RegistrationRecord(registration)
	if err != nil || record == nil {
		http.Error(w, "login temporarily unavailable", http.StatusInternalServerError)
		return
	}
	client := &opaque.ClientRecord{RegistrationRecord: record, CredentialIdentifier: []byte("administrator")}
	ke2, output, err := a.protocol.GenerateKE2(ke1, client)
	if err != nil {
		http.Error(w, "login could not be started", http.StatusBadRequest)
		return
	}
	transactionBytes := make([]byte, 32)
	if _, err = rand.Read(transactionBytes); err != nil {
		http.Error(w, "login temporarily unavailable", http.StatusInternalServerError)
		return
	}
	transaction := base64.RawURLEncoding.EncodeToString(transactionBytes)
	now := a.cfg.Now()
	a.mu.Lock()
	for id, pending := range a.logins {
		if !pending.expires.After(now) {
			delete(a.logins, id)
		}
	}
	if len(a.logins) >= 1024 {
		a.mu.Unlock()
		http.Error(w, "too many login attempts; try again shortly", http.StatusTooManyRequests)
		return
	}
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	a.logins[transaction] = pendingLogin{clientMAC: append([]byte(nil), output.ClientMAC...), expires: now.Add(a.cfg.LoginTTL), origin: r.Header.Get("Origin"), remoteIP: remoteIP}
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"transaction": transaction, "value": base64.RawURLEncoding.EncodeToString(ke2.Serialize())})
}

func (a *App) loginFinish(w http.ResponseWriter, r *http.Request) {
	if !a.https(r) {
		http.Error(w, "HTTPS is required", http.StatusUpgradeRequired)
		return
	}
	m, ok := a.decode(w, r)
	if !ok {
		return
	}
	if m.Transaction == nil || len(*m.Transaction) < 32 || len(*m.Transaction) > 128 || m.Grant != "" {
		http.Error(w, "login could not be verified", http.StatusUnauthorized)
		return
	}
	transaction := *m.Transaction
	a.mu.Lock()
	pending, exists := a.logins[transaction]
	delete(a.logins, transaction)
	a.mu.Unlock()
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !exists || !pending.expires.After(a.cfg.Now()) || pending.origin != r.Header.Get("Origin") || pending.remoteIP != remoteIP {
		http.Error(w, "login could not be verified", http.StatusUnauthorized)
		return
	}
	input, err := decodeValue(m.Value)
	if err != nil {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	ke3, err := a.des.KE3(input)
	if err != nil {
		http.Error(w, "login could not be verified", http.StatusUnauthorized)
		return
	}
	if a.protocol.LoginFinish(ke3, pending.clientMAC) != nil {
		http.Error(w, "login could not be verified", http.StatusUnauthorized)
		return
	}
	// This bearer token is independent random session material and is created only after KE3 verification.
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		http.Error(w, "login temporarily unavailable", http.StatusInternalServerError)
		return
	}
	digest := sha256.Sum256(token)
	expires := a.cfg.Now().Add(a.cfg.SessionTTL)
	a.mu.Lock()
	for existing, expiry := range a.sessions {
		if !expiry.After(a.cfg.Now()) {
			delete(a.sessions, existing)
		}
	}
	if len(a.sessions) >= 10000 {
		a.mu.Unlock()
		http.Error(w, "too many active sessions; try again shortly", http.StatusTooManyRequests)
		return
	}
	a.sessions[digest] = expires
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: base64.RawURLEncoding.EncodeToString(token), Path: "/", Expires: expires, MaxAge: int(a.cfg.SessionTTL.Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) sessionStatus(w http.ResponseWriter, r *http.Request) {
	if !a.https(r) {
		http.Error(w, "HTTPS is required", http.StatusUpgradeRequired)
		return
	}
	if _, ok := a.session(r); !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`{"authenticated":true}`))
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !a.https(r) || !a.originOK(r) || !a.allow(r) {
		http.Error(w, "request rejected", http.StatusForbidden)
		return
	}
	if digest, ok := a.session(r); ok {
		a.mu.Lock()
		delete(a.sessions, digest)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0), Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) session(r *http.Request) ([32]byte, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || len(cookie.Value) > 128 {
		return [32]byte{}, false
	}
	token, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil || len(token) != 32 {
		return [32]byte{}, false
	}
	digest := sha256.Sum256(token)
	a.mu.Lock()
	defer a.mu.Unlock()
	expires, ok := a.sessions[digest]
	if !ok || !expires.After(a.cfg.Now()) {
		delete(a.sessions, digest)
		return [32]byte{}, false
	}
	return digest, true
}
