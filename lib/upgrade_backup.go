package lib

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	entdialect "entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/bytemare/opaque"
	"github.com/eslym/deploy-manager/lib/ent"
	"github.com/pressly/goose/v3"
	"golang.org/x/sys/unix"
)

type backupManifest struct {
	Status        string            `json:"status"`
	SourceVersion int64             `json:"source_version"`
	TargetVersion int64             `json:"target_version"`
	Program       string            `json:"program"` // upgrader, not rollback program
	PriorProgram  programProvenance `json:"prior_program"`
	Created       string            `json:"created"`
	SHA256        string            `json:"sha256"`
}

// The caller holds exclusive cross-process storage access throughout this
// backup, migration, verification, and durable-unlock sequence.
func prepareUpgradeBackup(cfg Config, source, target int64, dbURL string) (retErr error) {
	markerPath, err := MaintenancePath(cfg)
	if err != nil {
		return err
	}
	dbPath, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return err
	}
	markerPath, err = filepath.Abs(markerPath)
	if err != nil {
		return err
	}
	if err := validateBackupPaths(cfg, markerPath); err != nil {
		return err
	}
	if err := validateExistingKeyBindings(cfg, dbURL); err != nil {
		return fmt.Errorf("existing key bindings failed pre-upgrade validation: %w", err)
	}
	program, _ := os.Executable()
	program = filepath.Base(program)
	if info, err := buildinfo.ReadFile(os.Args[0]); err == nil && info.Main.Version != "" {
		program += "@" + info.Main.Version
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				program += "#" + setting.Value
				break
			}
		}
	}
	priorProgram, err := readPriorProgramProvenance(cfg, source)
	if err != nil {
		return err
	}
	var operationBytes [16]byte
	if _, err := rand.Read(operationBytes[:]); err != nil {
		return err
	}
	operation := hex.EncodeToString(operationBytes[:])
	backupDir := dbPath + ".upgrade-backups"
	if err := ensurePrivateDirectory(backupDir); err != nil {
		return err
	}
	backupPath := filepath.Join(backupDir, fmt.Sprintf("%d-to-%d-%s.db", source, target, operation))
	marker := maintenanceMarker{Version: 1, SourceVersion: source, TargetVersion: target, Program: program, Operation: operation, Backup: backupPath, Phase: "backup-preparation"}
	if cfg.storageFault != nil {
		if err := cfg.storageFault("marker-create", nil); err != nil {
			return fmt.Errorf("cannot durably establish maintenance lock; no database maintenance performed: %w", err)
		}
	}
	if err := durableJSONReplace(markerPath, marker); err != nil {
		return fmt.Errorf("cannot durably establish maintenance lock; no database maintenance performed: %w", err)
	}
	defer func() {
		if retErr != nil && !errors.Is(retErr, ErrMaintenance) {
			retErr = errors.Join(ErrMaintenance, retErr)
		}
	}()
	phase := func(name string, db *sql.DB) error {
		if cfg.upgradeContext != nil {
			if err := cfg.upgradeContext.Err(); err != nil {
				return err
			}
		}
		if cfg.storageFault != nil {
			return cfg.storageFault(name, db)
		}
		return nil
	}
	if err := phase("backup-checkpoint", nil); err != nil {
		return fmt.Errorf("upgrade backup checkpoint failed; verified recovery copy is not guaranteed: %w", err)
	}
	db, err := sql.Open("sqlite3", dbURL+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=1000")
	if err != nil {
		return fmt.Errorf("open database for checkpoint; verified recovery copy is not guaranteed: %w", err)
	}
	db.SetMaxOpenConns(1)
	var busy, logFrames, checkpointed int
	err = db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed)
	if err == nil && (busy != 0 || logFrames != checkpointed) {
		err = fmt.Errorf("SQLite checkpoint incomplete (busy=%d log=%d checkpointed=%d)", busy, logFrames, checkpointed)
	}
	if err == nil {
		err = phase("backup-close", db)
	}
	closeErr := db.Close()
	if err == nil {
		err = closeErr
	} else {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return fmt.Errorf("upgrade checkpoint/close failed; verified recovery copy is not guaranteed: %w", err)
	}
	if err := checkDatabaseSidecars(dbPath); err != nil {
		return fmt.Errorf("unsafe SQLite sidecar state; verified recovery copy is not guaranteed: %w", err)
	}
	if err := copyExclusive(dbPath, backupPath, phase); err != nil {
		return fmt.Errorf("upgrade backup copy failed; verified recovery copy is not guaranteed: %w", err)
	}
	checksum, err := verifyStandaloneBackup(backupPath, source, phase)
	if err != nil {
		return fmt.Errorf("upgrade backup verification failed; verified recovery copy is not guaranteed: %w", err)
	}
	manifest := backupManifest{Status: "complete", SourceVersion: source, TargetVersion: target, Program: program, PriorProgram: priorProgram, Created: time.Now().UTC().Format(time.RFC3339Nano), SHA256: checksum}
	if err := phase("backup-finalize", nil); err != nil {
		return fmt.Errorf("verified backup could not be durably finalized; recovery copy is not guaranteed: %w", err)
	}
	if err := durableJSONReplace(backupPath+".json", manifest); err != nil {
		return fmt.Errorf("verified backup could not be durably finalized; recovery copy is not guaranteed: %w", err)
	}
	// Refresh phase information; inability to update cannot unlock the store.
	marker.Phase = "backup-complete-upgrade-paused"
	if err := phase("marker-update", nil); err != nil {
		return fmt.Errorf("verified backup %s exists, but maintenance marker update failed: %w", backupPath, err)
	}
	if err := durableJSONReplace(markerPath, marker); err != nil {
		return fmt.Errorf("verified backup %s exists, but maintenance marker update failed: %w", backupPath, err)
	}
	if err := applyProtectedUpgrade(cfg, dbURL, markerPath, marker); err != nil {
		return errors.Join(ErrMaintenance, err)
	}
	return nil
}

