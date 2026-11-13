package lib

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type projectCacheFS interface {
	MkdirTemp(string, string) (string, error)
	Mkdir(string, os.FileMode) error
	Rename(string, string) error
	RemoveAll(string) error
	Lstat(string) (os.FileInfo, error)
	ReadDir(string) ([]os.DirEntry, error)
}

type osProjectCacheFS struct{}

func (osProjectCacheFS) MkdirTemp(dir, pattern string) (string, error) {
	return os.MkdirTemp(dir, pattern)
}
func (osProjectCacheFS) Mkdir(path string, mode os.FileMode) error  { return os.Mkdir(path, mode) }
func (osProjectCacheFS) Rename(old, new string) error               { return os.Rename(old, new) }
func (osProjectCacheFS) RemoveAll(path string) error                { return os.RemoveAll(path) }
func (osProjectCacheFS) Lstat(path string) (os.FileInfo, error)     { return os.Lstat(path) }
func (osProjectCacheFS) ReadDir(path string) ([]os.DirEntry, error) { return os.ReadDir(path) }

func (p *projectManager) refreshStagePath(id string) string {
	return filepath.Join(p.root, ".git-stage-refresh-"+id)
}

func (p *projectManager) refreshBackupPath(id string) string {
	return filepath.Join(p.root, ".git-backup-refresh-"+id)
}

func validProjectID(id string) bool {
	if len(id) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16
}

func (p *projectManager) ensureCacheReady() error {
	if p.blocked != "" {
		return errors.New(p.blocked)
	}
	return nil
}

func (p *projectManager) checkPersistentCacheBudget() error {
	if _, err := directorySize(p.root, p.maxCacheBytes); err != nil {
		return errors.New("existing project caches exceed the configured GitMaxCacheBytes; increase the limit or explicitly recreate caches")
	}
	return nil
}

func (p *projectManager) cleanupStage(path string) error {
	if path == "" {
		return nil
	}
	if err := p.cacheFS.RemoveAll(path); err != nil {
		if _, statErr := p.cacheFS.Lstat(path); !os.IsNotExist(statErr) {
			p.blocked = "project cache staging cleanup failed; restart to reconcile cache files before retrying"
			return errors.New(p.blocked)
		}
	}
	if _, err := p.cacheFS.Lstat(path); !os.IsNotExist(err) {
		p.blocked = "project cache staging cleanup could not be confirmed; restart to reconcile cache files before retrying"
		return errors.New(p.blocked)
	}
	return nil
}

func (p *projectManager) markRefreshIntent(id string) error {
	_, err := p.db.Exec(`INSERT INTO project_refresh_journal(project_id) VALUES(?)`, id)
	return err
}

func (p *projectManager) clearRefreshIntent(id string) error {
	_, err := p.db.Exec(`DELETE FROM project_refresh_journal WHERE project_id=?`, id)
	return err
}

