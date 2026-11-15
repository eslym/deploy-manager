package lib

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bytemare/opaque"
	"golang.org/x/sys/unix"
)

var fixtureProgramVersion = "development-test"

func TestParentTraversalRejectedWithoutStorageSideEffects(t *testing.T) {
	for _, entrypoint := range []string{"daemon", "command"} {
		for _, relative := range []bool{false, true} {
			for _, field := range []string{"database", "opaque", "repository"} {
				t.Run(fmt.Sprintf("%s/%s/relative=%t", entrypoint, field, relative), func(t *testing.T) {
					root := t.TempDir()
					trusted, other := filepath.Join(root, "trusted"), filepath.Join(root, "other")
					for _, dir := range []string{trusted, filepath.Join(other, "child")} {
						if err := os.MkdirAll(dir, 0700); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.Symlink(filepath.Join(other, "child"), filepath.Join(trusted, "link")); err != nil {
						t.Fatal(err)
					}
					cfg := Config{DatabasePath: filepath.Join(trusted, "state.db"), OPAQUEKeyPath: filepath.Join(trusted, "opaque.key"), RepositoryKeyPath: filepath.Join(trusted, "repository.key"), DeploymentRoot: filepath.Join(trusted, "publications")}
					prefix := trusted + "/link/../"
					if relative {
						t.Chdir(root)
						prefix = "trusted/link/../"
					}
					switch field {
					case "database":
						cfg.DatabasePath = prefix + "state.db"
					case "opaque":
						cfg.OPAQUEKeyPath = prefix + "opaque.key"
					case "repository":
						cfg.RepositoryKeyPath = prefix + "repository.key"
					}
					if entrypoint == "daemon" {
						app, err := New(cfg)
						if app != nil {
							_ = app.Close()
						}
						if err == nil {
							t.Error("daemon accepted ambiguous storage path")
						}
					} else {
						g, err := NewGrantIssuer(cfg)
						if g != nil {
							_ = g.Close()
						}
						if err == nil {
							t.Error("command accepted ambiguous storage path")
						}
					}
					for _, want := range []struct {
						dir   string
						names []string
					}{{trusted, []string{"link"}}, {other, []string{"child"}}, {filepath.Join(other, "child"), []string{}}} {
						entries, err := os.ReadDir(want.dir)
						if err != nil {
							t.Fatal(err)
						}
						var names []string
						for _, entry := range entries {
							names = append(names, entry.Name())
						}
						if !slices.Equal(names, want.names) {
							t.Errorf("unsafe path caused filesystem side effects in %s: got %v, want %v", want.dir, names, want.names)
						}
					}
				})
			}
		}
	}
}

func TestCanonicalRelativeStoragePathsShareDaemonCoordination(t *testing.T) {
	for _, prefix := range []string{"", "./"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			t.Chdir(t.TempDir())
			cfg := Config{DatabasePath: prefix + "data/state.db", OPAQUEKeyPath: prefix + "data/opaque.key", RepositoryKeyPath: prefix + "data/repository.key"}
			daemon, err := New(cfg)
			if err != nil {
				t.Fatalf("ordinary relative paths rejected: %v", err)
			}
			defer daemon.Close()
			command, err := NewGrantIssuer(cfg)
			if err != nil {
				t.Fatalf("relative command alongside daemon: %v", err)
			}
			defer command.Close()
			if _, err := command.IssueRegistrationGrant(); err != nil {
				t.Fatal(err)
			}
			absolute := cfg
			absolute.DatabasePath, err = filepath.Abs(cfg.DatabasePath)
			if err != nil {
				t.Fatal(err)
			}
			if second, err := New(absolute); err == nil {
				_ = second.Close()
				t.Fatal("absolute path bypassed relative daemon ownership")
			}
		})
	}
}

