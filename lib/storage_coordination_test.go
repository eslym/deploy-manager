package lib

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/sys/unix"
)

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
		cfg.storageFault = func(name string, _ *sql.DB) error {
			if name == phase {
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
		if e == nil {
			t.Fatal("pending upgrade unexpectedly completed")
		}
		return
	case "upgrade-fail":
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

func startUpgradeCrashChild(t *testing.T, cfg Config, phase string) (*exec.Cmd, io.WriteCloser, io.ReadCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStorageCoordinationChild$")
	cmd.Env = append(os.Environ(), "DM_COORDINATION_CHILD=upgrade-crash", "DM_COORDINATION_DIR="+filepath.Dir(cfg.DatabasePath), "DM_UPGRADE_CRASH_PHASE="+phase)
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
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.Close(); err != nil {
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
	// Two separately named compatible fixture program artifacts model the
	// retained prior version and the test-only failing-upgrade version. The
	// latter runs the application's real storage startup/migration code below.
	v1, v2 := filepath.Join(dir, "fixture-v1"), filepath.Join(dir, "fixture-v2")
	for _, path := range []string{v1, v2} {
		if err := copyFile(os.Args[0], path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(v1+".build", []byte("fixture-compatible-v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v2+".build", []byte("fixture-compatible-v2\n"), 0600); err != nil {
		t.Fatal(err)
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
	backupBefore, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := OpenDaemon(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !locked.Locked() {
		t.Fatal("restart with retained marker was not locked")
	}
	_ = locked.Close()

	// These are the same explicit commands documented above: preserve the
	// failed set, stage the verified standalone database, install the prior
	// executable artifact, and archive the marker last. No recursive deletion.
	evidence := filepath.Join(dir, "evidence")
	if err := os.Mkdir(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	commands := `set -eu
cp -p "$DB" "$EVIDENCE/failed-state.sqlite"
for suffix in -wal -shm -journal; do
  if test -e "$DB$suffix"; then mv -- "$DB$suffix" "$EVIDENCE/failed-state.sqlite$suffix"; fi
done
cp -p "$MARKER" "$EVIDENCE/failed-state.sqlite.maintenance.json"
cp -- "$BACKUP" "$DB.restore-stage"
cmp -- "$BACKUP" "$DB.restore-stage"
mv -- "$DB.restore-stage" "$DB"
cp -- "$PRIOR" "$PROGRAM.restore-stage"
cmp -- "$PRIOR" "$PROGRAM.restore-stage"
mv -- "$PROGRAM.restore-stage" "$PROGRAM"
mv -- "$MARKER" "$EVIDENCE/restored-pair.maintenance.json"
`
	cmd := exec.Command("sh", "-c", commands)
	cmd.Env = append(os.Environ(), "DB="+cfg.DatabasePath, "MARKER="+marker, "BACKUP="+backup, "PRIOR="+v1, "PROGRAM="+v2, "EVIDENCE="+evidence)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("documented recovery commands failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker was not archived after restoring pair: %v", err)
	}
	if _, err := os.Stat(evidence + "/failed-state.sqlite"); err != nil {
		t.Fatalf("failed database evidence missing: %v", err)
	}
	if _, err := os.Stat(evidence + "/failed-state.sqlite.maintenance.json"); err != nil {
		t.Fatalf("failed marker evidence missing: %v", err)
	}
	if _, err := os.Stat(evidence + "/restored-pair.maintenance.json"); err != nil {
		t.Fatalf("archived marker missing: %v", err)
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
	// Run the retained prior executable as a real process against the restored
	// database; it reaches normal application readiness and exits cleanly.
	prior := exec.Command(v1, "-test.run=^TestStorageCoordinationChild$")
	prior.Env = append(os.Environ(), "DM_COORDINATION_CHILD=daemon", "DM_COORDINATION_DIR="+dir)
	stdin, err := prior.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := prior.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	prior.Stderr = os.Stderr
	if err := prior.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		_ = stdin.Close()
		_ = prior.Wait()
		t.Fatalf("prior executable did not reach readiness: %q %v", line, err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prior.Wait(); err != nil {
		t.Fatalf("prior executable failed after recovery: %v", err)
	}
}

func copyFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	return errors.Join(copyErr, closeErr)
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
