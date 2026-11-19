package lib

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bytemare/opaque"
	"github.com/eslym/deploy-manager/lib/ent"
)

const (
	sessionCookie         = "__Host-deploy-manager-session"
	insecureSessionCookie = "deploy-manager-session"
)

func (a *App) cookieName() string {
	if a.cfg.AllowHTTPOrigin && strings.HasPrefix(strings.ToLower(a.cfg.CanonicalOrigin), "http://") {
		return insecureSessionCookie
	}
	return sessionCookie
}

type pendingLogin struct {
	clientMAC  []byte
	expires    time.Time
	origin     string
	remoteIP   string
	generation int64
}

type sessionState struct {
	expires         time.Time
	generation      int64
	authenticatedAt time.Time
}

func (a *App) loginStart(w http.ResponseWriter, r *http.Request) {
	if !a.authSecure(r) {
		writeOutcomeError(w, http.StatusUpgradeRequired, "request.https_required", "HTTPS is required.")
		return
	}
	m, ok := a.decode(w, r)
	if !ok {
		return
	}
	if m.Transaction != nil || m.Grant != "" {
		writeOutcomeError(w, http.StatusBadRequest, "auth.protocol_invalid", "Invalid authentication request.")
		return
	}
	input, err := decodeValue(m.Value)
	if err != nil {
		writeOutcomeError(w, http.StatusBadRequest, "auth.protocol_invalid", "Invalid authentication request.")
		return
	}
	ke1, err := a.des.KE1(input)
	if err != nil {
		writeOutcomeError(w, http.StatusBadRequest, "auth.protocol_invalid", "Invalid authentication request.")
		return
	}
	adminRecord, err := a.store.Administrator.Get(r.Context(), 1)
	if err != nil {
		if ent.IsNotFound(err) {
			writeOutcomeError(w, http.StatusUnauthorized, "auth.login_failed", "Sign-in failed.")
		} else {
			writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		}
		return
	}
	record, err := a.des.RegistrationRecord(adminRecord.RegistrationRecord)
	if err != nil || record == nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	client := &opaque.ClientRecord{RegistrationRecord: record, CredentialIdentifier: []byte("administrator")}
	ke2, output, err := a.protocol.GenerateKE2(ke1, client)
	if err != nil {
		writeOutcomeError(w, http.StatusBadRequest, "auth.protocol_invalid", "Invalid authentication request.")
		return
	}
	transactionBytes := make([]byte, 32)
	if _, err = rand.Read(transactionBytes); err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
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
		writeOutcomeError(w, http.StatusTooManyRequests, "auth.capacity_exceeded", "Sign-in is temporarily unavailable.")
		return
	}
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	a.logins[transaction] = pendingLogin{clientMAC: append([]byte(nil), output.ClientMAC...), expires: now.Add(a.cfg.LoginTTL), origin: r.Header.Get("Origin"), remoteIP: remoteIP, generation: adminRecord.CredentialGeneration}
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	writeOutcome(w, http.StatusOK, "auth.login_started", "Sign-in started.", map[string]string{"transaction": transaction, "value": base64.RawURLEncoding.EncodeToString(ke2.Serialize())})
}

