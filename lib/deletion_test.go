package lib

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type deletionCacheFS struct {
	osProjectCacheFS
	fail bool
}

func (fs *deletionCacheFS) RemoveAll(path string) error {
	if fs.fail {
		return errors.New("injected cache removal failure")
	}
	return os.RemoveAll(path)
}

func TestProtectedDeletionConfirmationAndIndependentPublication(t *testing.T) {
	base := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key"), InsecureLocalHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	projectID, firstID, otherID := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	now := time.Now().UnixNano()
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "fixture", "https://example.test/repo.git", now); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{firstID, otherID} {
		path := app.deploymentPath(id)
		if err = os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(path, "index.html"), []byte(fmt.Sprintf("site-%d", i)), 0644); err != nil {
			t.Fatal(err)
		}
		identity, err := identity(path)
		if err != nil {
			t.Fatal(err)
		}
		commit := strings.Repeat("1", 40)
		if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,live_commit,live_dev,live_ino,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'live',?,?)`, id, projectID, fmt.Sprintf("site-%d", i), commit, commit, identity.Dev, identity.Ino, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err = app.db.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at) VALUES(?,?,?,1,'succeeded','Published',?,?)`, strings.Repeat("d", 31)+fmt.Sprint(i), id, commit, now, now); err != nil {
			t.Fatal(err)
		}
	}
	cache := app.projects.repoPath(projectID)
	if err = os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	lfsCache := filepath.Join(app.projects.root, "lfs-cache", projectID)
	if err = os.MkdirAll(lfsCache, 0700); err != nil {
		t.Fatal(err)
	}
	lfsPayload := filepath.Join(lfsCache, strings.Repeat("a", 64))
	if err = os.WriteFile(lfsPayload, []byte("verified payload"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	token := []byte(strings.Repeat("t", 32))
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	cookie := base64.RawURLEncoding.EncodeToString(token)
	if _, err = app.db.Exec(`UPDATE projects SET remote_url=? WHERE id=?`, `ssh://git@example.test/repo.git`, projectID); err != nil {
		t.Fatal(err)
	}
	keyResponse := projectPost(t, server.Client(), server.URL, cookie, "/api/repository-keys", `{"name":"shared project key","generate":true}`)
	if keyResponse.StatusCode != http.StatusCreated {
		t.Fatalf("shared key create status=%d: %s", keyResponse.StatusCode, responseText(keyResponse))
	}
	var sharedKey repositoryKey
	if err = json.NewDecoder(keyResponse.Body).Decode(&sharedKey); err != nil {
		t.Fatal(err)
	}
	keyResponse.Body.Close()
	assigned := projectPost(t, server.Client(), server.URL, cookie, "/api/projects/"+projectID+"/key", fmt.Sprintf(`{"keyId":%q}`, sharedKey.ID))
	assigned.Body.Close()
	if assigned.StatusCode != http.StatusNoContent {
		t.Fatalf("project key assignment status=%d", assigned.StatusCode)
	}
	if _, err = app.db.Exec(`INSERT INTO ssh_host_trust(host,port,trusted_key,warning) VALUES('example.test',22,?,'')`, []byte("trusted host key")); err != nil {
		t.Fatal(err)
	}
	path := "/api/deployments/" + firstID + "/delete"
	response := projectPost(t, server.Client(), server.URL, cookie, path, `{"confirm":false}`)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing confirmation status=%d", response.StatusCode)
	}
	if _, err = os.Stat(filepath.Join(app.deploymentPath(firstID), "index.html")); err != nil {
		t.Fatalf("cancelled deletion changed publication: %v", err)
	}
	setPublicationFaultPoint(func(point string) error {
		if point == "before-deployment-remove" {
			return errors.New("injected removal failure")
		}
		return nil
	})
	response = projectPost(t, server.Client(), server.URL, cookie, path, `{"confirm":true}`)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failed cleanup status=%d", response.StatusCode)
	}
	setPublicationFaultPoint(nil)
	listReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/deployments", nil)
	listReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	listResponse, err := server.Client().Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	var visible []deployment
	if err = json.NewDecoder(listResponse.Body).Decode(&visible); err != nil {
		t.Fatal(err)
	}
	listResponse.Body.Close()
	foundFailure := false
	for _, item := range visible {
		if item.ID == firstID && (item.Status == "deletion_failed" || item.Status == "deleting") && item.Error != "" {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatalf("deletion failure was not visible: %#v", visible)
	}
	response = projectPost(t, server.Client(), server.URL, cookie, path, `{"confirm":true}`)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("confirmed deletion retry status=%d", response.StatusCode)
	}
	if _, err = os.Stat(app.deploymentPath(firstID)); !os.IsNotExist(err) {
		t.Fatalf("deleted publication still exists: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(app.deploymentPath(otherID), "index.html")); err != nil || string(body) != "site-1" {
		t.Fatalf("independent publication changed: %q, %v", body, err)
	}
	var archived int
	if err = app.db.QueryRow(`SELECT count(*) FROM archived_deployment_jobs WHERE deployment_id=?`, firstID).Scan(&archived); err != nil || archived != 1 {
		t.Fatalf("deployment audit jobs not retained: count=%d err=%v", archived, err)
	}
	response = projectPost(t, server.Client(), server.URL, cookie, "/api/projects/"+projectID+"/delete", `{"confirm":true}`)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("project with deployment delete status=%d", response.StatusCode)
	}
	if _, err = os.Stat(cache); err != nil {
		t.Fatalf("blocked project deletion removed repository cache: %v", err)
	}
	response = projectPost(t, server.Client(), server.URL, cookie, "/api/deployments/"+otherID+"/delete", `{"confirm":true}`)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("second deployment deletion status=%d", response.StatusCode)
	}
	cacheFS := &deletionCacheFS{fail: true}
	app.projects.cacheFS = cacheFS
	response = projectPost(t, server.Client(), server.URL, cookie, "/api/projects/"+projectID+"/delete", `{"confirm":true}`)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("project cleanup failure status=%d", response.StatusCode)
	}
	cacheFS.fail = false
	response = projectPost(t, server.Client(), server.URL, cookie, "/api/projects/"+projectID+"/delete", `{"confirm":true}`)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("project deletion retry status=%d", response.StatusCode)
	}
	if _, err = os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("project cache remains after deletion: %v", err)
	}
	if _, err = os.Stat(lfsCache); !os.IsNotExist(err) {
		t.Fatalf("project LFS cache remains after deletion: %v", err)
	}
	if err = app.db.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, projectID).Scan(&archived); err != nil || archived != 0 {
		t.Fatalf("project record remains: %d %v", archived, err)
	}
	keysReq, _ := http.NewRequest(http.MethodGet, server.URL+"/api/repository-keys", nil)
	keysReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	keysResp, err := server.Client().Do(keysReq)
	if err != nil {
		t.Fatal(err)
	}
	var remainingKeys []repositoryKey
	if err = json.NewDecoder(keysResp.Body).Decode(&remainingKeys); err != nil {
		t.Fatal(err)
	}
	keysResp.Body.Close()
	if len(remainingKeys) != 1 || remainingKeys[0].ID != sharedKey.ID {
		t.Fatalf("project deletion removed a shared repository key: %#v", remainingKeys)
	}
	var trusted int
	if err = app.db.QueryRow(`SELECT count(*) FROM ssh_host_trust WHERE host='example.test' AND port=22`).Scan(&trusted); err != nil || trusted != 1 {
		t.Fatalf("project deletion removed global SSH trust: %d %v", trusted, err)
	}
}

