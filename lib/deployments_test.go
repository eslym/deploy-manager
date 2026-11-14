package lib

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
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

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	sqlite3 "github.com/mattn/go-sqlite3"
)

func TestNewFailsWhenAcceptedWebhookQueueCannotBeRestoredThenRestartsWork(t *testing.T) {
	base := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key"), DeployTimeout: time.Second}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	projectID := strings.Repeat("a", 32)
	deploymentID := strings.Repeat("b", 32)
	jobID := strings.Repeat("c", 32)
	now := time.Now().UnixNano()
	if _, err := app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at,last_fetched,warning) VALUES(?,?,?,?,?,'')`, projectID, "accepted-work", "https://example.test/repo.git", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,target_kind,target_name,generation,status,created_at,updated_at) VALUES(?,?,?,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','branch','main',1,'queued',?,?)`, deploymentID, projectID, "production", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at,durable) VALUES(?,?,? ,1,'queued','Verified GitHub branch update',?,?,1)`, jobID, deploymentID, strings.Repeat("a", 40), now, now); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	faultConfig := cfg
	faultConfig.beforeRestoreDeploymentJobs = func(db *sql.DB) error {
		db.SetMaxOpenConns(1)
		conn, err := db.Conn(context.Background())
		if err != nil {
			return err
		}
		defer conn.Close()
		return conn.Raw(func(driverConn any) error {
			conn, ok := driverConn.(*sqlite3.SQLiteConn)
			if !ok {
				return fmt.Errorf("unexpected SQLite connection %T", driverConn)
			}
			conn.RegisterAuthorizer(func(op int, arg1, _, _ string) int {
				if op == sqlite3.SQLITE_READ && arg1 == "deployment_jobs" {
					return sqlite3.SQLITE_DENY
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		})
	}
	if failed, err := New(faultConfig); err == nil {
		_ = failed.Close()
		t.Fatal("startup succeeded while restoration of accepted work was denied")
	}

	app, err = New(cfg)
	if err != nil {
		t.Fatalf("restart with readable queue failed: %v", err)
	}
	defer app.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := app.db.QueryRow(`SELECT status FROM deployment_jobs WHERE id=?`, jobID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "failed" || status == "superseded" || status == "succeeded" {
			var count int
			if err := app.db.QueryRow(`SELECT count(*) FROM deployment_jobs WHERE id=? AND durable=1`, jobID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("restored accepted job count = %d, want exactly one", count)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("durable accepted work remained queued after a successful restart")
}

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

// TestPublicationCrashChild is run in a separate Go test process by
// TestPublicationProcessCrashesReconcileDirectoryIdentity. os.Exit deliberately
// bypasses App.Close and deferred cleanup, unlike a returned injected error.
func TestPublicationCrashChild(t *testing.T) {
	point := os.Getenv("PUBLICATION_CRASH_POINT")
	if point == "" {
		return
	}
	cfg := Config{DatabasePath: os.Getenv("PUBLICATION_CRASH_DB"), OPAQUEKeyPath: os.Getenv("PUBLICATION_CRASH_KEY"), DeployGracePeriod: time.Nanosecond}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	setPublicationFaultPoint(func(at string) error {
		if at == point {
			os.Exit(86)
		}
		return nil
	})
	jobID, err := app.enqueueDeployment(os.Getenv("PUBLICATION_CRASH_DEPLOYMENT"), os.Getenv("PUBLICATION_CRASH_COMMIT"), 1)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := app.db.QueryRow(`SELECT status FROM deployment_jobs WHERE id=?`, jobID).Scan(&status); err == nil && status != "queued" && status != "running" {
			if point == "after-cleanup" && status == "succeeded" {
				time.Sleep(2 * time.Millisecond)
				if err := app.cleanupPublications(true); err != nil {
					t.Fatal(err)
				}
			}
			t.Fatalf("publication completed with status %q without reaching crash point %s", status, point)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("publication did not reach crash point %s", point)
}

func TestPublicationProcessCrashesReconcileDirectoryIdentity(t *testing.T) {
	points := []string{"after-stage-fsync", "after-intent", "after-exchange", "after-directory-fsync", "before-state", "after-state", "after-cleanup"}
	for _, hadOld := range []bool{false, true} {
		for _, point := range points {
			if point == "after-cleanup" && !hadOld {
				continue
			}
			t.Run(fmt.Sprintf("old-%t/%s", hadOld, point), func(t *testing.T) {
				base := t.TempDir()
				repoDir := filepath.Join(base, "fixture")
				commitID := createSmartFixture(t, repoDir)
				cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key"), DeployTimeout: 5 * time.Second}
				app, err := New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				deploymentID := strings.Repeat("d", 32)
				projectID := strings.Repeat("e", 32)
				now := time.Now().UnixNano()
				if _, err := app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at,last_fetched,warning) VALUES(?,?,?,?,?,'')`, projectID, "fixture", "https://example.test/repo.git", now, now); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(app.projects.repoPath(projectID), 0700); err != nil {
					t.Fatal(err)
				}
				if err := copyGitCache(repoDir, app.projects.repoPath(projectID), 64<<20); err != nil {
					t.Fatal(err)
				}
				liveCommit := ""
				liveDev, liveIno := uint64(0), uint64(0)
				if hadOld {
					if err := os.MkdirAll(app.deploymentPath(deploymentID), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(app.deploymentPath(deploymentID), "old.html"), []byte("old-live"), 0644); err != nil {
						t.Fatal(err)
					}
					oldID, err := identity(app.deploymentPath(deploymentID))
					if err != nil {
						t.Fatal(err)
					}
					liveCommit, liveDev, liveIno = strings.Repeat("0", 40), oldID.Dev, oldID.Ino
				}
				if _, err := app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,generation,live_commit,live_dev,live_ino,status,created_at,updated_at) VALUES(?,?,?, ?,1,?,?,?,'live',?,?)`, deploymentID, projectID, "crash-test", commitID, liveCommit, liveDev, liveIno, now, now); err != nil {
					t.Fatal(err)
				}
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}

				cmd := exec.Command(os.Args[0], "-test.run=^TestPublicationCrashChild$")
				cmd.Env = append(os.Environ(), "PUBLICATION_CRASH_POINT="+point, "PUBLICATION_CRASH_DB="+cfg.DatabasePath, "PUBLICATION_CRASH_KEY="+cfg.OPAQUEKeyPath, "PUBLICATION_CRASH_DEPLOYMENT="+deploymentID, "PUBLICATION_CRASH_COMMIT="+commitID)
				output, err := cmd.CombinedOutput()
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
					t.Fatalf("child did not exit at crash point %q (err=%v): %s", point, err, output)
				}

				app, err = New(cfg)
				if err != nil {
					t.Fatalf("restart reconciliation at %s: %v", point, err)
				}
				path := app.deploymentPath(deploymentID)
				liveID, liveErr := identity(path)
				shouldSwitch := point == "after-exchange" || point == "after-directory-fsync" || point == "before-state" || point == "after-state" || point == "after-cleanup"
				if shouldSwitch {
					if liveErr != nil {
						t.Fatalf("new publication missing after %s: %v", point, liveErr)
					}
					if b, e := os.ReadFile(filepath.Join(path, "README.md")); e != nil || string(b) != "searchable feature\n" {
						t.Fatalf("new live content after %s = %q, %v", point, b, e)
					}
				} else if hadOld {
					if liveErr != nil || liveID != (dirIdentity{Dev: liveDev, Ino: liveIno}) {
						t.Fatalf("old publication changed before switch at %s: id=%+v err=%v", point, liveID, liveErr)
					}
				} else if !os.IsNotExist(liveErr) {
					t.Fatalf("first publication became visible before switch at %s: %v", point, liveErr)
				}
				firstIdentity := liveID
				if shouldSwitch {
					firstIdentity = liveID
				}
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
				app, err = New(cfg)
				if err != nil {
					t.Fatalf("second restart reconciliation at %s: %v", point, err)
				}
				defer app.Close()
				if shouldSwitch {
					secondIdentity, e := identity(path)
					if e != nil || secondIdentity != firstIdentity {
						t.Fatalf("restart repeated/lost switch at %s: first=%+v second=%+v err=%v", point, firstIdentity, secondIdentity, e)
					}
				}
				var intents int
				if err := app.db.QueryRow(`SELECT count(*) FROM publication_intents WHERE deployment_id=?`, deploymentID).Scan(&intents); err != nil || intents != 0 {
					t.Fatalf("intent remains after reconciliation: %d, %v", intents, err)
				}
				var jobStatus string
				if err := app.db.QueryRow(`SELECT status FROM deployment_jobs WHERE deployment_id=? ORDER BY created_at DESC LIMIT 1`, deploymentID).Scan(&jobStatus); err != nil {
					t.Fatal(err)
				}
				wantJobStatus := "failed"
				if point == "after-state" || point == "after-cleanup" {
					wantJobStatus = "succeeded"
				}
				if jobStatus != wantJobStatus {
					t.Fatalf("interrupted job status=%q, want %q", jobStatus, wantJobStatus)
				}
			})
		}
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

func TestPublicationCleanupResumesAfterRestartAfterManyFailures(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	var nowMu sync.RWMutex
	cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key"), Now: func() time.Time { nowMu.RLock(); defer nowMu.RUnlock(); return now }}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("b", 32)
	job := strings.Repeat("c", 32)
	if err := os.MkdirAll(app.deploymentPath(id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.stagePath(id, job), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.stagePath(id, job), "old.html"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	oldID, err := identity(app.stagePath(id, job))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO publication_cleanup(deployment_id,job_id,path_name,dev,ino,not_before,last_error,attempts) VALUES(?,?,?,?,?,?,?,0)`, id, job, "."+id+".stage-"+job, oldID.Dev, oldID.Ino, now.Add(-time.Second).UnixNano(), ""); err != nil {
		t.Fatal(err)
	}
	previousFault := setPublicationFaultPoint(func(point string) error {
		if point == "before-cleanup" {
			return errors.New("temporary cleanup failure")
		}
		return nil
	})
	defer func() { setPublicationFaultPoint(previousFault) }()
	for i := 0; i < 9; i++ {
		if err := app.cleanupPublications(false); err != nil {
			t.Fatal(err)
		}
		nowMu.Lock()
		now = now.Add(time.Hour)
		nowMu.Unlock()
	}
	var attempts int
	if err := app.db.QueryRow(`SELECT attempts FROM publication_cleanup WHERE deployment_id=? AND job_id=?`, id, job).Scan(&attempts); err != nil || attempts != 9 {
		t.Fatalf("persisted cleanup attempts=%d err=%v", attempts, err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh process has no in-memory retry state. The persisted obligation
	// must still be eligible and complete once the filesystem permits cleanup.
	app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	setPublicationFaultPoint(nil)
	nowMu.Lock()
	now = now.Add(time.Hour)
	nowMu.Unlock()
	if err := app.cleanupPublications(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(app.stagePath(id, job)); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not resume after restart: %v", err)
	}
	var remaining int
	if err := app.db.QueryRow(`SELECT count(*) FROM publication_cleanup WHERE deployment_id=? AND job_id=?`, id, job).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("cleanup obligation remaining=%d err=%v", remaining, err)
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

func TestExportHydratesCommittedLFSPointerIntoActualFile(t *testing.T) {
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
	payload := []byte("hydrated from the controlled HTTPS LFS server")
	digest := sha256.Sum256(payload)
	oid := hex.EncodeToString(digest[:])
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(payload))
	if err := os.Mkdir(filepath.Join(repoDir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".gitattributes":        "[attr]one two\n[attr]two filter=lfs\n\"nested/asset\\x20file.dat\" one\nnested/reset.dat one\n",
		"nested/.gitattributes": "reset.dat !filter\n",
		"nested/asset file.dat": pointer,
		"nested/reset.dat":      pointer,
		"nested/unselected.dat": pointer,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := worktree.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repoDir, ".git", "info"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".git", "info", "attributes"), []byte("nested/unselected.dat filter=lfs\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Point isolated host-global Git configuration at a rule that would select
	// the uncontrolled pointer if the exporter consulted ambient attributes.
	home := t.TempDir()
	globalAttributes := filepath.Join(home, "attributes")
	if err := os.WriteFile(globalAttributes, []byte("nested/unselected.dat filter=lfs\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\tattributesFile = "+globalAttributes+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	commitID, err := worktree.Commit("LFS tracked object", &git.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.test", When: time.Now().UTC()}})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(commitID)
	if err != nil {
		t.Fatal(err)
	}
	var actionURL string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lfs/objects/batch" {
			_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":%q}}}]}`, oid, len(payload), actionURL)
			return
		}
		if r.URL.Path == "/object" {
			_, _ = w.Write(payload)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	actionURL = server.URL + "/object"
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	oldRoots := lfsTestRootCAs
	lfsTestRootCAs = pool
	t.Cleanup(func() { lfsTestRootCAs = oldRoots })
	hydrator, err := newLFSHydrator("0123456789abcdef0123456789abcdef", lfsProjectSettings{Endpoint: server.URL + "/lfs", ObjectOrigins: []string{server.URL}, AllowInternal: true, InternalAddresses: map[string][]string{server.URL: {"127.0.0.1"}}}, t.TempDir(), 1<<20, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "export")
	if err = os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	if err = exportTrackedTree(context.Background(), repo, commit, out, exportLimits{files: 20, bytes: 1024, timeout: time.Second}, hydrator); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(out, "nested", "asset file.dat"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("published asset = %q, %v; want hydrated bytes", got, err)
	}
	for _, name := range []string{"reset.dat", "unselected.dat"} {
		got, err := os.ReadFile(filepath.Join(out, "nested", name))
		if err != nil || string(got) != pointer {
			t.Fatalf("non-selected pointer %s changed: %q, %v", name, got, err)
		}
	}
}

func TestNegativeTrackedAttributeFailsPublicationAndKeepsLiveFiles(t *testing.T) {
	base := t.TempDir()
	repoDir := filepath.Join(base, "fixture")
	previousCommit := createSmartFixture(t, repoDir)
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
	if err := os.WriteFile(filepath.Join(repoDir, ".gitattributes"), []byte("!private.dat filter=lfs\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "private.dat"), []byte("must never become live"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add(".gitattributes"); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("private.dat"); err != nil {
		t.Fatal(err)
	}
	badCommit, err := worktree.Commit("forbidden negative attribute", &git.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "fixture@example.test", When: time.Now().UTC()}})
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key"), DeployTimeout: 5 * time.Second}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	deploymentID := strings.Repeat("d", 32)
	projectID := strings.Repeat("e", 32)
	now := time.Now().UnixNano()
	if _, err := app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at,last_fetched,warning) VALUES(?,?,?,?,?,'')`, projectID, "fixture", "https://example.test/repo.git", now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(app.projects.repoPath(projectID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := copyGitCache(repoDir, app.projects.repoPath(projectID), 64<<20); err != nil {
		t.Fatal(err)
	}
	livePath := app.deploymentPath(deploymentID)
	if err := os.MkdirAll(livePath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(livePath, "index.html"), []byte("previous live version"), 0644); err != nil {
		t.Fatal(err)
	}
	liveID, err := identity(livePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,target_kind,generation,live_commit,live_dev,live_ino,status,created_at,updated_at) VALUES(?,?,?,?,'commit',1,?,?,?,'live',?,?)`, deploymentID, projectID, "negative-pattern", badCommit.String(), previousCommit, liveID.Dev, liveID.Ino, now, now); err != nil {
		t.Fatal(err)
	}
	cookie := installAPITestSession(t, app)
	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	jobID, err := app.enqueueDeployment(deploymentID, badCommit.String(), 1)
	if err != nil {
		t.Fatal(err)
	}
	waitForDeploymentJob(t, server.Client(), server.URL, cookie, deploymentID, jobID, "failed")
	if got, err := os.ReadFile(filepath.Join(livePath, "index.html")); err != nil || string(got) != "previous live version" {
		t.Fatalf("negative attribute changed existing live content: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(livePath, "private.dat")); !os.IsNotExist(err) {
		t.Fatalf("forbidden commit content became live: %v", err)
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
			previousFault := setPublicationFaultPoint(func(point string) error {
				if point == faultAt {
					return errors.New("simulated process interruption")
				}
				return nil
			})
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
			setPublicationFaultPoint(previousFault)
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
			defer func() { setPublicationFaultPoint(previousFault) }()
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
