package lib

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/eslym/deploy-manager/lib/ent/projectrefreshjournal"
)

type refreshLifecycleError struct {
	status  int
	message string
}

func (e *refreshLifecycleError) Error() string { return e.message }

// promoteRefresh owns the durable portion of a manual refresh: journal
// creation, cache promotion, metadata reconciliation, rollback, and cleanup.
func (p *projectManager) promoteRefresh(ctx context.Context, id, stage string, refs []projectRef, fetchedAt time.Time) (string, error) {
	backup := p.refreshBackupPath(id)
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	if _, err := p.cacheFS.Lstat(backup); err == nil {
		message := "an unresolved previous project cache exists; restart to reconcile before refreshing"
		p.setBlocked(message)
		return "", &refreshLifecycleError{http.StatusServiceUnavailable, message}
	} else if !os.IsNotExist(err) {
		return "", &refreshLifecycleError{http.StatusInternalServerError, "could not inspect previous project cache"}
	}
	if err := p.markRefreshIntent(id); err != nil {
		return "", &refreshLifecycleError{http.StatusInternalServerError, "could not record refresh recovery state"}
	}
	if err := p.cacheFS.Rename(p.repoPath(id), backup); err != nil {
		if rollbackErr := p.rollbackRefresh(id, false); rollbackErr != nil {
			message := "project cache state is ambiguous after promotion failure; project operations are blocked until restart recovery succeeds"
			p.setBlocked(message)
			return "", &refreshLifecycleError{http.StatusServiceUnavailable, message}
		}
		return "", &refreshLifecycleError{http.StatusInternalServerError, "refreshed repository could not be safely promoted; previous cache retained"}
	}
	if err := p.cacheFS.Rename(stage, p.repoPath(id)); err != nil {
		if rollbackErr := p.rollbackRefresh(id, true); rollbackErr != nil {
			message := "project cache restoration failed; project operations are blocked until restart recovery succeeds"
			p.setBlocked(message)
			return "", &refreshLifecycleError{http.StatusServiceUnavailable, message}
		}
		return "", &refreshLifecycleError{http.StatusInternalServerError, "refreshed repository could not be safely promoted; previous cache restored"}
	}
	if err := replaceProjectMetadata(ctx, p.store, id, refs, fetchedAt); err != nil {
		pending, journalErr := p.refreshIntentExists(id)
		if journalErr != nil || pending {
			if rollbackErr := p.rollbackRefresh(id, true); rollbackErr != nil {
				message := "refresh commit and cache restoration failed; project operations are blocked until restart recovery succeeds"
				p.setBlocked(message)
				return "", &refreshLifecycleError{http.StatusServiceUnavailable, message}
			}
			return "", &refreshLifecycleError{http.StatusInternalServerError, "refresh could not be committed; previous cache and refs restored"}
		}
	}
	if err := p.removeOwnedPath(backup); err != nil {
		message := "refresh committed but previous cache cleanup failed; restart to reconcile before further project changes"
		p.setBlocked(message)
		p.setProjectWarning(ctx, id, message)
		return message, nil
	}
	return "", nil
}

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
	if actual, err := identity(p.root); err != nil || actual != p.rootIdentity {
		return errors.New("project cache root identity changed; project operations are blocked")
	}
	p.stateMu.RLock()
	blocked := p.blocked
	p.stateMu.RUnlock()
	if blocked != "" {
		return errors.New(blocked)
	}
	return nil
}

const cacheRecreationMarkerContents = "deploy-manager project cache recreation v1\n"

func (p *projectManager) cacheRecreationPath(id string) string {
	return filepath.Join(p.root, ".cache-recreate-"+id)
}

func (p *projectManager) cacheRecreationStagePath(id string) string {
	return filepath.Join(p.root, ".cache-recreate-stage-"+id)
}

