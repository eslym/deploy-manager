package lib

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eslym/deploy-manager/lib/ent"
	projectent "github.com/eslym/deploy-manager/lib/ent/project"
	"github.com/eslym/deploy-manager/lib/ent/projectref"
	"github.com/eslym/deploy-manager/lib/ent/projectrefreshjournal"
	"github.com/eslym/deploy-manager/lib/ent/projectsshkey"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

type projectManager struct {
	db                                 *sql.DB
	store                              *ent.Client
	root                               string
	rootIdentity                       dirIdentity
	timeout                            time.Duration
	maxTransferBytes                   int64
	maxCacheBytes                      int64
	maxRefs                            int
	maxAdvertisementBytes              int64
	mu                                 sync.Mutex
	cacheLifecycleMu                   sync.RWMutex
	cacheMu                            sync.RWMutex
	lfsCacheMu                         sync.Mutex
	lfsAuthMu                          sync.RWMutex
	stateMu                            sync.RWMutex
	cacheFS                            projectCacheFS
	blocked                            string
	branchFetchBeforeProjectLock       func()
	cacheRecreationBeforeLifecycleLock func()
}

const maxGitTreeObjectBytes = 16 << 20
const maxGitCommitObjectBytes = 1 << 20

var errGitTransferTooLarge = errors.New("Git server response exceeds the configured transfer limit")
var errGitUnsupportedSmartHTTP = errors.New("server does not provide a supported smart HTTPS Git repository")
var errGitUnsupportedSHA256 = errors.New("SHA-256 Git repositories are not supported")
var errGitIncompleteRepository = errors.New("repository is incomplete or uses an unsupported partial clone")
var errGitTooManyRefs = errors.New("repository has more references than the configured limit")

type gitHTTPPolicyKey struct{}

type gitHTTPPolicy struct {
	transferBytes      int64
	advertisementBytes int64
}

func withGitHTTPPolicy(ctx context.Context, manager *projectManager) context.Context {
	return context.WithValue(ctx, gitHTTPPolicyKey{}, gitHTTPPolicy{transferBytes: manager.maxTransferBytes, advertisementBytes: manager.maxAdvertisementBytes})
}

var configureTestGitTransport = func() error { return nil }

type project struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	URL           string     `json:"url"`
	LastFetched   *time.Time `json:"lastFetched"`
	Warning       string     `json:"warning,omitempty"`
	KeyID         string     `json:"keyId,omitempty"`
	GitCacheBytes int64      `json:"gitCacheBytes"`
	LFSCacheBytes int64      `json:"lfsCacheBytes"`
}

type projectRef struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Commit string `json:"commit"`
}

func newProjectManager(db *sql.DB, store *ent.Client, root string, timeout time.Duration, maxCacheBytes int64) (*projectManager, error) {
	if err := configureTestGitTransport(); err != nil {
		return nil, fmt.Errorf("invalid test Git transport configuration: %w", err)
	}
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	if timeout < time.Second || timeout > 10*time.Minute {
		return nil, errors.New("Git timeout must be between 1 second and 10 minutes")
	}
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("project cache root must be a real directory")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return nil, errors.New("project cache root must not traverse symbolic links")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	rootIdentity, err := identity(root)
	if err != nil {
		return nil, err
	}
	manager := &projectManager{db: db, store: store, root: root, rootIdentity: rootIdentity, timeout: timeout, maxCacheBytes: maxCacheBytes, cacheFS: osProjectCacheFS{}}
	if err := manager.reconcileProjectCacheRecreations(); err != nil {
		return nil, fmt.Errorf("project cache recreation recovery failed; refusing to start: %w", err)
	}
	if err := manager.reconcileCacheFiles(); err != nil {
		return nil, fmt.Errorf("project cache recovery failed; refusing to start: %w", err)
	}
	if err := manager.reconcileLFSCacheTemps(); err != nil {
		return nil, fmt.Errorf("LFS cache recovery failed; refusing to start: %w", err)
	}
	return manager, nil
}

