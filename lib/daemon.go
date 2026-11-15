package lib

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Daemon owns one HTTP daemon's lifetime, including when persistent maintenance
// state forbids constructing an App. Transport must be established by the caller
// before OpenDaemon, and Close must follow the end of HTTP serving.
type Daemon struct {
	app        *App
	ownership  *storageLock
	closeOnce  sync.Once
	closeErr   error
	diagnostic string
}

// OpenDaemon obtains daemon-only ownership, then either prepares the normal
// application or retains ownership with only a minimal maintenance handler.
// Compatible grant issuers do not acquire this daemon ownership.
func OpenDaemon(cfg Config) (*Daemon, error) {
	app, ownership, err := startDaemon(cfg)
	if err != nil && !errors.Is(err, ErrMaintenance) {
		return nil, err
	}
	return &Daemon{app: app, ownership: ownership, diagnostic: localMaintenanceDiagnostic(cfg)}, nil
}

// Diagnostic is for local operator logs only. The HTTP maintenance handler
// intentionally remains generic and never exposes marker contents.
func (d *Daemon) Diagnostic() string { return d.diagnostic }

func localMaintenanceDiagnostic(cfg Config) string {
	path, err := MaintenancePath(cfg)
	if err != nil {
		return "maintenance locked; marker location unavailable"
	}
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return "maintenance locked; marker is unreadable or unavailable"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "maintenance locked; marker is unreadable or unavailable"
	}
	var marker maintenanceMarker
	if err := json.Unmarshal(b, &marker); err != nil {
		return "maintenance locked; marker is malformed"
	}
	if marker.Operation == "" || marker.Phase == "" {
		return "maintenance locked; marker lacks operator diagnostics"
	}
	backup := marker.Backup
	if backup == "" {
		backup = "none finalized"
	}
	return fmt.Sprintf("maintenance locked: phase=%s source=%d target=%d operation=%s backup=%s", safeDiagnosticToken(marker.Phase), marker.SourceVersion, marker.TargetVersion, safeDiagnosticToken(marker.Operation), strconv.Quote(backup))
}

func safeDiagnosticToken(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return -1
	}, value)
}

func (d *Daemon) Locked() bool { return d.app == nil }

func (d *Daemon) Handler() http.Handler {
	if d.Locked() {
		return MaintenanceHandler()
	}
	return d.app.Handler()
}

func (d *Daemon) Close() error {
	d.closeOnce.Do(func() {
		if d.app != nil {
			d.closeErr = d.app.Close()
		}
		d.closeErr = errors.Join(d.closeErr, d.ownership.Close())
	})
	return d.closeErr
}
