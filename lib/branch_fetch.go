package lib

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/eslym/deploy-manager/lib/ent/projectsshkey"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

var errBranchFetchRetriesExhausted = errors.New("temporary branch fetch failures exhausted the bounded retry policy")

type transientGitNetworkError struct{}

func (transientGitNetworkError) Error() string { return "temporary Git network failure" }

// fetchProjectForBranch refreshes the project's shared bare cache using the
// same bounded staging and journaled promotion policy as an explicit refresh.
// Ref metadata is only committed after the refreshed object store is promoted.
func (a *App) fetchProjectForBranch(ctx context.Context, id string) error {
	var err error
	for attempt := 1; attempt <= a.cfg.FetchRetryAttempts; attempt++ {
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
		if attempt == a.cfg.FetchRetryAttempts {
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
	if p.branchFetchBeforeProjectLock != nil {
		p.branchFetchBeforeProjectLock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Keep the recreation lock order: project protocol lock, then lifecycle lock.
	p.cacheLifecycleMu.RLock()
	defer p.cacheLifecycleMu.RUnlock()
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
		parsed, host, port, _, parseErr := safeSSHRemote(remote)
		if parseErr != nil {
			return errors.New("SSH repository configuration is invalid")
		}
		assignment, assignErr := a.store.ProjectSSHKey.Query().Where(projectsshkey.IDEQ(id)).Only(ctx)
		if assignErr != nil {
			return errors.New("SSH repository key is unavailable")
		}
		user := "git"
		if parsed.User != nil {
			user = parsed.User.Username()
		}
		auth, err = a.projectSSHAuth(assignment.KeyID, host, port, user)
		if err != nil {
			return errors.New("SSH repository authentication is unavailable")
		}
	} else if _, err := url.ParseRequestURI(remote); err != nil {
		return errors.New("repository URL is invalid")
	}
	result := p.refreshProjectCache(ctx, id, remote, auth)
	if result.message != "" {
		if result.cause != nil && retryableBranchFetch(result.cause) {
			var serverErr transientGitHTTPError
			if errors.As(result.cause, &serverErr) {
				return serverErr
			}
			return transientGitNetworkError{}
		}
		return errors.New(result.message)
	}
	if result.warning != "" {
		return errors.New(result.warning)
	}
	return nil
}
