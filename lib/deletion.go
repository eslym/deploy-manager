package lib

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eslym/deploy-manager/lib/ent/archiveddeploymentjob"
	deploymentmodel "github.com/eslym/deploy-manager/lib/ent/deployment"
	"github.com/eslym/deploy-manager/lib/ent/deploymentdeletion"
	"github.com/eslym/deploy-manager/lib/ent/deploymentjob"
	"github.com/eslym/deploy-manager/lib/ent/deploymentremovalpath"
	projectmodel "github.com/eslym/deploy-manager/lib/ent/project"
	"github.com/eslym/deploy-manager/lib/ent/projectdeletion"
	"github.com/eslym/deploy-manager/lib/ent/publicationcleanup"
	"github.com/eslym/deploy-manager/lib/ent/publicationintent"
)

func (a *App) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&input) != nil || !input.Confirm {
		http.Error(w, "explicit deletion confirmation is required", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	if !validProjectID(id) {
		http.NotFound(w, r)
		return
	}
	now := a.cfg.Now().UnixNano()
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "deployment deletion is temporarily unavailable", 500)
		return
	}
	var generation int64
	err = tx.QueryRow(`SELECT generation FROM deployments WHERE id=?`, id).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		http.NotFound(w, r)
		return
	}
	if err != nil {
		_ = tx.Rollback()
		http.Error(w, "deployment deletion is temporarily unavailable", 500)
		return
	}
	if _, err = tx.Exec(`INSERT INTO deployment_deletions(deployment_id,status,created_at,updated_at,retry_after) VALUES(?,'deleting',?,?,0) ON CONFLICT(deployment_id) DO UPDATE SET status='deleting',last_error='',updated_at=excluded.updated_at,retry_after=0`, id, now, now); err == nil {
		_, err = tx.Exec(`UPDATE deployments SET generation=generation+1,status='deleting',error='',updated_at=? WHERE id=? AND generation=?`, now, id, generation)
	}
	if err == nil {
		_, err = tx.Exec(`UPDATE deployment_jobs SET status='superseded',log=substr(log || char(10) || 'Deployment deletion requested',1,8192),updated_at=? WHERE deployment_id=? AND status IN ('queued','running')`, now, id)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		http.Error(w, "deployment deletion could not be recorded", 500)
		return
	}
	a.cancelDeploymentJobs(id)
	if err = a.finishDeploymentDeletion(id); err != nil {
		http.Error(w, "deletion is pending; cleanup is blocked and will be retried", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) resumeDeploymentDeletions() error {
	pending, err := a.store.DeploymentDeletion.Query().Exist(context.Background())
	if err != nil {
		return err
	}
	if !pending {
		return nil
	}
	entries, err := a.store.DeploymentDeletion.Query().Where(
		deploymentdeletion.StatusNEQ("deleted"),
		deploymentdeletion.RetryAfterLTE(a.cfg.Now().UnixNano()),
	).Order(deploymentdeletion.ByCreatedAt()).All(context.Background())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = a.finishDeploymentDeletion(entry.ID); err != nil {
			_, _ = a.store.DeploymentDeletion.Update().Where(deploymentdeletion.IDEQ(entry.ID)).SetStatus("blocked").SetLastError("safe cleanup is pending; check publication paths and retry").SetUpdatedAt(a.cfg.Now().UnixNano()).Save(context.Background())
		}
	}
	return nil
}

