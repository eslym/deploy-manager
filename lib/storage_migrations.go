package lib

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const currentSchemaVersion int64 = 1

type pendingUpgrade struct{ source, target int64 }

func (p pendingUpgrade) Error() string {
	return fmt.Sprintf("supported schema upgrade pending (%d to %d)", p.source, p.target)
}

var requiredSchemaTables = []string{
	"registration_grants", "administrator", "opaque_profile", "repository_key_store", "repository_keys",
	"projects", "project_refs", "project_refresh_journal", "project_ssh_keys", "project_deletions",
	"ssh_host_trust", "project_lfs_settings", "deployments", "deployment_jobs", "publication_intents",
	"publication_cleanup", "deployment_removal_paths", "deployment_deletions", "archived_deployment_jobs",
	"project_webhooks", "project_gitea_webhooks", "webhook_deliveries",
}

func initializeSchema(db *sql.DB, fresh bool) error {
	return initializeSchemaWithConfig(db, fresh, Config{})
}

func schemaTargetVersion(cfg Config) int64 {
	if cfg.upgradeTargetVersion > 0 {
		return cfg.upgradeTargetVersion
	}
	return currentSchemaVersion
}

func migrationSource(cfg Config) (fs.FS, error) {
	if cfg.migrationFS != nil {
		return cfg.migrationFS, nil
	}
	return fs.Sub(migrationFiles, "migrations")
}

func initializeSchemaWithConfig(db *sql.DB, fresh bool, cfg Config) error {
	if err := validateSQLiteRuntime(db); err != nil {
		return err
	}
	files, err := migrationSource(cfg)
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return fmt.Errorf("configure embedded schema migrations: %w", err)
	}
	ctx := context.Background()
	var userObjects int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&userObjects); err != nil {
		return fmt.Errorf("inspect database before schema initialization: %w", err)
	}
	targetVersion := schemaTargetVersion(cfg)
	if fresh {
		if userObjects != 0 {
			return errors.New("database path contains existing or partial state; refusing initialization; use a new isolated location for explicit rebootstrap")
		}
		if _, err := provider.Up(ctx); err != nil {
			return fmt.Errorf("initialize fresh database schema: %w; use a new isolated data location for rebootstrap; existing keys were not replaced", err)
		}
	} else {
		var historyTable int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='goose_db_version'`).Scan(&historyTable); err != nil {
			return fmt.Errorf("inspect database migration history: %w", err)
		}
		if historyTable != 1 {
			return errors.New("existing database has no supported goose migration history; refusing to alter unversioned or partial state; use a new isolated location for explicit rebootstrap")
		}
		if pending, found, err := inspectPendingMigration(db, targetVersion); err != nil {
			return err
		} else if found {
			return pending
		}
		current, target, err := provider.GetVersions(ctx)
		if err != nil {
			return fmt.Errorf("database migration history is missing, malformed, or unsupported; refusing to modify existing state: %w", err)
		}
		if err := validateMigrationState(db, current, targetVersion); err != nil {
			return err
		}
		if current < targetVersion && target == targetVersion {
			return pendingUpgrade{current, targetVersion}
		}
		if current != targetVersion || target != targetVersion {
			return fmt.Errorf("database schema version mismatch (database %d, executable %d); refusing unsupported state", current, targetVersion)
		}
		pending, err := provider.HasPending(ctx)
		if err != nil || pending {
			if err == nil {
				err = errors.New("pending migrations are unavailable")
			}
			return fmt.Errorf("database migration history is not current; refusing to modify existing state: %w", err)
		}
	}
	return validateStoragePragmas(db)
}

func inspectVersionOneHistory(db *sql.DB) (bool, error) {
	rows, err := db.Query(`SELECT version_id,is_applied FROM goose_db_version ORDER BY id`)
	if err != nil {
		return false, fmt.Errorf("read database migration history: %w", err)
	}
	defer rows.Close()
	var got [][2]int64
	for rows.Next() {
		var v, a int64
		if err := rows.Scan(&v, &a); err != nil {
			return false, err
		}
		got = append(got, [2]int64{v, a})
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(got) == 2 && got[0] == [2]int64{0, 1} && got[1] == [2]int64{currentSchemaVersion, 0} {
		return true, nil
	}
	return false, nil
}

func validateVersionOneHistory(db *sql.DB) error {
	return validateMigrationHistory(db, currentSchemaVersion)
}

func validateMigrationHistory(db *sql.DB, target int64) error {
	rows, err := db.Query(`SELECT version_id,is_applied FROM goose_db_version ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read database migration history: %w", err)
	}
	defer rows.Close()
	want := make([][2]int64, 0, target+1)
	want = append(want, [2]int64{0, 1})
	for version := int64(1); version <= target; version++ {
		want = append(want, [2]int64{version, 1})
	}
	index := 0
	for rows.Next() {
		var version, applied int64
		if err := rows.Scan(&version, &applied); err != nil {
			return fmt.Errorf("read database migration history: %w", err)
		}
		if index >= len(want) || version != want[index][0] || applied != want[index][1] {
			return fmt.Errorf("database migration history is unknown or malformed at row %d (version=%d applied=%d); refusing to modify existing state", index, version, applied)
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read database migration history: %w", err)
	}
	if index != len(want) {
		return errors.New("database migration history is incomplete; refusing to modify existing state")
	}
	return nil
}

