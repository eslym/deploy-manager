package lib

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/eslym/deploy-manager/lib/ent"
	"github.com/eslym/deploy-manager/lib/ent/administrator"
	"github.com/eslym/deploy-manager/lib/ent/registrationgrant"
)

// refreshCredentialGenerationLocked synchronizes sessions and pending logins
// with durable credential replacements made by another App process.
func (a *App) refreshCredentialGenerationLocked() error {
	adminRecord, err := a.store.Administrator.Get(context.Background(), 1)
	var generation int64
	if err != nil {
		if !ent.IsNotFound(err) {
			return err
		}
	} else {
		generation = adminRecord.CredentialGeneration
	}
	if generation != a.credentialGeneration {
		a.credentialGeneration = generation
		a.sessions = make(map[[32]byte]sessionState)
		a.logins = make(map[string]pendingLogin)
		a.registrationTxs = make(map[string]registrationTransaction)
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
		if _, ok := a.session(r); !ok {
			writeOutcomeError(w, http.StatusUnauthorized, "auth.session_required", "Sign in to continue.")
			return
		}
		writeOutcomeError(w, http.StatusUnauthorized, "auth.recent_login_required", "Sign in again before changing your password.")
		return
	}
	if m.Grant != "" || m.Transaction != nil {
		writeOutcomeError(w, http.StatusBadRequest, "auth.protocol_invalid", "Invalid authentication request.")
		return
	}
	b, err := decodeValue(m.Value)
	if err != nil {
		writeOutcomeError(w, http.StatusBadRequest, "auth.protocol_invalid", "Invalid authentication request.")
		return
	}
	request, err := a.des.RegistrationRequest(b)
	if err != nil {
		writeOutcomeError(w, http.StatusBadRequest, "auth.protocol_invalid", "Invalid authentication request.")
		return
	}
	response, err := a.protocol.RegistrationResponse(request, []byte("administrator"), nil)
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	writeOutcome(w, http.StatusOK, "auth.password_change_started", "Password change started.", map[string]string{"value": base64.RawURLEncoding.EncodeToString(response.Serialize())})
}

func (a *App) passwordChangeFinish(w http.ResponseWriter, r *http.Request) {
	m, ok := a.decode(w, r)
	if !ok {
		return
	}
	if m.Grant != "" || m.Transaction != nil {
		writeOutcomeError(w, http.StatusBadRequest, "auth.protocol_invalid", "Invalid authentication request.")
		return
	}
	b, err := decodeValue(m.Value)
	if err != nil {
		writeOutcomeError(w, http.StatusBadRequest, "auth.protocol_invalid", "Invalid authentication request.")
		return
	}
	if rec, err := a.des.RegistrationRecord(b); err != nil || rec == nil {
		writeOutcomeError(w, http.StatusBadRequest, "auth.protocol_invalid", "Invalid authentication request.")
		return
	}
	digest, ok := a.session(r)
	if !ok {
		writeOutcomeError(w, http.StatusUnauthorized, "auth.session_required", "Sign in to continue.")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, exists := a.sessions[digest]
	if !exists || s.generation != a.credentialGeneration || a.cfg.Now().Sub(s.authenticatedAt) > a.cfg.RecentAuthTTL {
		writeOutcomeError(w, http.StatusUnauthorized, "auth.recent_login_required", "Sign in again before changing your password.")
		return
	}
	a.replaceCredential(w, nil, b, "change", digest, "", "")
}

// replaceCredential changes only the administrator's OPAQUE record and its
// credential generation. The profile and external OPAQUE key generation stay fixed.
// The caller holds a.mu so login completion cannot issue a stale session.
func (a *App) replaceCredential(w http.ResponseWriter, grantHash, record []byte, purpose string, sessionDigest [32]byte, transaction, transactionOrigin string) {
	if err := a.refreshCredentialGenerationLocked(); err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	old := a.credentialGeneration
	ctx := context.Background()
	tx, err := a.store.Tx(ctx)
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	defer tx.Rollback()
	now := a.cfg.Now().UnixNano()
	if purpose == "recovery" {
		pending, ok := a.pendingRegistrationLocked(transaction, transactionOrigin, "recovery")
		if !ok || !bytes.Equal(pending.grantHash[:], grantHash) {
			writeOutcomeError(w, http.StatusGone, "auth.registration_expired", "Registration expired or unavailable. Start again.")
			return
		}
		res, e := tx.RegistrationGrant.Delete().Where(registrationgrant.IDEQ(grantHash), registrationgrant.PurposeEQ("recovery"), registrationgrant.CredentialGenerationEQ(old), registrationgrant.ExpiresAtGTE(now)).Exec(ctx)
		if e != nil {
			writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
			return
		}
		if res != 1 {
			writeOutcomeError(w, http.StatusGone, "auth.registration_expired", "Registration expired or unavailable. Start again.")
			return
		}
	} else {
		s, ok := a.sessions[sessionDigest]
		if !ok || s.generation != old || a.cfg.Now().Sub(s.authenticatedAt) > a.cfg.RecentAuthTTL {
			writeOutcomeError(w, http.StatusUnauthorized, "auth.recent_login_required", "Sign in again before changing your password.")
			return
		}
	}
	res, err := tx.Administrator.Update().Where(administrator.IDEQ(1), administrator.CredentialGenerationEQ(old), administrator.ProfileGenerationEQ(1)).SetRegistrationRecord(record).AddCredentialGeneration(1).Save(ctx)
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	if res != 1 {
		writeOutcomeError(w, http.StatusConflict, "request.conflict", "State changed. Refresh and try again.")
		return
	}
	if _, err = tx.RegistrationGrant.Delete().Exec(ctx); err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	if err = tx.Commit(); err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	a.credentialGeneration = old + 1
	a.sessions = make(map[[32]byte]sessionState)
	a.logins = make(map[string]pendingLogin)
	a.registrationTxs = make(map[string]registrationTransaction)
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0), Secure: a.cookieName() == sessionCookie, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	if purpose == "recovery" {
		writeOutcome(w, http.StatusOK, "auth.registration_completed", "Registration completed. Sign in to continue.", nil)
		return
	}
	writeOutcome(w, http.StatusOK, "auth.password_changed", "Password changed. Sign in again.", nil)
}
