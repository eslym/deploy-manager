package lib

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/eslym/deploy-manager/lib/ent/projectsshkey"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

var errBranchFetchRetriesExhausted = errors.New("temporary branch fetch failures exhausted the bounded retry policy")

type transientGitNetworkError struct{}

func (transientGitNetworkError) Error() string { return "temporary Git network failure" }

const branchFetchMaxAttempts = 3

// fetchProjectForBranch refreshes the project's shared bare cache using the
// same bounded staging and journaled promotion policy as an explicit refresh.
// Ref metadata is only committed after the refreshed object store is promoted.
func (a *App) fetchProjectForBranch(ctx context.Context, id string) error {
	a.projects.cacheLifecycleMu.RLock()
	defer a.projects.cacheLifecycleMu.RUnlock()
	var err error
	for attempt := 1; attempt <= branchFetchMaxAttempts; attempt++ {
		err = a.fetchProjectForBranchAttempt(ctx, id)
		if err == nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryableBranchFetch(err) {
			return err
		}
		if attempt == branchFetchMaxAttempts {
			return errBranchFetchRetriesExhausted
		}
		delay := time.Duration(attempt) * 100 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func retryableBranchFetch(err error) bool {
	if err == nil {
		return false
	}
	var serverErr transientGitHTTPError
	if errors.As(err, &serverErr) {
		return serverErr.status >= 500 && serverErr.status <= 599
	}
	var networkErr transientGitNetworkError
	if errors.As(err, &networkErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		status := statusErr.StatusCode()
		return status >= 500 && status <= 599
	}
	var unexpected *plumbing.UnexpectedError
	if errors.As(err, &unexpected) && unexpected.Err != nil {
		return retryableBranchFetch(unexpected.Err)
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ETIMEDOUT)
}

func (a *App) fetchProjectForBranchAttempt(ctx context.Context, id string) error {
	p := a.projects
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.ensureCacheReady(); err != nil {
		return err
	}
	storedProject, err := a.store.Project.Get(ctx, id)
	if err != nil {
		return errors.New("project is unavailable")
	}
	remote := storedProject.RemoteURL
	var auth transport.AuthMethod
	if strings.HasPrefix(remote, "ssh://") {
		parsed, host, port, _, err := safeSSHRemote(remote)
		if err != nil {
			return errors.New("SSH repository configuration is invalid")
		}
		assignment, err := a.store.ProjectSSHKey.Query().Where(projectsshkey.IDEQ(id)).Only(ctx)
		if err != nil {
			return errors.New("SSH repository key is unavailable")
		}
		keyID := assignment.KeyID
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
		if retryableBranchFetch(err) {
			var serverErr transientGitHTTPError
			if errors.As(err, &serverErr) {
				return serverErr
			}
			return transientGitNetworkError{}
		}
		return errors.New(sanitizeGitError(err, remote))
	}
	if err = ctx.Err(); err != nil {
		return err
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := p.cacheFS.Lstat(backup); err == nil {
		return errors.New("an unresolved previous project cache exists")
	} else if !os.IsNotExist(err) {
		return errors.New("previous project cache cannot be inspected")
	}
	if err := p.markRefreshIntent(id); err != nil {
		return errors.New("project refresh state could not be recorded")
	}
	if err := ctx.Err(); err != nil {
		if rollbackErr := p.rollbackRefresh(id, false); rollbackErr != nil {
			return errors.New("cancelled project refresh could not be rolled back")
		}
		return err
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
	if err := replaceProjectMetadata(ctx, p.store, id, refs, time.Now().UTC()); err != nil {
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