// markCacheRecreation durably records intent before any cache path is touched.
func (p *projectManager) markCacheRecreation(id string) error {
	if !validProjectID(id) {
		return errors.New("invalid project identifier for cache recreation")
	}
	stage := p.cacheRecreationStagePath(id)
	file, err := os.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(file, cacheRecreationMarkerContents); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		_ = os.Remove(stage)
		return err
	}
	if closeErr != nil {
		_ = os.Remove(stage)
		return closeErr
	}
	if err := os.Rename(stage, p.cacheRecreationPath(id)); err != nil {
		_ = os.Remove(stage)
		return err
	}
	return syncDirectory(p.root)
}

func (p *projectManager) clearCacheRecreation(id string) error {
	path := p.cacheRecreationPath(id)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(p.root)
}

// reconcileProjectCacheRecreations completes cache removal before accepting work.
func (p *projectManager) reconcileProjectCacheRecreations() error {
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".cache-recreate-stage-") {
			id := strings.TrimPrefix(name, ".cache-recreate-stage-")
			if !validProjectID(id) || !entry.Type().IsRegular() || entry.Type()&os.ModeSymlink != 0 {
				return errors.New("cache recreation staging marker has an unexpected type")
			}
			if err := os.Remove(filepath.Join(p.root, name)); err != nil {
				return fmt.Errorf("remove abandoned cache recreation marker: %w", err)
			}
			continue
		}
		if !strings.HasPrefix(name, ".cache-recreate-") {
			continue
		}
		id := strings.TrimPrefix(name, ".cache-recreate-")
		if !validProjectID(id) {
			return errors.New("cache recreation marker contains an invalid project identifier")
		}
		path := p.cacheRecreationPath(id)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("cache recreation marker has an unexpected type")
		}
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != cacheRecreationMarkerContents {
			return errors.New("cache recreation marker is malformed")
		}
		if err := p.clearProjectCaches(id); err != nil {
			return fmt.Errorf("finish project cache recreation: %w", err)
		}
		if err := p.clearCacheRecreation(id); err != nil {
			return fmt.Errorf("clear project cache recreation marker: %w", err)
		}
	}
	return nil
}

func (p *projectManager) clearProjectCaches(id string) error {
	if !validProjectID(id) {
		return errors.New("invalid project identifier for cache recreation")
	}
	lfsRoot := filepath.Join(p.root, "lfs-cache")
	if info, err := os.Lstat(lfsRoot); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("LFS cache root is not a real directory; recreation stopped")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, path := range []string{p.repoPath(id), filepath.Join(lfsRoot, id)} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("project cache path is not a real directory; recreation stopped")
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("project cache could not be removed")
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return errors.New("project cache removal could not be confirmed")
		}
	}
	if err := syncDirectory(p.root); err != nil {
		return fmt.Errorf("sync project cache root: %w", err)
	}
	if _, err := os.Lstat(lfsRoot); err == nil {
		if err := syncDirectory(lfsRoot); err != nil {
			return fmt.Errorf("sync LFS cache root: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (p *projectManager) setBlocked(message string) {
	p.stateMu.Lock()
	p.blocked = message
	p.stateMu.Unlock()
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
			message := "project cache staging cleanup failed; restart to reconcile cache files before retrying"
			p.setBlocked(message)
			return errors.New(message)
		}
	}
	if _, err := p.cacheFS.Lstat(path); !os.IsNotExist(err) {
		message := "project cache staging cleanup could not be confirmed; restart to reconcile cache files before retrying"
		p.setBlocked(message)
		return errors.New(message)
	}
	return nil
}

func (p *projectManager) markRefreshIntent(id string) error {
	_, err := p.store.ProjectRefreshJournal.Create().SetID(id).Save(context.Background())
	return err
}

func (p *projectManager) clearRefreshIntent(id string) error {
	_, err := p.store.ProjectRefreshJournal.Delete().Where(projectrefreshjournal.IDEQ(id)).Exec(context.Background())
	return err
}

func (p *projectManager) refreshIntentExists(id string) (bool, error) {
	return p.store.ProjectRefreshJournal.Query().Where(projectrefreshjournal.IDEQ(id)).Exist(context.Background())
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
	journals, err := p.store.ProjectRefreshJournal.Query().Order(projectrefreshjournal.ByID()).All(context.Background())
	if err != nil {
		return err
	}
	for _, journal := range journals {
		id := journal.ID
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