func validateExistingKeyBindings(cfg Config, dbURL string) error {
	db, err := sql.Open("sqlite3", dbURL+"?mode=ro&_busy_timeout=1000")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	masterPath := cfg.RepositoryKeyPath
	if masterPath == "" {
		masterPath = cfg.DatabasePath + ".repository-master-key"
	}
	master, err := loadRepositoryMasterKey(masterPath, true)
	if err != nil {
		return err
	}
	defer clear(master)
	store := ent.NewClient(ent.Driver(entsql.OpenDB(entdialect.SQLite, db)))
	if err := validateRepositoryKeyStore(store, db, master); err != nil {
		return err
	}
	configuration := opaque.DefaultConfiguration()
	_, generation, err := loadOPAQUEServerKey(configuration, cfg.OPAQUEKeyPath, true)
	if err != nil {
		return err
	}
	const profile = "RFC9807-OPAQUE-3DH;ristretto255-SHA512;HKDF-SHA512;HMAC-SHA512;Argon2id-v19-i1-m65536-p4;context-empty;identities-default"
	var storedProfile, keyGeneration string
	var profileGeneration int
	if err := db.QueryRow(`SELECT profile_generation,key_generation,profile FROM opaque_profile WHERE singleton=1`).Scan(&profileGeneration, &keyGeneration, &storedProfile); err != nil {
		return errors.New("OPAQUE key/profile binding is missing or unavailable")
	}
	if profileGeneration != 1 || keyGeneration != generation || storedProfile != profile {
		return errors.New("OPAQUE key/profile generation mismatch")
	}
	var accountGeneration int
	err = db.QueryRow(`SELECT profile_generation FROM administrator WHERE id=1`).Scan(&accountGeneration)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return errors.New("administrator/profile binding is unavailable")
	}
	if err == nil && accountGeneration != profileGeneration {
		return errors.New("administrator/profile generation mismatch")
	}
	return nil
}