func (p *projectManager) refreshIntentExists(id string) (bool, error) {
	var exists int
	err := p.db.QueryRow(`SELECT 1 FROM project_refresh_journal WHERE project_id=?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// rollbackRefresh restores the previous cache before clearing its durable
// journal. On any failure the journal and backup remain for startup recovery.
func (p *projectManager) rollbackRefresh(id string, liveIsUncommitted bool) error {
	live, backup := p.repoPath(id), p.refreshBackupPath(id)
	backupInfo, backupErr := p.cacheFS.Lstat(backup)
	if backupErr != nil && !os.IsNotExist(backupErr) {
		return backupErr
	}
	if backupErr == nil {
		if !backupInfo.IsDir() {
			return errors.New("previous project cache has an unexpected type")
		}
		if _, liveErr := p.cacheFS.Lstat(live); liveErr == nil {
			if err := p.removeOwnedPath(live); err != nil {
				return fmt.Errorf("remove uncommitted project cache: %w", err)
			}
		} else if !os.IsNotExist(liveErr) {
			return liveErr
		}
		if err := p.cacheFS.Rename(backup, live); err != nil {
			return fmt.Errorf("restore previous project cache: %w", err)
		}
	} else if _, liveErr := p.cacheFS.Lstat(live); liveErr != nil {
		return errors.New("neither previous nor live project cache is available for rollback")
	} else if liveIsUncommitted {
		return errors.New("previous project cache backup is missing; cannot prove live cache is the previous version")
	}
	if err := p.clearRefreshIntent(id); err != nil {
		return fmt.Errorf("clear refresh recovery journal: %w", err)
	}
	return nil
}

// reconcileCacheFiles runs before the project manager accepts work. A journal
// exists until the cache and ref metadata commit together; its presence means
// recovery must restore the previous cache. Unjournaled backup directories are
// post-commit leftovers and can be safely deleted.
func (p *projectManager) reconcileCacheFiles() error {
	rows, err := p.db.Query(`SELECT project_id FROM project_refresh_journal ORDER BY project_id`)
	if err != nil {
		return err
	}
	var pending []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range pending {
		if !validProjectID(id) {
			return errors.New("cache recovery journal contains an invalid project identifier")
		}
		live, backup, stage := p.repoPath(id), p.refreshBackupPath(id), p.refreshStagePath(id)
		backupInfo, backupErr := p.cacheFS.Lstat(backup)
		if backupErr != nil && !os.IsNotExist(backupErr) {
			return fmt.Errorf("inspect previous project cache during recovery: %w", backupErr)
		}
		if backupErr == nil {
			if !backupInfo.IsDir() {
				return errors.New("previous project cache has an unexpected type during recovery")
			}
			if info, liveErr := p.cacheFS.Lstat(live); liveErr == nil {
				if !info.IsDir() {
					return errors.New("live project cache has an unexpected type during recovery")
				}
				if err := p.cacheFS.RemoveAll(live); err != nil {
					return fmt.Errorf("remove uncommitted project cache during recovery: %w", err)
				}
			} else if !os.IsNotExist(liveErr) {
				return fmt.Errorf("inspect live project cache during recovery: %w", liveErr)
			}
			if err := p.cacheFS.Rename(backup, live); err != nil {
				return fmt.Errorf("restore previous project cache during recovery: %w", err)
			}
		} else if _, liveErr := p.cacheFS.Lstat(live); os.IsNotExist(liveErr) {
			return errors.New("project cache recovery has neither a live nor previous cache")
		} else if liveErr != nil {
			return fmt.Errorf("inspect live project cache during recovery: %w", liveErr)
		}
		if err := p.removeOwnedPath(stage); err != nil {
			return fmt.Errorf("remove staged project cache during recovery: %w", err)
		}
		if err := p.clearRefreshIntent(id); err != nil {
			return fmt.Errorf("clear recovered project cache journal: %w", err)
		}
	}
	entries, err := p.cacheFS.ReadDir(p.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".git-stage-") || strings.HasPrefix(name, ".git-backup-refresh-") {
			if strings.HasSuffix(name, ".previous") {
				return errors.New("legacy project cache backup has no recovery journal; refusing ambiguous cache cleanup")
			}
			if err := p.removeOwnedPath(filepath.Join(p.root, name)); err != nil {
				return fmt.Errorf("remove orphan project cache staging path: %w", err)
			}
		}
	}
	return p.checkPersistentCacheBudget()
}

func (p *projectManager) removeOwnedPath(path string) error {
	if err := p.cacheFS.RemoveAll(path); err != nil {
		if _, statErr := p.cacheFS.Lstat(path); !os.IsNotExist(statErr) {
			return err
		}
	}
	if _, err := p.cacheFS.Lstat(path); !os.IsNotExist(err) {
		return errors.New("cache path still exists after cleanup")
	}
	return nil
}

// stagedResponseWriter delays the small JSON/error response until cleanup has
// completed, so a failed cleanup is visible instead of being hidden behind a
// successful or stale response.
type stagedResponseWriter struct {
	dst    http.ResponseWriter
	header http.Header
	status int
	body   []byte
}

func newStagedResponseWriter(dst http.ResponseWriter) *stagedResponseWriter {
	return &stagedResponseWriter{dst: dst, header: dst.Header().Clone()}
}

func (w *stagedResponseWriter) Header() http.Header { return w.header }
func (w *stagedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *stagedResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.body = append(w.body, body...)
	return len(body), nil
}

func (w *stagedResponseWriter) replace(status int, message string) {
	w.status = status
	w.body = []byte(message + "\n")
	w.header.Set("Content-Type", "text/plain; charset=utf-8")
	w.header.Set("X-Content-Type-Options", "nosniff")
}

func (w *stagedResponseWriter) Flush() {
	for key, values := range w.header {
		w.dst.Header()[key] = append([]string(nil), values...)
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.dst.WriteHeader(status)
	_, _ = w.dst.Write(w.body)
}
