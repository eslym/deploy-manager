package lib

import (
	"database/sql"
	"errors"
	"net/http"
	"time"
)

// refreshCredentialGenerationLocked synchronizes sessions and pending logins
// with durable credential replacements made by another App process.
func (a *App) refreshCredentialGenerationLocked() error {
	var generation int64
	err := a.db.QueryRow(`SELECT credential_generation FROM administrator WHERE id=1`).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		generation, err = 0, nil
	}
	if err != nil {
		return err
	}
	if generation != a.credentialGeneration {
		a.credentialGeneration = generation
		a.sessions = make(map[[32]byte]sessionState)
		a.logins = make(map[string]pendingLogin)
	}
	return nil
}

func (a *App) recentSession(r *http.Request) bool {
	digest, ok := a.session(r)
	if !ok {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refreshCredentialGenerationLocked() != nil {
		return false
	}
	s, ok := a.sessions[digest]
	return ok && s.generation == a.credentialGeneration && a.cfg.Now().Sub(s.authenticatedAt) <= a.cfg.RecentAuthTTL
}

func (a *App) passwordChangeStart(w http.ResponseWriter, r *http.Request) {
	m, ok := a.decode(w, r)
	if !ok {
		return
	}
	if !a.recentSession(r) {
		http.Error(w, "recent authentication required", http.StatusUnauthorized)
		return
	}
	if m.Grant != "" || m.Transaction != nil {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	b, err := decodeValue(m.Value)
	if err != nil {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	request, err := a.des.RegistrationRequest(b)
	if err != nil {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	response, err := a.protocol.RegistrationResponse(request, []byte("administrator"), nil)
	if err != nil {
		http.Error(w, "password change unavailable", http.StatusInternalServerError)
		return
	}
	jsonValue(w, response.Serialize())
}

func (a *App) passwordChangeFinish(w http.ResponseWriter, r *http.Request) {
	m, ok := a.decode(w, r)
	if !ok {
		return
	}
	if m.Grant != "" || m.Transaction != nil {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	b, err := decodeValue(m.Value)
	if err != nil {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	if rec, err := a.des.RegistrationRecord(b); err != nil || rec == nil {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	digest, ok := a.session(r)
	if !ok {
		http.Error(w, "recent authentication required", http.StatusUnauthorized)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, exists := a.sessions[digest]
	if !exists || s.generation != a.credentialGeneration || a.cfg.Now().Sub(s.authenticatedAt) > a.cfg.RecentAuthTTL {
		http.Error(w, "recent authentication required", http.StatusUnauthorized)
		return
	}
	a.replaceCredential(w, nil, b, "change", digest)
}

// replaceCredential changes only the administrator's OPAQUE record and its
// credential generation. The profile and external OPAQUE key generation stay fixed.
// The caller holds a.mu so login completion cannot issue a stale session.
func (a *App) replaceCredential(w http.ResponseWriter, grantHash, record []byte, purpose string, sessionDigest [32]byte) {
	if err := a.refreshCredentialGenerationLocked(); err != nil {
		http.Error(w, "credential replacement unavailable", http.StatusInternalServerError)
		return
	}
	old := a.credentialGeneration
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "credential replacement unavailable", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	now := a.cfg.Now().UnixNano()
	if purpose == "recovery" {
		res, e := tx.Exec(`DELETE FROM registration_grants WHERE token_hash=? AND purpose='recovery' AND credential_generation=? AND expires_at>=?`, grantHash, old, now)
		if e != nil {
			http.Error(w, "credential replacement unavailable", http.StatusInternalServerError)
			return
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			http.Error(w, "grant expired, superseded, or already used", http.StatusGone)
			return
		}
	} else {
		s, ok := a.sessions[sessionDigest]
		if !ok || s.generation != old || a.cfg.Now().Sub(s.authenticatedAt) > a.cfg.RecentAuthTTL {
			http.Error(w, "recent authentication required", http.StatusUnauthorized)
			return
		}
	}
	res, err := tx.Exec(`UPDATE administrator SET registration_record=?,credential_generation=credential_generation+1 WHERE id=1 AND credential_generation=? AND profile_generation=1`, record, old)
	if err != nil {
		http.Error(w, "credential replacement unavailable", http.StatusInternalServerError)
		return
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		http.Error(w, "credential replacement conflicted", http.StatusConflict)
		return
	}
	if _, err = tx.Exec(`DELETE FROM registration_grants`); err != nil {
		http.Error(w, "credential replacement unavailable", http.StatusInternalServerError)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "credential replacement unavailable", http.StatusInternalServerError)
		return
	}
	a.credentialGeneration = old + 1
	a.sessions = make(map[[32]byte]sessionState)
	a.logins = make(map[string]pendingLogin)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0), Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
