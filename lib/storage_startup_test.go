package lib

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDaemonRecordsProgramProvenanceAndGrantCLILeavesItUntouched(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	path := provenancePath(cfg)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recorded programProvenance
	if err := json.Unmarshal(before, &recorded); err != nil {
		t.Fatal(err)
	}
	hash, err := fileSHA256(recorded.Executable)
	if err != nil || hash != recorded.SHA256 || recorded.Schema != 1 || recorded.Database != cfg.DatabasePath {
		t.Fatalf("recorded daemon provenance is not bound to this executable/install/schema: %+v err=%v", recorded, err)
	}
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("compatible CLI overwrote daemon provenance: %v", err)
	}
}

func TestDaemonStartupFailsClosedWhenRunningExecutablePathIsRemoved(t *testing.T) {
	if dir := os.Getenv("DM_PROVENANCE_EXECUTABLE_TEST"); dir != "" {
		cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key")}
		initial, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := initial.Close(); err != nil {
			t.Fatal(err)
		}
		cfg.upgradeTargetVersion = 2
		cfg.migrationFS = fstest.MapFS{
			"00001_base.sql":    {Data: []byte("-- +goose Up\nSELECT 1;\n-- +goose Down\nSELECT 1;\n")},
			"00002_forward.sql": {Data: []byte("-- +goose Up\nCREATE TABLE provenance_upgrade_witness(value TEXT NOT NULL);\n-- +goose Down\nDROP TABLE provenance_upgrade_witness;\n")},
		}
		dbPath, err := filepath.Abs(cfg.DatabasePath)
		if err != nil {
			t.Fatal(err)
		}
		dbURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath)}).String()
		var pending pendingUpgrade
		if err := preflightExistingSchema(dbURL, cfg); !errors.As(err, &pending) || pending.source != 1 || pending.target != 2 {
			t.Fatalf("test fixture is not a valid pending 1-to-2 daemon upgrade: pending=%+v err=%v", pending, err)
		}
		provenanceBefore, err := os.ReadFile(provenancePath(cfg))
		if err != nil {
			t.Fatal(err)
		}
		databaseBefore, err := os.ReadFile(cfg.DatabasePath)
		if err != nil {
			t.Fatal(err)
		}
		path, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		moved := path + ".moved"
		replacement := path + ".replacement"
		mode := os.Getenv("DM_PROVENANCE_EXECUTABLE_MODE")
		if mode == "replaced" {
			if err := os.WriteFile(replacement, []byte("not the running executable"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Rename(path, moved); err != nil {
			t.Fatal(err)
		}
		if mode == "replaced" {
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
		}
		defer func() {
			_ = os.Remove(path)
			_ = os.Rename(moved, path)
		}()
		app, err := New(cfg)
		if app != nil {
			_ = app.Close()
			t.Fatal("daemon started after its executable pathname was removed")
		}
		if err == nil || (!strings.Contains(err.Error(), "configured running program") && !strings.Contains(err.Error(), "configured executable no longer identifies")) {
			t.Fatalf("daemon startup did not fail for the invalid executable pathname: %v", err)
		}
		provenanceAfter, err := os.ReadFile(provenancePath(cfg))
		if err != nil || !bytes.Equal(provenanceBefore, provenanceAfter) {
			t.Fatalf("failed daemon changed provenance: %v", err)
		}
		databaseAfter, err := os.ReadFile(cfg.DatabasePath)
		if err != nil || !bytes.Equal(databaseBefore, databaseAfter) {
			t.Fatalf("invalid executable pathname changed database before maintenance: %v", err)
		}
		for _, path := range []string{cfg.DatabasePath + ".maintenance.json", cfg.DatabasePath + ".upgrade-backups"} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid executable pathname created upgrade artifact %s: %v", path, err)
			}
		}
		return
	}

	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	childBinary := filepath.Join(dir, "test-binary")
	source, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := os.OpenFile(childBinary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	if _, err := io.Copy(destination, source); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(source.Close(), destination.Close()); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"removed", "replaced"} {
		modeDir := filepath.Join(dir, mode)
		if err := os.Mkdir(modeDir, 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(childBinary, "-test.run=^TestDaemonStartupFailsClosedWhenRunningExecutablePathIsRemoved$")
		cmd.Env = append(os.Environ(), "DM_PROVENANCE_EXECUTABLE_TEST="+modeDir, "DM_PROVENANCE_EXECUTABLE_MODE="+mode)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child daemon fail-closed check (%s): %v\n%s", mode, err, output)
		}
	}
}