func safeRemote(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, errors.New("use a public HTTPS repository URL without credentials, query, or fragment")
	}
	if u.Port() != "" {
		if _, err := fmt.Sscanf(u.Port(), "%d", new(int)); err != nil {
			return nil, errors.New("repository URL has an invalid port")
		}
	}
	return u, nil
}

func safeSSHRemote(raw string) (*url.URL, string, int, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "ssh" || u.Hostname() == "" || u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" {
		return nil, "", 0, "", errors.New("use an SSH repository URL with a host and path, without query or fragment")
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return nil, "", 0, "", errors.New("SSH repository URLs cannot contain passwords")
		}
	}
	host, hostErr := normalizeSSHHostname(u.Hostname())
	if hostErr != nil {
		return nil, "", 0, "", errors.New("SSH repository URL has an invalid host")
	}
	port := 22
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, "", 0, "", errors.New("SSH repository URL has an invalid port")
		}
	}
	path := strings.TrimPrefix(u.EscapedPath(), "/")
	decoded, decodeErr := url.PathUnescape(path)
	if decodeErr != nil || decoded == "" || strings.ContainsAny(decoded, "\r\n\x00") || strings.Contains(decoded, "..") || strings.HasPrefix(decoded, "-") {
		return nil, "", 0, "", errors.New("SSH repository URL has an invalid repository path")
	}
	user := "git"
	if u.User != nil {
		user = u.User.Username()
	}
	if user == "" || strings.ContainsAny(user, "\r\n\x00 /\\") {
		return nil, "", 0, "", errors.New("SSH repository URL has an invalid username")
	}
	canonical := (&url.URL{Scheme: "ssh", User: url.User(user), Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/" + decoded}).String()
	return u, host, port, canonical, nil
}

func (p *projectManager) repoPath(id string) string { return filepath.Join(p.root, id) }

func (a *App) projectAuthorized(w http.ResponseWriter, r *http.Request, mutation bool) bool {
	if !a.authSecure(r) {
		http.Error(w, "HTTPS is required", http.StatusUpgradeRequired)
		return false
	}
	if _, ok := a.session(r); !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return false
	}
	if mutation && (r.Method != http.MethodPost || !a.originOK(r)) {
		http.Error(w, "request rejected", http.StatusForbidden)
		return false
	}
	if mutation && !a.allow(r) {
		http.Error(w, "too many requests; try again shortly", http.StatusTooManyRequests)
		return false
	}
	return true
}

