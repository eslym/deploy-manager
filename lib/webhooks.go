package lib

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

const githubWebhookProvider = "github"
const giteaWebhookProvider = "gitea"

var webhookDeliveryID = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

func (a *App) initWebhooks() error {
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS project_webhooks(project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,secret BLOB NOT NULL,created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS project_gitea_webhooks(project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,secret BLOB NOT NULL,created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS webhook_deliveries(project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,provider TEXT NOT NULL,delivery_id TEXT NOT NULL,event TEXT NOT NULL,outcome TEXT NOT NULL,created_at INTEGER NOT NULL,PRIMARY KEY(project_id,provider,delivery_id))`,
	} {
		if _, err := a.db.Exec(query); err != nil {
			return err
		}
	}
	return nil
}

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
	return gcm.Open(nil, nonce, sealed[gcm.NonceSize():], []byte(provider+"-webhook:"+projectID))
}

func (a *App) githubWebhookSettings(w http.ResponseWriter, r *http.Request) {
	a.providerWebhookSettings(w, r, githubWebhookProvider)
}

func (a *App) giteaWebhookSettings(w http.ResponseWriter, r *http.Request) {
	a.providerWebhookSettings(w, r, giteaWebhookProvider)
}

// Keep existing GitHub encrypted records intact; each provider has its own
// per-project credential. Table names are application constants, never input.
func webhookSettingsTable(provider string) string {
	if provider == giteaWebhookProvider {
		return "project_gitea_webhooks"
	}
	return "project_webhooks"
}

func (a *App) providerWebhookSettings(w http.ResponseWriter, r *http.Request, provider string) {
	id := r.PathValue("id")
	table := webhookSettingsTable(provider)
	if r.Method == http.MethodGet {
		if !a.projectAuthorized(w, r, false) {
			return
		}
		var configured int
		if err := a.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE project_id=?`, id).Scan(&configured); err != nil {
			http.Error(w, "webhook settings are unavailable", http.StatusInternalServerError)
			return
		}
		if configured == 0 {
			var exists int
			if err := a.db.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, id).Scan(&exists); err != nil || exists == 0 {
				http.NotFound(w, r)
				return
			}
		}
		writeJSON(w, map[string]any{"configured": configured != 0, "url": "/webhooks/" + provider + "/" + id})
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
		http.Error(w, "request a secret rotation", http.StatusBadRequest)
		return
	}
	if err := r.Body.Close(); err != nil {
		http.Error(w, "request could not be read", http.StatusBadRequest)
		return
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		http.Error(w, "webhook secret could not be created", http.StatusInternalServerError)
		return
	}
	// Webhook providers sign with the literal value copied into their Secret
	// field. Encrypt that printable key, not its decoded random source bytes.
	printableSecret := hex.EncodeToString(secret)
	sealed, err := a.sealProviderWebhookSecret(provider, id, []byte(printableSecret))
	if err != nil {
		http.Error(w, "webhook secret could not be protected", http.StatusInternalServerError)
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "webhook secret could not be saved", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, id).Scan(&exists); err != nil || exists == 0 {
		http.NotFound(w, r)
		return
	}
	if _, err = tx.Exec(`INSERT INTO `+table+`(project_id,secret,created_at) VALUES(?,?,?) ON CONFLICT(project_id) DO UPDATE SET secret=excluded.secret,created_at=excluded.created_at`, id, sealed, a.cfg.Now().UnixNano()); err != nil {
		http.Error(w, "webhook secret could not be saved", http.StatusInternalServerError)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "webhook secret could not be saved", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONStatus(w, http.StatusCreated, map[string]string{"url": "/webhooks/" + provider + "/" + id, "secret": printableSecret})
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
		http.Error(w, "request rejected", http.StatusForbidden)
		return
	}
	projectID := r.PathValue("id")
	if !validProjectID(projectID) {
		http.NotFound(w, r)
		return
	}
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	signature := r.Header.Get("X-Hub-Signature-256")
	if provider == giteaWebhookProvider {
		deliveryID = r.Header.Get("X-Gitea-Delivery")
		event = r.Header.Get("X-Gitea-Event")
		signature = r.Header.Get("X-Gitea-Signature")
	}
	if !webhookDeliveryID.MatchString(deliveryID) || len(event) == 0 || len(event) > 64 || strings.ContainsAny(event, "\r\n\x00") {
		http.Error(w, "invalid webhook headers", http.StatusBadRequest)
		return
	}
	if provider == githubWebhookProvider {
		if !strings.HasPrefix(signature, "sha256=") {
			http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
			return
		}
		signature = strings.TrimPrefix(signature, "sha256=")
	}
	if len(signature) != 64 {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return
	}
	expectedSignature, err := hex.DecodeString(signature)
	if err != nil || len(expectedSignature) != sha256.Size {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return
	}
	var sealed []byte
	err = a.db.QueryRow(`SELECT secret FROM `+webhookSettingsTable(provider)+` WHERE project_id=?`, projectID).Scan(&sealed)
	if err != nil {
		http.Error(w, "webhook is not configured", http.StatusNotFound)
		return
	}
	secret, err := a.openProviderWebhookSecret(provider, projectID, sealed)
	if err != nil {
		http.Error(w, "webhook is unavailable", http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.WebhookMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	clear(secret)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "webhook body is too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "webhook body could not be read", http.StatusBadRequest)
		}
		return
	}
	secret, err = a.openProviderWebhookSecret(provider, projectID, sealed)
	if err != nil {
		http.Error(w, "webhook is unavailable", http.StatusInternalServerError)
		return
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	clear(secret)
	if !hmac.Equal(mac.Sum(nil), expectedSignature) {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return
	}
	var push githubPush
	branch := ""
	deleted := false
	if event == "push" {
		if err := json.Unmarshal(body, &push); err != nil || len(push.Ref) > 1024 || plumbing.ReferenceName(push.Ref).Validate() != nil || !validCommitTarget(push.After) || (!plumbing.ReferenceName(push.Ref).IsBranch() && !(provider == giteaWebhookProvider && plumbing.ReferenceName(push.Ref).IsTag())) {
			http.Error(w, "invalid push event", http.StatusBadRequest)
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
			http.Error(w, "webhook work queue is full", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "webhook could not be durably accepted", http.StatusServiceUnavailable)
		}
		return
	}
	if a.webhookFault != nil {
		if err := a.webhookFault("before-ack"); err != nil {
			http.Error(w, "delivery acknowledgement was interrupted", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

var errWebhookQueueFull = errors.New("webhook queue full")

func (a *App) webhookSecret(projectID string) ([]byte, error) {
	var sealed []byte
	if err := a.db.QueryRow(`SELECT secret FROM project_webhooks WHERE project_id=?`, projectID).Scan(&sealed); err != nil {
		return nil, err
	}
	return sealed, nil
}

// A delivery acknowledged before a process crash may have been executing when
// the process stopped. Keep that job's interruption visible, then persist one
// fresh latest-tip request for the still-configured branch on the next start.
func (a *App) requeueInterruptedWebhookJobs() error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT j.id,d.id,d.commit_id,d.generation,j.automation_epoch FROM deployment_jobs j JOIN deployments d ON d.id=j.deployment_id WHERE j.status='failed' AND j.durable=1 AND j.automatic=1 AND j.retry_enqueued=0 AND j.log LIKE '%interrupted by service restart%' AND d.target_kind='branch' AND d.status NOT IN ('deleting','deletion_failed') ORDER BY j.created_at`)
	if err != nil {
		return err
	}
	type interrupted struct {
		job, deployment, commit string
		generation              int64
		epoch                   int64
	}
	var items []interrupted
	for rows.Next() {
		var item interrupted
		if err = rows.Scan(&item.job, &item.deployment, &item.commit, &item.generation, &item.epoch); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, item := range items {
		jobID, idErr := randomID()
		if idErr != nil {
			return idErr
		}
		now := a.cfg.Now().UnixNano()
		if _, err = tx.Exec(`UPDATE deployment_jobs SET retry_enqueued=1 WHERE id=? AND retry_enqueued=0`, item.job); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at,durable,automatic,automation_epoch) VALUES(?,?,?,?,'queued','Recovered accepted branch update after restart',?,?,1,1,?)`, jobID, item.deployment, item.commit, item.generation, now, now, item.epoch); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE deployments SET status=CASE WHEN automation_paused=0 THEN 'queued' ELSE status END,error=CASE WHEN automation_paused=0 THEN '' ELSE error END,updated_at=? WHERE id=? AND generation=? AND status NOT IN ('deleting','deletion_failed')`, now, item.deployment, item.generation); err != nil {
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
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO webhook_deliveries(project_id,provider,delivery_id,event,outcome,created_at) VALUES(?,?,?,?,?,?)`, projectID, provider, deliveryID, event, "ignored", a.cfg.Now().UnixNano()); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil
		}
		return err
	}
	outcome := "ignored"
	jobIDs := make([]string, 0)
	if event == "push" && branch != "" && !deleted {
		rows, queryErr := tx.Query(`SELECT id,generation,commit_id,automation_epoch FROM deployments WHERE project_id=? AND target_kind='branch' AND target_name=? AND status NOT IN ('deleting','deletion_failed') ORDER BY id`, projectID, branch)
		if queryErr != nil {
			return queryErr
		}
		type target struct {
			id, commit string
			generation int64
			epoch      int64
		}
		var targets []target
		for rows.Next() {
			var x target
			if err = rows.Scan(&x.id, &x.generation, &x.commit, &x.epoch); err != nil {
				rows.Close()
				return err
			}
			targets = append(targets, x)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, target := range targets {
			var pending string
			err = tx.QueryRow(`SELECT id FROM deployment_jobs WHERE deployment_id=? AND status='queued' AND automatic=1 AND generation=? AND automation_epoch=? ORDER BY created_at DESC LIMIT 1`, target.id, target.generation, target.epoch).Scan(&pending)
			if err == nil {
				if _, err = tx.Exec(`UPDATE deployment_jobs SET log='Latest branch update queued',updated_at=? WHERE id=? AND status='queued'`, a.cfg.Now().UnixNano(), pending); err != nil {
					return err
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
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
			if _, err = tx.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at,durable,automatic,automation_epoch) VALUES(?,?,?,?,'queued',?,?,?,1,1,?)`, jobID, target.id, target.commit, target.generation, "Verified "+provider+" branch update", a.cfg.Now().UnixNano(), a.cfg.Now().UnixNano(), target.epoch); err != nil {
				return err
			}
			if _, err = tx.Exec(`UPDATE deployments SET status=CASE WHEN automation_paused=0 THEN 'queued' ELSE status END,error=CASE WHEN automation_paused=0 THEN '' ELSE error END,updated_at=? WHERE id=? AND generation=? AND status NOT IN ('deleting','deletion_failed')`, a.cfg.Now().UnixNano(), target.id, target.generation); err != nil {
				return err
			}
			jobIDs = append(jobIDs, jobID)
		}
		if len(targets) > 0 {
			outcome = "queued"
		}
	}
	if _, err = tx.Exec(`UPDATE webhook_deliveries SET outcome=? WHERE project_id=? AND provider=? AND delivery_id=?`, outcome, projectID, provider, deliveryID); err != nil {
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
		var accepted int
		if verifyErr := a.db.QueryRow(`SELECT count(*) FROM webhook_deliveries WHERE project_id=? AND provider=? AND delivery_id=?`, projectID, provider, deliveryID).Scan(&accepted); verifyErr != nil || accepted != 1 {
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
	keepSlots = true
	if postCommitErr != nil {
		return postCommitErr
	}
	return nil
}