func (a *App) finishDeploymentDeletion(id string) error {
	unlock, err := a.lockDeployment(id)
	if err != nil {
		return a.recordDeletionFailure(id, err)
	}
	defer unlock()
	if err = a.reconcileDeploymentIntent(id); err != nil {
		return a.recordDeletionFailure(id, err)
	}
	if err = a.recordDeploymentRemovalPaths(id); err != nil {
		return a.recordDeletionFailure(id, err)
	}
	removals, err := a.store.DeploymentRemovalPath.Query().Where(deploymentremovalpath.IDEQ(id)).All(context.Background())
	if err != nil {
		return a.recordDeletionFailure(id, err)
	}
	type removal struct {
		name     string
		identity dirIdentity
	}
	pending := make([]removal, 0, len(removals))
	for _, row := range removals {
		pending = append(pending, removal{name: row.PathName, identity: dirIdentity{Dev: uint64(row.Dev), Ino: uint64(row.Ino)}})
	}
	for _, x := range pending {
		path := filepath.Join(a.cfg.DeploymentRoot, x.name)
		if err = validateRemovalName(id, x.name); err != nil {
			return a.recordDeletionFailure(id, err)
		}
		ok, e := sameIdentity(path, x.identity)
		if e != nil {
			return a.recordDeletionFailure(id, e)
		}
		if ok {
			if err = fault("before-deployment-remove"); err != nil {
				return a.recordDeletionFailure(id, err)
			}
			if err = os.RemoveAll(path); err == nil {
				err = syncDirectory(a.cfg.DeploymentRoot)
			}
			if err != nil {
				_, _ = a.db.Exec(`UPDATE deployment_removal_paths SET last_error='cleanup failed; retry scheduled',attempts=attempts+1 WHERE deployment_id=? AND path_name=?`, id, x.name)
				return a.recordDeletionFailure(id, err)
			}
			if err = fault("after-deployment-removal"); err != nil {
				return a.recordDeletionFailure(id, err)
			}
		} else if _, e = os.Lstat(path); !os.IsNotExist(e) {
			return a.recordDeletionFailure(id, errors.New("removal path identity changed; cleanup blocked"))
		}
		if _, err = a.store.DeploymentRemovalPath.Delete().Where(deploymentremovalpath.IDEQ(id), deploymentremovalpath.PathNameEQ(x.name)).Exec(context.Background()); err != nil {
			return a.recordDeletionFailure(id, err)
		}
		_, _ = a.store.PublicationCleanup.Delete().Where(publicationcleanup.IDEQ(id), publicationcleanup.PathNameEQ(x.name)).Exec(context.Background())
	}
	return a.finishDeploymentDeletionRecord(id)
}

func validateRemovalName(id, name string) error {
	if name == id {
		return nil
	}
	if !validProjectID(id) || len(name) != 1+len(id)+len(".stage-")+32 || name[:1] != "." || name[1:33] != id || name[33:40] != ".stage-" || !validProjectID(name[40:]) {
		return errors.New("removal journal contains an unsafe path")
	}
	return nil
}

func (a *App) recordDeploymentRemovalPaths(id string) error {
	deployment, err := a.store.Deployment.Get(context.Background(), id)
	if err != nil {
		return err
	}
	add := func(name string, want dirIdentity) error {
		if err := validateRemovalName(id, name); err != nil {
			return err
		}
		path := filepath.Join(a.cfg.DeploymentRoot, name)
		actual, e := identity(path)
		if os.IsNotExist(e) {
			_, _ = a.store.PublicationCleanup.Delete().Where(publicationcleanup.IDEQ(id), publicationcleanup.PathNameEQ(name)).Exec(context.Background())
			return nil
		}
		if e != nil {
			return e
		}
		if actual != want {
			return errors.New("publication directory identity changed; cleanup blocked")
		}
		_, e = a.db.Exec(`INSERT OR IGNORE INTO deployment_removal_paths(deployment_id,path_name,dev,ino) VALUES(?,?,?,?)`, id, name, want.Dev, want.Ino)
		return e
	}
	if deployment.LiveCommit != "" {
		if err := add(id, dirIdentity{uint64(deployment.LiveDev), uint64(deployment.LiveIno)}); err != nil {
			return err
		}
	} else if _, err := os.Lstat(a.deploymentPath(id)); err == nil {
		return errors.New("unrecorded live publication; cleanup blocked")
	} else if !os.IsNotExist(err) {
		return err
	}
	cleanups, err := a.store.PublicationCleanup.Query().Where(publicationcleanup.IDEQ(id)).All(context.Background())
	if err != nil {
		return err
	}
	type item struct {
		name string
		id   dirIdentity
	}
	items := make([]item, 0, len(cleanups))
	for _, row := range cleanups {
		items = append(items, item{name: row.PathName, id: dirIdentity{Dev: uint64(row.Dev), Ino: uint64(row.Ino)}})
	}
	for _, x := range items {
		if err = add(x.name, x.id); err != nil {
			return err
		}
	}
	// A worker can fail before it writes a publication intent. Once deletion
	// owns the deployment lock, its remaining stage paths are manager-owned and
	// can be journaled by identity before removal.
	entries, err := os.ReadDir(a.cfg.DeploymentRoot)
	if err != nil {
		return err
	}
	prefix := "." + id + ".stage-"
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		job := strings.TrimPrefix(name, prefix)
		if !validProjectID(job) {
			return errors.New("unrecognized deployment stage path; cleanup blocked")
		}
		known, e := a.store.DeploymentJob.Query().Where(deploymentjob.IDEQ(job), deploymentjob.DeploymentIDEQ(id)).Exist(context.Background())
		if e != nil {
			return e
		}
		if !known {
			return errors.New("stage has no recorded deployment job")
		}
		dirID, e := identity(filepath.Join(a.cfg.DeploymentRoot, name))
		if e != nil {
			return e
		}
		if _, e = a.db.Exec(`INSERT OR IGNORE INTO deployment_removal_paths(deployment_id,path_name,dev,ino) VALUES(?,?,?,?)`, id, name, dirID.Dev, dirID.Ino); e != nil {
			return e
		}
	}
	return nil
}

