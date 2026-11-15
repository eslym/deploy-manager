package lib

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type maintenanceMarker struct {
	Version       int    `json:"version"`
	SourceVersion int64  `json:"source_version"`
	TargetVersion int64  `json:"target_version"`
	Program       string `json:"program"`
	Operation     string `json:"operation"`
	Backup        string `json:"backup"`
	Phase         string `json:"phase"`
}

// ErrMaintenance means application data must not be accessed. Its diagnostic is
// deliberately independent of marker contents, paths and filesystem errors.
var ErrMaintenance = errors.New("installation is maintenance-locked; stop all database users and follow operator recovery before explicitly clearing the marker")

// MaintenancePath is reserved beside the database, independently of key paths.
// Marker writing/durability belongs to storage maintenance, not this reader.
func MaintenancePath(cfg Config) (string, error) {
	if err := validateStoragePaths(cfg); err != nil {
		return "", err
	}
	if cfg.DatabasePath == "" {
		return "", errors.New("database path is required")
	}
	return cfg.DatabasePath + ".maintenance.json", nil
}

// CheckMaintenance performs no database, key or marker-content reads. Any marker
// entry (including malformed, unreadable, symlink or non-regular entries) locks
// the installation. Only proven absence on a safe path permits data access.
// Callers coordinating database access must recheck under their access guard and
// retain that guard until all application data access has finished.
func CheckMaintenance(cfg Config) error {
	path, err := MaintenancePath(cfg)
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return ErrMaintenance
	}
	// A missing marker must not later be created accidentally as a secret key
	// or publication directory by otherwise normal initialization.
	for _, configured := range []string{cfg.OPAQUEKeyPath, cfg.RepositoryKeyPath, cfg.DeploymentRoot} {
		if configured == "" {
			continue
		}
		candidate, err := filepath.Abs(configured)
		if err != nil || candidate == path || strings.HasPrefix(candidate, path+string(os.PathSeparator)) {
			return ErrMaintenance
		}
	}
	// Reject aliased ancestors as well as an aliased marker. Walk top-down so a
	// missing component is absence, but a dangling symlink is never absence.
	var parents []string
	for p := filepath.Dir(path); p != filepath.Dir(p); p = filepath.Dir(p) {
		parents = append(parents, p)
	}
	for i := len(parents) - 1; i >= 0; i-- {
		info, err := os.Lstat(parents[i])
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || !info.IsDir() {
			return ErrMaintenance
		}
	}
	_, err = os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	return ErrMaintenance
}

// MaintenanceHandler has no application dependencies, routing or public
// diagnostics. In particular, it never loads the SPA or authentication crypto.
func MaintenanceHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("Service unavailable: maintenance required.\n"))
		}
	})
}