func TestProjectDeletionTombstoneBlocksLFSCacheRecreation(t *testing.T) {
	base := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key"), InsecureLocalHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	projectID := strings.Repeat("a", 32)
	now := time.Now().UnixNano()
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "fixture", "https://example.test/repo.git", now); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO project_lfs_settings(project_id,endpoint,object_origins,internal_addresses,updated_at) VALUES(?,?,?,'{}',?)`, projectID, "https://example.test/info/lfs", `["https://example.test"]`, now); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO project_deletions(project_id,cache_dev,cache_ino,status,created_at,updated_at) VALUES(?,0,0,'deleting',?,?)`, projectID, now, now); err != nil {
		t.Fatal(err)
	}
	hydrator, err := app.projectLFSHydrator(projectID)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("must not be cached after deletion")
	digest := sha256.Sum256(payload)
	if _, err = hydrator.hydrate(t.Context(), lfsPointer{OID: fmt.Sprintf("%x", digest), Size: int64(len(payload))}); err == nil || !strings.Contains(err.Error(), "deletion is in progress") {
		t.Fatalf("LFS hydration during deletion returned %v", err)
	}
	if _, err = os.Stat(filepath.Join(app.projects.root, "lfs-cache", projectID)); !os.IsNotExist(err) {
		t.Fatalf("LFS cache was recreated during deletion: %v", err)
	}
	if _, err = app.db.Exec(`DELETE FROM project_deletions WHERE project_id=?`, projectID); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`DELETE FROM projects WHERE id=?`, projectID); err != nil {
		t.Fatal(err)
	}
	if err = hydrator.cacheAllowed(); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("stale LFS hydrator remained enabled after project deletion: %v", err)
	}
}