func TestGrantIssuerAlongsideDaemonDoesNotRunRecovery(t *testing.T) {
	base := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")}
	daemon, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	cache := filepath.Join(cfg.DatabasePath+".projects", ".git-stage-interrupted")
	stage := filepath.Join(cfg.DatabasePath+".publications", "."+strings.Repeat("a", 32)+".stage-"+strings.Repeat("b", 32))
	for _, path := range []string{cache, stage} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg.beforeRestoreDeploymentJobs = func(*sql.DB) error {
		t.Fatal("grant issuer attempted daemon queue restoration")
		return nil
	}
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer issuer.Close()
	if _, err := issuer.IssueRegistrationGrant(); err != nil {
		t.Fatalf("setup alongside daemon: %v", err)
	}
	if _, err := issuer.IssueRecoveryGrant(); err == nil {
		t.Fatal("recovery allowed without an administrator")
	}
	for _, path := range []string{cache, stage} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("CLI performed daemon recovery on %s: %v", path, err)
		}
	}
}

func TestGrantIssuerRefusesPendingUpgradeWithoutChangingStorage(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE goose_db_version SET is_applied=0 WHERE version_id=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.upgradeTargetVersion = 2
	cfg.migrationFS = fstest.MapFS{
		"00001_base.sql":    {Data: []byte("-- +goose Up\nSELECT 1;\n-- +goose Down\nSELECT 1;\n")},
		"00002_forward.sql": {Data: []byte("-- +goose Up\nCREATE TABLE cli_upgrade_witness(value TEXT NOT NULL);\n-- +goose Down\nDROP TABLE cli_upgrade_witness;\n")},
	}
	databaseBefore, err := os.ReadFile(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	opaqueBefore, err := os.ReadFile(cfg.OPAQUEKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	masterBefore, err := os.ReadFile(cfg.DatabasePath + ".repository-master-key")
	if err != nil {
		t.Fatal(err)
	}
	if cli, err := NewGrantIssuer(cfg); err == nil {
		_ = cli.Close()
		t.Fatal("CLI grant issuer accepted a pending existing-store upgrade")
	}
	for path, before := range map[string][]byte{cfg.DatabasePath: databaseBefore, cfg.OPAQUEKeyPath: opaqueBefore, cfg.DatabasePath + ".repository-master-key": masterBefore} {
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("CLI changed %s: read error=%v", path, err)
		}
	}
	for _, path := range []string{cfg.DatabasePath + ".maintenance.json", cfg.DatabasePath + ".upgrade-backups"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("CLI created upgrade artifact %s: %v", path, err)
		}
	}
}

func TestStorageReadinessFaultClosesResourcesBeforeNormalWork(t *testing.T) {
	base := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")}
	want := errors.New("injected storage verification failure")
	var opened *sql.DB
	cfg.storageFault = func(phase string, db *sql.DB) error {
		if phase == "access" {
			return nil
		}
		if phase != "verify" {
			t.Fatalf("unexpected fault phase %q", phase)
		}
		opened = db
		return want
	}
	app, err := New(cfg)
	if app != nil {
		app.Close()
	}
	if app != nil || !errors.Is(err, want) {
		t.Fatalf("startup returned app=%t, error=%v; want no app and injected error", app != nil, err)
	}
	if opened == nil || opened.Ping() == nil {
		t.Fatal("failed readiness did not close persistence")
	}
	for _, path := range []string{cfg.DatabasePath + ".projects", cfg.DatabasePath + ".publications"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("normal startup touched %s before readiness: %v", path, err)
		}
	}
	cfg.storageFault = nil
	app, err = New(cfg)
	if err != nil {
		t.Fatalf("valid restart after preparation failure: %v", err)
	}
	defer app.Close()
	if _, err := app.IssueRegistrationGrant(); err != nil {
		t.Fatalf("ready startup cannot issue setup grant: %v", err)
	}
}

// Exercise the application constructor, not the private preparation phases.
func TestStorageValidationFailureLeavesCacheRecoveryForSuccessfulRestart(t *testing.T) {
	base := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(cfg.DatabasePath+".projects", ".git-stage-interrupted")
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(orphan, "content")
	if err := os.WriteFile(witness, []byte("pending cache recovery"), 0600); err != nil {
		t.Fatal(err)
	}
	invalidWebhook, invalidDeploy := cfg, cfg
	invalidWebhook.WebhookMaxBodyBytes = 1
	invalidDeploy.DeployConcurrency = -1
	for _, invalid := range []Config{invalidWebhook, invalidDeploy} {
		if failed, err := New(invalid); err == nil {
			failed.Close()
			t.Fatal("invalid storage/startup configuration was accepted")
		}
		if data, err := os.ReadFile(witness); err != nil || string(data) != "pending cache recovery" {
			t.Fatalf("failed readiness performed cache recovery: content=%q error=%v", data, err)
		}
	}
	app, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("successful restart did not recover orphan cache: %v", err)
	}
}
