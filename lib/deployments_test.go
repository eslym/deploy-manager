package lib

import (
	"context"
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
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestExportPublishesDotfilesAndRejectsTrackedSymlinks(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "repo")
	commitID := createSmartFixture(t, repoDir)
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".well-known"), []byte("visible"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add(".well-known"); err != nil {
		t.Fatal(err)
	}
	commitID = commitFixtureFile(t, repoDir, worktree, "dotfile", "feature").String()
	commit, err := repo.CommitObject(plumbing.NewHash(commitID))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "export")
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	if err := exportTrackedTree(context.Background(), repo, commit, out, exportLimits{files: 10, bytes: 1024, timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, ".well-known")); err != nil || string(b) != "visible" {
		t.Fatalf("tracked dotfile = %q, err=%v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(out, ".git")); !os.IsNotExist(err) {
		t.Fatalf("Git metadata appeared in export: %v", err)
	}

	if err := os.Symlink("README.md", filepath.Join(repoDir, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("linked"); err != nil {
		t.Fatal(err)
	}
	linkedID, err := worktree.Commit("tracked symlink", &git.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.test", When: time.Now().UTC()}})
	if err != nil {
		t.Fatal(err)
	}
	linked, err := repo.CommitObject(linkedID)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "export")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := exportTrackedTree(context.Background(), repo, linked, other, exportLimits{files: 10, bytes: 1024, timeout: time.Second}); err == nil {
		t.Fatal("tracked symlink was accepted")
	}
}

func TestCommitTargetRequiresFullSHA1ObjectID(t *testing.T) {
	for _, target := range []string{"abc", "", "012345678901234567890123456789012345678Z", "012345678901234567890123456789012345678901"} {
		if validCommitTarget(target) {
			t.Errorf("accepted non-full or invalid commit target %q", target)
		}
	}
	if !validCommitTarget("0123456789012345678901234567890123456789") {
		t.Fatal("rejected full SHA-1 commit id")
	}
}

func TestExportFailsClosedOnCommitTrackedLFSSelection(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "repo")
	createSmartFixture(t, repoDir)
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("feature/search")}); err != nil {
		t.Fatal(err)
	}
	attrs := "*.dat filter=lfs\n"
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:0123456789012345678901234567890123456789012345678901234567890123\nsize 8\n"
	if err := os.WriteFile(filepath.Join(repoDir, ".gitattributes"), []byte(attrs), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "asset.dat"), []byte(pointer), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add(".gitattributes"); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("asset.dat"); err != nil {
		t.Fatal(err)
	}
	commitID, err := worktree.Commit("tracked LFS pointer", &git.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.test", When: time.Now().UTC()}})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(commitID)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "export")
	if err = os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	err = exportTrackedTree(context.Background(), repo, commit, out, exportLimits{files: 20, bytes: 1024, timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "LFS") {
		t.Fatalf("selected LFS pointer export error=%v", err)
	}
}

func TestRestartReconcilesCompletedFirstDirectorySwitchByIdentity(t *testing.T) {
	for _, faultAt := range []string{"before-intent", "after-intent", "after-exchange", "after-directory-fsync", "before-state", "after-state"} {
		t.Run(faultAt, func(t *testing.T) {
			base := t.TempDir()
			repoDir := filepath.Join(base, "fixture")
			commitID := createSmartFixture(t, repoDir)
			cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key"), DeployTimeout: 5 * time.Second}
			app, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			projectID := strings.Repeat("a", 32)
			if _, err := app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at,last_fetched,warning) VALUES(?,?,?,?,?,'')`, projectID, "fixture", "https://example.test/repo.git", time.Now().UnixNano(), time.Now().UnixNano()); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(app.projects.repoPath(projectID), 0700); err != nil {
				t.Fatal(err)
			}
			if err := copyGitCache(repoDir, app.projects.repoPath(projectID), 64<<20); err != nil {
				t.Fatal(err)
			}
			cookie := installAPITestSession(t, app)
			server := httptest.NewTLSServer(app.Handler())
			client := server.Client()
			previousFault := publicationFaultPoint
			publicationFaultPoint = func(point string) error {
				if point == faultAt {
					return errors.New("simulated process interruption")
				}
				return nil
			}
			response := projectPost(t, client, server.URL, cookie, "/api/deployments", `{"projectId":"`+projectID+`","name":"recovery","commit":"`+commitID+`"}`)
			if response.StatusCode != http.StatusCreated {
				t.Fatalf("create status %d: %s", response.StatusCode, responseText(response))
			}
			var created struct {
				ID string `json:"id"`
			}
			if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			deadline := time.Now().Add(5 * time.Second)
			for {
				var status string
				_ = app.db.QueryRow(`SELECT status FROM deployment_jobs WHERE deployment_id=?`, created.ID).Scan(&status)
				if status == "failed" || status == "succeeded" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("faulted job did not settle, status=%q", status)
				}
				time.Sleep(10 * time.Millisecond)
			}
			publicationFaultPoint = previousFault
			path := app.deploymentPath(created.ID)
			_, beforeRestartErr := os.Stat(path)
			server.Close()
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			app, err = New(cfg)
			if err != nil {
				t.Fatalf("reconcile after switched-first-publication crash: %v", err)
			}
			defer app.Close()
			defer func() { publicationFaultPoint = previousFault }()
			var liveCommit, status, jobStatus string
			if err := app.db.QueryRow(`SELECT live_commit,status FROM deployments WHERE id=?`, created.ID).Scan(&liveCommit, &status); err != nil {
				t.Fatal(err)
			}
			if err := app.db.QueryRow(`SELECT status FROM deployment_jobs WHERE deployment_id=?`, created.ID).Scan(&jobStatus); err != nil {
				t.Fatal(err)
			}
			shouldBeLive := faultAt == "after-exchange" || faultAt == "after-directory-fsync" || faultAt == "before-state" || faultAt == "after-state"
			wantJobStatus := "failed"
			if faultAt == "after-state" {
				wantJobStatus = "succeeded"
			}
			if shouldBeLive && (liveCommit != commitID || status != "live") || !shouldBeLive && (liveCommit != "" || status != "failed") || jobStatus != wantJobStatus {
				t.Fatalf("recovered live=%q deployment=%q interrupted job=%q", liveCommit, status, jobStatus)
			}
			if shouldBeLive {
				if contents, err := os.ReadFile(filepath.Join(path, "README.md")); err != nil || string(contents) != "searchable feature\n" {
					t.Fatalf("recovered files=%q err=%v", contents, err)
				}
			} else if beforeRestartErr == nil {
				t.Fatal("publication before the switch unexpectedly created live files")
			}
			var intents int
			if err := app.db.QueryRow(`SELECT count(*) FROM publication_intents WHERE deployment_id=?`, created.ID).Scan(&intents); err != nil || intents != 0 {
				t.Fatalf("remaining intents=%d err=%v", intents, err)
			}
		})
	}
}
