package lib

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	var project, kind, branch string
	if err := a.db.QueryRow(`SELECT project_id,target_kind,target_name FROM deployments WHERE id=?`, r.PathValue("id")).Scan(&project, &kind, &branch); err != nil {
		http.NotFound(w, r)
		return
	}
	if kind != "branch" {
		http.Error(w, "rollback requires a branch target", 409)
		return
	}
	page, limit := 1, 20
	var err error
	if value := r.URL.Query().Get("page"); value != "" {
		page, err = strconv.Atoi(value)
		if err != nil || page < 1 || page > 100000 {
			http.Error(w, "invalid history page", 400)
			return
		}
	}
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			http.Error(w, "invalid history page size", 400)
			return
		}
	}
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
	if len(search) > 256 {
		http.Error(w, "history search is too long", 400)
		return
	}
	a.projects.cacheMu.RLock()
	defer a.projects.cacheMu.RUnlock()
	if err := a.projects.ensureCacheReady(); err != nil {
		http.Error(w, "repository recovery is required", 409)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.DeployTimeout)
	defer cancel()
	repo, err := git.PlainOpen(a.projects.repoPath(project))
	if err != nil {
		http.Error(w, "repository unavailable", 409)
		return
	}
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if err != nil {
		http.Error(w, "configured branch is missing; refresh the repository", 409)
		return
	}
	out := []rollbackCommit{}
	matched := 0
	id := ref.Hash()
	for id != plumbing.ZeroHash {
		if ctx.Err() != nil {
			http.Error(w, "history lookup timed out; narrow your search", http.StatusServiceUnavailable)
			return
		}
		if size, err := repo.Storer.EncodedObjectSize(id); err != nil || size > maxGitCommitObjectBytes {
			http.Error(w, "history commit is unavailable or too large", 409)
			return
		}
		commit, err := repo.CommitObject(id)
		if err != nil {
			http.Error(w, "history objects unavailable", 409)
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
	writeJSON(w, map[string]any{"commits": out, "page": page, "hasMore": hasMore})
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
		Commit string `json:"commit"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || !validCommitTarget(in.Commit) {
		http.Error(w, "choose a full 40-character commit ID", 400)
		return
	}
	select {
	case a.deploySlots <- struct{}{}:
	default:
		http.Error(w, "publication queue is full", 503)
		return
	}
	reserved := true
	defer func() {
		if reserved {
			<-a.deploySlots
		}
	}()
	id := r.PathValue("id")
	job, err := randomID()
	if err != nil {
		http.Error(w, "rollback could not be queued", 500)
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "rollback could not be queued", 500)
		return
	}
	defer tx.Rollback()
	var kind, status string
	var generation int64
	if err = tx.QueryRow(`SELECT target_kind,status,generation FROM deployments WHERE id=?`, id).Scan(&kind, &status, &generation); err != nil {
		http.NotFound(w, r)
		return
	}
	if kind != "branch" || status == "deleting" || status == "deletion_failed" || status == "recovery_required" {
		http.Error(w, "rollback requires an available branch deployment", 409)
		return
	}
	now := a.cfg.Now().UnixNano()
	if _, err = tx.Exec(`UPDATE deployments SET generation=generation+1,status='queued',error='',updated_at=? WHERE id=?`, now, id); err == nil {
		_, err = tx.Exec(`UPDATE deployment_jobs SET status='superseded',log=substr(log || char(10) || 'Superseded by temporary rollback',1,8192),updated_at=? WHERE deployment_id=? AND status IN ('queued','running')`, now, id)
	}
	if err == nil {
		_, err = tx.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,rollback_commit,generation,status,log,created_at,updated_at,durable) VALUES(?,?,?,?,?,'queued','Temporary rollback queued',?,?,1)`, job, id, in.Commit, in.Commit, generation+1, now, now)
	}
	if err != nil || tx.Commit() != nil {
		http.Error(w, "rollback could not be queued", 500)
		return
	}
	a.cancelDeploymentJobsBefore(id, generation+1)
	a.deployQueue <- job
	reserved = false
	writeJSONStatus(w, 202, map[string]string{"jobId": job, "status": "queued"})
}
