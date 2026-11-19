package lib

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	entdb "github.com/eslym/deploy-manager/lib/ent"
	depmodel "github.com/eslym/deploy-manager/lib/ent/deployment"
	jobmodel "github.com/eslym/deploy-manager/lib/ent/deploymentjob"
	projectmodel "github.com/eslym/deploy-manager/lib/ent/project"
	"github.com/eslym/deploy-manager/lib/ent/projectgiteawebhook"
	"github.com/eslym/deploy-manager/lib/ent/projectwebhook"
	"github.com/eslym/deploy-manager/lib/ent/webhookdelivery"
	"github.com/go-git/go-git/v5/plumbing"
	sqlite3 "github.com/mattn/go-sqlite3"
)

const githubWebhookProvider = "github"
const giteaWebhookProvider = "gitea"

var webhookDeliveryID = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

func (a *App) webhookCipher() (cipher.AEAD, error) {
	block, err := aes.NewCipher(a.repositoryMaster)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (a *App) sealWebhookSecret(projectID string, secret []byte) ([]byte, error) {
	return a.sealProviderWebhookSecret(githubWebhookProvider, projectID, secret)
}

func (a *App) sealProviderWebhookSecret(provider, projectID string, secret []byte) ([]byte, error) {
	gcm, err := a.webhookCipher()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, secret, []byte(provider+"-webhook:"+projectID)), nil
}

func (a *App) openWebhookSecret(projectID string, sealed []byte) ([]byte, error) {
	return a.openProviderWebhookSecret(githubWebhookProvider, projectID, sealed)
}

func (a *App) openProviderWebhookSecret(provider, projectID string, sealed []byte) ([]byte, error) {
	gcm, err := a.webhookCipher()
	if err != nil || len(sealed) < gcm.NonceSize() {
		return nil, errors.New("webhook secret unavailable")
	}
	nonce := sealed[:gcm.NonceSize()]
	secret, err := gcm.Open(nil, nonce, sealed[gcm.NonceSize():], []byte(provider+"-webhook:"+projectID))
	if err == nil && provider == githubWebhookProvider && len(secret) == 32 {
		// Original GitHub records encrypted the random source, but setup showed
		// its hex encoding. Normalize that unambiguous legacy format to the
		// literal key the administrator entered in GitHub. Do not also accept
		// signatures made with the undisplayed raw bytes. New records already
		// encrypt the 64-character key; provider-bound AAD remains unchanged.
		printable := make([]byte, hex.EncodedLen(len(secret)))
		hex.Encode(printable, secret)
		clear(secret)
		secret = printable
	}
	return secret, err
}

func (a *App) githubWebhookSettings(w http.ResponseWriter, r *http.Request) {
	a.providerWebhookSettings(w, r, githubWebhookProvider)
}

func (a *App) giteaWebhookSettings(w http.ResponseWriter, r *http.Request) {
	a.providerWebhookSettings(w, r, giteaWebhookProvider)
}

