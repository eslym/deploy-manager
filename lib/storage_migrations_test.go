package lib

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestFreshSchemaIsCompleteAndReopenIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=1000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := initializeSchema(db, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"registration_grants", "administrator", "opaque_profile", "repository_key_store", "repository_keys",
		"projects", "project_refs", "project_refresh_journal", "project_ssh_keys", "project_deletions",
		"ssh_host_trust", "project_lfs_settings", "deployments", "deployment_jobs", "publication_intents",
		"publication_cleanup", "deployment_removal_paths", "deployment_deletions", "archived_deployment_jobs",
		"project_webhooks", "project_gitea_webhooks", "webhook_deliveries",
	} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("schema table %q missing (count=%d, err=%v)", name, count, err)
		}
	}
	var version int64
	if err := db.QueryRow(`SELECT max(version_id) FROM goose_db_version WHERE is_applied=1`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("migration version=%d err=%v", version, err)
	}
	if err := initializeSchema(db, false); err != nil {
		t.Fatalf("current-version reopen: %v", err)
	}
}

func TestExistingUnversionedDatabaseIsNotModified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unversioned.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE retained(value TEXT NOT NULL); INSERT INTO retained(value) VALUES('preserve')`); err != nil {
		t.Fatal(err)
	}
	if err := initializeSchema(db, false); err == nil {
		t.Fatal("unversioned existing database unexpectedly accepted")
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM retained`).Scan(&value); err != nil || value != "preserve" {
		t.Fatalf("existing data changed or became unreadable: value=%q err=%v", value, err)
	}
	var history int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='goose_db_version'`).Scan(&history); err != nil || history != 0 {
		t.Fatalf("unsupported database was stamped: history=%d err=%v", history, err)
	}
}

func TestCLIRefusesUnversionedDatabaseBeforeCreatingExternalKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	db, err := sql.Open("sqlite3", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE retained(value TEXT NOT NULL); INSERT INTO retained(value) VALUES('preserve')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewGrantIssuer(cfg); err == nil {
		t.Fatal("CLI grant issuer accepted an unversioned existing database")
	}
	after, err := os.ReadFile(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("refusing an unversioned database changed its file contents")
	}
	for _, path := range []string{cfg.OPAQUEKeyPath, cfg.DatabasePath + ".repository-master-key"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("unsupported existing database touched external key path %q: %v", path, err)
		}
	}
}

func TestExistingStoreDoesNotRecreateMissingOPAQUEProfileBinding(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.Close(); err != nil {
		t.Fatal(err)
	}
	keyBefore, err := os.ReadFile(cfg.OPAQUEKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM opaque_profile WHERE singleton=1`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := NewGrantIssuer(cfg); err == nil {
		_ = unexpected.Close()
		t.Fatal("existing store silently recreated its missing OPAQUE profile binding")
	}
	keyAfter, err := os.ReadFile(cfg.OPAQUEKeyPath)
	if err != nil || string(keyBefore) != string(keyAfter) {
		t.Fatalf("existing OPAQUE key changed: read error=%v", err)
	}
	db, err = sql.Open("sqlite3", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var profiles int
	if err := db.QueryRow(`SELECT count(*) FROM opaque_profile`).Scan(&profiles); err != nil || profiles != 0 {
		t.Fatalf("missing OPAQUE binding was recreated: count=%d err=%v", profiles, err)
	}
}

func TestGrantIssuerRefusesOlderAndNewerSchemaWithoutGrantMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update string
	}{
		{name: "older", update: `UPDATE goose_db_version SET is_applied=0 WHERE version_id=1`},
		{name: "newer", update: `INSERT INTO goose_db_version(version_id,is_applied) VALUES(2,1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if _, err := db.Exec(tc.update); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if unexpected, err := NewGrantIssuer(cfg); err == nil {
				_ = unexpected.Close()
				t.Fatal("CLI grant issuer accepted schema mismatch")
			}
			db, err = sql.Open("sqlite3", cfg.DatabasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var grants int
			if err := db.QueryRow(`SELECT count(*) FROM registration_grants`).Scan(&grants); err != nil || grants != 0 {
				t.Fatalf("schema mismatch issued or changed grants: count=%d err=%v", grants, err)
			}
		})
	}
}
