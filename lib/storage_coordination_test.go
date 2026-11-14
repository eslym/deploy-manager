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
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

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
	}
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
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