func (a *App) providerWebhookSettings(w http.ResponseWriter, r *http.Request, provider string) {
	id := r.PathValue("id")
	if r.Method == http.MethodGet {
		if !a.projectAuthorized(w, r, false) {
			return
		}
		configured, err := a.webhookSettingsExist(r.Context(), provider, id)
		if err != nil {
			writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
			return
		}
		if !configured {
			exists, err := a.store.Project.Query().Where(projectmodel.ID(id)).Exist(r.Context())
			if err != nil || !exists {
				writeOutcomeError(w, http.StatusNotFound, "request.not_found", "Resource not found.")
				return
			}
		}
		writeOutcome(w, http.StatusOK, "webhook.settings_loaded", "Webhook settings loaded.", map[string]any{"configured": configured, "url": "/webhooks/" + provider + "/" + id})
		return
	}
	if !a.projectAuthorized(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var input struct {
		Rotate bool `json:"rotate"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || !input.Rotate {
		writeOutcomeError(w, http.StatusBadRequest, "request.invalid", "Request a webhook secret rotation.")
		return
	}
	if err := r.Body.Close(); err != nil {
		writeOutcomeError(w, http.StatusBadRequest, "request.invalid_json", "Invalid JSON request.")
		return
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	// Webhook providers sign with the literal value copied into their Secret
	// field. Encrypt that printable key, not its decoded random source bytes.
	printableSecret := hex.EncodeToString(secret)
	sealed, err := a.sealProviderWebhookSecret(provider, id, []byte(printableSecret))
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	tx, err := a.store.Tx(r.Context())
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	defer tx.Rollback()
	exists, err := tx.Project.Query().Where(projectmodel.ID(id)).Exist(r.Context())
	if err != nil || !exists {
		writeOutcomeError(w, http.StatusNotFound, "request.not_found", "Resource not found.")
		return
	}
	if err = saveProviderWebhookSecret(r.Context(), tx, provider, id, sealed, a.cfg.Now().UnixNano()); err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	if err = tx.Commit(); err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeOutcome(w, http.StatusCreated, "webhook.secret_rotated", "Webhook secret rotated.", map[string]string{"url": "/webhooks/" + provider + "/" + id, "secret": printableSecret})
	clear(secret)
}

type githubPush struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
}

func (a *App) githubWebhook(w http.ResponseWriter, r *http.Request) {
	a.providerWebhook(w, r, githubWebhookProvider)
}

func (a *App) giteaWebhook(w http.ResponseWriter, r *http.Request) {
	a.providerWebhook(w, r, giteaWebhookProvider)
}

func (a *App) providerWebhook(w http.ResponseWriter, r *http.Request, provider string) {
	if r.Method != http.MethodPost || !a.https(r) {
		writeOutcomeError(w, http.StatusForbidden, "request.https_required", "HTTPS is required.")
		return
	}
	projectID := r.PathValue("id")
	if !validProjectID(projectID) {
		writeOutcomeError(w, http.StatusNotFound, "request.not_found", "Resource not found.")
		return
	}
	deliveryID, event, signature := providerWebhookHeaders(r, provider)
	if !webhookDeliveryID.MatchString(deliveryID) || len(event) == 0 || len(event) > 64 || strings.ContainsAny(event, "\r\n\x00") {
		writeOutcomeError(w, http.StatusBadRequest, "request.invalid", "Webhook headers are invalid.")
		return
	}
	if provider == githubWebhookProvider {
		if !strings.HasPrefix(signature, "sha256=") {
			writeOutcomeError(w, http.StatusUnauthorized, "webhook.signature_invalid", "Webhook authentication failed.")
			return
		}
		signature = strings.TrimPrefix(signature, "sha256=")
	}
	if len(signature) != 64 {
		writeOutcomeError(w, http.StatusUnauthorized, "webhook.signature_invalid", "Webhook authentication failed.")
		return
	}
	expectedSignature, err := hex.DecodeString(signature)
	if err != nil || len(expectedSignature) != sha256.Size {
		writeOutcomeError(w, http.StatusUnauthorized, "webhook.signature_invalid", "Webhook authentication failed.")
		return
	}
	sealed, err := loadProviderWebhookSecret(r.Context(), a.store, provider, projectID)
	if err != nil {
		writeOutcomeError(w, http.StatusNotFound, "webhook.not_configured", "Webhook is not configured.")
		return
	}
	secret, err := a.openProviderWebhookSecret(provider, projectID, sealed)
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.WebhookMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	clear(secret)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeOutcomeError(w, http.StatusRequestEntityTooLarge, "request.too_large", "Request exceeds the allowed limit.")
		} else {
			writeOutcomeError(w, http.StatusBadRequest, "request.invalid", "Webhook request could not be read.")
		}
		return
	}
	secret, err = a.openProviderWebhookSecret(provider, projectID, sealed)
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	clear(secret)
	if !hmac.Equal(mac.Sum(nil), expectedSignature) {
		writeOutcomeError(w, http.StatusUnauthorized, "webhook.signature_invalid", "Webhook authentication failed.")
		return
	}
	var push githubPush
	branch := ""
	deleted := false
	if event == "push" {
		if err := json.Unmarshal(body, &push); err != nil || len(push.Ref) > 1024 || plumbing.ReferenceName(push.Ref).Validate() != nil || !validCommitTarget(push.After) || (!plumbing.ReferenceName(push.Ref).IsBranch() && !(provider == giteaWebhookProvider && plumbing.ReferenceName(push.Ref).IsTag())) {
			writeOutcomeError(w, http.StatusBadRequest, "request.invalid", "Webhook event is invalid.")
			return
		}
		if plumbing.ReferenceName(push.Ref).IsBranch() {
			branch = strings.TrimPrefix(push.Ref, "refs/heads/")
		}
		deleted = push.Deleted || push.After == strings.Repeat("0", 40)
	}
	clear(body)
	if err := a.recordWebhookDelivery(provider, projectID, deliveryID, event, branch, deleted); err != nil {
		if errors.Is(err, errWebhookQueueFull) {
			writeOutcomeError(w, http.StatusServiceUnavailable, "operation.capacity_exceeded", "Work capacity is unavailable. Try again later.")
		} else {
			writeOutcomeError(w, http.StatusServiceUnavailable, "service.unavailable", "Service is temporarily unavailable.")
		}
		return
	}
	if a.webhookFault != nil {
		if err := a.webhookFault("before-ack"); err != nil {
			writeOutcomeError(w, http.StatusServiceUnavailable, "service.unavailable", "Service is temporarily unavailable.")
			return
		}
	}
	writeOutcome(w, http.StatusOK, "webhook.accepted", "Webhook accepted.", nil)
}

var errWebhookQueueFull = errors.New("webhook queue full")

func saveProviderWebhookSecret(ctx context.Context, tx *entdb.Tx, provider, projectID string, sealed []byte, createdAt int64) error {
	if provider == giteaWebhookProvider {
		return upsertGiteaWebhookSecret(ctx, tx, projectID, sealed, createdAt)
	}
	return upsertGitHubWebhookSecret(ctx, tx, projectID, sealed, createdAt)
}

func upsertGitHubWebhookSecret(ctx context.Context, tx *entdb.Tx, projectID string, sealed []byte, createdAt int64) error {
	exists, err := tx.ProjectWebhook.Query().Where(projectwebhook.ID(projectID)).Exist(ctx)
	if err != nil {
		return err
	}
	if exists {
		_, err := tx.ProjectWebhook.UpdateOneID(projectID).SetSecret(sealed).SetCreatedAt(createdAt).Save(ctx)
		return err
	}
	_, err = tx.ProjectWebhook.Create().SetID(projectID).SetSecret(sealed).SetCreatedAt(createdAt).Save(ctx)
	return err
}

func providerWebhookHeaders(r *http.Request, provider string) (deliveryID, event, signature string) {
	if provider == giteaWebhookProvider {
		return r.Header.Get("X-Gitea-Delivery"), r.Header.Get("X-Gitea-Event"), r.Header.Get("X-Gitea-Signature")
	}
	return r.Header.Get("X-GitHub-Delivery"), r.Header.Get("X-GitHub-Event"), r.Header.Get("X-Hub-Signature-256")
}

func loadProviderWebhookSecret(ctx context.Context, store *entdb.Client, provider, projectID string) ([]byte, error) {
	if provider == giteaWebhookProvider {
		row, err := store.ProjectGiteaWebhook.Get(ctx, projectID)
		if err != nil {
			return nil, err
		}
		return row.Secret, nil
	}
	row, err := store.ProjectWebhook.Get(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return row.Secret, nil
}

func upsertGiteaWebhookSecret(ctx context.Context, tx *entdb.Tx, projectID string, sealed []byte, createdAt int64) error {
	exists, err := tx.ProjectGiteaWebhook.Query().Where(projectgiteawebhook.ID(projectID)).Exist(ctx)
	if err != nil {
		return err
	}
	if exists {
		_, err = tx.ProjectGiteaWebhook.UpdateOneID(projectID).SetSecret(sealed).SetCreatedAt(createdAt).Save(ctx)
		return err
	}
	_, err = tx.ProjectGiteaWebhook.Create().SetID(projectID).SetSecret(sealed).SetCreatedAt(createdAt).Save(ctx)
	return err
}

func (a *App) webhookSettingsExist(ctx context.Context, provider, projectID string) (bool, error) {
	if provider == giteaWebhookProvider {
		return a.store.ProjectGiteaWebhook.Query().Where(projectgiteawebhook.ID(projectID)).Exist(ctx)
	}
	return a.store.ProjectWebhook.Query().Where(projectwebhook.ID(projectID)).Exist(ctx)
}

func (a *App) webhookSecret(projectID string) ([]byte, error) {
	row, err := a.store.ProjectWebhook.Get(context.Background(), projectID)
	if err != nil {
		return nil, err
	}
	return row.Secret, nil
}

// A delivery acknowledged before a process crash may have been executing when
// the process stopped. Keep that job's interruption visible, then persist one
// fresh latest-tip request for the still-configured branch on the next start.
func (a *App) requeueInterruptedWebhookJobs() error {
	// Rollback jobs are durable too, but interruption must never convert an
	// explicit commit selection into an unsolicited branch-tip catch-up.
	ctx := context.Background()
	tx, err := a.store.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.DeploymentJob.Query().Where(jobmodel.Status("failed"), jobmodel.Durable(1), jobmodel.Automatic(1), jobmodel.RollbackCommit(""), jobmodel.RetryEnqueued(0), jobmodel.LogContains("interrupted by service restart")).WithDeployment().Order(jobmodel.ByCreatedAt()).All(ctx)
	if err != nil {
		return err
	}
	type interrupted struct {
		job, deployment, commit string
		generation              int64
		epoch                   int64
	}
	var items []interrupted
	for _, row := range rows {
		deployment := row.Edges.Deployment
		if deployment == nil || deployment.TargetKind != "branch" || deployment.Status == "deleting" || deployment.Status == "deletion_failed" {
			continue
		}
		items = append(items, interrupted{job: row.ID, deployment: deployment.ID, commit: deployment.CommitID, generation: row.Generation, epoch: row.AutomationEpoch})
	}
	for _, item := range items {
		jobID, idErr := randomID()
		if idErr != nil {
			return idErr
		}
		now := a.cfg.Now().UnixNano()
		if _, err = tx.DeploymentJob.UpdateOneID(item.job).Where(jobmodel.RetryEnqueued(0)).SetRetryEnqueued(1).Save(ctx); err != nil {
			return err
		}
		if _, err = tx.DeploymentJob.Create().SetID(jobID).SetDeploymentID(item.deployment).SetCommitID(item.commit).SetGeneration(item.generation).SetStatus("queued").SetLog("Recovered accepted branch update after restart").SetCreatedAt(now).SetUpdatedAt(now).SetDurable(1).SetAutomatic(1).SetAutomationEpoch(item.epoch).Save(ctx); err != nil {
			return err
		}
		deployment, getErr := tx.Deployment.Get(ctx, item.deployment)
		if getErr != nil {
			return getErr
		}
		update := tx.Deployment.Update().Where(depmodel.ID(item.deployment), depmodel.Generation(item.generation), depmodel.StatusNotIn("deleting", "deletion_failed")).SetUpdatedAt(now)
		if deployment.AutomationPaused == 0 {
			update.SetStatus("queued").SetError("")
		}
		if _, err = update.Save(ctx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *App) recordWebhookDelivery(provider, projectID, deliveryID, event, branch string, deleted bool) error {
	reservedSlots := 0
	keepSlots := false
	defer func() {
		if !keepSlots {
			for reservedSlots > 0 {
				<-a.deploySlots
				reservedSlots--
			}
		}
	}()
	tx, err := a.store.Tx(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.WebhookDelivery.Create().SetID(projectID).SetProvider(provider).SetDeliveryID(deliveryID).SetEvent(event).SetOutcome("ignored").SetCreatedAt(a.cfg.Now().UnixNano()).Save(context.Background()); err != nil {
		var sqliteErr sqlite3.Error
		// The physical primary key is exactly (project, provider, delivery ID);
		// only that key conflict proves the receipt was already accepted. Other
		// constraint errors (notably FK/trigger failures) must not be acked.
		if errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintPrimaryKey {
			return nil
		}
		return err
	}
	outcome := "ignored"
	jobIDs := make([]string, 0)
	invalidationIDs := make([]string, 0)
	if event == "push" && branch != "" && !deleted {
		targets, queryErr := tx.Deployment.Query().Where(depmodel.ProjectID(projectID), depmodel.TargetKind("branch"), depmodel.TargetName(branch), depmodel.StatusNotIn("deleting", "deletion_failed")).All(context.Background())
		if queryErr != nil {
			return queryErr
		}
		sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
		for _, target := range targets {
			// A later webhook is separate tip work, not a coalesced rollback.
			pendingJobs, queryErr := tx.DeploymentJob.Query().Where(jobmodel.DeploymentID(target.ID), jobmodel.Status("queued"), jobmodel.Automatic(1), jobmodel.RollbackCommit(""), jobmodel.Generation(target.Generation), jobmodel.AutomationEpoch(target.AutomationEpoch)).Order(jobmodel.ByCreatedAt()).All(context.Background())
			if queryErr != nil {
				return queryErr
			}
			if len(pendingJobs) != 0 {
				pending := pendingJobs[len(pendingJobs)-1]
				if _, err = tx.DeploymentJob.UpdateOneID(pending.ID).Where(jobmodel.Status("queued")).SetLog("Latest branch update queued").SetUpdatedAt(a.cfg.Now().UnixNano()).Save(context.Background()); err != nil {
					return err
				}
				invalidationIDs = append(invalidationIDs, target.ID)
				continue
			}
			select {
			case a.deploySlots <- struct{}{}:
				reservedSlots++
			default:
				return errWebhookQueueFull
			}
			jobID, idErr := randomID()
			if idErr != nil {
				return idErr
			}
			if _, err = tx.DeploymentJob.Create().SetID(jobID).SetDeploymentID(target.ID).SetCommitID(target.CommitID).SetGeneration(target.Generation).SetStatus("queued").SetLog("Verified " + provider + " branch update").SetCreatedAt(a.cfg.Now().UnixNano()).SetUpdatedAt(a.cfg.Now().UnixNano()).SetDurable(1).SetAutomatic(1).SetAutomationEpoch(target.AutomationEpoch).Save(context.Background()); err != nil {
				return err
			}
			update := tx.Deployment.Update().Where(depmodel.ID(target.ID), depmodel.Generation(target.Generation), depmodel.StatusNotIn("deleting", "deletion_failed"))
			if target.AutomationPaused == 0 {
				update.SetStatus("queued").SetError("")
			}
			if _, err = update.SetUpdatedAt(a.cfg.Now().UnixNano()).Save(context.Background()); err != nil {
				return err
			}
			jobIDs = append(jobIDs, jobID)
			invalidationIDs = append(invalidationIDs, target.ID)
		}
		if len(targets) > 0 {
			outcome = "queued"
		}
	}
	if _, err = tx.WebhookDelivery.Update().Where(webhookdelivery.ID(projectID), webhookdelivery.Provider(provider), webhookdelivery.DeliveryID(deliveryID)).SetOutcome(outcome).Save(context.Background()); err != nil {
		return err
	}
	if a.webhookFault != nil {
		if err = a.webhookFault("before-commit"); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		// SQLite may report an uncertain commit outcome. Only treat it as accepted
		// after checking the durable receipt; otherwise GitHub must retry.
		_, verifyErr := a.store.WebhookDelivery.Query().Where(webhookdelivery.ID(projectID), webhookdelivery.Provider(provider), webhookdelivery.DeliveryID(deliveryID)).Only(context.Background())
		if verifyErr != nil {
			return fmt.Errorf("commit outcome unavailable: %w", err)
		}
	}
	var postCommitErr error
	if a.webhookFault != nil {
		postCommitErr = a.webhookFault("after-commit")
	}
	for _, id := range jobIDs {
		a.deployQueue <- id
	}
	for _, id := range invalidationIDs {
		a.invalidateJob(id)
	}
	keepSlots = true
	if postCommitErr != nil {
		return postCommitErr
	}
	return nil
}
