package lib

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestStorageReadinessFaultClosesResourcesBeforeNormalWork(t *testing.T) {
	base := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(base, "state.sqlite"), OPAQUEKeyPath: filepath.Join(base, "opaque.key")}
	want := errors.New("injected storage verification failure")
	var opened *sql.DB
	cfg.storageFault = func(phase string, db *sql.DB) error {
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