func applyProtectedUpgrade(cfg Config, dbURL, markerPath string, marker maintenanceMarker) error {
	ctx := cfg.upgradeContext
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	phase := func(name string, db *sql.DB) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cfg.storageFault != nil {
			return cfg.storageFault(name, db)
		}
		return nil
	}
	setPhase := func(name string) error {
		if cfg.storageFault != nil {
			if err := cfg.storageFault("marker-phase-"+name, nil); err != nil {
				return fmt.Errorf("cannot durably record upgrade phase %s; previous maintenance marker retained: %w", name, err)
			}
		}
		marker.Phase = name
		if err := durableJSONReplace(markerPath, marker); err != nil {
			return fmt.Errorf("cannot durably record upgrade phase %s; previous maintenance marker retained: %w", name, err)
		}
		return nil
	}
	if err := setPhase("vacuum"); err != nil {
		return err
	}
	if err := phase("upgrade-vacuum", nil); err != nil {
		return fmt.Errorf("upgrade VACUUM cancelled or unavailable; verified backup retained: %w", err)
	}
	db, err := sql.Open("sqlite3", dbURL+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=1000&_txlock=immediate")
	if err != nil {
		return fmt.Errorf("open database for protected upgrade: %w", err)
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `VACUUM`)
	if err == nil {
		err = phase("upgrade-after-vacuum", db)
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	} else {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return fmt.Errorf("upgrade VACUUM failed; verified backup retained: %w", err)
	}
	if err := setPhase("migration"); err != nil {
		return err
	}
	files, err := migrationSource(cfg)
	if err != nil {
		return err
	}
	db, err = sql.Open("sqlite3", dbURL+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=1000&_txlock=immediate")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("configure protected migrations: %w", err)
	}
	if err = phase("upgrade-migration", db); err == nil {
		_, err = provider.Up(ctx)
	}
	providerCloseErr := provider.Close()
	if err == nil {
		err = providerCloseErr
	} else {
		err = errors.Join(err, providerCloseErr)
	}
	if err != nil {
		return fmt.Errorf("embedded migration failed; verified backup retained: %w", err)
	}
	if err := setPhase("verification"); err != nil {
		return err
	}
	if err = phase("upgrade-verify", nil); err != nil {
		return fmt.Errorf("post-upgrade verification failed; verified backup retained: %w", err)
	}
	db, err = sql.Open("sqlite3", dbURL+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=1000")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if err = validateMigrationHistory(db, marker.TargetVersion); err == nil {
		err = validateStoragePragmas(db)
	}
	if err == nil {
		err = validateExistingKeyBindings(cfg, dbURL)
	}
	if err == nil {
		err = phase("upgrade-clean-close", db)
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	} else {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return fmt.Errorf("post-upgrade verification or database close failed; verified backup retained: %w", err)
	}
	if err := setPhase("unlock"); err != nil {
		return err
	}
	if err := phase("marker-remove", nil); err != nil {
		return fmt.Errorf("verified upgrade could not durably remove maintenance marker; backup retained: %w", err)
	}
	if err := durableRemoveWithSync(markerPath, func(dir *os.File) error {
		if err := phase("marker-remove-sync", nil); err != nil {
			return err
		}
		return dir.Sync()
	}); err != nil {
		return fmt.Errorf("verified upgrade could not durably remove maintenance marker; backup retained: %w", err)
	}
	return nil
}

func durableRemove(path string) error {
	return durableRemoveWithSync(path, func(dir *os.File) error { return dir.Sync() })
}

func durableRemoveWithSync(path string, syncDirectory func(*os.File) error) error {
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var marker maintenanceMarker
	if err := json.Unmarshal(old, &marker); err != nil {
		return errors.New("maintenance marker became unreadable before removal")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		_ = dir.Close()
		return err
	}
	err = syncDirectory(dir)
	closeErr := dir.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		// A failed directory sync must not leave this process able to proceed as
		// ready. Re-establish the fail-closed marker before returning.
		return errors.Join(err, durableJSONReplace(path, marker))
	}
	return nil
}

