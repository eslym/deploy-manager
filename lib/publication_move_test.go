package lib

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
)

// Exercise both sides of the durable rename boundary through the authenticated
// API; a new daemon must observe the inode rather than replaying the rename.
func TestPublicationMoveReconcilesInterruptedRename(t *testing.T) {
	for _, phase := range []string{"before-move-rename", "after-move-rename", "before-move-directory-sync", "after-move-sync", "after-move-verify", "before-move-state"} {
		t.Run(phase, func(t *testing.T) {
			base := t.TempDir()
			cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")}
			app, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			project, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
			now := time.Now().UnixNano()
			oldPath, newPath := filepath.Join(app.cfg.DeploymentRoot, id), filepath.Join(app.cfg.DeploymentRoot, "moved-site")
			if err = os.Mkdir(oldPath, 0755); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(oldPath, "index.html"), []byte("last valid release"), 0644); err != nil {
				t.Fatal(err)
			}
			oldID, err := identity(oldPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, project, "fixture", "https://example.test/repo.git", now); err != nil {
				t.Fatal(err)
			}
			if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,live_commit,live_dev,live_ino,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'live',?,?)`, id, project, "legacy", strings.Repeat("c", 40), strings.Repeat("c", 40), oldID.Dev, oldID.Ino, now, now); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewTLSServer(app.Handler())
			cookie := installAPITestSession(t, app)
			previous := setPublicationFaultPoint(func(point string) error {
				if point == phase {
					return errors.New("injected move interruption")
				}
				return nil
			})
			response := projectPost(t, server.Client(), server.URL, cookie, "/api/deployments/"+id+"/move", `{"publicationPath":"moved-site","expectedPublicationPath":"`+oldPath+`","confirm":true}`)
			setPublicationFaultPoint(previous)
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("fault not reached: %d %s", response.StatusCode, responseText(response))
			}
			response.Body.Close()
			// While the move intent is unresolved, the old persisted name is
			// not necessarily a usable document root (rename may have happened).
			request, e := http.NewRequest(http.MethodGet, server.URL+"/api/deployments", nil)
			if e != nil {
				t.Fatal(e)
			}
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
			response, e = server.Client().Do(request)
			if e != nil {
				t.Fatal(e)
			}
			if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(responseText(response), "move") {
				t.Fatalf("unresolved move exposed a document root: HTTP %d", response.StatusCode)
			}
			var name string
			if err = app.db.QueryRow(`SELECT publication_name FROM deployments WHERE id=?`, id).Scan(&name); err != nil || name != id {
				t.Fatalf("claimed incomplete move: %q %v", name, err)
			}
			server.Close()
			if err = app.Close(); err != nil {
				t.Fatal(err)
			}
			app, err = New(cfg)
			if err != nil {
				t.Fatalf("restart did not reconcile move: %v", err)
			}
			defer app.Close()
			wantName, wantPath := "moved-site", newPath
			if phase == "before-move-rename" {
				wantName, wantPath = id, oldPath
			}
			if err = app.db.QueryRow(`SELECT publication_name FROM deployments WHERE id=?`, id).Scan(&name); err != nil || name != wantName {
				t.Fatalf("reconciled name=%q, want %q: %v", name, wantName, err)
			}
			if got, e := identity(wantPath); e != nil || got != oldID {
				t.Fatalf("recovery lost the live inode: %v %v", got, e)
			}
			if data, e := os.ReadFile(filepath.Join(wantPath, "index.html")); e != nil || string(data) != "last valid release" {
				t.Fatalf("recovery lost content: %q %v", data, e)
			}
			var intents int
			if err = app.db.QueryRow(`SELECT count(*) FROM publication_moves`).Scan(&intents); err != nil || intents != 0 {
				t.Fatalf("recovery retained move intent: %d %v", intents, err)
			}
		})
	}
}

func TestPublicationMoveRefusesAmbiguousRestartAndOccupiedDestination(t *testing.T) {
	base := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	project, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
	now := time.Now().UnixNano()
	oldPath, newPath := filepath.Join(app.cfg.DeploymentRoot, id), filepath.Join(app.cfg.DeploymentRoot, "occupied-site")
	if err = os.Mkdir(oldPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(newPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(newPath, "keep"), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	oldID, err := identity(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, project, "fixture", "https://example.test/repo.git", now); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,live_commit,live_dev,live_ino,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'live',?,?)`, id, project, "legacy", strings.Repeat("c", 40), strings.Repeat("c", 40), oldID.Dev, oldID.Ino, now, now); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(app.Handler())
	cookie := installAPITestSession(t, app)
	response := projectPost(t, server.Client(), server.URL, cookie, "/api/deployments/"+id+"/move", `{"publicationPath":"occupied-site","expectedPublicationPath":"`+oldPath+`","confirm":true}`)
	if response.StatusCode != 409 {
		t.Fatalf("occupied path accepted: %d %s", response.StatusCode, responseText(response))
	}
	response.Body.Close()
	if data, e := os.ReadFile(filepath.Join(newPath, "keep")); e != nil || string(data) != "foreign" {
		t.Fatalf("occupied directory changed: %q %v", data, e)
	}
	link := filepath.Join(app.cfg.DeploymentRoot, "symlink-site")
	if err := os.Symlink(oldPath, link); err != nil {
		t.Fatal(err)
	}
	response = projectPost(t, server.Client(), server.URL, cookie, "/api/deployments/"+id+"/move", `{"publicationPath":"symlink-site","expectedPublicationPath":"`+oldPath+`","confirm":true}`)
	if response.StatusCode != 409 {
		t.Fatalf("symlink destination accepted: %d %s", response.StatusCode, responseText(response))
	}
	response.Body.Close()
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO publication_moves(deployment_id,source_name,destination_name,old_dev,old_ino,created_at) VALUES(?,?,?,?,?,?)`, id, id, "occupied-site", oldID.Dev, oldID.Ino, now); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,publication_name,created_at,updated_at) VALUES(?,?,'other','',?, ?,?)`, strings.Repeat("d", 32), project, "occupied-site", now, now); err == nil {
		t.Fatal("new deployment claimed a move-reserved destination")
	}
	server.Close()
	if err = app.Close(); err != nil {
		t.Fatal(err)
	}
	if restarted, e := New(cfg); e == nil {
		restarted.Close()
		t.Fatal("ambiguous occupied move started normally")
	}
	if data, e := os.ReadFile(filepath.Join(newPath, "keep")); e != nil || string(data) != "foreign" {
		t.Fatalf("failed recovery removed occupied directory: %q %v", data, e)
	}
	if got, e := identity(oldPath); e != nil || got != oldID {
		t.Fatalf("failed recovery changed live release: %v %v", got, e)
	}
}

