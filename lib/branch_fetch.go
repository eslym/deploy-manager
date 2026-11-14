package lib

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// fetchProjectForBranch refreshes the project's shared bare cache using the
// same bounded staging and journaled promotion policy as an explicit refresh.
// Ref metadata is only committed after the refreshed object store is promoted.
func (a *App) fetchProjectForBranch(ctx context.Context, id string) error {
	p := a.projects
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureCacheReady(); err != nil {
		return err
	}
	var remote string
	if err := p.db.QueryRow(`SELECT remote_url FROM projects WHERE id=?`, id).Scan(&remote); err != nil {
		return errors.New("project is unavailable")
	}
	var auth transport.AuthMethod
	if strings.HasPrefix(remote, "ssh://") {
		parsed, host, port, _, err := safeSSHRemote(remote)
		if err != nil {
			return errors.New("SSH repository configuration is invalid")
		}
		var keyID string
		if err := p.db.QueryRow(`SELECT key_id FROM project_ssh_keys WHERE project_id=?`, id).Scan(&keyID); err != nil {
			return errors.New("SSH repository key is unavailable")
		}
		user := "git"
		if parsed.User != nil {
			user = parsed.User.Username()
		}
		auth, err = a.projectSSHAuth(keyID, host, port, user)
		if err != nil {
			return errors.New("SSH repository authentication is unavailable")
		}
	} else if _, err := url.ParseRequestURI(remote); err != nil {
		return errors.New("repository URL is invalid")
	}
	if err := p.checkPersistentCacheBudget(); err != nil {
		return err
	}
	stage := p.refreshStagePath(id)
	if err := p.cacheFS.Mkdir(stage, 0700); err != nil {
		return errors.New("could not prepare repository refresh")
	}
	defer func() { _ = p.cleanupStage(stage) }()
	p.cacheMu.RLock()
	if err := copyGitCache(p.repoPath(id), stage, p.maxCacheBytes); err != nil {
		p.cacheMu.RUnlock()
		return errors.New("could not stage project repository")
	}
	p.cacheMu.RUnlock()
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	repo, err := git.Open(boundedStorage(stage, p.maxCacheBytes), nil)
	if err != nil {
		return errors.New("project repository is unavailable")
	}
	if err = repo.FetchContext(withGitHTTPPolicy(ctx, p), &git.FetchOptions{RemoteName: "origin", Tags: git.AllTags, Force: true, Prune: true, Auth: auth}); err != nil && err != git.NoErrAlreadyUpToDate {
		return errors.New(sanitizeGitError(err, remote))
	}
	refs, err := projectRefsFromRepository(stage, p.maxRefs)
	if err != nil {
		return errors.New(sanitizeGitError(err, remote))
	}
	used, err := directorySize(p.root, 2*p.maxCacheBytes)
	if err != nil {
		return errors.New("project cache size could not be checked")
	}
	stageSize, err := directorySize(stage, p.maxCacheBytes)
	if err != nil || stageSize > p.maxCacheBytes {
		return errors.New("refreshed repository exceeds the configured cache limit")
	}
	liveSize, err := directorySize(p.repoPath(id), p.maxCacheBytes)
	if err != nil || used < liveSize || used-liveSize > p.maxCacheBytes {
		return errors.New("refreshed repository exceeds the configured cache limit")
	}
	backup := p.refreshBackupPath(id)
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	if _, err := p.cacheFS.Lstat(backup); err == nil {
		return errors.New("an unresolved previous project cache exists")
	} else if !os.IsNotExist(err) {
		return errors.New("previous project cache cannot be inspected")
	}
	if err := p.markRefreshIntent(id); err != nil {
		return errors.New("project refresh state could not be recorded")
	}
	if err := p.cacheFS.Rename(p.repoPath(id), backup); err != nil {
		_ = p.rollbackRefresh(id, false)
		return errors.New("project cache could not be safely staged")
	}
	if err := p.cacheFS.Rename(stage, p.repoPath(id)); err != nil {
		if p.rollbackRefresh(id, true) != nil {
			return errors.New("project cache recovery is required")
		}
		return errors.New("refreshed project cache could not be promoted")
	}
	if err := replaceProjectMetadata(p.db, id, refs, time.Now().UTC()); err != nil {
		if p.rollbackRefresh(id, true) != nil {
			return errors.New("project cache recovery is required")
		}
		return errors.New("project refresh could not be committed")
	}
	if err := p.removeOwnedPath(backup); err != nil {
		p.setBlocked("project cache cleanup is pending restart recovery")
		return errors.New("project cache cleanup is pending recovery")
	}
	return nil
}