// Real processes exercise the OS lock lifetime; no in-process mutex can pass.
func TestStorageCoordinationChild(t *testing.T) {
	mode := os.Getenv("DM_COORDINATION_CHILD")
	if mode == "" {
		return
	}
	dir := os.Getenv("DM_COORDINATION_DIR")
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), DeploymentRoot: filepath.Join(dir, "publications")}
	var closer io.Closer
	var err error
	switch mode {
	case "daemon":
		closer, err = New(cfg)
	case "command":
		closer, err = NewGrantIssuer(cfg)
	case "issue":
		g, e := NewGrantIssuer(cfg)
		if e != nil {
			t.Fatal(e)
		}
		defer g.Close()
		if _, e := g.IssueRegistrationGrant(); e != nil {
			t.Fatal(e)
		}
		return
	case "maintenance":
		f, e := os.OpenFile(cfg.DatabasePath+".access.lock", os.O_RDWR, 0)
		if e != nil {
			t.Fatal(e)
		}
		closer = f
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	case "upgrade-crash":
		phase := os.Getenv("DM_UPGRADE_CRASH_PHASE")
		if os.Getenv("DM_UPGRADE_CRASH_FIXTURE") == "1" {
			cfg.upgradeTargetVersion = 2
			cfg.migrationFS = fstest.MapFS{
				"00001_base.sql":    {Data: []byte("-- +goose Up\nSELECT 1;\n-- +goose Down\nSELECT 1;\n")},
				"00002_fixture.sql": {Data: []byte("-- +goose Up\nCREATE TABLE crash_fixture(value TEXT NOT NULL);\n-- +goose Down\nDROP TABLE crash_fixture;\n")},
			}
		}
		reached := false
		cfg.storageFault = func(name string, _ *sql.DB) error {
			if name == phase {
				reached = true
				fmt.Println("ready")
				_, _ = io.Copy(io.Discard, os.Stdin)
				return errors.New("test process interrupted")
			}
			return nil
		}
		g, e := New(cfg)
		if g != nil {
			_ = g.Close()
		}
		if !reached {
			t.Fatalf("upgrade exited before requested crash point %q: %v", phase, e)
		}
		if e == nil {
			t.Fatal("pending upgrade unexpectedly completed")
		}
		return
	case "upgrade-fail":
		if fixtureProgramVersion != "fixture-v2" {
			t.Fatalf("failing-upgrade process has wrong fixture identity %q", fixtureProgramVersion)
		}
		cfg.upgradeTargetVersion = 2
		cfg.migrationFS = fstest.MapFS{
			"00001_base.sql":            {Data: []byte("-- +goose Up\nSELECT 1;\n-- +goose Down\nSELECT 1;\n")},
			"00002_fixture_failure.sql": {Data: []byte("-- +goose Up\nCREATE TABLE fixture_partial(value TEXT NOT NULL);\nINSERT INTO missing_fixture_table VALUES('fail');\n-- +goose Down\nDROP TABLE fixture_partial;\n")},
		}
		if _, err := New(cfg); err == nil {
			t.Fatal("fixture upgrade unexpectedly succeeded")
		} else {
			fmt.Fprintln(os.Stderr, "fixture-v2 startup failed as expected:", err)
			os.Exit(1)
		}
		return
	case "locked-restart":
		daemon, e := OpenDaemon(cfg)
		if e != nil {
			t.Fatal(e)
		}
		defer daemon.Close()
		if !daemon.Locked() {
			t.Fatal("restart with retained marker became ready")
		}
		fmt.Println("restart remains maintenance-locked")
		return
	case "verify-recovered":
		if fixtureProgramVersion != "fixture-v1" {
			t.Fatalf("recovery process has wrong prior fixture identity %q", fixtureProgramVersion)
		}
		app, e := New(cfg)
		if e != nil {
			t.Fatal(e)
		}
		defer app.Close()
		cookie := completeFixtureLogin(t, app, os.Getenv("DM_FIXTURE_PASSWORD"))
		request := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
		request.RemoteAddr, request.Host = "127.0.0.1:12345", "manager.example"
		request.TLS = &tls.ConnectionState{}
		request.Header.Set("Origin", "https://manager.example")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("prior fixture authenticated API returned %d: %s", response.Code, response.Body.String())
		}
		keyID := os.Getenv("DM_FIXTURE_REPOSITORY_KEY")
		if !app.repositoryKeyCanSign(keyID, []byte("rehearsal-key-signature-check")) {
			t.Fatal("prior fixture could not decrypt and sign with the restored repository key")
		}
		jobID := os.Getenv("DM_FIXTURE_JOB")
		var durable, generation int
		var jobCommit, jobStatus string
		if e := app.db.QueryRow(`SELECT durable,generation,commit_id,status FROM deployment_jobs WHERE id=?`, jobID).Scan(&durable, &generation, &jobCommit, &jobStatus); e != nil || durable != 1 || generation != 1 || jobCommit != strings.Repeat("d", 40) {
			t.Fatalf("accepted durable job state was not preserved: durable=%d generation=%d commit=%q status=%q err=%v", durable, generation, jobCommit, jobStatus, e)
		}
		deploymentID := os.Getenv("DM_FIXTURE_DEPLOYMENT")
		publication := app.deploymentPath(deploymentID)
		gotIdentity, e := identity(publication)
		if e != nil {
			t.Fatal(e)
		}
		wantDev, _ := strconv.ParseUint(os.Getenv("DM_FIXTURE_PUBLICATION_DEV"), 10, 64)
		wantIno, _ := strconv.ParseUint(os.Getenv("DM_FIXTURE_PUBLICATION_INO"), 10, 64)
		if gotIdentity.Dev != wantDev || gotIdentity.Ino != wantIno {
			t.Fatalf("publication directory identity changed: got %d/%d want %d/%d", gotIdentity.Dev, gotIdentity.Ino, wantDev, wantIno)
		}
		content, e := os.ReadFile(filepath.Join(publication, "index.html"))
		if e != nil || string(content) != "prior-publication-witness" {
			t.Fatalf("published content changed/lost: %q err=%v", content, e)
		}
		var recordedDev, recordedIno uint64
		if e := app.db.QueryRow(`SELECT live_dev,live_ino FROM deployments WHERE id=?`, deploymentID).Scan(&recordedDev, &recordedIno); e != nil || recordedDev != wantDev || recordedIno != wantIno {
			t.Fatalf("publication identity was not conservatively retained in state: %d/%d err=%v", recordedDev, recordedIno, e)
		}
		fmt.Println("ready: prior fixture login, repository key, job, and publication verified")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestUpgradePreparationCrashWindowsRemainLockedAcrossRestart(t *testing.T) {
	for _, phase := range []string{"backup-checkpoint", "marker-update"} {
		t.Run(phase, func(t *testing.T) {
			cfg := coordinationConfig(t)
			issuer, err := NewGrantIssuer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_ = issuer.Close()
			db, err := sql.Open("sqlite3", cfg.DatabasePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE goose_db_version SET is_applied=0 WHERE version_id=1`); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			cmd, stdin, stdout := startUpgradeCrashChild(t, cfg, phase)
			if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("child did not stop at %s: %q %v", phase, line, err)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = stdin.Close()
			_ = cmd.Wait()
			if _, err := os.Stat(cfg.DatabasePath + ".maintenance.json"); err != nil {
				t.Fatalf("crash at %s lost maintenance marker: %v", phase, err)
			}
			if phase == "marker-update" {
				entries, err := os.ReadDir(cfg.DatabasePath + ".upgrade-backups")
				if err != nil || len(entries) != 2 {
					t.Fatalf("crash after backup finalization did not retain copy and manifest: entries=%v err=%v", entries, err)
				}
			}
			daemon, err := OpenDaemon(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer daemon.Close()
			if !daemon.Locked() {
				t.Fatalf("restart after %s escaped maintenance mode", phase)
			}
		})
	}
}

func TestProtectedUpgradeProcessCrashesRemainLockedAcrossRestart(t *testing.T) {
	for _, phase := range []string{"marker-phase-vacuum", "upgrade-vacuum", "upgrade-after-vacuum", "upgrade-migration", "marker-phase-verification", "upgrade-verify", "upgrade-clean-close", "marker-phase-unlock", "marker-remove"} {
		t.Run(phase, func(t *testing.T) {
			cfg := coordinationConfig(t)
			issuer, err := NewGrantIssuer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := issuer.Close(); err != nil {
				t.Fatal(err)
			}
			cmd, stdin, stdout := startUpgradeCrashChild(t, cfg, phase, true)
			if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("upgrade did not reach crash point %s: %q %v", phase, line, err)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = stdin.Close()
			_ = cmd.Wait()
			if _, err := os.Stat(cfg.DatabasePath + ".maintenance.json"); err != nil {
				t.Fatalf("process crash at %s lost durable maintenance marker: %v", phase, err)
			}
			restart, err := OpenDaemon(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer restart.Close()
			if !restart.Locked() {
				t.Fatalf("restart after %s crash became ready", phase)
			}
		})
	}
}

func TestCrashBeforeMaintenanceMarkerDurabilityLeavesStoreUsableAndUnchanged(t *testing.T) {
	cfg := coordinationConfig(t)
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.Close(); err != nil {
		t.Fatal(err)
	}
	paths := []string{cfg.DatabasePath, cfg.OPAQUEKeyPath, cfg.DatabasePath + ".repository-master-key"}
	before := make(map[string][]byte, len(paths))
	for _, path := range paths {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	cmd, stdin, stdout := startUpgradeCrashChild(t, cfg, "marker-create", true)
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("upgrade did not reach pre-marker crash point: %q %v", line, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	_ = cmd.Wait()
	if _, err := os.Lstat(cfg.DatabasePath + ".maintenance.json"); !os.IsNotExist(err) {
		t.Fatalf("pre-durability crash unexpectedly left a marker: %v", err)
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(want) {
			t.Fatalf("pre-marker crash changed %s: %v", path, err)
		}
	}
	app, err := New(cfg)
	if err != nil {
		t.Fatalf("untouched current store did not become ready after pre-marker crash: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(cfg.DatabasePath + ".upgrade-backups"); err != nil || len(entries) != 0 {
		t.Fatalf("pre-marker crash created maintenance backup artifacts: entries=%v err=%v", entries, err)
	}
}

func startUpgradeCrashChild(t *testing.T, cfg Config, phase string, fixture ...bool) (*exec.Cmd, io.WriteCloser, io.ReadCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStorageCoordinationChild$")
	cmd.Env = append(os.Environ(), "DM_COORDINATION_CHILD=upgrade-crash", "DM_COORDINATION_DIR="+filepath.Dir(cfg.DatabasePath), "DM_UPGRADE_CRASH_PHASE="+phase)
	if len(fixture) > 0 && fixture[0] {
		cmd.Env = append(cmd.Env, "DM_UPGRADE_CRASH_FIXTURE=1")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	return cmd, stdin, stdout
}

// TestDocumentedOperatorRecoveryRehearsal drives the real storage upgrade in a
// child process, lets a real goose migration fail, then runs the documented
// quarantine/stage/verify/unlock file commands against a temporary installation.
func TestDocumentedOperatorRecoveryRehearsal(t *testing.T) {
	dir := t.TempDir()
	publicationRoot := filepath.Join(dir, "publications")
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true, DeploymentRoot: publicationRoot, GitTimeout: time.Second}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const password = "test-only OPAQUE recovery password"
	grant, err := app.IssueRegistrationGrant()
	if err != nil {
		t.Fatal(err)
	}
	transaction, record := newRegistrationFlowWithIdentities(t, app, grant, "https://manager.example", password, nil, nil)
	if status := finishTestRegistration(app, "", transaction, record, "https://manager.example"); status != http.StatusNoContent {
		t.Fatalf("OPAQUE fixture registration status=%d", status)
	}
	key, err := app.createRepositoryKey("recovery witness", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !app.repositoryKeyCanSign(key.ID, []byte("rehearsal-key-signature-check")) {
		t.Fatal("fixture repository key failed pre-upgrade signing check")
	}
	projectID, deploymentID, jobID := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	now := time.Now().UTC().UnixNano()
	if _, err := app.db.Exec(`INSERT INTO projects(id,name,remote_url,created_at,last_fetched,warning) VALUES(?,?,?,?,?,'')`, projectID, "recovery-witness", "https://unreachable.invalid/repository.git", now, now); err != nil {
		t.Fatal(err)
	}
	publication := app.deploymentPath(deploymentID)
	if err := os.MkdirAll(publication, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publication, "index.html"), []byte("prior-publication-witness"), 0644); err != nil {
		t.Fatal(err)
	}
	publicationIdentity, err := identity(publication)
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("d", 40)
	if _, err := app.db.Exec(`INSERT INTO deployments(id,project_id,name,commit_id,generation,live_commit,live_dev,live_ino,status,created_at,updated_at,target_kind,target_name) VALUES(?,?,?, ?,1,?,?,?,'live',?,?,'branch','main')`, deploymentID, projectID, "live-witness", commit, commit, publicationIdentity.Dev, publicationIdentity.Ino, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO deployment_jobs(id,deployment_id,commit_id,generation,status,log,created_at,updated_at,durable,automatic) VALUES(?,?,?,1,'queued','accepted before upgrade',?,?,1,0)`, jobID, deploymentID, commit, now, now); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	opaqueBefore, err := os.ReadFile(cfg.OPAQUEKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	repositoryBefore, err := os.ReadFile(cfg.DatabasePath + ".repository-master-key")
	if err != nil {
		t.Fatal(err)
	}
	// Compile genuinely distinct test-only versions: the linker identity is part
	// of the executable, and each version asserts it before performing its role.
	v1, v2 := filepath.Join(dir, "fixture-v1"), filepath.Join(dir, "fixture-v2")
	buildFixtureProgram(t, v1, "fixture-v1")
	buildFixtureProgram(t, v2, "fixture-v2")
	v1Hash, err := fileSHA256(v1)
	if err != nil {
		t.Fatal(err)
	}
	v2Hash, err := fileSHA256(v2)
	if err != nil || v1Hash == v2Hash {
		t.Fatalf("fixture binaries must have distinct build identities: v1=%s v2=%s err=%v", v1Hash, v2Hash, err)
	}
	child := exec.Command(v2, "-test.run=^TestStorageCoordinationChild$")
	child.Env = append(os.Environ(), "DM_COORDINATION_CHILD=upgrade-fail", "DM_COORDINATION_DIR="+dir)
	if output, err := child.CombinedOutput(); err == nil {
		t.Fatalf("failing fixture upgrade returned success: %s", output)
	}
	marker := cfg.DatabasePath + ".maintenance.json"
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("failed process did not leave marker: %v", err)
	}
	entries, err := os.ReadDir(cfg.DatabasePath + ".upgrade-backups")
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected finalized backup and manifest, entries=%v err=%v", entries, err)
	}
	backup := ""
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".db") {
			backup = filepath.Join(cfg.DatabasePath+".upgrade-backups", entry.Name())
		}
	}
	if backup == "" {
		t.Fatal("finalized database backup not found")
	}
	manifestBytes, err := os.ReadFile(backup + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Status        string `json:"status"`
		Program       string `json:"program"`
		SourceVersion int64  `json:"source_version"`
		TargetVersion int64  `json:"target_version"`
		SHA256        string `json:"sha256"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.Status != "complete" || manifest.SourceVersion != 1 || manifest.TargetVersion != 2 || !strings.HasPrefix(manifest.Program, "fixture-v2") {
		t.Fatalf("verified backup manifest does not identify the failing fixture build: %+v err=%v", manifest, err)
	}
	backupBefore, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	backupHash, err := fileSHA256(backup)
	if err != nil || backupHash != manifest.SHA256 {
		t.Fatalf("finalized backup checksum does not match its manifest: got %s want %s err=%v", backupHash, manifest.SHA256, err)
	}
	restart := exec.Command(v2, "-test.run=^TestStorageCoordinationChild$")
	restart.Env = append(os.Environ(), "DM_COORDINATION_CHILD=locked-restart", "DM_COORDINATION_DIR="+dir)
	if output, err := restart.CombinedOutput(); err != nil || !strings.Contains(string(output), "restart remains maintenance-locked") {
		t.Fatalf("real restart escaped maintenance lock: err=%v output=%s", err, output)
	}

	// Run the same standalone file-phase script operators use. Quarantine,
	// restore, and marker archival are separate so evidence and external-key
	// checks happen before unlocking.
	evidence := filepath.Join(dir, "evidence")
	script, err := filepath.Abs("../docs/operations/upgrade-recovery-files.sh")
	if err != nil {
		t.Fatal(err)
	}
	uid, gid := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid())
	baseEnv := append(os.Environ(), "DB="+cfg.DatabasePath, "MARKER="+marker, "BACKUP="+backup, "BACKUP_SHA256="+backupHash, "PRIOR="+v1, "PRIOR_SHA256="+v1Hash, "PROGRAM="+v2, "EVIDENCE="+evidence, "OWNER="+uid, "GROUP="+gid, "PROGRAM_OWNER="+uid, "PROGRAM_GROUP="+gid)
	cmd := exec.Command("sh", script, "quarantine")
	cmd.Env = baseEnv
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("documented recovery quarantine action failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("quarantine action cleared the maintenance marker: %v", err)
	}
	cmd = exec.Command("sh", script, "restore")
	cmd.Env = baseEnv
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("documented recovery restore action failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("restore action cleared the maintenance marker before key checks: %v", err)
	}
	if _, err := os.Stat(evidence + "/failed-state.sqlite"); err != nil {
		t.Fatalf("failed database evidence missing: %v", err)
	}
	if _, err := os.Stat(evidence + "/failed-state.sqlite.maintenance.json"); err != nil {
		t.Fatalf("failed marker evidence missing: %v", err)
	}
	if _, err := os.Stat(evidence + "/restored-pair.maintenance.json"); !os.IsNotExist(err) {
		t.Fatalf("restore action archived marker before external-key verification: %v", err)
	}
	restoredProgramHash, err := fileSHA256(v2)
	if err != nil || restoredProgramHash != v1Hash {
		t.Fatalf("recovery did not install the exact prior executable: got %s want %s err=%v", restoredProgramHash, v1Hash, err)
	}
	backupAfter, err := os.ReadFile(backup)
	if err != nil || string(backupBefore) != string(backupAfter) {
		t.Fatalf("recovery commands modified the finalized backup: %v", err)
	}
	for path, before := range map[string][]byte{cfg.OPAQUEKeyPath: opaqueBefore, cfg.DatabasePath + ".repository-master-key": repositoryBefore} {
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("recovery changed or lost external key %s: %v", path, err)
		}
	}
	archive := exec.Command("sh", script, "archive-marker")
	archive.Env = baseEnv
	if output, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("documented marker-archive action failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker remains after explicit archive action: %v", err)
	}
	if _, err := os.Stat(evidence + "/restored-pair.maintenance.json"); err != nil {
		t.Fatalf("archived marker missing: %v", err)
	}
	// The script replaced v2's installed executable path with v1. Run that
	// actual restored file as the distinct prior build, not a test helper binary.
	prior := exec.Command(v2, "-test.run=^TestStorageCoordinationChild$")
	prior.Env = append(os.Environ(), "DM_COORDINATION_CHILD=verify-recovered", "DM_COORDINATION_DIR="+dir,
		"DM_FIXTURE_PASSWORD="+password, "DM_FIXTURE_REPOSITORY_KEY="+key.ID, "DM_FIXTURE_JOB="+jobID,
		"DM_FIXTURE_DEPLOYMENT="+deploymentID, "DM_FIXTURE_PUBLICATION_DEV="+strconv.FormatUint(publicationIdentity.Dev, 10),
		"DM_FIXTURE_PUBLICATION_INO="+strconv.FormatUint(publicationIdentity.Ino, 10))
	output, err := prior.CombinedOutput()
	if err != nil {
		t.Fatalf("restored prior executable failed the observable checks: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "ready: prior fixture login, repository key, job, and publication verified") {
		t.Fatalf("restored prior executable did not report all verification checks: %s", output)
	}
}

func buildFixtureProgram(t *testing.T, output, version string) {
	t.Helper()
	cmd := exec.Command("go", "test", "-c", "-o", output, "-ldflags", "-X=github.com/eslym/deploy-manager/lib.fixtureProgramVersion="+version)
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s fixture program: %v\n%s", version, err, result)
	}
}

func fileSHA256(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), nil
}

func completeFixtureLogin(t *testing.T, app *App, password string) *http.Cookie {
	t.Helper()
	const origin = "https://manager.example"
	client, err := opaque.NewClient(opaque.DefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	defer client.ClearState()
	ke1, err := client.GenerateKE1([]byte(password))
	if err != nil {
		t.Fatal(err)
	}
	startBody, err := json.Marshal(protocolMessage{Value: base64.RawURLEncoding.EncodeToString(ke1.Serialize())})
	if err != nil {
		t.Fatal(err)
	}
	start := registrationRequest(app, http.MethodPost, "/opaque/login/start", startBody, origin)
	if start.Code != http.StatusOK {
		t.Fatalf("OPAQUE login start returned %d: %s", start.Code, start.Body.String())
	}
	var startResponse struct {
		Transaction string `json:"transaction"`
		Value       string `json:"value"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &startResponse); err != nil || startResponse.Transaction == "" {
		t.Fatalf("invalid OPAQUE login response: %+v (%v)", startResponse, err)
	}
	ke2Bytes, err := base64.RawURLEncoding.DecodeString(startResponse.Value)
	if err != nil {
		t.Fatal(err)
	}
	ke2, err := client.Deserialize.KE2(ke2Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ke3, _, _, err := client.GenerateKE3(ke2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	finishBody, err := json.Marshal(protocolMessage{Transaction: &startResponse.Transaction, Value: base64.RawURLEncoding.EncodeToString(ke3.Serialize())})
	if err != nil {
		t.Fatal(err)
	}
	finish := registrationRequest(app, http.MethodPost, "/opaque/login/finish", finishBody, origin)
	if finish.Code != http.StatusNoContent {
		t.Fatalf("OPAQUE login finish returned %d: %s", finish.Code, finish.Body.String())
	}
	for _, cookie := range finish.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	t.Fatal("OPAQUE login did not issue a session cookie")
	return nil
}

func TestConcurrentCommandTransactionsRespectGrantLimit(t *testing.T) {
	cfg := coordinationConfig(t)
	daemon := startCoordinationChild(t, cfg, "daemon")
	_ = daemon
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStorageCoordinationChild$")
			cmd.Env = append(os.Environ(), "DM_COORDINATION_CHILD=issue", "DM_COORDINATION_DIR="+filepath.Dir(cfg.DatabasePath))
			output, err := cmd.CombinedOutput()
			if err != nil && !strings.Contains(string(output), "too many outstanding setup links") {
				results <- fmt.Errorf("unexpected command failure: %s: %w", output, err)
				return
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "unexpected") {
			t.Error(err)
		}
	}
	if successes != 10 {
		t.Fatalf("successful grants = %d, want exact transaction cap 10", successes)
	}
}

func startCoordinationChild(t *testing.T, cfg Config, mode string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStorageCoordinationChild$")
	cmd.Env = append(os.Environ(), "DM_COORDINATION_CHILD="+mode, "DM_COORDINATION_DIR="+filepath.Dir(cfg.DatabasePath))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child readiness: %q %v", line, err)
	}
	return cmd
}

