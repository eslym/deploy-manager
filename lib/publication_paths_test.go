package lib

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
)

func TestPublicationPathStaysOwnedAcrossFirstReleaseReplacementAndDeletion(t *testing.T) {
	base := t.TempDir()
	repoDir := filepath.Join(base, "source")
	firstCommit := createSmartFixture(t, repoDir)
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	secondCommit := commitFixtureFile(t, repoDir, worktree, "replacement", "second release").String()
	app, err := New(Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key"), DeployTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	projectID := strings.Repeat("4", 32)
	now := time.Now().UnixNano()
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "fixture", "https://example.test/repo.git", now); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(app.projects.repoPath(projectID), 0700); err != nil {
		t.Fatal(err)
	}
	if err = copyGitCache(repoDir, app.projects.repoPath(projectID), 64<<20); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	cookie := installAPITestSession(t, app)
	response := projectPost(t, server.Client(), server.URL, cookie, "/api/deployments", `{"projectId":"`+projectID+`","name":"site","commit":"`+firstCommit+`"}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d: %s", response.StatusCode, responseText(response))
	}
	var created struct {
		ID, JobID string
	}
	if err = json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	path := filepath.Join(app.cfg.DeploymentRoot, created.ID)
	assertPublication := func(jobID, contents string) dirIdentity {
		t.Helper()
		waitForDeploymentJob(t, server.Client(), server.URL, cookie, created.ID, jobID, "succeeded")
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/deployments", nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		res, e := server.Client().Do(request)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		var listed []deployment
		if e = json.NewDecoder(res.Body).Decode(&listed); e != nil {
			t.Fatal(e)
		}
		if len(listed) != 1 || listed[0].Publication != path {
			t.Fatalf("publication path changed: %+v; want %s", listed, path)
		}
		if data, e := os.ReadFile(filepath.Join(path, "README.md")); e != nil || string(data) != contents {
			t.Fatalf("live contents=%q, err=%v", data, e)
		}
		id, e := identity(path)
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	firstIdentity := assertPublication(created.JobID, "searchable feature\n")
	if _, err = app.db.Exec(`UPDATE deployments SET commit_id=?,generation=2,status='queued' WHERE id=?`, secondCommit, created.ID); err != nil {
		t.Fatal(err)
	}
	jobID, err := app.enqueueDeployment(created.ID, secondCommit, 2)
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity := assertPublication(jobID, "second release\n")
	if secondIdentity == firstIdentity {
		t.Fatal("replacement did not exchange the live directory")
	}
	response = projectPost(t, server.Client(), server.URL, cookie, "/api/deployments/"+created.ID+"/delete", `{"confirm":true}`)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d", response.StatusCode)
	}
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("published path survived deletion: %v", err)
	}
}

// A real directory at the right name is not necessarily the release owned by
// this deployment. Never exchange it away on the strength of its name alone.
func TestReplacementRefusesUnrecordedLiveDirectoryIdentity(t *testing.T) {
	base := t.TempDir()
	repoDir := filepath.Join(base, "source")
	commit := createSmartFixture(t, repoDir)
	app, err := New(Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	projectID, deploymentID, jobID := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	now := time.Now().UnixNano()
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "fixture", "https://example.test/repo.git", now); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(app.projects.repoPath(projectID), 0700); err != nil {
		t.Fatal(err)
	}
	if err = copyGitCache(repoDir, app.projects.repoPath(projectID), 64<<20); err != nil {
		t.Fatal(err)
	}
	live := app.deploymentPath(deploymentID)
	if err = os.Mkdir(live, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(live, "index.html"), []byte("recorded release"), 0644); err != nil {
		t.Fatal(err)
	}
	recorded, err := identity(live)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,generation,live_commit,live_dev,live_ino,status,created_at,updated_at) VALUES(?,?,?, ?,1,?,?,?,'live',?,?)`, deploymentID, projectID, "site", commit, strings.Repeat("0", 40), recorded.Dev, recorded.Ino, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at) VALUES(?,?,?,1,'running','',?,?)`, jobID, deploymentID, commit, now, now); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(live, live+".preserved"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(live, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(live, "foreign.html"), []byte("do not remove"), 0644); err != nil {
		t.Fatal(err)
	}
	unexpected, err := identity(live)
	if err != nil {
		t.Fatal(err)
	}
	err = app.publish(context.Background(), jobID, deployment{ID: deploymentID, ProjectID: projectID, Commit: commit}, 1)
	if err == nil {
		t.Fatal("replacement exchanged an unrecorded live directory")
	}
	if actual, e := identity(live); e != nil || actual != unexpected {
		t.Fatalf("unrecorded live directory was changed: %+v, %v", actual, e)
	}
	if contents, e := os.ReadFile(filepath.Join(live, "foreign.html")); e != nil || string(contents) != "do not remove" {
		t.Fatalf("unrecorded contents were removed: %q, %v", contents, e)
	}
	var intents int
	if err = app.db.QueryRow(`SELECT count(*) FROM publication_intents WHERE deployment_id=?`, deploymentID).Scan(&intents); err != nil || intents != 0 {
		t.Fatalf("unsafe replacement recorded intent: %d, %v", intents, err)
	}
}

func TestDeletionRefusesSymlinkedRecordedStage(t *testing.T) {
	base := t.TempDir()
	app, err := New(Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	projectID, deploymentID, jobID := strings.Repeat("d", 32), strings.Repeat("e", 32), strings.Repeat("f", 32)
	now := time.Now().UnixNano()
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "fixture", "https://example.test/repo.git", now); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,status,created_at,updated_at) VALUES(?,?,?,'','deleting',?,?)`, deploymentID, projectID, "site", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployment_deletions(deployment_id,status,created_at,updated_at) VALUES(?,'deleting',?,?)`, deploymentID, now, now); err != nil {
		t.Fatal(err)
	}
	stage := app.stagePath(deploymentID, jobID)
	if err = os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	want, err := identity(stage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployment_removal_paths(deployment_id,path_name,dev,ino) VALUES(?,?,?,?)`, deploymentID, filepath.Base(stage), want.Dev, want.Ino); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(stage); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err = os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(outside, "keep"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, stage); err != nil {
		t.Fatal(err)
	}
	if err = app.finishDeploymentDeletion(deploymentID); err == nil {
		t.Fatal("deletion accepted a symlinked stage")
	}
	if contents, e := os.ReadFile(filepath.Join(outside, "keep")); e != nil || string(contents) != "safe" {
		t.Fatalf("outside directory changed: %q, %v", contents, e)
	}
	if info, e := os.Lstat(stage); e != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("unsafe stage was removed: %v, %v", info, e)
	}
}

