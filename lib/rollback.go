package lib

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	deploymentmodel "github.com/eslym/deploy-manager/lib/ent/deployment"
	jobmodel "github.com/eslym/deploy-manager/lib/ent/deploymentjob"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

type rollbackCommit struct {
	ID      string    `json:"id"`
	ShortID string    `json:"shortId"`
	Subject string    `json:"subject"`
	Author  string    `json:"author"`
	Date    time.Time `json:"date"`
}

func (a *App) rollbackHistory(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, false) {
		return
	}
	d, err := a.store.Deployment.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeOutcomeError(w, http.StatusNotFound, "request.not_found", "Resource not found.")
		return
	}
	if d.TargetKind != "branch" {
		writeOutcomeError(w, http.StatusConflict, "request.conflict", "State changed. Refresh and try again.")
		return
	}
	page, limit := 1, 20
	if value := r.URL.Query().Get("page"); value != "" {
		page, err = strconv.Atoi(value)
		if err != nil || page < 1 || page > 100000 {
			writeOutcomeError(w, http.StatusBadRequest, "request.invalid", "Check the request and try again.")
			return
		}
	}
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			writeOutcomeError(w, http.StatusBadRequest, "request.invalid", "Check the request and try again.")
			return
		}
	}
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
	if len(search) > 256 {
		writeOutcomeError(w, http.StatusBadRequest, "request.invalid", "Check the request and try again.")
		return
	}
	a.projects.cacheMu.RLock()
	defer a.projects.cacheMu.RUnlock()
	if err := a.projects.ensureCacheReady(); err != nil {
		writeOutcomeError(w, http.StatusServiceUnavailable, "project.cache_recovery_required", "Repository cache recovery is required.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.DeployTimeout)
	defer cancel()
	repo, err := git.PlainOpen(a.projects.repoPath(d.ProjectID))
	if err != nil {
		writeOutcomeError(w, http.StatusConflict, "request.conflict", "State changed. Refresh and try again.")
		return
	}
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", d.TargetName), true)
	if err != nil {
		writeOutcomeError(w, http.StatusConflict, "git.target_unavailable", "Selected Git target is unavailable.")
		return
	}
	out := []rollbackCommit{}
	matched := 0
	id := ref.Hash()
	for id != plumbing.ZeroHash {
		if ctx.Err() != nil {
			writeOutcomeError(w, http.StatusServiceUnavailable, "service.unavailable", "Service is temporarily unavailable.")
			return
		}
		if size, err := repo.Storer.EncodedObjectSize(id); err != nil || size > maxGitCommitObjectBytes {
			writeOutcomeError(w, http.StatusConflict, "request.conflict", "State changed. Refresh and try again.")
			return
		}
		commit, err := repo.CommitObject(id)
		if err != nil {
			writeOutcomeError(w, http.StatusConflict, "request.conflict", "State changed. Refresh and try again.")
			return
		}
		subject := strings.SplitN(commit.Message, "\n", 2)[0]
		if search == "" || strings.Contains(strings.ToLower(id.String()+" "+subject+" "+commit.Author.Name), search) {
			if matched >= (page-1)*limit {
				out = append(out, rollbackCommit{id.String(), id.String()[:12], subject, commit.Author.Name, commit.Author.When})
			}
			matched++
			if len(out) > limit {
				break
			}
		}
		if len(commit.ParentHashes) == 0 {
			break
		}
		id = commit.ParentHashes[0]
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	writeOutcome(w, http.StatusOK, "deployment.history_loaded", "Commit history loaded.", map[string]any{"commits": out, "page": page, "hasMore": hasMore})
}

// Eligibility traverses all parents, independently of first-parent browsing.
// The caller holds cacheMu, so the branch and object graph are one snapshot.
func rollbackAncestor(ctx context.Context, repo *git.Repository, branch, selected string) error {
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if err != nil {
		return errors.New("rollback branch is missing")
	}
	selectedHash := plumbing.NewHash(selected)
	if size, err := repo.Storer.EncodedObjectSize(selectedHash); err != nil || size > maxGitCommitObjectBytes {
		return errors.New("rollback commit unavailable or too large")
	}
	wanted, err := repo.CommitObject(selectedHash)
	if err != nil || wanted.Hash.String() != selected {
		return errors.New("rollback commit unavailable")
	}
	pending := []plumbing.Hash{ref.Hash()}
	seen := map[plumbing.Hash]bool{}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if id == wanted.Hash {
			return nil
		}
		if size, err := repo.Storer.EncodedObjectSize(id); err != nil || size > maxGitCommitObjectBytes {
			return errors.New("rollback ancestry commit unavailable or too large")
		}
		commit, err := repo.CommitObject(id)
		if err != nil {
			return errors.New("rollback ancestry objects unavailable")
		}
		pending = append(pending, commit.ParentHashes...)
	}
	return errors.New("rollback commit is not an ancestor of the configured branch")
}

func (a *App) temporaryRollback(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var in struct {
		Commit    string `json:"commit"`
		Permanent bool   `json:"permanent,omitempty"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || !validCommitTarget(in.Commit) {
		writeOutcomeError(w, http.StatusBadRequest, "request.invalid", "Check the request and try again.")
		return
	}
	slot, ok := a.reserveDeploymentSlot()
	if !ok {
		writeOutcomeError(w, http.StatusServiceUnavailable, "operation.capacity_exceeded", "Work capacity is unavailable. Try again later.")
		return
	}
	defer slot.release()
	id := r.PathValue("id")
	job, err := randomID()
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
	d, err := tx.Deployment.Get(r.Context(), id)
	if err != nil {
		writeOutcomeError(w, http.StatusNotFound, "request.not_found", "Resource not found.")
		return
	}
	if d.TargetKind != "branch" || d.Status == "deleting" || d.Status == "deletion_failed" || d.Status == "recovery_required" {
		writeOutcomeError(w, http.StatusConflict, "request.conflict", "State changed. Refresh and try again.")
		return
	}
	now := a.cfg.Now().UnixNano()
	supersedeMessage := "Superseded by temporary rollback"
	if in.Permanent {
		supersedeMessage = "Superseded by permanent rollback"
	}
	newGeneration := d.Generation + 1
	update := tx.Deployment.Update().Where(deploymentmodel.IDEQ(id), deploymentmodel.GenerationEQ(d.Generation)).AddGeneration(1).SetStatus("queued").SetError("").SetUpdatedAt(now)
	if in.Permanent {
		update.SetAutomationPaused(1)
		if d.AutomationPaused == 0 {
			update.AddAutomationEpoch(1)
		}
	}
	var changed int
	changed, err = update.Save(r.Context())
	if err == nil && changed != 1 {
		writeOutcomeError(w, http.StatusConflict, "request.conflict", "State changed. Refresh and try again.")
		return
	}
	if err == nil {
		jobs, queryErr := tx.DeploymentJob.Query().Where(jobmodel.DeploymentIDEQ(id), jobmodel.StatusIn("queued", "running")).All(r.Context())
		err = queryErr
		for _, old := range jobs {
			if err != nil {
				break
			}
			logText := old.Log + "\n" + supersedeMessage
			if len(logText) > 8192 {
				logText = logText[:8192]
			}
			_, err = tx.DeploymentJob.Update().Where(jobmodel.IDEQ(old.ID), jobmodel.StatusEQ(old.Status)).SetStatus("superseded").SetLog(logText).SetUpdatedAt(now).Save(r.Context())
		}
	}
	if err == nil {
		logText := "Temporary rollback queued"
		if in.Permanent {
			logText = "Permanent rollback queued; automatic deployment paused"
		}
		_, err = tx.DeploymentJob.Create().SetID(job).SetDeploymentID(id).SetCommitID(in.Commit).SetRollbackCommit(in.Commit).SetGeneration(newGeneration).SetStatus("queued").SetLog(logText).SetCreatedAt(now).SetUpdatedAt(now).SetDurable(1).Save(r.Context())
	}
	if err != nil || tx.Commit() != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	a.invalidateJob(id)
	a.cancelDeploymentJobsBefore(id, newGeneration)
	slot.enqueue(job)
	writeOutcome(w, http.StatusAccepted, "deployment.rollback_queued", "Rollback queued.", map[string]string{"jobId": job, "status": "queued"})
}