func TestCrossProcessDaemonCrashAndAccessLifetime(t *testing.T) {
	cfg := coordinationConfig(t)
	daemon := startCoordinationChild(t, cfg, "daemon")
	if app, err := New(cfg); err == nil {
		_ = app.Close()
		t.Fatal("second process daemon accepted")
	}
	command, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := command.IssueRegistrationGrant(); err != nil {
		t.Fatal(err)
	}
	_ = command.Close()
	if err := daemon.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = daemon.Wait()
	// A command holds shared access for its whole lifetime, including after checks.
	cli := startCoordinationChild(t, cfg, "command")
	f, err := os.OpenFile(cfg.DatabasePath+".access.lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != unix.EWOULDBLOCK {
		t.Fatalf("maintenance overlapped CLI: %v", err)
	}
	if err := cli.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cli.Wait()
	maintenance := startCoordinationChild(t, cfg, "maintenance")
	if g, err := NewGrantIssuer(cfg); err == nil {
		_ = g.Close()
		t.Fatal("CLI overlapped maintenance")
	}
	if app, err := New(cfg); err == nil {
		_ = app.Close()
		t.Fatal("daemon overlapped maintenance")
	}
	if err := maintenance.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = maintenance.Wait()
	restarted, err := New(cfg)
	if err != nil {
		t.Fatalf("crash did not release ownership/access: %v", err)
	}
	defer restarted.Close()
}