func TestFirstPublicationRefusesOccupiedDestination(t *testing.T) {
	base := t.TempDir()
	repoDir := filepath.Join(base, "source")
	commit := createSmartFixture(t, repoDir)
	app, err := New(Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	projectID, deploymentID, jobID := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)
	now := time.Now().UnixNano()
	if _, err = app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at) VALUES(?,?,?,?)`, projectID, "fixture", "https://example.test/repo.git", now); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(app.projects.repoPath(projectID), 0700); err != nil {
		t.Fatal(err)
	}
	if err = copyGitCache(repoDir, app.projects.repoPath(projectID), 64<<20); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,generation,status,created_at,updated_at) VALUES(?,?,?, ?,1,'queued',?,?)`, deploymentID, projectID, "site", commit, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err = app.db.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at) VALUES(?,?,?,1,'running','',?,?)`, jobID, deploymentID, commit, now, now); err != nil {
		t.Fatal(err)
	}
	live := app.deploymentPath(deploymentID)
	if err = os.Mkdir(live, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(live, "keep"), []byte("unowned"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = app.publish(context.Background(), jobID, deployment{ID: deploymentID, ProjectID: projectID, Commit: commit}, 1); err == nil {
		t.Fatal("first publication accepted an occupied live path")
	}
	if contents, e := os.ReadFile(filepath.Join(live, "keep")); e != nil || string(contents) != "unowned" {
		t.Fatalf("first publication changed an unowned destination: %q, %v", contents, e)
	}
}
