package lib

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
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

func TestPendingUpgradeFinalizesIndependentVerifiedDatabaseBackupAndStaysLocked(t *testing.T) {
	dir := t.TempDir()
	publication := filepath.Join(dir, "published")
	if err := os.Mkdir(publication, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publication, "witness"), []byte("publication untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "state.db.projects")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "witness"), []byte("cache untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), DeploymentRoot: publication, InsecureLocalHTTP: true}
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.Close(); err != nil {
		t.Fatal(err)
	}
	masterBefore, err := os.ReadFile(cfg.DatabasePath + ".repository-master-key")
	if err != nil {
		t.Fatal(err)
	}
	opaqueBefore, err := os.ReadFile(cfg.OPAQUEKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", cfg.DatabasePath+"?_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE backup_wal_witness(value TEXT NOT NULL); INSERT INTO backup_wal_witness VALUES('committed-in-wal'); UPDATE goose_db_version SET is_applied=0 WHERE version_id=1`); err != nil {
		t.Fatal(err)
	}
	var marker maintenanceMarker
	if _, err := NewGrantIssuer(cfg); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("pending upgrade result=%v, want locked", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	markerData, err := os.ReadFile(cfg.DatabasePath + ".maintenance.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(markerData, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.SourceVersion != 0 || marker.TargetVersion != 1 || marker.Operation == "" || marker.Program == "" || marker.Phase != "backup-complete-upgrade-paused" {
		t.Fatalf("incomplete maintenance marker: %+v", marker)
	}
	backupInfo, err := os.Lstat(marker.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if backupInfo.Mode().Perm() != 0600 || backupInfo.Sys().(*syscall.Stat_t).Nlink != 1 {
		t.Fatalf("backup mode/link count: %v", backupInfo)
	}
	backup, err := sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: filepath.ToSlash(marker.Backup)}).String()+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var witness string
	if err := backup.QueryRow(`SELECT value FROM backup_wal_witness`).Scan(&witness); err != nil || witness != "committed-in-wal" {
		t.Fatalf("WAL witness=%q err=%v", witness, err)
	}
	if err := validateDatabaseIntegrity(backup); err != nil {
		t.Fatal(err)
	}
	masterAfter, err := os.ReadFile(cfg.DatabasePath + ".repository-master-key")
	if err != nil || string(masterBefore) != string(masterAfter) {
		t.Fatalf("external key changed: %v", err)
	}
	opaqueAfter, err := os.ReadFile(cfg.OPAQUEKeyPath)
	if err != nil || string(opaqueBefore) != string(opaqueAfter) {
		t.Fatalf("OPAQUE key changed: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(publication, "witness")); err != nil || string(b) != "publication untouched" {
		t.Fatalf("publication changed: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(cache, "witness")); err != nil || string(b) != "cache untouched" {
		t.Fatalf("cache changed: %v", err)
	}
	manifestData, err := os.ReadFile(marker.Backup + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest backupManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "complete" || manifest.SourceVersion != 0 || manifest.TargetVersion != 1 || len(manifest.SHA256) != 64 {
		t.Fatalf("invalid backup manifest: %+v", manifest)
	}
	backupBytes, err := os.ReadFile(marker.Backup)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(backupBytes)
	if manifest.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("backup manifest checksum does not match the finalized database")
	}
	backupEntries, err := os.ReadDir(filepath.Dir(marker.Backup))
	if err != nil {
		t.Fatal(err)
	}
	if len(backupEntries) != 2 {
		t.Fatalf("database-only backup directory contains unexpected artifacts: %v", backupEntries)
	}
	liveDB, err := sql.Open("sqlite3", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := liveDB.Exec(`UPDATE backup_wal_witness SET value='changed-live'`); err != nil {
		t.Fatal(err)
	}
	_ = liveDB.Close()
	if err := backup.QueryRow(`SELECT value FROM backup_wal_witness`).Scan(&witness); err != nil || witness != "committed-in-wal" {
		t.Fatalf("backup not independent: value=%q err=%v", witness, err)
	}
	if _, err := NewGrantIssuer(cfg); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("restart escaped maintenance lock: %v", err)
	}
	daemon, err := OpenDaemon(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	if !daemon.Locked() {
		t.Fatal("daemon left maintenance mode after preparation")
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		recorder := httptest.NewRecorder()
		daemon.Handler().ServeHTTP(recorder, httptest.NewRequest(method, "/api/projects", nil))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("locked %s status=%d", method, recorder.Code)
		}
		if method == http.MethodHead && recorder.Body.Len() != 0 {
			t.Fatalf("locked HEAD returned a body: %q", recorder.Body.String())
		}
	}
}

func TestPendingUpgradeFailureKeepsDurableMarkerWithoutCompletedBackup(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
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
	cfg.storageFault = func(phase string, _ *sql.DB) error {
		if phase == "backup-checkpoint" {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := NewGrantIssuer(cfg); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("failed preparation did not lock: %v", err)
	}
	if _, err := os.Stat(cfg.DatabasePath + ".maintenance.json"); err != nil {
		t.Fatalf("durable marker missing: %v", err)
	}
	entries, err := os.ReadDir(cfg.DatabasePath + ".upgrade-backups")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			t.Fatalf("injected checkpoint failure reported completed backup: %s", entry.Name())
		}
	}
}

func TestUpgradePreparationFaultsNeverUnlockAndOnlyFinalizedBackupsAreComplete(t *testing.T) {
	for _, phase := range []string{"backup-close", "backup-copy", "backup-integrity", "backup-finalize", "marker-update"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
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
			cfg.storageFault = func(got string, _ *sql.DB) error {
				if got == phase {
					return errors.New("injected " + phase)
				}
				return nil
			}
			if err := prepareUpgradeBackup(cfg, 0, 1, (&url.URL{Scheme: "file", Path: filepath.ToSlash(cfg.DatabasePath)}).String()); !errors.Is(err, ErrMaintenance) {
				t.Fatalf("fault did not keep installation locked: %v", err)
			}
			if _, err := os.Stat(cfg.DatabasePath + ".maintenance.json"); err != nil {
				t.Fatalf("marker missing after %s failure: %v", phase, err)
			}
			entries, err := os.ReadDir(cfg.DatabasePath + ".upgrade-backups")
			if err != nil {
				t.Fatal(err)
			}
			completed := false
			for _, entry := range entries {
				if filepath.Ext(entry.Name()) == ".json" {
					var m backupManifest
					b, err := os.ReadFile(filepath.Join(cfg.DatabasePath+".upgrade-backups", entry.Name()))
					if err != nil {
						t.Fatal(err)
					}
					if err = json.Unmarshal(b, &m); err != nil {
						t.Fatal(err)
					}
					completed = m.Status == "complete"
				}
			}
			if completed != (phase == "marker-update") {
				t.Fatalf("phase %s completed-copy status=%v", phase, completed)
			}
		})
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