func TestInitializationExcludesAllDatabaseUsers(t *testing.T) {
	cfg := coordinationConfig(t)
	held, release := make(chan struct{}), make(chan struct{})
	cfg.storageFault = func(phase string, _ *sql.DB) error {
		if phase == "access" {
			close(held)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		g, err := NewGrantIssuer(cfg)
		if g != nil {
			_ = g.Close()
		}
		done <- err
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("initialization ended before access hook: %v", err)
	case <-time.After(time.Second):
		t.Fatal("initialization did not acquire access")
	}
	other := cfg
	other.storageFault = nil
	start := time.Now()
	command, err := NewGrantIssuer(other)
	if command != nil {
		_ = command.Close()
	}
	close(release)
	if initErr := <-done; initErr != nil {
		t.Fatal(initErr)
	}
	if err == nil || !strings.Contains(err.Error(), "access") {
		t.Fatalf("command overlapped initialization: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("contention was not bounded")
	}
	other.storageFault = func(string, *sql.DB) error { return errors.New("verify failed") }
	if g, err := NewGrantIssuer(other); err == nil {
		_ = g.Close()
		t.Fatal("expected preparation failure")
	}
	other.storageFault = nil
	g, err := NewGrantIssuer(other)
	if err != nil {
		t.Fatalf("failed preparation leaked access: %v", err)
	}
	defer g.Close()
}

func TestUnsafeCoordinationPathsRefusedBeforeKeyAccess(t *testing.T) {
	for _, entrypoint := range []string{"daemon", "command"} {
		for _, kind := range []string{"symlink", "hardlink", "permissions", "fifo", "nonempty", "key-alias", "database-symlink", "database-hardlink", "parent-symlink", "writable-ancestor"} {
			t.Run(entrypoint+"/"+kind, func(t *testing.T) {
				cfg := coordinationConfig(t)
				target := filepath.Join(filepath.Dir(cfg.DatabasePath), "untouched")
				if err := os.WriteFile(target, []byte("evidence"), 0600); err != nil {
					t.Fatal(err)
				}
				lock := cfg.DatabasePath + ".daemon.lock"
				if entrypoint == "command" {
					lock = cfg.DatabasePath + ".access.lock"
				}
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(target, lock)
				case "hardlink":
					err = os.Link(target, lock)
				case "permissions":
					err = os.WriteFile(lock, nil, 0666)
					if err == nil {
						err = os.Chmod(lock, 0666)
					}
				case "fifo":
					err = unix.Mkfifo(lock, 0600)
				case "nonempty":
					err = os.WriteFile(lock, []byte("not a lock identity"), 0600)
				case "key-alias":
					cfg.OPAQUEKeyPath = lock
				case "database-symlink":
					err = os.Symlink(target, cfg.DatabasePath)
				case "database-hardlink":
					err = os.Link(target, cfg.DatabasePath)
				case "parent-symlink":
					alias := filepath.Join(t.TempDir(), "alias")
					err = os.Symlink(filepath.Dir(cfg.DatabasePath), alias)
					cfg.DatabasePath = filepath.Join(alias, "state.db")
				case "writable-ancestor":
					unsafe := t.TempDir()
					err = os.Chmod(unsafe, 0777)
					cfg.DatabasePath = filepath.Join(unsafe, "private", "state.db")
				}
				if err != nil {
					t.Fatal(err)
				}
				if entrypoint == "command" {
					var g *GrantIssuer
					g, err = NewGrantIssuer(cfg)
					if g != nil {
						_ = g.Close()
					}
				} else {
					var app *App
					app, err = New(cfg)
					if app != nil {
						_ = app.Close()
					}
				}
				if err == nil {
					t.Fatal("unsafe path accepted")
				}
				if data, err := os.ReadFile(target); err != nil || string(data) != "evidence" {
					t.Fatalf("aliased file changed: %q, %v", data, err)
				}
				if kind != "key-alias" {
					if _, err := os.Lstat(cfg.OPAQUEKeyPath); !os.IsNotExist(err) {
						t.Fatalf("key path touched before refusal: %v", err)
					}
				}
			})
		}
	}
}

func coordinationConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), DeploymentRoot: filepath.Join(dir, "publications")}
}

func TestDaemonOwnershipAllowsCommandButRefusesSecondDaemon(t *testing.T) {
	cfg := coordinationConfig(t)
	daemon, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Close() })
	start := time.Now()
	second, err := New(cfg)
	if second != nil {
		_ = second.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("second daemon: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("second daemon refusal was not prompt")
	}
	command, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatalf("compatible command alongside daemon: %v", err)
	}
	defer command.Close()
	if _, err := command.IssueRegistrationGrant(); err != nil {
		t.Fatal(err)
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatalf("restart after close: %v", err)
	}
	defer restarted.Close()
}
