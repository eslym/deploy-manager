package lib

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sys/unix"
)

// Keep the historical migrations in synthetic upgrade tests so an applied v2
// database is never accidentally treated as a v1 database.
func testUpgradeMigrations(t *testing.T, extras fstest.MapFS) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{}
	for _, name := range []string{"00001_initial.sql", "00002_publication_name.sql"} {
		data, err := fs.ReadFile(migrationFiles, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = &fstest.MapFile{Data: data}
	}
	for name, file := range extras {
		files[name] = file
	}
	return files
}

// A real v1 installation is required when exercising the old v0-to-v1
// interrupted-upgrade history; unapplying v1 on a v2 store corrupts history.
func testVersionOneConfig(t *testing.T, cfg Config) Config {
	t.Helper()
	files := testUpgradeMigrations(t, nil)
	delete(files, "00002_publication_name.sql")
	cfg.migrationFS = files
	cfg.upgradeTargetVersion = 1
	return cfg
}

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

func TestProtectedUpgradeVerifiesAndDurablyUnlocksBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.Close(); err != nil {
		t.Fatal(err)
	}
	markerPath, err := MaintenancePath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	marker := maintenanceMarker{Version: 1, SourceVersion: currentSchemaVersion, TargetVersion: currentSchemaVersion, Program: "fixture", Operation: "fixture", Backup: filepath.Join(dir, "verified-backup.db"), Phase: "migration"}
	if err := durableJSONReplace(markerPath, marker); err != nil {
		t.Fatal(err)
	}
	if err := applyProtectedUpgrade(cfg, (&url.URL{Scheme: "file", Path: filepath.ToSlash(cfg.DatabasePath)}).String(), markerPath, marker); err != nil {
		t.Fatalf("protected completion failed: %v", err)
	}
	if _, err := os.Lstat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("maintenance marker remains after verified completion: %v", err)
	}
	readyIssuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatalf("readiness was not restored after durable unlock: %v", err)
	}
	if err := readyIssuer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPendingUpgradeCompletesThroughStorageStartupWithFixtureMigration(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true,
		upgradeTargetVersion: currentSchemaVersion + 1,
	}
	cfg.migrationFS = testUpgradeMigrations(t, fstest.MapFS{
		"00003_forward.sql": {Data: []byte("-- +goose Up\nCREATE TABLE fixture_upgrade(value TEXT NOT NULL);\nINSERT INTO fixture_upgrade VALUES('completed');\n-- +goose Down\nDROP TABLE fixture_upgrade;\n")},
	})
	first, err := NewGrantIssuer(Config{DatabasePath: cfg.DatabasePath, OPAQUEKeyPath: cfg.OPAQUEKeyPath, InsecureLocalHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestProgramProvenance(t, cfg, currentSchemaVersion)
	before, err := os.ReadFile(cfg.OPAQUEKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	lockChecked := false
	cfg.storageFault = func(phase string, _ *sql.DB) error {
		if phase != "upgrade-migration" {
			return nil
		}
		f, err := os.OpenFile(cfg.DatabasePath+".access.lock", os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return errors.New("CLI shared access overlapped protected migration")
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		lockChecked = true
		return nil
	}
	app, err := New(cfg)
	if err != nil {
		t.Fatalf("startup did not finish protected upgrade: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if !lockChecked {
		t.Fatal("protected migration phase was not reached")
	}
	if _, err := os.Lstat(cfg.DatabasePath + ".maintenance.json"); !os.IsNotExist(err) {
		t.Fatalf("successful upgrade left marker: %v", err)
	}
	after, err := os.ReadFile(cfg.OPAQUEKeyPath)
	if err != nil || string(before) != string(after) {
		t.Fatalf("upgrade changed external OPAQUE key: %v", err)
	}
	db, err := sql.Open("sqlite3", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT value FROM fixture_upgrade`).Scan(&value); err != nil || value != "completed" {
		t.Fatalf("fixture migration value=%q err=%v", value, err)
	}
	backupDir := cfg.DatabasePath + ".upgrade-backups"
	entries, err := os.ReadDir(backupDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("verified backup was not retained: entries=%d err=%v", len(entries), err)
	}
}

func TestProtectedUpgradeCancellationRetainsMarker(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.Close(); err != nil {
		t.Fatal(err)
	}
	markerPath, err := MaintenancePath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	marker := maintenanceMarker{Version: 1, SourceVersion: currentSchemaVersion, TargetVersion: currentSchemaVersion, Program: "fixture", Operation: "fixture", Backup: filepath.Join(dir, "verified-backup.db"), Phase: "migration"}
	if err := durableJSONReplace(markerPath, marker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg.upgradeContext = ctx
	if err := applyProtectedUpgrade(cfg, (&url.URL{Scheme: "file", Path: filepath.ToSlash(cfg.DatabasePath)}).String(), markerPath, marker); err == nil {
		t.Fatal("cancelled protected upgrade succeeded")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("cancellation cleared maintenance marker: %v", err)
	}
}

func TestPendingUpgradeValidatesExternalKeyBindingBeforeEstablishingMaintenance(t *testing.T) {
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
	if _, err := db.Exec(`UPDATE goose_db_version SET is_applied=0 WHERE version_id=?`, currentSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	masterPath := cfg.DatabasePath + ".repository-master-key"
	key, err := os.ReadFile(masterPath)
	if err != nil {
		t.Fatal(err)
	}
	key[0] ^= 0xff
	if err := os.WriteFile(masterPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil || errors.Is(err, ErrMaintenance) {
		t.Fatalf("mismatched key binding should fail before entering maintenance: %v", err)
	}
	if _, err := os.Lstat(cfg.DatabasePath + ".maintenance.json"); !os.IsNotExist(err) {
		t.Fatalf("key validation failure wrote maintenance marker: %v", err)
	}
	if _, err := os.Stat(cfg.DatabasePath + ".upgrade-backups"); !os.IsNotExist(err) {
		t.Fatalf("key validation failure created backup directory: %v", err)
	}
}

func TestProtectedUpgradeFailuresRemainLockedAndDoNotRunLaterPhases(t *testing.T) {
	for _, test := range []struct{ fault, marker string }{
		{"upgrade-vacuum", "vacuum"}, {"upgrade-migration", "migration"}, {"upgrade-verify", "verification"},
		{"marker-remove", "unlock"}, {"marker-remove-sync", "unlock"},
		{"marker-phase-vacuum", "migration"},
	} {
		failedPhase := test.fault
		t.Run(failedPhase, func(t *testing.T) {
			dir := t.TempDir()
			cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
			issuer, err := NewGrantIssuer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := issuer.Close(); err != nil {
				t.Fatal(err)
			}
			markerPath, err := MaintenancePath(cfg)
			if err != nil {
				t.Fatal(err)
			}
			marker := maintenanceMarker{Version: 1, SourceVersion: currentSchemaVersion, TargetVersion: currentSchemaVersion, Program: "fixture", Operation: "fixture", Backup: filepath.Join(dir, "verified-backup.db"), Phase: "migration"}
			if err := durableJSONReplace(markerPath, marker); err != nil {
				t.Fatal(err)
			}
			laterPhaseRan := false
			cfg.storageFault = func(phase string, _ *sql.DB) error {
				if phase == failedPhase {
					return errors.New("injected failure")
				}
				if failedPhase == "upgrade-vacuum" && phase == "upgrade-migration" {
					laterPhaseRan = true
				}
				return nil
			}
			if err := applyProtectedUpgrade(cfg, (&url.URL{Scheme: "file", Path: filepath.ToSlash(cfg.DatabasePath)}).String(), markerPath, marker); err == nil {
				t.Fatal("injected protected phase failure was ignored")
			}
			if failedPhase == "upgrade-vacuum" && laterPhaseRan {
				t.Fatal("migration ran after VACUUM failure")
			}
			if _, err := os.Lstat(markerPath); err != nil {
				t.Fatalf("failure cleared maintenance marker: %v", err)
			}
			markerBytes, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			var persisted maintenanceMarker
			if err := json.Unmarshal(markerBytes, &persisted); err != nil {
				t.Fatal(err)
			}
			if persisted.Phase != test.marker {
				t.Fatalf("failure phase=%q, want durable marker phase %q", persisted.Phase, test.marker)
			}
			daemon, err := OpenDaemon(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer daemon.Close()
			if diagnostic := daemon.Diagnostic(); !strings.Contains(diagnostic, fmt.Sprintf("source=%d target=%d", currentSchemaVersion, currentSchemaVersion)) || !strings.Contains(diagnostic, "operation=fixture") || !strings.Contains(diagnostic, "backup=") || !strings.Contains(diagnostic, "phase=") {
				t.Fatalf("operator diagnostic lacks recovery context: %q", diagnostic)
			}
			recorder := httptest.NewRecorder()
			daemon.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), persisted.Backup) {
				t.Fatalf("maintenance HTTP response exposed diagnostics or escaped lock: code=%d body=%q", recorder.Code, recorder.Body.String())
			}
			if _, err := NewGrantIssuer(cfg); !errors.Is(err, ErrMaintenance) {
				t.Fatalf("failed upgrade did not remain locked: %v", err)
			}
		})
	}
}

func TestDaemonDiagnosticQuotesUntrustedBackupPath(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key")}
	backup := filepath.Join(dir, "backup\nforged\r\x1b[31mline")
	markerPath, err := MaintenancePath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	marker := maintenanceMarker{Version: 1, SourceVersion: currentSchemaVersion, TargetVersion: currentSchemaVersion + 1, Program: "fixture", Operation: "op-123", Backup: backup, Phase: "migration"}
	if err := durableJSONReplace(markerPath, marker); err != nil {
		t.Fatal(err)
	}
	daemon, err := OpenDaemon(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	diagnostic := daemon.Diagnostic()
	if strings.Contains(diagnostic, "\n") || strings.Contains(diagnostic, "\r") {
		t.Fatalf("diagnostic spans multiple log lines: %q", diagnostic)
	}
	for _, r := range diagnostic {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("diagnostic contains raw control character U+%04X: %q", r, diagnostic)
		}
	}
	if !strings.Contains(diagnostic, "backup="+strconv.Quote(backup)) {
		t.Fatalf("backup path was not safely quoted in diagnostic: %q", diagnostic)
	}
}

func TestProtectedUpgradeRunsEmbeddedForwardMigrationAndRetainsPartialProgress(t *testing.T) {
	for _, tc := range []struct {
		name       string
		migrations fstest.MapFS
		target     int64
		wantValue  bool
	}{
		{name: "success", target: currentSchemaVersion + 1, wantValue: true, migrations: fstest.MapFS{
			"00003_forward.sql": {Data: []byte("-- +goose Up\nCREATE TABLE upgrade_witness(value TEXT NOT NULL);\nINSERT INTO upgrade_witness VALUES('migrated');\n-- +goose Down\nDROP TABLE upgrade_witness;\n")},
		}},
		{name: "partial multi migration", target: currentSchemaVersion + 2, migrations: fstest.MapFS{
			"00003_forward.sql": {Data: []byte("-- +goose Up\nCREATE TABLE upgrade_witness(value TEXT NOT NULL);\nINSERT INTO upgrade_witness VALUES('first committed migration');\n-- +goose Down\nDROP TABLE upgrade_witness;\n")},
			"00004_fail.sql":    {Data: []byte("-- +goose Up\nCREATE TABLE rolled_back(value TEXT NOT NULL);\nINSERT INTO missing_table VALUES('failure');\n-- +goose Down\nDROP TABLE rolled_back;\n")},
		}},
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
			cfg.migrationFS = testUpgradeMigrations(t, tc.migrations)
			markerPath, err := MaintenancePath(cfg)
			if err != nil {
				t.Fatal(err)
			}
			marker := maintenanceMarker{Version: 1, SourceVersion: currentSchemaVersion, TargetVersion: tc.target, Program: "fixture", Operation: "fixture", Backup: filepath.Join(dir, "verified-backup.db"), Phase: "migration"}
			if err := durableJSONReplace(markerPath, marker); err != nil {
				t.Fatal(err)
			}
			err = applyProtectedUpgrade(cfg, (&url.URL{Scheme: "file", Path: filepath.ToSlash(cfg.DatabasePath)}).String(), markerPath, marker)
			db, openErr := sql.Open("sqlite3", cfg.DatabasePath)
			if openErr != nil {
				t.Fatal(openErr)
			}
			defer db.Close()
			if tc.wantValue {
				if err != nil {
					t.Fatalf("upgrade failed: %v", err)
				}
				var value string
				if err := db.QueryRow(`SELECT value FROM upgrade_witness`).Scan(&value); err != nil || value != "migrated" {
					t.Fatalf("migration value=%q err=%v", value, err)
				}
				if _, err := os.Lstat(markerPath); !os.IsNotExist(err) {
					t.Fatalf("successful upgrade left marker: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("failing second migration unexpectedly succeeded")
				}
				var value string
				if err := db.QueryRow(`SELECT value FROM upgrade_witness`).Scan(&value); err != nil || value != "first committed migration" {
					t.Fatalf("earlier migration was not preserved: value=%q err=%v", value, err)
				}
				if _, err := os.Stat(markerPath); err != nil {
					t.Fatalf("partial upgrade cleared marker: %v", err)
				}
				var secondApplied int
				if err := db.QueryRow(`SELECT is_applied FROM goose_db_version WHERE version_id=?`, currentSchemaVersion+1).Scan(&secondApplied); err != nil || secondApplied != 1 {
					t.Fatalf("first migration history=%d err=%v", secondApplied, err)
				}
				var rolledBack int
				if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='rolled_back'`).Scan(&rolledBack); err != nil || rolledBack != 0 {
					t.Fatalf("failed transactional migration left DDL behind: count=%d err=%v", rolledBack, err)
				}
			}
		})
	}
}

