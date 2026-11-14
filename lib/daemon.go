package lib

import (
	"errors"
	"net/http"
	"sync"
)

// Daemon owns one HTTP daemon's lifetime, including when persistent maintenance
// state forbids constructing an App. Transport must be established by the caller
// before OpenDaemon, and Close must follow the end of HTTP serving.
type Daemon struct {
	app       *App
	ownership *storageLock
	closeOnce sync.Once
	closeErr  error
}

// OpenDaemon obtains daemon-only ownership, then either prepares the normal
// application or retains ownership with only a minimal maintenance handler.
// Compatible grant issuers do not acquire this daemon ownership.
func OpenDaemon(cfg Config) (*Daemon, error) {
	app, ownership, err := startDaemon(cfg)
	if err != nil && !errors.Is(err, ErrMaintenance) {
		return nil, err
	}
	return &Daemon{app: app, ownership: ownership}, nil
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