func (a *App) reconcileDeploymentIntent(id string) error {
	exists, err := a.store.PublicationIntent.Query().Where(publicationintent.IDEQ(id)).Exist(context.Background())
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := a.reconcilePublicationsFor(id); err != nil {
		return err
	}
	exists, err = a.store.PublicationIntent.Query().Where(publicationintent.IDEQ(id)).Exist(context.Background())
	if err != nil {
		return err
	}
	if exists {
		return errors.New("publication intent remains unresolved")
	}
	return nil
}

func (a *App) finishDeploymentDeletionRecord(id string) error {
	ctx := context.Background()
	tx, err := a.store.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	remaining, err := tx.DeploymentRemovalPath.Query().Where(deploymentremovalpath.IDEQ(id)).Count(ctx)
	if err != nil {
		return err
	}
	if remaining != 0 {
		return errors.New("publication cleanup remains pending")
	}
	intent, err := tx.PublicationIntent.Query().Where(publicationintent.IDEQ(id)).Exist(ctx)
	if err != nil {
		return err
	}
	cleanup, err := tx.PublicationCleanup.Query().Where(publicationcleanup.IDEQ(id)).Exist(ctx)
	if err != nil {
		return err
	}
	if intent || cleanup {
		return errors.New("publication cleanup journal remains pending")
	}
	jobs, err := tx.DeploymentJob.Query().Where(deploymentjob.DeploymentIDEQ(id)).All(ctx)
	if err != nil {
		return err
	}
	jobIDs := make([]string, 0, len(jobs))
	for _, job := range jobs {
		jobIDs = append(jobIDs, job.ID)
	}
	if len(jobIDs) != 0 {
		if _, err = tx.ArchivedDeploymentJob.Delete().Where(archiveddeploymentjob.IDIn(jobIDs...)).Exec(ctx); err != nil {
			return err
		}
	}
	for _, job := range jobs {
		if _, err = tx.ArchivedDeploymentJob.Create().SetID(job.ID).SetDeploymentID(job.DeploymentID).SetCommitID(job.CommitID).SetGeneration(job.Generation).SetStatus(job.Status).SetLog(job.Log).SetCreatedAt(job.CreatedAt).SetUpdatedAt(job.UpdatedAt).Save(ctx); err != nil {
			return err
		}
	}
	if _, err = tx.DeploymentJob.Delete().Where(deploymentjob.DeploymentIDEQ(id)).Exec(ctx); err != nil {
		return err
	}
	if err = tx.Deployment.DeleteOneID(id).Exec(ctx); err != nil {
		return err
	}
	if err = tx.DeploymentDeletion.DeleteOneID(id).Exec(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) recordDeletionFailure(id string, cause error) error {
	var attempts int
	_ = a.db.QueryRow(`SELECT attempts FROM deployment_deletions WHERE deployment_id=?`, id).Scan(&attempts)
	delay := retryDelay(attempts)
	next := a.cfg.Now().Add(delay).UnixNano()
	_, _ = a.db.Exec(`UPDATE deployment_deletions SET status='blocked',last_error='safe cleanup is pending; check publication paths and retry',attempts=attempts+1,retry_after=?,updated_at=? WHERE deployment_id=?`, next, a.cfg.Now().UnixNano(), id)
	_, _ = a.db.Exec(`UPDATE deployments SET status='deletion_failed',error='Deletion is blocked; cleanup will retry after the path is safe',updated_at=? WHERE id=? AND status='deleting'`, a.cfg.Now().UnixNano(), id)
	return fmt.Errorf("deployment deletion pending: %w", cause)
}

func retryDelay(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 10 {
		attempts = 10
	}
	return time.Second * time.Duration(1<<attempts)
}

func (a *App) deleteProject(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in) != nil || !in.Confirm {
		http.Error(w, "explicit deletion confirmation is required", 400)
		return
	}
	p := a.projects
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lfsAuthMu.Lock()
	defer p.lfsAuthMu.Unlock()
	id := r.PathValue("id")
	if !validProjectID(id) {
		http.NotFound(w, r)
		return
	}
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "project deletion is temporarily unavailable", 500)
		return
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRow(`SELECT count(*) FROM deployments WHERE project_id=?`, id).Scan(&count); err != nil {
		http.Error(w, "project deletion is temporarily unavailable", 500)
		return
	}
	if count != 0 {
		http.Error(w, "remove this project's deployments before deleting the project", http.StatusConflict)
		return
	}
	var exists int
	if err = tx.QueryRow(`SELECT 1 FROM projects WHERE id=?`, id).Scan(&exists); err != nil {
		http.NotFound(w, r)
		return
	}
	cache := p.repoPath(id)
	cacheID, err := identity(cache)
	if err != nil {
		http.Error(w, "project cache identity could not be verified; deletion is blocked", http.StatusServiceUnavailable)
		return
	}
	p.lfsCacheMu.Lock()
	lfsCachePath := filepath.Join(p.root, "lfs-cache", id)
	var lfsCacheID dirIdentity
	if lfsCacheID, err = identity(lfsCachePath); os.IsNotExist(err) {
		err = nil
	} else if err == nil {
		info, statErr := os.Lstat(lfsCachePath)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			err = errors.New("project LFS cache identity could not be verified")
		}
	}
	if err != nil {
		p.lfsCacheMu.Unlock()
		http.Error(w, "project LFS cache identity could not be verified; deletion is blocked", http.StatusServiceUnavailable)
		return
	}
	now := a.cfg.Now().UnixNano()
	if _, err = tx.Exec(`INSERT INTO project_deletions(project_id,cache_dev,cache_ino,lfs_dev,lfs_ino,status,last_error,created_at,updated_at,retry_after) VALUES(?,?,?,?,?,'deleting','',?,?,0) ON CONFLICT(project_id) DO UPDATE SET cache_dev=excluded.cache_dev,cache_ino=excluded.cache_ino,lfs_dev=excluded.lfs_dev,lfs_ino=excluded.lfs_ino,status='deleting',last_error='',updated_at=excluded.updated_at,retry_after=0`, id, cacheID.Dev, cacheID.Ino, lfsCacheID.Dev, lfsCacheID.Ino, now, now); err != nil {
		p.lfsCacheMu.Unlock()
		http.Error(w, "project deletion could not be recorded", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		p.lfsCacheMu.Unlock()
		http.Error(w, "project deletion could not be recorded", 500)
		return
	}
	p.lfsCacheMu.Unlock()
	if err = a.finishProjectDeletion(id, cacheID, lfsCacheID); err != nil {
		http.Error(w, "project deletion is pending; cache cleanup will retry", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) finishProjectDeletion(id string, want, wantLFS dirIdentity) error {
	p := a.projects
	if actual, err := identity(p.root); err != nil || actual != p.rootIdentity {
		a.setProjectDeletionError(id)
		return errors.New("project cache root identity changed; deletion blocked")
	}
	if err := fault("before-project-lfs-remove"); err != nil {
		a.setProjectDeletionError(id)
		return err
	}
	if err := p.removeProjectLFSPayloads(id, wantLFS); err != nil {
		a.setProjectDeletionError(id)
		return err
	}
	if err := fault("after-project-lfs-remove"); err != nil {
		a.setProjectDeletionError(id)
		return err
	}
	path := p.repoPath(id)
	actual, err := identity(path)
	if err == nil && actual != want {
		a.setProjectDeletionError(id)
		return errors.New("project cache identity changed; deletion blocked")
	}
	if err == nil {
		if err = fault("before-project-cache-remove"); err != nil {
			a.setProjectDeletionError(id)
			return err
		}
		if err = p.cacheFS.RemoveAll(path); err == nil {
			err = syncDirectory(p.root)
		}
		if err != nil {
			a.setProjectDeletionError(id)
			return err
		}
		if err = fault("after-project-cache-remove"); err != nil {
			a.setProjectDeletionError(id)
			return err
		}
	} else if !os.IsNotExist(err) {
		a.setProjectDeletionError(id)
		return err
	}
	tx, err := a.store.Tx(context.Background())
	if err != nil {
		a.setProjectDeletionError(id)
		return err
	}
	defer tx.Rollback()
	n, err := tx.Deployment.Query().Where(deploymentmodel.ProjectID(id)).Count(context.Background())
	if err != nil {
		_ = tx.Rollback()
		a.setProjectDeletionError(id)
		return err
	}
	if n != 0 {
		_ = tx.Rollback()
		a.setProjectDeletionError(id)
		return errors.New("project has deployments")
	}
	if err = tx.Project.DeleteOneID(id).Exec(context.Background()); err != nil {
		_ = tx.Rollback()
		a.setProjectDeletionError(id)
		return err
	}
	if err = tx.ProjectDeletion.DeleteOneID(id).Exec(context.Background()); err != nil {
		_ = tx.Rollback()
		a.setProjectDeletionError(id)
		return err
	}
	if err = tx.Commit(); err != nil {
		a.setProjectDeletionError(id)
		return err
	}
	return nil
}
func (a *App) setProjectDeletionError(id string) {
	deletion, err := a.store.ProjectDeletion.Get(context.Background(), id)
	if err != nil {
		return
	}
	now := a.cfg.Now()
	_, _ = a.store.ProjectDeletion.Update().Where(projectdeletion.IDEQ(id)).SetStatus("blocked").SetLastError("cache cleanup is pending; retry deletion").AddAttempts(1).SetRetryAfter(now.Add(retryDelay(deletion.Attempts)).UnixNano()).SetUpdatedAt(now.UnixNano()).Save(context.Background())
	_, _ = a.store.Project.Update().Where(projectmodel.IDEQ(id)).SetWarning("Deletion is blocked; cache cleanup will retry").Save(context.Background())
}

func (a *App) retryProjectDeletions() {
	entries, err := a.store.ProjectDeletion.Query().Where(projectdeletion.RetryAfterLTE(a.cfg.Now().UnixNano())).All(context.Background())
	if err != nil || len(entries) == 0 {
		return
	}
	type x struct {
		id       string
		identity dirIdentity
		lfs      dirIdentity
	}
	xs := make([]x, 0, len(entries))
	for _, entry := range entries {
		xs = append(xs, x{id: entry.ID, identity: dirIdentity{Dev: uint64(entry.CacheDev), Ino: uint64(entry.CacheIno)}, lfs: dirIdentity{Dev: uint64(entry.LfsDev), Ino: uint64(entry.LfsIno)}})
	}
	for _, v := range xs {
		a.projects.mu.Lock()
		a.projects.lfsAuthMu.Lock()
		a.projects.cacheMu.Lock()
		_ = a.finishProjectDeletion(v.id, v.identity, v.lfs)
		a.projects.cacheMu.Unlock()
		a.projects.lfsAuthMu.Unlock()
		a.projects.mu.Unlock()
	}
}
