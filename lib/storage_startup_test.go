package lib

import (
	"os"
	"path/filepath"
	"testing"
)

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
	invalid := cfg
	invalid.WebhookMaxBodyBytes = 1
	if failed, err := New(invalid); err == nil {
		failed.Close()
		t.Fatal("invalid storage/startup configuration was accepted")
	}
	if data, err := os.ReadFile(witness); err != nil || string(data) != "pending cache recovery" {
		t.Fatalf("failed readiness performed cache recovery: content=%q error=%v", data, err)
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
