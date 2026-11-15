package lib

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"golang.org/x/sys/unix"
)

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
	path, err := os.Executable()
	if err != nil {
		return programProvenance{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return programProvenance{}, err
	}
	binary, err := os.ReadFile(path)
	if err != nil {
		return programProvenance{}, fmt.Errorf("read running program for provenance: %w", err)
	}
	hash := sha256.Sum256(binary)
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
	identity, err := databaseFileIdentity(database)
	if err != nil {
		return programProvenance{}, err
	}
	return programProvenance{Format: 1, Database: database, DatabaseIdentity: identity, Schema: schema, Executable: path, Version: version, Revision: revision, SHA256: hex.EncodeToString(hash[:])}, nil
}

func databaseFileIdentity(path string) (string, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x", uint64(stat.Dev), uint64(stat.Ino)), nil
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
	identity, err := databaseFileIdentity(database)
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