func TestProtectedForeignKeyRebuildUsesSingleConnectionAndRestoresEnforcement(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	cfg := Config{DatabasePath: dbPath, OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE rebuild_parent(id INTEGER PRIMARY KEY, name TEXT NOT NULL); CREATE TABLE rebuild_child(id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES rebuild_parent(id) ON DELETE RESTRICT); INSERT INTO rebuild_parent VALUES(1,'parent'); INSERT INTO rebuild_child VALUES(1,1);`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.migrationFS = testUpgradeMigrations(t, fstest.MapFS{
		"00003_rebuild.sql": {Data: []byte(`-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys=OFF;
BEGIN;
CREATE TABLE rebuild_parent_new(id INTEGER PRIMARY KEY, name TEXT NOT NULL, extra TEXT NOT NULL DEFAULT '');
INSERT INTO rebuild_parent_new(id,name) SELECT id,name FROM rebuild_parent;
CREATE TABLE rebuild_child_new(id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES rebuild_parent_new(id) ON DELETE RESTRICT);
INSERT INTO rebuild_child_new(id,parent_id) SELECT id,parent_id FROM rebuild_child;
DROP TABLE rebuild_child;
DROP TABLE rebuild_parent;
ALTER TABLE rebuild_parent_new RENAME TO rebuild_parent;
ALTER TABLE rebuild_child_new RENAME TO rebuild_child;
COMMIT;
PRAGMA foreign_keys=ON;
-- +goose Down
SELECT 1;
`)},
	})
	markerPath, err := MaintenancePath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	marker := maintenanceMarker{Version: 1, SourceVersion: currentSchemaVersion, TargetVersion: currentSchemaVersion + 1, Program: "fixture", Operation: "fixture", Backup: filepath.Join(dir, "backup.db"), Phase: "migration"}
	if err := durableJSONReplace(markerPath, marker); err != nil {
		t.Fatal(err)
	}
	if err := applyProtectedUpgrade(cfg, (&url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath)}).String(), markerPath, marker); err != nil {
		t.Fatalf("single-connection foreign-key rebuild deadlocked or failed: %v", err)
	}
	db, err = sql.Open("sqlite3", dbPath+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var enabled, children int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign-key enforcement=%d err=%v", enabled, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM rebuild_child`).Scan(&children); err != nil || children != 1 {
		t.Fatalf("rebuilt child count=%d err=%v", children, err)
	}
	if err := validateDatabaseIntegrity(db); err != nil {
		t.Fatalf("rebuilt schema failed foreign-key validation: %v", err)
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
	cfg = testVersionOneConfig(t, cfg)
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
	if _, err := db.Exec(`CREATE TABLE backup_wal_witness(value TEXT NOT NULL); INSERT INTO backup_wal_witness VALUES('committed-in-wal')`); err != nil {
		t.Fatal(err)
	}
	writeTestProgramProvenance(t, cfg, 1)
	cfg.migrationFS = nil
	cfg.upgradeTargetVersion = 0
	cfg.storageFault = func(phase string, _ *sql.DB) error {
		if phase == "upgrade-verify" {
			return errors.New("injected verification failure")
		}
		return nil
	}
	var marker maintenanceMarker
	if _, err := New(cfg); !errors.Is(err, ErrMaintenance) {
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
	if marker.SourceVersion != 1 || marker.TargetVersion != currentSchemaVersion || marker.Operation == "" || marker.Program == "" || marker.Phase != "verification" {
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
	if manifest.Status != "complete" || manifest.SourceVersion != 1 || manifest.TargetVersion != currentSchemaVersion || len(manifest.SHA256) != 64 {
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
	cfg = testVersionOneConfig(t, cfg)
	issuer, err := NewGrantIssuer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = issuer.Close()
	db, err := sql.Open("sqlite3", cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	writeTestProgramProvenance(t, cfg, 1)
	cfg.migrationFS = nil
	cfg.upgradeTargetVersion = 0
	cfg.storageFault = func(phase string, _ *sql.DB) error {
		if phase == "backup-checkpoint" {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := New(cfg); !errors.Is(err, ErrMaintenance) {
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
			cfg = testVersionOneConfig(t, cfg)
			issuer, err := NewGrantIssuer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_ = issuer.Close()
			db, err := sql.Open("sqlite3", cfg.DatabasePath)
			if err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			provenance, err := currentProgramProvenance(cfg, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := durableJSONReplace(provenancePath(cfg), provenance); err != nil {
				t.Fatal(err)
			}
			cfg.storageFault = func(got string, _ *sql.DB) error {
				if got == phase {
					return errors.New("injected " + phase)
				}
				return nil
			}
			if err := prepareUpgradeBackup(cfg, 1, currentSchemaVersion, (&url.URL{Scheme: "file", Path: filepath.ToSlash(cfg.DatabasePath)}).String()); !errors.Is(err, ErrMaintenance) {
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

func TestBackupCopyCrashChild(t *testing.T) {
	if os.Getenv("DM_BACKUP_CRASH_CHILD") != "1" {
		return
	}
	database, opaqueKey := os.Getenv("DM_BACKUP_CRASH_DATABASE"), os.Getenv("DM_BACKUP_CRASH_OPAQUE")
	cfg := Config{DatabasePath: database, OPAQUEKeyPath: opaqueKey, InsecureLocalHTTP: true}
	cfg.storageFault = func(phase string, _ *sql.DB) error {
		if phase == "backup-copy-chunk" {
			_ = unix.Kill(os.Getpid(), unix.SIGKILL)
		}
		return nil
	}
	dbURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(database)}).String()
	if err := prepareUpgradeBackup(cfg, 1, currentSchemaVersion, dbURL); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	os.Exit(0)
}

func TestRealProcessCrashDuringPartialBackupCopyStaysLocked(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DatabasePath: filepath.Join(dir, "state.db"), OPAQUEKeyPath: filepath.Join(dir, "opaque.key"), InsecureLocalHTTP: true}
	cfg = testVersionOneConfig(t, cfg)
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
	if _, err := db.Exec(`CREATE TABLE copy_witness(payload BLOB NOT NULL); INSERT INTO copy_witness VALUES(zeroblob(2097152))`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	databaseIdentity, err := filesystemPathIdentity(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	provenance := programProvenance{Format: 1, Database: cfg.DatabasePath, DatabaseIdentity: databaseIdentity, Schema: 1, Executable: "/test/prior", Version: "fixture-v1", SHA256: strings.Repeat("a", 64)}
	if err := durableJSONReplace(provenancePath(cfg), provenance); err != nil {
		t.Fatal(err)
	}
	backupDir := cfg.DatabasePath + ".upgrade-backups"
	if err := ensurePrivateDirectory(backupDir); err != nil {
		t.Fatal(err)
	}
	completed := filepath.Join(backupDir, "prior-completed.db")
	if err := os.WriteFile(completed, []byte("immutable completed backup"), 0600); err != nil {
		t.Fatal(err)
	}
	completedManifest := completed + ".json"
	if err := os.WriteFile(completedManifest, []byte(`{"status":"complete","sha256":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	completedBefore, _ := os.ReadFile(completed)
	completedManifestBefore, _ := os.ReadFile(completedManifest)
	databaseBefore, _ := os.ReadFile(cfg.DatabasePath)
	keyBefore, _ := os.ReadFile(cfg.OPAQUEKeyPath)
	child := exec.Command(os.Args[0], "-test.run=^TestBackupCopyCrashChild$")
	child.Env = append(os.Environ(), "DM_BACKUP_CRASH_CHILD=1", "DM_BACKUP_CRASH_DATABASE="+cfg.DatabasePath, "DM_BACKUP_CRASH_OPAQUE="+cfg.OPAQUEKeyPath)
	childOutput, childErr := child.CombinedOutput()
	if childErr == nil {
		t.Fatalf("backup child unexpectedly survived SIGKILL: %s", childOutput)
	}
	t.Logf("backup child terminated: %v; %s", childErr, childOutput)
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	partialFound := false
	for _, entry := range entries {
		path := filepath.Join(backupDir, entry.Name())
		if strings.HasSuffix(entry.Name(), ".db") && entry.Name() != "prior-completed.db" {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() > 0 && info.Size() < int64(len(databaseBefore)) {
				partialFound = true
			}
		}
		if strings.HasSuffix(entry.Name(), ".json") && entry.Name() != filepath.Base(completedManifest) {
			t.Fatalf("partial backup received a finalized manifest: %s", entry.Name())
		}
	}
	if !partialFound {
		t.Fatalf("process was not killed after writing a partial nontrivial backup (source=%d entries=%v)", len(databaseBefore), entries)
	}
	if _, err := os.Stat(cfg.DatabasePath + ".maintenance.json"); err != nil {
		t.Fatalf("durable marker missing after SIGKILL: %v", err)
	}
	locked, err := OpenDaemon(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !locked.Locked() {
		t.Fatal("restart opened application after partial-copy crash")
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	for path, before := range map[string][]byte{cfg.DatabasePath: databaseBefore, cfg.OPAQUEKeyPath: keyBefore, completed: completedBefore, completedManifest: completedManifestBefore} {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("locked restart touched %s: %v", path, err)
		}
	}
}

func writeTestProgramProvenance(t *testing.T, cfg Config, schema int64) {
	t.Helper()
	provenance, err := currentProgramProvenance(cfg, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := durableJSONReplace(provenancePath(cfg), provenance); err != nil {
		t.Fatal(err)
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
		{name: "older", update: fmt.Sprintf(`UPDATE goose_db_version SET is_applied=0 WHERE version_id=%d`, currentSchemaVersion)},
		{name: "newer", update: fmt.Sprintf(`INSERT INTO goose_db_version(version_id,is_applied) VALUES(%d,1)`, currentSchemaVersion+1)},
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