func (a *App) listProjects(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, false) {
		return
	}
	if err := a.projects.ensureCacheReady(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	projects, err := a.store.Project.Query().Order(ent.Asc(projectent.FieldCreatedAt), ent.Asc(projectent.FieldID)).All(r.Context())
	if err != nil {
		http.Error(w, "projects are temporarily unavailable", 500)
		return
	}
	out := []project{}
	for _, stored := range projects {
		x := project{ID: stored.ID, Name: stored.Name, URL: stored.RemoteURL, Warning: stored.Warning}
		if stored.LastFetched != nil {
			t := time.Unix(0, *stored.LastFetched).UTC()
			x.LastFetched = &t
		}
		out = append(out, x)
	}
	a.projects.cacheLifecycleMu.RLock()
	a.projects.cacheMu.RLock()
	a.projects.lfsCacheMu.Lock()
	for i := range out {
		if !validProjectID(out[i].ID) {
			continue
		}
		var sizeErr error
		out[i].GitCacheBytes, sizeErr = projectCacheSize(a.projects.repoPath(out[i].ID))
		if sizeErr == nil {
			out[i].LFSCacheBytes, sizeErr = projectLFSCacheSize(a.projects.root, out[i].ID)
		}
		if sizeErr != nil {
			a.projects.lfsCacheMu.Unlock()
			a.projects.cacheMu.RUnlock()
			a.projects.cacheLifecycleMu.RUnlock()
			http.Error(w, "project cache sizes are temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	a.projects.lfsCacheMu.Unlock()
	a.projects.cacheMu.RUnlock()
	a.projects.cacheLifecycleMu.RUnlock()
	if len(out) != 0 {
		assignedKeyIDs := make(map[string]string)
		const assignmentBatchSize = 500
		for start := 0; start < len(out); start += assignmentBatchSize {
			end := min(start+assignmentBatchSize, len(out))
			projectIDs := make([]string, end-start)
			for i := start; i < end; i++ {
				projectIDs[i-start] = out[i].ID
			}
			assignments, assignmentErr := a.store.ProjectSSHKey.Query().Where(projectsshkey.IDIn(projectIDs...)).All(r.Context())
			if assignmentErr != nil {
				http.Error(w, "projects are temporarily unavailable", 500)
				return
			}
			for _, assignment := range assignments {
				assignedKeyIDs[assignment.ID] = assignment.KeyID
			}
		}
		for i := range out {
			if keyID, ok := assignedKeyIDs[out[i].ID]; ok {
				out[i].KeyID = keyID
			}
		}
	}
	writeJSON(w, out)
}

func (a *App) recreateProjectCache(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	id := r.PathValue("id")
	if !validProjectID(id) {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var input struct {
		Confirm bool `json:"confirm"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || !input.Confirm {
		http.Error(w, "explicit cache recreation confirmation is required", http.StatusBadRequest)
		return
	}
	p := a.projects
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cacheRecreationBeforeLifecycleLock != nil {
		p.cacheRecreationBeforeLifecycleLock()
	}
	if err := p.ensureCacheReady(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	p.cacheLifecycleMu.Lock()
	defer p.cacheLifecycleMu.Unlock()
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.lfsCacheMu.Lock()
	defer p.lfsCacheMu.Unlock()
	if _, err := a.store.Project.Get(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := p.markCacheRecreation(id); err != nil {
		http.Error(w, "cache recreation could not be recorded", http.StatusServiceUnavailable)
		return
	}
	if err := p.clearProjectCaches(id); err != nil {
		p.setBlocked("project cache recreation is incomplete; restart to finish safely")
		http.Error(w, "cache recreation is incomplete; restart to finish safely", http.StatusServiceUnavailable)
		return
	}
	if err := p.clearCacheRecreation(id); err != nil {
		p.setBlocked("project cache recreation is incomplete; restart to finish safely")
		http.Error(w, "cache recreation is incomplete; restart to finish safely", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func projectCacheSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("project cache path is not a real directory")
	}
	return directorySize(path, int64(^uint64(0)>>1))
}

func projectLFSCacheSize(root, id string) (int64, error) {
	lfsRoot := filepath.Join(root, "lfs-cache")
	info, err := os.Lstat(lfsRoot)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("LFS cache root is not a real directory")
	}
	return projectCacheSize(filepath.Join(lfsRoot, id))
}

func (a *App) createProject(w http.ResponseWriter, r *http.Request) {
	buffered := newStagedResponseWriter(w)
	defer buffered.Flush()
	w = buffered
	if !a.projectAuthorized(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var input struct {
		Name  string `json:"name"`
		URL   string `json:"url"`
		KeyID string `json:"keyId"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil || strings.TrimSpace(input.Name) == "" || len(input.Name) > 100 {
		http.Error(w, "enter a project name and repository URL", 400)
		return
	}
	remote, sshHost, sshPort, keyID := "", "", 0, input.KeyID
	var sshURL *url.URL
	if strings.HasPrefix(strings.TrimSpace(input.URL), "ssh://") {
		var err error
		sshURL, sshHost, sshPort, remote, err = safeSSHRemote(input.URL)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if keyID == "" {
			http.Error(w, "select a repository key for this SSH project", 400)
			return
		}
	} else {
		if keyID != "" {
			http.Error(w, "repository keys are only used with SSH repositories", 400)
			return
		}
		u, err := safeRemote(input.URL)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		remote = u.String()
	}
	p := a.projects
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureCacheReady(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if keyID != "" {
		if _, err := a.store.RepositoryKey.Get(r.Context(), keyID); err != nil {
			http.Error(w, "select an available repository key", 400)
			return
		}
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		http.Error(w, "project creation is temporarily unavailable", 500)
		return
	}
	id := fmt.Sprintf("%x", idBytes)
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	ctx = withGitHTTPPolicy(ctx, p)
	if size, sizeErr := directorySize(p.root, p.maxCacheBytes); sizeErr != nil || size >= p.maxCacheBytes {
		http.Error(w, "project caches have no remaining space under the configured storage limit", http.StatusRequestEntityTooLarge)
		return
	}
	stage, err := p.cacheFS.MkdirTemp(p.root, ".git-stage-create-")
	if err != nil {
		http.Error(w, "could not prepare bounded repository staging", 500)
		return
	}
	defer func() {
		if cleanupErr := p.cleanupStage(stage); cleanupErr != nil {
			buffered.replace(http.StatusServiceUnavailable, cleanupErr.Error())
		}
	}()
	var auth transport.AuthMethod
	if sshURL != nil {
		user := "git"
		if sshURL.User != nil {
			user = sshURL.User.Username()
		}
		var authErr error
		auth, authErr = a.projectSSHAuth(keyID, sshHost, sshPort, user)
		if authErr != nil {
			http.Error(w, "managed repository key is unavailable", http.StatusBadRequest)
			return
		}
	}
	_, err = git.CloneContext(ctx, boundedStorage(stage, p.maxCacheBytes), nil, &git.CloneOptions{URL: remote, NoCheckout: true, Tags: git.AllTags, Auth: auth})
	if err != nil {
		if errors.Is(err, errGitCacheLimit) {
			http.Error(w, "repository object storage exceeded the configured cache limit", http.StatusRequestEntityTooLarge)
			return
		}
		message := sanitizeGitError(err, remote)
		status := http.StatusBadRequest
		if strings.Contains(strings.ToLower(err.Error()), "ssh host key is not approved") {
			status = http.StatusConflict
		}
		if strings.Contains(strings.ToLower(err.Error()), "unable to authenticate") || strings.Contains(strings.ToLower(err.Error()), "authentication failed") {
			status = http.StatusBadGateway
		}
		http.Error(w, message, status)
		return
	}
	if size, sizeErr := directorySize(p.root, p.maxCacheBytes); sizeErr != nil || size > p.maxCacheBytes {
		http.Error(w, "project caches exceed the configured storage limit", http.StatusRequestEntityTooLarge)
		return
	}
	if err = p.cacheFS.Rename(stage, p.repoPath(id)); err != nil {
		http.Error(w, "repository fetched but could not be promoted from staging", 500)
		return
	}
	stage = ""
	now := time.Now().UTC()
	tx, txErr := a.store.Tx(ctx)
	if txErr == nil {
		_, txErr = tx.Project.Create().SetID(id).SetName(strings.TrimSpace(input.Name)).SetRemoteURL(remote).SetCreatedAt(now.UnixNano()).SetLastFetched(now.UnixNano()).SetWarning("").Save(ctx)
		if txErr == nil && keyID != "" {
			_, txErr = tx.ProjectSSHKey.Create().SetID(id).SetKeyID(keyID).Save(ctx)
		}
		if txErr == nil {
			txErr = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	}
	err = txErr
	if err != nil {
		_ = os.RemoveAll(p.repoPath(id))
		http.Error(w, "could not save project", 500)
		return
	}
	if err = saveProjectRefs(context.Background(), p.store, id, p.repoPath(id), p.maxRefs); err != nil {
		if cleanupTx, cleanupErr := p.store.Tx(r.Context()); cleanupErr == nil {
			cleanupErr = cleanupTx.Project.DeleteOneID(id).Exec(r.Context())
			if cleanupErr == nil {
				cleanupErr = cleanupTx.Commit()
			} else {
				_ = cleanupTx.Rollback()
			}
		}
		_ = os.RemoveAll(p.repoPath(id))
		if errors.Is(err, errGitIncompleteRepository) || errors.Is(err, errGitTooManyRefs) {
			http.Error(w, sanitizeGitError(err, remote), http.StatusBadRequest)
		} else {
			http.Error(w, "repository fetched but project could not be saved; try again", 500)
		}
		return
	}
	writeJSONStatus(w, http.StatusCreated, project{ID: id, Name: strings.TrimSpace(input.Name), URL: remote, LastFetched: &now, KeyID: keyID})
}

func (a *App) projectRefs(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, false) {
		return
	}
	id := r.PathValue("id")
	if _, err := a.store.Project.Get(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	storedRefs, err := a.store.ProjectRef.Query().Where(projectref.IDEQ(id)).Order(ent.Asc(projectref.FieldKind), ent.Asc(projectref.FieldName)).All(r.Context())
	if err != nil {
		http.Error(w, "refs are temporarily unavailable", 500)
		return
	}
	refs := make([]projectRef, 0, len(storedRefs))
	for _, stored := range storedRefs {
		refs = append(refs, projectRef{Kind: stored.Kind, Name: stored.Name, Commit: stored.CommitID})
	}
	writeJSON(w, refs)
}

func (a *App) refreshProject(w http.ResponseWriter, r *http.Request) {
	buffered := newStagedResponseWriter(w)
	defer buffered.Flush()
	w = buffered
	if !a.projectAuthorized(w, r, true) {
		return
	}
	p := a.projects
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureCacheReady(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if !validProjectID(id) {
		http.NotFound(w, r)
		return
	}
	storedProject, projectErr := a.store.Project.Get(r.Context(), id)
	if projectErr != nil {
		http.NotFound(w, r)
		return
	}
	remote := storedProject.RemoteURL
	var keyID, sshUser string
	var sshAuth transport.AuthMethod
	if strings.HasPrefix(remote, "ssh://") {
		parsed, host, port, _, parseErr := safeSSHRemote(remote)
		assignment, assignmentErr := a.store.ProjectSSHKey.Query().Where(projectsshkey.IDEQ(id)).Only(r.Context())
		if parseErr != nil || assignmentErr != nil {
			http.Error(w, "SSH repository key is unavailable", 400)
			return
		}
		keyID = assignment.KeyID
		sshUser = "git"
		if parsed.User != nil {
			sshUser = parsed.User.Username()
		}
		var authErr error
		sshAuth, authErr = a.projectSSHAuth(keyID, host, port, sshUser)
		if authErr != nil {
			http.Error(w, "managed repository key is unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	refresh := p.manualRefresh(r.Context(), id, remote, sshAuth)
	if refresh.message != "" {
		http.Error(w, refresh.message, refresh.status)
		return
	}
	writeJSON(w, map[string]any{"lastFetched": refresh.fetchedAt, "warning": refresh.warning})
}

func (a *App) assignProjectKey(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	var input struct {
		KeyID string `json:"keyId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&input); err != nil || input.KeyID == "" {
		http.Error(w, "select a repository key", 400)
		return
	}
	p := a.projects
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lfsAuthMu.Lock()
	defer p.lfsAuthMu.Unlock()
	id := r.PathValue("id")
	storedProject, projectErr := a.store.Project.Get(r.Context(), id)
	if projectErr != nil {
		http.NotFound(w, r)
		return
	}
	remote := storedProject.RemoteURL
	if !strings.HasPrefix(remote, "ssh://") {
		http.Error(w, "repository keys can only be assigned to SSH projects", 400)
		return
	}
	tx, err := a.store.Tx(r.Context())
	if err != nil {
		http.Error(w, "project key assignment is temporarily unavailable", 500)
		return
	}
	defer tx.Rollback()
	if _, err = tx.RepositoryKey.Get(r.Context(), input.KeyID); err != nil {
		http.Error(w, "repository key was not found", 404)
		return
	}
	exists, err := tx.ProjectSSHKey.Query().Where(projectsshkey.IDEQ(id)).Exist(r.Context())
	if err == nil && exists {
		_, err = tx.ProjectSSHKey.Update().Where(projectsshkey.IDEQ(id)).SetKeyID(input.KeyID).Save(r.Context())
	} else if err == nil {
		_, err = tx.ProjectSSHKey.Create().SetID(id).SetKeyID(input.KeyID).Save(r.Context())
	}
	if err != nil {
		http.Error(w, "project key assignment is temporarily unavailable", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "project key assignment is temporarily unavailable", 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func saveProjectRefs(ctx context.Context, store *ent.Client, id, dir string, maxRefs int) error {
	refs, err := projectRefsFromRepository(dir, maxRefs)
	if err != nil {
		return err
	}
	return replaceProjectMetadataRefs(ctx, store, id, refs)
}

func projectRefsFromRepository(dir string, maxRefs int) ([]projectRef, error) {
	repo, err := git.PlainOpen(dir)
	if err != nil {
		return nil, err
	}
	refs, err := repo.References()
	if err != nil {
		return nil, err
	}
	defer refs.Close()
	result := make([]projectRef, 0)
	count := 0
	checkedObjects := make(map[plumbing.Hash]struct{})
	iterErr := refs.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name()
		kind := ""
		shortName := name.Short()
		if strings.HasPrefix(name.String(), "refs/remotes/origin/") {
			kind = "branch"
			shortName = strings.TrimPrefix(name.String(), "refs/remotes/origin/")
		} else if name.IsTag() {
			kind = "tag"
		} else {
			return nil
		}
		if count >= maxRefs {
			return errGitTooManyRefs
		}
		hash := ref.Hash()
		if kind == "tag" {
			target, peelErr := peelTagCommit(repo, hash)
			if peelErr != nil {
				return fmt.Errorf("tag %s does not resolve to a commit", shortName)
			}
			hash = target
		}
		if size, sizeErr := repo.Storer.EncodedObjectSize(hash); sizeErr == nil && size > maxGitCommitObjectBytes {
			return errors.New("repository contains an oversized commit object")
		}
		c, e := repo.CommitObject(hash)
		if e != nil {
			return errGitIncompleteRepository
		}
		if err := verifyCompleteCommit(repo, c, checkedObjects, 100000); err != nil {
			return err
		}
		result = append(result, projectRef{Kind: kind, Name: shortName, Commit: c.Hash.String()})
		count++
		return nil
	})
	if iterErr != nil {
		return nil, iterErr
	}
	return result, nil
}

func replaceProjectMetadata(ctx context.Context, store *ent.Client, id string, refs []projectRef, fetched time.Time) error {
	tx, err := store.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := replaceProjectMetadataRefsTx(ctx, tx, id, refs); err != nil {
		return err
	}
	if _, err := tx.Project.Update().Where(projectent.ID(id)).SetLastFetched(fetched.UnixNano()).SetWarning("").Save(ctx); err != nil {
		return err
	}
	if _, err := tx.ProjectRefreshJournal.Delete().Where(projectrefreshjournal.IDEQ(id)).Exec(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceProjectMetadataRefs(ctx context.Context, store *ent.Client, id string, refs []projectRef) error {
	tx, err := store.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := replaceProjectMetadataRefsTx(ctx, tx, id, refs); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceProjectMetadataRefsTx(ctx context.Context, tx *ent.Tx, id string, refs []projectRef) error {
	if _, err := tx.ProjectRef.Delete().Where(projectref.IDEQ(id)).Exec(ctx); err != nil {
		return err
	}
	for _, ref := range refs {
		if _, err := tx.ProjectRef.Create().SetID(id).SetKind(ref.Kind).SetName(ref.Name).SetCommitID(ref.Commit).Save(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (p *projectManager) setProjectWarning(ctx context.Context, id, warning string) {
	_, _ = p.store.Project.Update().Where(projectent.ID(id)).SetWarning(warning).Save(ctx)
}

func verifyCompleteCommit(repo *git.Repository, commit *object.Commit, checked map[plumbing.Hash]struct{}, maxObjects int) error {
	var visitTree func(plumbing.Hash) error
	visitTree = func(hash plumbing.Hash) error {
		if _, exists := checked[hash]; exists {
			return nil
		}
		if len(checked) >= maxObjects {
			return errors.New("repository tree exceeds the supported object-count limit")
		}
		if size, sizeErr := repo.Storer.EncodedObjectSize(hash); sizeErr != nil || size > maxGitTreeObjectBytes {
			return errors.New("repository tree object is missing or exceeds the supported object-size limit")
		}
		checked[hash] = struct{}{}
		tree, err := repo.TreeObject(hash)
		if err != nil {
			return errGitIncompleteRepository
		}
		for _, entry := range tree.Entries {
			switch entry.Mode {
			case filemode.Dir:
				if err := visitTree(entry.Hash); err != nil {
					return err
				}
			case filemode.Regular, filemode.Executable, filemode.Symlink:
				if _, exists := checked[entry.Hash]; exists {
					continue
				}
				if len(checked) >= maxObjects {
					return errors.New("repository tree exceeds the supported object-count limit")
				}
				if err := repo.Storer.HasEncodedObject(entry.Hash); err != nil {
					return errGitIncompleteRepository
				}
				checked[entry.Hash] = struct{}{}
			case filemode.Submodule:
				// A gitlink points at another repository and is not an object
				// expected in this project's object database.
				continue
			default:
				// Browse-only project caches still fail closed for gitlinks and
				// unsupported tree entries rather than presenting them as complete.
				return errors.New("repository contains an unsupported tree entry")
			}
		}
		return nil
	}
	if _, exists := checked[commit.Hash]; !exists {
		if len(checked) >= maxObjects {
			return errors.New("repository tree exceeds the supported object-count limit")
		}
		checked[commit.Hash] = struct{}{}
	}
	return visitTree(commit.TreeHash)
}

func sanitizeGitError(err error, remote string) string {
	s := strings.ReplaceAll(err.Error(), remote, "repository URL")
	lower := strings.ToLower(s)
	if strings.HasPrefix(remote, "ssh://") {
		if strings.Contains(lower, "ssh host key is not approved") || strings.Contains(lower, "host key could not be verified") {
			return "SSH host key is not approved; independently verify the observed fingerprint in SSH host warnings before approval"
		}
		if strings.Contains(lower, "authentication") || strings.Contains(lower, "authenticate") {
			return "SSH authentication failed; verify the selected repository key is authorized by the repository host"
		}
		return "SSH repository fetch failed; check the repository URL and SSH service availability"
	}
	if errors.Is(err, errGitUnsupportedSmartHTTP) {
		return "server does not provide a supported smart HTTPS Git repository"
	}
	if errors.Is(err, errGitUnsupportedSHA256) {
		return "SHA-256 Git repositories are not supported; use a conventional SHA-1 repository"
	}
	if errors.Is(err, errGitIncompleteRepository) {
		return "partial or incomplete repositories are not supported; use a complete SHA-1 repository"
	}
	if errors.Is(err, errGitTooManyRefs) {
		return "repository has more refs than the configured limit; reduce the number of branches and tags"
	}
	if strings.Contains(lower, "partial") || strings.Contains(lower, "incomplete") || strings.Contains(lower, "object not found") {
		return "partial or incomplete repositories are not supported; use a complete SHA-1 repository"
	}
	if strings.Contains(lower, "unable to fetch packfile") {
		return "repository did not provide a complete Git pack; partial or incomplete repositories are not supported"
	}
	if strings.Contains(lower, "sha-256") || strings.Contains(lower, "sha256") || strings.Contains(lower, "object-format") {
		return "SHA-256 Git repositories are not supported; use a conventional SHA-1 repository"
	}
	if errors.Is(err, errGitTransferTooLarge) || strings.Contains(lower, "transfer limit") {
		return "repository transfer exceeded the configured byte limit"
	}
	if errors.Is(err, errGitCacheLimit) {
		return "repository object storage exceeded the configured cache limit; the previous cache was retained"
	}
	if strings.Contains(lower, "authentication") || strings.Contains(lower, "authorization") {
		return "repository could not be read anonymously; use a public HTTPS repository"
	}
	if strings.Contains(strings.ToLower(s), "not found") {
		return "repository was not found or does not allow anonymous access"
	}
	if strings.Contains(strings.ToLower(s), "protocol") || strings.Contains(strings.ToLower(s), "smart") {
		return "server does not provide a supported smart HTTPS Git repository"
	}
	return "repository fetch failed: " + truncate(s, 240)
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, 200, v) }
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// The bounded protocol client rejects all redirects: a repository cannot redirect
// a fetch to another origin, credentials, or an internal service.
func init() {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{Proxy: nil, DialContext: publicGitDialContext(dialer)}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	installProjectHTTPSClient(client)
}

func publicGitDialContext(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		remote, _, err := net.SplitHostPort(conn.RemoteAddr().String())
		ip, parseErr := netip.ParseAddr(remote)
		if err != nil || parseErr != nil || !isPublicGitIP(ip) {
			_ = conn.Close()
			return nil, errors.New("repository host resolved to a non-public address")
		}
		return conn, nil
	}
}

func isPublicGitIP(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	var blocked []string
	if ip.Is4() {
		blocked = []string{
			"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
			"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24",
			"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		}
	} else {
		// Only globally allocated unicast space is eligible. Explicit exclusions
		// cover protocol assignments, documentation, translation and transition
		// ranges that net.IP/netip classify as global-unicast.
		if !netip.MustParsePrefix("2000::/3").Contains(ip) {
			return false
		}
		blocked = []string{
			"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20",
		}
	}
	for _, block := range blocked {
		if netip.MustParsePrefix(block).Contains(ip) {
			return false
		}
	}
	return true
}

func installProjectHTTPSClient(client *http.Client) {
	bounded := *client
	bounded.Transport = limitedResponseTransport{base: client.Transport}
	gitclient.InstallProtocol("https", githttp.NewClientWithOptions(&bounded, &githttp.ClientOptions{RedirectPolicy: githttp.NoFollowRedirects}))
}

type limitedResponseTransport struct {
	base http.RoundTripper
}

type transientGitHTTPError struct{ status int }

func (e transientGitHTTPError) Error() string { return "temporary Git server response" }

func (t limitedResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= http.StatusInternalServerError && response.StatusCode <= 599 {
		_ = response.Body.Close()
		return nil, transientGitHTTPError{status: response.StatusCode}
	}
	policy, ok := req.Context().Value(gitHTTPPolicyKey{}).(gitHTTPPolicy)
	if !ok || policy.transferBytes <= 0 || policy.advertisementBytes <= 0 {
		policy = gitHTTPPolicy{transferBytes: 64 << 20, advertisementBytes: 4 << 20}
	}
	if response.ContentLength > policy.transferBytes {
		_ = response.Body.Close()
		return nil, errGitTransferTooLarge
	}
	response.Body = &limitedResponseBody{ReadCloser: response.Body, limit: policy.transferBytes}
	if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/info/refs") && req.URL.Query().Get("service") == "git-upload-pack" {
		// Preserve the actual HTTP status for go-git's typed transport error.
		// Treating an outage response as a malformed Git advertisement obscures
		// transient 5xx failures and prevents safe bounded retry classification.
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return response, nil
		}
		contentType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
		if contentType != "application/x-git-upload-pack-advertisement" {
			_ = response.Body.Close()
			return nil, errGitUnsupportedSmartHTTP
		}
		limit := policy.advertisementBytes
		if transferLimit := policy.transferBytes; limit > transferLimit {
			limit = transferLimit
		}
		if response.ContentLength > limit {
			_ = response.Body.Close()
			return nil, errGitTransferTooLarge
		}
		advertisement, readErr := io.ReadAll(&limitedResponseBody{ReadCloser: response.Body, limit: limit})
		_ = response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if bytes.Contains(advertisement, []byte("object-format=sha256")) || bytes.Contains(advertisement, []byte("object-format=sha-256")) {
			return nil, errGitUnsupportedSHA256
		}
		response.Body = io.NopCloser(bytes.NewReader(advertisement))
	}
	return response, nil
}

type limitedResponseBody struct {
	io.ReadCloser
	limit int64
	read  int64
}

func (b *limitedResponseBody) Read(p []byte) (int, error) {
	if b.read >= b.limit {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, errGitTransferTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.limit-b.read {
		p = p[:b.limit-b.read]
	}
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	return n, err
}

func directorySize(root string, stopAfter int64) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		if size > stopAfter {
			return errors.New("repository cache exceeds limit")
		}
		return nil
	})
	return size, err
}