func (a *App) loginFinish(w http.ResponseWriter, r *http.Request) {
	if !a.authSecure(r) {
		writeOutcomeError(w, http.StatusUpgradeRequired, "request.https_required", "HTTPS is required.")
		return
	}
	m, ok := a.decode(w, r)
	if !ok {
		return
	}
	if m.Transaction == nil || len(*m.Transaction) < 32 || len(*m.Transaction) > 128 || m.Grant != "" {
		writeOutcomeError(w, http.StatusUnauthorized, "auth.login_failed", "Sign-in failed.")
		return
	}
	transaction := *m.Transaction
	a.mu.Lock()
	pending, exists := a.logins[transaction]
	delete(a.logins, transaction)
	a.mu.Unlock()
	remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !exists || !pending.expires.After(a.cfg.Now()) || pending.origin != r.Header.Get("Origin") || pending.remoteIP != remoteIP {
		writeOutcomeError(w, http.StatusUnauthorized, "auth.login_failed", "Sign-in failed.")
		return
	}
	input, err := decodeValue(m.Value)
	if err != nil {
		writeOutcomeError(w, http.StatusBadRequest, "auth.protocol_invalid", "Invalid authentication request.")
		return
	}
	ke3, err := a.des.KE3(input)
	if err != nil {
		writeOutcomeError(w, http.StatusUnauthorized, "auth.login_failed", "Sign-in failed.")
		return
	}
	if a.protocol.LoginFinish(ke3, pending.clientMAC) != nil {
		writeOutcomeError(w, http.StatusUnauthorized, "auth.login_failed", "Sign-in failed.")
		return
	}
	// This bearer token is independent random session material and is created only after KE3 verification.
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	digest := sha256.Sum256(token)
	expires := a.cfg.Now().Add(a.cfg.SessionTTL)
	a.mu.Lock()
	if err := a.refreshCredentialGenerationLocked(); err != nil {
		a.mu.Unlock()
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	for existing, expiry := range a.sessions {
		if !expiry.expires.After(a.cfg.Now()) {
			delete(a.sessions, existing)
		}
	}
	if pending.generation != a.credentialGeneration {
		a.mu.Unlock()
		writeOutcomeError(w, http.StatusUnauthorized, "auth.login_failed", "Sign-in failed.")
		return
	}
	if len(a.sessions) >= 10000 {
		a.mu.Unlock()
		writeOutcomeError(w, http.StatusTooManyRequests, "auth.capacity_exceeded", "Sign-in is temporarily unavailable.")
		return
	}
	a.sessions[digest] = sessionState{expires: expires, generation: pending.generation, authenticatedAt: a.cfg.Now()}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: base64.RawURLEncoding.EncodeToString(token), Path: "/", Expires: expires, MaxAge: int(a.cfg.SessionTTL.Seconds()), Secure: a.cookieName() == sessionCookie, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeOutcome(w, http.StatusOK, "auth.login_completed", "Signed in.", nil)
}

func (a *App) sessionStatus(w http.ResponseWriter, r *http.Request) {
	if !a.authSecure(r) {
		writeOutcomeError(w, http.StatusUpgradeRequired, "request.https_required", "HTTPS is required.")
		return
	}
	if _, ok := a.session(r); !ok {
		writeOutcomeError(w, http.StatusUnauthorized, "auth.session_required", "Sign in to continue.")
		return
	}
	writeOutcome(w, http.StatusOK, "auth.session_valid", "Signed in.", map[string]bool{"authenticated": true})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOutcomeError(w, http.StatusMethodNotAllowed, "request.method_not_allowed", "Method not allowed.")
		return
	}
	if !a.authSecure(r) {
		writeOutcomeError(w, http.StatusUpgradeRequired, "request.https_required", "HTTPS is required.")
		return
	}
	if !a.originOK(r) {
		writeOutcomeError(w, http.StatusForbidden, "request.origin_rejected", "Request origin rejected.")
		return
	}
	if !a.allow(r) {
		writeOutcomeError(w, http.StatusTooManyRequests, "request.rate_limited", "Too many requests. Try again shortly.")
		return
	}
	if digest, ok := a.session(r); ok {
		a.mu.Lock()
		delete(a.sessions, digest)
		a.mu.Unlock()
		if a.events != nil {
			a.events.revoke(digest)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0), Secure: a.cookieName() == sessionCookie, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeOutcome(w, http.StatusOK, "auth.logged_out", "Signed out.", nil)
}

func (a *App) session(r *http.Request) ([32]byte, bool) {
	cookie, err := r.Cookie(a.cookieName())
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
	if a.refreshCredentialGenerationLocked() != nil {
		return [32]byte{}, false
	}
	state, ok := a.sessions[digest]
	if !ok || !state.expires.After(a.cfg.Now()) || state.generation != a.credentialGeneration {
		delete(a.sessions, digest)
		return [32]byte{}, false
	}
	return digest, true
}