func validateBackupPaths(cfg Config, marker string) error {
	base, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return err
	}
	marker, err = filepath.Abs(marker)
	if err != nil {
		return err
	}
	backupDir := base + ".upgrade-backups"
	dbInfo, dbErr := os.Stat(base)
	for name, path := range map[string]string{"OPAQUE key": cfg.OPAQUEKeyPath, "repository key": cfg.RepositoryKeyPath, "deployment root": cfg.DeploymentRoot} {
		if path == "" {
			continue
		}
		p, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if p == base || p == marker || p == backupDir || p == backupDir+string(os.PathSeparator) || p == base+"-wal" || p == base+"-shm" {
			return fmt.Errorf("%s path aliases upgrade database or backup", name)
		}
		if p == backupDir || filepath.Dir(p) == backupDir {
			return fmt.Errorf("%s path aliases database-only backup location", name)
		}
		if info, statErr := os.Lstat(p); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s path must not be a symlink during upgrade maintenance", name)
			}
			if dbErr == nil && !info.IsDir() && os.SameFile(info, dbInfo) {
				return fmt.Errorf("%s path aliases the SQLite database", name)
			}
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect %s path: %w", name, statErr)
		}
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("backup directory must be an owner-only real directory")
	}
	if st, ok := info.Sys().(*unix.Stat_t); ok && (st.Uid != uint32(os.Geteuid()) || st.Nlink < 2) {
		return errors.New("backup directory must be owned by the current user")
	}
	return nil
}

func durableJSONReplace(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := ensureTrustedDirectory(dir); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("metadata destination must be an owner-only regular file")
		}
		if st, ok := info.Sys().(*unix.Stat_t); ok && (st.Uid != uint32(os.Geteuid()) || st.Nlink != 1) {
			return errors.New("metadata destination must be owned and unaliased")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".maintenance-"+hex.EncodeToString(nonce[:])+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = d.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}

func ensureTrustedDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("metadata parent must be a trusted real directory not writable by others")
	}
	if st, ok := info.Sys().(*unix.Stat_t); ok && st.Uid != uint32(os.Geteuid()) {
		return errors.New("metadata parent must be owned by the current user")
	}
	return nil
}

func checkDatabaseSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		p := path + suffix
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("unsafe sidecar %q", suffix)
		}
		if (suffix == "-wal" || suffix == "-journal") && info.Size() != 0 {
			return fmt.Errorf("SQLite sidecar %q remains nonempty after checkpoint and close", suffix)
		}
		if st, ok := info.Sys().(*unix.Stat_t); ok && (st.Uid != uint32(os.Geteuid()) || st.Nlink != 1) {
			return fmt.Errorf("aliased sidecar %q", suffix)
		}
	}
	return nil
}

func copyExclusive(source, destination string, phase func(string, *sql.DB) error) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("source database is not a restrictive regular file")
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	buffer := make([]byte, 64<<10)
	var copyErr error
	for {
		n, readErr := io.ReadFull(in, buffer)
		if n > 0 {
			written, writeErr := out.Write(buffer[:n])
			if writeErr != nil {
				copyErr = writeErr
				break
			}
			if written != n {
				copyErr = io.ErrShortWrite
				break
			}
			if hookErr := phase("backup-copy-chunk", nil); hookErr != nil {
				copyErr = hookErr
				break
			}
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			copyErr = readErr
			break
		}
	}
	if copyErr == nil {
		copyErr = phase("backup-copy", nil)
	}
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	d, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	return nil
}

func verifyStandaloneBackup(path string, version int64, phase func(string, *sql.DB) error) (string, error) {
	if err := phase("backup-integrity", nil); err != nil {
		return "", err
	}
	readURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + "?mode=ro&immutable=1&_busy_timeout=1000"
	db, err := sql.Open("sqlite3", readURL)
	if err != nil {
		return "", err
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := validateDatabaseIntegrity(db); err != nil {
		return "", err
	}
	if pending, err := inspectVersionOneHistory(db); err != nil {
		return "", err
	} else if !pending {
		if err := validateVersionOneHistory(db); err != nil {
			return "", err
		}
	}
	var got int64
	if err := db.QueryRow(`SELECT max(version_id) FROM goose_db_version WHERE is_applied=1`).Scan(&got); err != nil {
		return "", err
	}
	if got != version {
		return "", fmt.Errorf("backup schema version %d does not match source %d", got, version)
	}
	if err := db.Close(); err != nil {
		return "", err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return "", fmt.Errorf("read-only backup verification left SQLite sidecar %q", suffix)
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