func TestPublicationMoveSerializesWithQueuedDeployment(t *testing.T) {
	base := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	project, id := strings.Repeat("a", 32), strings.Repeat("b", 32)
	oldPath := filepath.Join(app.cfg.DeploymentRoot, id)
	if err := os.Mkdir(oldPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldPath, "index.html"), []byte("live"), 0644); err != nil {
		t.Fatal(err)
	}
	owned, err := identity(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	if _, err := app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, project, "fixture", "https://example.test/repo.git", now); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("c", 40)
	if _, err := app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,live_commit,live_dev,live_ino,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'live',?,?)`, id, project, "legacy", commit, commit, owned.Dev, owned.Ino, now, now); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	cookie := installAPITestSession(t, app)
	unlock, err := app.lockDeployment(id)
	if err != nil {
		t.Fatal(err)
	}
	job, err := app.enqueueDeployment(id, commit, 1)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	moveDone := make(chan int, 1)
	go func() {
		response := projectPost(t, server.Client(), server.URL, cookie, "/api/deployments/"+id+"/move", `{"publicationPath":"moved-site","expectedPublicationPath":"`+oldPath+`","confirm":true}`)
		moveDone <- response.StatusCode
		response.Body.Close()
	}()
	unlock()
	if status := <-moveDone; status != http.StatusOK {
		t.Fatalf("concurrent move status=%d", status)
	}
	waitForDeploymentJob(t, server.Client(), server.URL, cookie, id, job, "noop")
	newPath := filepath.Join(app.cfg.DeploymentRoot, "moved-site")
	if got, e := identity(newPath); e != nil || got != owned {
		t.Fatalf("serialized move lost live directory: %v %v", got, e)
	}
	if _, e := os.Lstat(oldPath); !os.IsNotExist(e) {
		t.Fatalf("serialized move left old path: %v", e)
	}
}

func TestMoveLegacyPublicationPreservesReleaseAndFutureDeletion(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "source")
	commit := createSmartFixture(t, repo)
	fixture, err := git.PlainOpen(repo)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := fixture.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	secondCommit := commitFixtureFile(t, repo, worktree, "moved", "new release").String()
	app, err := New(Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key"), DeployTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	project := strings.Repeat("a", 32)
	if _, err := app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, project, "fixture", "https://example.test/repo.git", time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(app.projects.repoPath(project), 0700); err != nil {
		t.Fatal(err)
	}
	if err := copyGitCache(repo, app.projects.repoPath(project), 64<<20); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	cookie := installAPITestSession(t, app)
	created := projectPost(t, server.Client(), server.URL, cookie, "/api/deployments", `{"projectId":"`+project+`","name":"legacy","commit":"`+commit+`"}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create: %s", responseText(created))
	}
	var createdOutcome struct {
		Data struct {
			ID    string `json:"id"`
			JobID string `json:"jobId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(created.Body).Decode(&createdOutcome); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	deployment := createdOutcome.Data
	waitForDeploymentJob(t, server.Client(), server.URL, cookie, deployment.ID, deployment.JobID, "succeeded")
	old := filepath.Join(app.cfg.DeploymentRoot, deployment.ID)
	newPath := filepath.Join(app.cfg.DeploymentRoot, "readable-site")
	oldID, err := identity(old)
	if err != nil {
		t.Fatal(err)
	}
	move := func(path string, confirm bool) *http.Response {
		t.Helper()
		flag := "false"
		if confirm {
			flag = "true"
		}
		return projectPost(t, server.Client(), server.URL, cookie, "/api/deployments/"+deployment.ID+"/move", `{"publicationPath":"`+path+`","expectedPublicationPath":"`+old+`","confirm":`+flag+`}`)
	}
	for _, name := range []string{"../escape", "readable-site"} {
		response := move(name, false)
		if response.StatusCode < 400 {
			t.Fatalf("unconfirmed move %q succeeded", name)
		}
		response.Body.Close()
	}
	for _, name := range []string{"../escape", "sub/site", ".deployment-lock-" + deployment.ID, strings.Repeat("f", 32)} {
		response := move(name, true)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("unsafe move %q status=%d", name, response.StatusCode)
		}
		response.Body.Close()
	}
	response := move("readable-site", true)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("move status %d: %s", response.StatusCode, responseText(response))
	}
	var moved map[string]string
	if err := json.NewDecoder(response.Body).Decode(&moved); err != nil {
		t.Fatalf("decode legacy move response: %v", err)
	}
	if moved["publicationPath"] != newPath || moved["code"] != "" {
		t.Fatalf("move route unexpectedly changed contract: %#v", moved)
	}
	response.Body.Close()
	if got, err := identity(newPath); err != nil || got != oldID {
		t.Fatalf("move changed release identity: %v %v", got, err)
	}
	if data, err := os.ReadFile(filepath.Join(newPath, "README.md")); err != nil || string(data) != "searchable feature\n" {
		t.Fatalf("move changed content: %q %v", data, err)
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("old location still published: %v", err)
	}
	response = move("second-site", true)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale reviewed path moved again: %d %s", response.StatusCode, responseText(response))
	}
	response.Body.Close()
	if got, err := identity(newPath); err != nil || got != oldID {
		t.Fatalf("stale confirmation changed publication: %v %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(app.cfg.DeploymentRoot, "second-site")); !os.IsNotExist(err) {
		t.Fatalf("stale confirmation created another document root: %v", err)
	}
	response = move("readable-site", true)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("no-op move status: %d", response.StatusCode)
	}
	response.Body.Close()
	if _, err := app.db.Exec(`UPDATE deployments SET commit_id=?,generation=generation+1 WHERE id=?`, secondCommit, deployment.ID); err != nil {
		t.Fatal(err)
	}
	job, err := app.enqueueDeployment(deployment.ID, secondCommit, 2)
	if err != nil {
		t.Fatal(err)
	}
	waitForDeploymentJob(t, server.Client(), server.URL, cookie, deployment.ID, job, "succeeded")
	if data, err := os.ReadFile(filepath.Join(newPath, "README.md")); err != nil || string(data) != "new release\n" {
		t.Fatalf("later deployment did not use moved path: %q %v", data, err)
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("later deployment recreated old path: %v", err)
	}
	response = projectPost(t, server.Client(), server.URL, cookie, "/api/deployments/"+deployment.ID+"/delete", `{"confirm":true}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d %s", response.StatusCode, responseText(response))
	}
	var deleted struct {
		Code string `json:"code"`
		Data any    `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&deleted); err != nil || deleted.Code != "deployment.deleted" || deleted.Data != nil {
		t.Fatalf("unexpected deployment-delete outcome: %#v %v", deleted, err)
	}
	response.Body.Close()
	if _, err := os.Lstat(newPath); !os.IsNotExist(err) {
		t.Fatalf("moved publication survived deletion: %v", err)
	}
}