func TestProjectDeletionRefusesReplacedLFSCachePath(t *testing.T) {
	base := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key"), InsecureLocalHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	projectID := strings.Repeat("c", 32)
	cachePath := app.projects.repoPath(projectID)
	lfsPath := filepath.Join(app.projects.root, "lfs-cache", projectID)
	if err = os.Mkdir(cachePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(lfsPath, 0700); err != nil {
		t.Fatal(err)
	}
	cacheID, err := identity(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	lfsID, err := identity(lfsPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "fixture", "https://example.test/repo.git", now); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO project_deletions(project_id,cache_dev,cache_ino,lfs_dev,lfs_ino,status,created_at,updated_at) VALUES(?,?,?,?,?,'deleting',?,?)`, projectID, cacheID.Dev, cacheID.Ino, lfsID.Dev, lfsID.Ino, now, now); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(lfsPath, lfsPath+".preserved"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(lfsPath, 0700); err != nil {
		t.Fatal(err)
	}
	app.projects.mu.Lock()
	app.projects.cacheMu.Lock()
	err = app.finishProjectDeletion(projectID, cacheID, lfsID)
	app.projects.cacheMu.Unlock()
	app.projects.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("deletion of replaced LFS cache returned %v", err)
	}
	if _, err = os.Stat(lfsPath); err != nil {
		t.Fatalf("replacement LFS cache was removed: %v", err)
	}
	if _, err = os.Stat(cachePath); err != nil {
		t.Fatalf("project Git cache removed despite LFS identity mismatch: %v", err)
	}
}

func TestProjectDeletionCrashChild(t *testing.T) {
	point := os.Getenv("PROJECT_DELETE_CRASH_POINT")
	if point == "" {
		return
	}
	setPublicationFaultPoint(func(at string) error {
		if at == point {
			os.Exit(86)
		}
		return nil
	})
	app, err := New(Config{DatabasePath: os.Getenv("PROJECT_DELETE_CRASH_DB"), OPAQUEKeyPath: os.Getenv("PROJECT_DELETE_CRASH_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	t.Fatalf("startup did not reach project deletion crash point %q", point)
}

func TestProjectDeletionProcessCrashResumesByIdentity(t *testing.T) {
	for _, point := range []string{"before-project-lfs-remove", "after-project-lfs-remove", "before-project-cache-remove", "after-project-cache-remove"} {
		t.Run(point, func(t *testing.T) {
			base := t.TempDir()
			cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")}
			app, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			projectID := strings.Repeat("a", 32)
			cachePath := app.projects.repoPath(projectID)
			lfsPath := filepath.Join(app.projects.root, "lfs-cache", projectID)
			if err = os.Mkdir(cachePath, 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(lfsPath, 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(lfsPath, strings.Repeat("b", 64)), []byte("payload"), 0600); err != nil {
				t.Fatal(err)
			}
			cacheID, err := identity(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			lfsID, err := identity(lfsPath)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UnixNano()
			if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "fixture", "https://example.test/repo.git", now); err != nil {
				t.Fatal(err)
			}
			if _, err = app.db.Exec(`INSERT INTO project_deletions(project_id,cache_dev,cache_ino,lfs_dev,lfs_ino,status,created_at,updated_at) VALUES(?,?,?,?,?,'deleting',?,?)`, projectID, cacheID.Dev, cacheID.Ino, lfsID.Dev, lfsID.Ino, now, now); err != nil {
				t.Fatal(err)
			}
			if err = app.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestProjectDeletionCrashChild$")
			cmd.Env = append(os.Environ(), "PROJECT_DELETE_CRASH_POINT="+point, "PROJECT_DELETE_CRASH_DB="+cfg.DatabasePath, "PROJECT_DELETE_CRASH_KEY="+cfg.OPAQUEKeyPath)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("child did not crash at %s: %v: %s", point, err, output)
			}
			app, err = New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			for _, path := range []string{cachePath, lfsPath} {
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("cache path remains after restart: %s: %v", path, err)
				}
			}
			var count int
			if err = app.db.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, projectID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("project deletion did not finalize after restart: %d %v", count, err)
			}
		})
	}
}

func TestDeploymentDeletionCrashChild(t *testing.T) {
	point := os.Getenv("DEPLOYMENT_DELETE_CRASH_POINT")
	if point == "" {
		return
	}
	setPublicationFaultPoint(func(at string) error {
		if at == point {
			os.Exit(86)
		}
		return nil
	})
	app, err := New(Config{DatabasePath: os.Getenv("DEPLOYMENT_DELETE_CRASH_DB"), OPAQUEKeyPath: os.Getenv("DEPLOYMENT_DELETE_CRASH_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	t.Fatalf("startup did not reach deletion crash point %q", point)
}

func TestDeploymentDeletionProcessCrashResumesByIdentity(t *testing.T) {
	for _, point := range []string{"before-deployment-remove", "after-deployment-removal"} {
		t.Run(point, func(t *testing.T) {
			base := t.TempDir()
			cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")}
			app, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			projectID, deploymentID := strings.Repeat("e", 32), strings.Repeat("f", 32)
			now := time.Now().UnixNano()
			live := app.deploymentPath(deploymentID)
			if err = os.Mkdir(live, 0755); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(live, "index.html"), []byte("published"), 0644); err != nil {
				t.Fatal(err)
			}
			liveID, err := identity(live)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "fixture", "https://example.test/repo.git", now); err != nil {
				t.Fatal(err)
			}
			commit := strings.Repeat("2", 40)
			if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,live_commit,live_dev,live_ino,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'deleting',?,?)`, deploymentID, projectID, "site", commit, commit, liveID.Dev, liveID.Ino, now, now); err != nil {
				t.Fatal(err)
			}
			if _, err = app.db.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at) VALUES(?,?,?,1,'succeeded','Published',?,?)`, strings.Repeat("1", 32), deploymentID, commit, now, now); err != nil {
				t.Fatal(err)
			}
			if _, err = app.db.Exec(`INSERT INTO deployment_deletions(deployment_id,status,created_at,updated_at) VALUES(?,'deleting',?,?)`, deploymentID, now, now); err != nil {
				t.Fatal(err)
			}
			if err = app.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestDeploymentDeletionCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOYMENT_DELETE_CRASH_POINT="+point, "DEPLOYMENT_DELETE_CRASH_DB="+cfg.DatabasePath, "DEPLOYMENT_DELETE_CRASH_KEY="+cfg.OPAQUEKeyPath)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("child did not crash at %s: %v: %s", point, err, output)
			}
			app, err = New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			if _, err = os.Stat(live); !os.IsNotExist(err) {
				t.Fatalf("publication remains after restart: %v", err)
			}
			var count int
			if err = app.db.QueryRow(`SELECT count(*) FROM deployments WHERE id=?`, deploymentID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("deployment not finalized: %d %v", count, err)
			}
			if err = app.db.QueryRow(`SELECT count(*) FROM archived_deployment_jobs WHERE deployment_id=?`, deploymentID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("audit job lost: %d %v", count, err)
			}
		})
	}
}

func TestDeletionInvalidatesPublicationPreparedBeforeIntent(t *testing.T) {
	for _, point := range []string{"after-stage-fsync", "after-exchange"} {
		t.Run(point, func(t *testing.T) { deletionRaceAt(t, point) })
	}
}

func deletionRaceAt(t *testing.T, pausePoint string) {
	t.Helper()
	base := t.TempDir()
	repoDir := filepath.Join(base, "source")
	commit := createSmartFixture(t, repoDir)
	app, err := New(Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key"), DeployTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	projectID, deploymentID := strings.Repeat("9", 32), strings.Repeat("8", 32)
	now := time.Now().UnixNano()
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "race", "https://example.test/repo.git", now); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(app.projects.repoPath(projectID), 0700); err != nil {
		t.Fatal(err)
	}
	if err = copyGitCache(repoDir, app.projects.repoPath(projectID), 64<<20); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,generation,status,created_at,updated_at) VALUES(?,?,?, ?,1,'queued',?,?)`, deploymentID, projectID, "race", commit, now, now); err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	setPublicationFaultPoint(func(point string) error {
		if point == pausePoint {
			once.Do(func() { close(reached) })
			<-release
		}
		return nil
	})
	defer func() { setPublicationFaultPoint(nil) }()
	jobID, err := app.enqueueDeployment(deploymentID, commit, 1)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not reach its prepared-stage boundary")
	}
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	token := []byte(strings.Repeat("r", 32))
	digest := sha256.Sum256(token)
	app.mu.Lock()
	app.sessions[digest] = sessionState{expires: time.Now().Add(time.Hour), generation: app.credentialGeneration}
	app.mu.Unlock()
	cookie := base64.RawURLEncoding.EncodeToString(token)
	responseCh := make(chan *http.Response, 1)
	errorCh := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/deployments/"+deploymentID+"/delete", strings.NewReader(`{"confirm":true}`))
		req.Header.Set("Origin", server.URL)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		response, e := server.Client().Do(req)
		if e != nil {
			errorCh <- e
			return
		}
		responseCh <- response
	}()
	deadline := time.Now().Add(5 * time.Second)
	if pausePoint == "after-exchange" {
		// The publication transaction has already performed its namespace switch
		// and still holds SQLite's write lock. Let it commit before deletion can
		// record its tombstone; deletion must then remove the just-published tree.
		close(release)
		goto deletionMarked
	}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/deployments", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		res, e := server.Client().Do(req)
		if e == nil {
			var values []deployment
			if json.NewDecoder(res.Body).Decode(&values) == nil {
				for _, d := range values {
					if d.ID == deploymentID && d.Status == "deleting" {
						res.Body.Close()
						close(release)
						goto deletionMarked
					}
				}
			}
			res.Body.Close()
		}
		time.Sleep(time.Millisecond * 10)
	}
	t.Fatal("deletion was not visible before publication resumed")
deletionMarked:
	select {
	case e := <-errorCh:
		t.Fatal(e)
	case response := <-responseCh:
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("delete status=%d: %s", response.StatusCode, responseText(response))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deletion did not finish")
	}
	if _, err = os.Stat(app.deploymentPath(deploymentID)); !os.IsNotExist(err) {
		t.Fatalf("publication was recreated or retained after deletion: %v", err)
	}
	var status string
	if err = app.db.QueryRow(`SELECT status FROM archived_deployment_jobs WHERE id=?`, jobID).Scan(&status); err != nil || status != "superseded" {
		if pausePoint != "after-exchange" || err != nil || status != "succeeded" {
			t.Fatalf("prepared job status=%q err=%v", status, err)
		}
	}
}
