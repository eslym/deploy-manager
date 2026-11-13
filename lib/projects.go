package lib

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

type projectManager struct {
	db      *sql.DB
	root    string
	timeout time.Duration
	mu      sync.Mutex
}

type project struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	URL         string     `json:"url"`
	LastFetched *time.Time `json:"lastFetched"`
	Warning     string     `json:"warning,omitempty"`
}

type projectRef struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Commit string `json:"commit"`
}

func newProjectManager(db *sql.DB, root string, timeout time.Duration) (*projectManager, error) {
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	if timeout < time.Second || timeout > 10*time.Minute {
		return nil, errors.New("Git timeout must be between 1 second and 10 minutes")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS projects(id TEXT PRIMARY KEY,name TEXT NOT NULL,remote_url TEXT NOT NULL,created_at INTEGER NOT NULL,last_fetched INTEGER,warning TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS project_refs(project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,kind TEXT NOT NULL,name TEXT NOT NULL,commit_id TEXT NOT NULL,PRIMARY KEY(project_id,kind,name))`,
	} {
		if _, err := db.Exec(q); err != nil {
			return nil, err
		}
	}
	return &projectManager{db: db, root: root, timeout: timeout}, nil
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

func (p *projectManager) repoPath(id string) string { return filepath.Join(p.root, id) }

func (a *App) projectAuthorized(w http.ResponseWriter, r *http.Request, mutation bool) bool {
	if !a.https(r) {
		http.Error(w, "HTTPS is required", http.StatusUpgradeRequired)
		return false
	}
	if _, ok := a.session(r); !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return false
	}
	if mutation && (r.Method != http.MethodPost || !a.originOK(r) || !a.allow(r)) {
		http.Error(w, "request rejected", http.StatusForbidden)
		return false
	}
	return true
}

func (a *App) listProjects(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, false) {
		return
	}
	rows, err := a.db.Query(`SELECT id,name,remote_url,last_fetched,warning FROM projects ORDER BY created_at,id`)
	if err != nil {
		http.Error(w, "projects are temporarily unavailable", 500)
		return
	}
	defer rows.Close()
	out := []project{}
	for rows.Next() {
		var x project
		var fetched sql.NullInt64
		if err := rows.Scan(&x.ID, &x.Name, &x.URL, &fetched, &x.Warning); err != nil {
			http.Error(w, "projects are temporarily unavailable", 500)
			return
		}
		if fetched.Valid {
			t := time.Unix(0, fetched.Int64).UTC()
			x.LastFetched = &t
		}
		out = append(out, x)
	}
	if rows.Err() != nil {
		http.Error(w, "projects are temporarily unavailable", 500)
		return
	}
	writeJSON(w, out)
}

func (a *App) createProject(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var input struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil || strings.TrimSpace(input.Name) == "" || len(input.Name) > 100 {
		http.Error(w, "enter a project name and HTTPS repository URL", 400)
		return
	}
	u, err := safeRemote(input.URL)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	p := a.projects
	p.mu.Lock()
	defer p.mu.Unlock()
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		http.Error(w, "project creation is temporarily unavailable", 500)
		return
	}
	id := fmt.Sprintf("%x", idBytes)
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	remote := u.String()
	_, err = git.PlainCloneContext(ctx, p.repoPath(id), true, &git.CloneOptions{URL: remote, NoCheckout: true, Tags: git.AllTags})
	if err != nil {
		_ = os.RemoveAll(p.repoPath(id))
		http.Error(w, sanitizeGitError(err, remote), 400)
		return
	}
	now := time.Now().UTC()
	_, err = p.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at,last_fetched,warning) VALUES(?,?,?,?,?,'')`, id, strings.TrimSpace(input.Name), remote, now.UnixNano(), now.UnixNano())
	if err != nil {
		_ = os.RemoveAll(p.repoPath(id))
		http.Error(w, "could not save project", 500)
		return
	}
	if err = saveProjectRefs(p.db, id, p.repoPath(id)); err != nil {
		_, _ = p.db.Exec(`DELETE FROM projects WHERE id=?`, id)
		_ = os.RemoveAll(p.repoPath(id))
		http.Error(w, "repository fetched but project could not be saved; try again", 500)
		return
	}
	writeJSONStatus(w, http.StatusCreated, project{ID: id, Name: strings.TrimSpace(input.Name), URL: remote, LastFetched: &now})
}

func (a *App) projectRefs(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, false) {
		return
	}
	id := r.PathValue("id")
	var found int
	if a.db.QueryRow(`SELECT 1 FROM projects WHERE id=?`, id).Scan(&found) != nil {
		http.NotFound(w, r)
		return
	}
	rows, err := a.db.Query(`SELECT kind,name,commit_id FROM project_refs WHERE project_id=? ORDER BY kind,name`, id)
	if err != nil {
		http.Error(w, "refs are temporarily unavailable", 500)
		return
	}
	defer rows.Close()
	refs := []projectRef{}
	for rows.Next() {
		var x projectRef
		if rows.Scan(&x.Kind, &x.Name, &x.Commit) != nil {
			http.Error(w, "refs are temporarily unavailable", 500)
			return
		}
		refs = append(refs, x)
	}
	writeJSON(w, refs)
}

