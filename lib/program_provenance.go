package lib

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type executableFingerprint struct {
	path     string
	identity string
	sha256   string
}

var runningExecutable struct {
	sync.Once
	fingerprint executableFingerprint
	err         error
}

// programProvenance is non-secret installation metadata written only by a
// successfully prepared daemon. CLI grant issuers must never replace it.
type programProvenance struct {
	Format           int    `json:"format"`
	Database         string `json:"database"`
	DatabaseIdentity string `json:"database_identity"`
	Schema           int64  `json:"schema"`
	Executable       string `json:"executable"`
	Version          string `json:"version"`
	Revision         string `json:"revision,omitempty"`
	SHA256           string `json:"sha256"`
}

func provenancePath(cfg Config) string { return cfg.DatabasePath + ".program.json" }

func currentProgramProvenance(cfg Config, schema int64) (programProvenance, error) {
	fingerprint, err := cachedRunningExecutableFingerprint()
	if err != nil {
		return programProvenance{}, err
	}
	path := fingerprint.path
	if err := verifyExecutablePath(path, fingerprint.identity); err != nil {
		return programProvenance{}, err
	}
	version, revision := "devel", ""
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" {
			version = info.Main.Version
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
		}
	}
	database, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return programProvenance{}, err
	}
	identity, err := filesystemPathIdentity(database)
	if err != nil {
		return programProvenance{}, err
	}
	return programProvenance{Format: 1, Database: database, DatabaseIdentity: identity, Schema: schema, Executable: path, Version: version, Revision: revision, SHA256: fingerprint.sha256}, nil
}

// Linux keeps the running executable inode deny-write while it is mapped. The
// digest therefore describes the exact running image under normal filesystem
// semantics. Privileged or nonstandard same-inode mutation is outside that
// guarantee; device/inode metadata is an identity check, not a content proof.
func cachedRunningExecutableFingerprint() (executableFingerprint, error) {
	runningExecutable.Do(func() {
		path, err := os.Executable()
		if err != nil {
			runningExecutable.err = fmt.Errorf("resolve running program path for provenance: %w", err)
			return
		}
		path, err = filepath.Abs(path)
		if err != nil {
			runningExecutable.err = fmt.Errorf("resolve absolute running program path for provenance: %w", err)
			return
		}
		f, err := os.Open("/proc/self/exe")
		if err != nil {
			runningExecutable.err = fmt.Errorf("open running program for provenance: %w", err)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			runningExecutable.err = fmt.Errorf("stat running program for provenance: %w", err)
			return
		}
		if !info.Mode().IsRegular() {
			runningExecutable.err = errors.New("running program for provenance is not a regular file")
			return
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			runningExecutable.err = fmt.Errorf("hash running program for provenance: %w", err)
			return
		}
		var stat unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
			runningExecutable.err = errors.New("running program identity unavailable")
			return
		}
		fingerprint := executableFingerprint{
			path:     path,
			identity: fileIdentity(stat.Dev, stat.Ino),
			sha256:   hex.EncodeToString(h.Sum(nil)),
		}
		if err := verifyExecutablePath(path, fingerprint.identity); err != nil {
			runningExecutable.err = err
			return
		}
		runningExecutable.fingerprint = fingerprint
	})
	return runningExecutable.fingerprint, runningExecutable.err
}

func preflightRunningExecutablePath() error {
	fingerprint, err := cachedRunningExecutableFingerprint()
	if err != nil {
		return err
	}
	return verifyExecutablePath(fingerprint.path, fingerprint.identity)
}

func verifyExecutablePath(path, runningIdentity string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open configured running program for provenance: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat configured running program for provenance: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil || !info.Mode().IsRegular() || fileIdentity(stat.Dev, stat.Ino) != runningIdentity {
		return errors.New("configured executable no longer identifies the running program")
	}
	return nil
}

func filesystemPathIdentity(path string) (string, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return "", err
	}
	return fileIdentity(stat.Dev, stat.Ino), nil
}

func fileIdentity(device, inode uint64) string {
	return fmt.Sprintf("%x:%x", device, inode)
}

func readPriorProgramProvenance(cfg Config, source int64) (programProvenance, error) {
	path := provenancePath(cfg)
	if err := ensureTrustedDirectory(filepath.Dir(path)); err != nil {
		return programProvenance{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return programProvenance{}, fmt.Errorf("prior program provenance unavailable; refuse to claim a matching rollback executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return programProvenance{}, errors.New("prior program provenance must be an owner-only regular file")
	}
	var p programProvenance
	b, err := os.ReadFile(path)
	if err != nil {
		return programProvenance{}, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return programProvenance{}, fmt.Errorf("read prior program provenance: %w", err)
	}
	database, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return programProvenance{}, err
	}
	identity, err := filesystemPathIdentity(database)
	if err != nil {
		return programProvenance{}, err
	}
	if p.Format != 1 || p.Schema != source || p.Database != database || p.DatabaseIdentity != identity || p.Executable == "" || p.Version == "" || len(p.SHA256) != 64 || strings.Trim(p.SHA256, "0123456789abcdef") != "" {
		return programProvenance{}, errors.New("prior program provenance does not match this installation and source schema; refusing upgrade")
	}
	return p, nil
}

func writeProgramProvenance(cfg Config, db *sql.DB) error {
	var current int64
	if err := db.QueryRow(`SELECT max(version_id) FROM goose_db_version WHERE is_applied=1`).Scan(&current); err != nil {
		return err
	}
	p, err := currentProgramProvenance(cfg, current)
	if err != nil {
		return err
	}
	return durableJSONReplace(provenancePath(cfg), p)
}