func validateStoragePragmas(db *sql.DB) error {
	if err := validateSQLiteRuntime(db); err != nil {
		return err
	}
	var journal string
	var synchronous, foreignKeys int
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		return err
	}
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		return err
	}
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return err
	}
	if journal != "wal" || synchronous != 2 || foreignKeys != 1 {
		return fmt.Errorf("SQLite safety settings unavailable (journal=%s synchronous=%d foreign_keys=%d)", journal, synchronous, foreignKeys)
	}
	if err := validateSchemaTables(db); err != nil {
		return err
	}
	return validateDatabaseIntegrity(db)
}

func preflightExistingSchema(dbURL string, configs ...Config) error {
	cfg := Config{}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	db, err := sql.Open("sqlite3", dbURL+"?mode=ro&_busy_timeout=1000")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := validateSQLiteRuntime(db); err != nil {
		return err
	}
	var journal string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		return err
	}
	if journal != "wal" {
		return fmt.Errorf("existing SQLite journal mode is %q, expected WAL; refusing to change existing state", journal)
	}
	var historyTable int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='goose_db_version'`).Scan(&historyTable); err != nil {
		return fmt.Errorf("inspect database migration history: %w", err)
	}
	if historyTable != 1 {
		return errors.New("existing database has no supported goose migration history; refusing to alter unversioned or partial state; use a new isolated location for explicit rebootstrap")
	}
	if pending, found, err := inspectPendingMigration(db, schemaTargetVersion(cfg)); err != nil {
		return err
	} else if found {
		return pending
	}
	files, err := migrationSource(cfg)
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return err
	}
	defer provider.Close()
	current, target, err := provider.GetVersions(context.Background())
	if err != nil {
		return fmt.Errorf("database migration history is malformed or unsupported: %w", err)
	}
	wantTarget := schemaTargetVersion(cfg)
	if err := validateMigrationState(db, current, wantTarget); err != nil {
		return err
	}
	if current < wantTarget && target == wantTarget {
		return pendingUpgrade{current, wantTarget}
	}
	if current != wantTarget || target != wantTarget {
		return fmt.Errorf("database schema version mismatch (database %d, executable %d); refusing unsupported state", current, wantTarget)
	}
	if pending, err := provider.HasPending(context.Background()); err != nil || pending {
		if err == nil {
			err = errors.New("pending migrations are unavailable")
		}
		return fmt.Errorf("database migration history is not current; refusing to modify existing state: %w", err)
	}
	if err := validateSchemaTables(db); err != nil {
		return err
	}
	return validateDatabaseIntegrity(db)
}

func inspectPendingMigration(db *sql.DB, target int64) (pendingUpgrade, bool, error) {
	rows, err := db.Query(`SELECT version_id,is_applied FROM goose_db_version ORDER BY id`)
	if err != nil {
		return pendingUpgrade{}, false, fmt.Errorf("read database migration history: %w", err)
	}
	defer rows.Close()
	var got [][2]int64
	for rows.Next() {
		var v, a int64
		if err := rows.Scan(&v, &a); err != nil {
			return pendingUpgrade{}, false, err
		}
		got = append(got, [2]int64{v, a})
	}
	if err := rows.Err(); err != nil {
		return pendingUpgrade{}, false, err
	}
	if len(got) == 0 || got[len(got)-1] != [2]int64{target, 0} {
		return pendingUpgrade{}, false, nil
	}
	if err := rows.Close(); err != nil {
		return pendingUpgrade{}, false, err
	}
	if err := validateMigrationState(db, target-1, target); err != nil {
		return pendingUpgrade{}, false, err
	}
	return pendingUpgrade{target - 1, target}, true, nil
}

func validateMigrationState(db *sql.DB, appliedThrough, expectedTarget int64) error {
	rows, err := db.Query(`SELECT version_id,is_applied FROM goose_db_version ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read database migration history: %w", err)
	}
	defer rows.Close()
	var got [][2]int64
	for rows.Next() {
		var v, a int64
		if err := rows.Scan(&v, &a); err != nil {
			return err
		}
		got = append(got, [2]int64{v, a})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	want := make([][2]int64, 0, appliedThrough+2)
	want = append(want, [2]int64{0, 1})
	for v := int64(1); v <= appliedThrough; v++ {
		want = append(want, [2]int64{v, 1})
	}
	if appliedThrough < expectedTarget && len(got) == len(want)+1 && got[len(got)-1] == [2]int64{appliedThrough + 1, 0} {
		want = append(want, [2]int64{appliedThrough + 1, 0})
	}
	if len(got) != len(want) {
		return errors.New("database migration history is incomplete or malformed; refusing to modify existing state")
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("database migration history is unknown or malformed at row %d; refusing to modify existing state", i)
		}
	}
	return nil
}

func validateSQLiteRuntime(db *sql.DB) error {
	var sqliteVersion string
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&sqliteVersion); err != nil {
		return err
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(sqliteVersion, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return fmt.Errorf("unrecognized SQLite runtime version %q", sqliteVersion)
	}
	if major < 3 || major == 3 && (minor < 51 || minor == 51 && patch < 3) {
		return fmt.Errorf("SQLite %s predates the WAL-reset fix (3.51.3); refusing startup", sqliteVersion)
	}
	return nil
}

func validateSchemaTables(db *sql.DB) error {
	for _, name := range requiredSchemaTables {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("required schema table %q is missing; refusing startup", name)
		}
	}
	return nil
}

func validateDatabaseIntegrity(db *sql.DB) error {
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("SQLite integrity check failed: %s", integrity)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("SQLite foreign-key integrity check failed")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}