func (a *App) refreshProject(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	p := a.projects
	p.mu.Lock()
	defer p.mu.Unlock()
	id := r.PathValue("id")
	var remote string
	if p.db.QueryRow(`SELECT remote_url FROM projects WHERE id=?`, id).Scan(&remote) != nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	repo, err := git.PlainOpen(p.repoPath(id))
	if err == nil {
		err = repo.FetchContext(ctx, &git.FetchOptions{RemoteName: "origin", Tags: git.AllTags, Force: true, Prune: true})
	}
	if err == git.NoErrAlreadyUpToDate {
		err = nil
	}
	if err == nil {
		err = saveProjectRefs(p.db, id, p.repoPath(id))
	}
	if err != nil {
		warning := sanitizeGitError(err, remote)
		_, _ = p.db.Exec(`UPDATE projects SET warning=? WHERE id=?`, warning, id)
		http.Error(w, warning, 502)
		return
	}
	now := time.Now().UTC()
	_, err = p.db.Exec(`UPDATE projects SET last_fetched=?,warning='' WHERE id=?`, now.UnixNano(), id)
	if err != nil {
		http.Error(w, "fetch succeeded but status could not be saved", 500)
		return
	}
	writeJSON(w, map[string]any{"lastFetched": now})
}

func saveProjectRefs(db *sql.DB, id, dir string) error {
	repo, err := git.PlainOpen(dir)
	if err != nil {
		return err
	}
	refs, err := repo.References()
	if err != nil {
		return err
	}
	defer refs.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM project_refs WHERE project_id=?`, id); err != nil {
		return err
	}
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
		hash := ref.Hash()
		if kind == "tag" {
			_, e := repo.TagObject(hash)
			if e == nil {
				target, e := repo.ResolveRevision(plumbing.Revision(name.String()))
				if e != nil {
					return fmt.Errorf("tag %s does not resolve to a commit", shortName)
				}
				hash = *target
			}
		}
		c, e := repo.CommitObject(hash)
		if e != nil {
			return nil
		}
		_, e = tx.Exec(`INSERT INTO project_refs(project_id,kind,name,commit_id) VALUES(?,?,?,?)`, id, kind, shortName, c.Hash.String())
		return e
	})
	if iterErr != nil {
		return iterErr
	}
	return tx.Commit()
}

func sanitizeGitError(err error, remote string) string {
	s := strings.ReplaceAll(err.Error(), remote, "repository URL")
	if strings.Contains(strings.ToLower(s), "authentication") || strings.Contains(strings.ToLower(s), "authorization") {
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
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		remote, _, err := net.SplitHostPort(conn.RemoteAddr().String())
		ip := net.ParseIP(remote)
		if err != nil || ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			_ = conn.Close()
			return nil, errors.New("repository host resolved to a non-public address")
		}
		return conn, nil
	}}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	gitclient.InstallProtocol("https", githttp.NewClientWithOptions(client, &githttp.ClientOptions{RedirectPolicy: githttp.NoFollowRedirects}))
}
