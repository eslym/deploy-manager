package lib

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Lock files are permanent installation identities, never unlinked on close.
// Closing the descriptor (including on process death) releases OS ownership.
type storageLock struct{ file *os.File }

func acquireDaemonOwnership(cfg Config) (*storageLock, error) {
	f, err := openCoordinationFile(cfg, ".daemon.lock")
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon ownership unavailable; another daemon may be running: %w", err)
	}
	return &storageLock{file: f}, nil
}

func (l *storageLock) Close() error {
	if l == nil {
		return nil
	}
	return l.file.Close()
}

// acquireStorageAccess protects the complete check-to-use interval, not just
// startup checks. Fresh initialization upgrades to exclusive access and checks
// existence again after acquisition; no database/key work precedes this guard.
// Future version inspection/maintenance belongs under this same guard (#31).
func acquireStorageAccess(cfg Config) (*storageLock, bool, error) {
	f, err := openCoordinationFile(cfg, ".access.lock")
	if err != nil {
		return nil, false, err
	}
	l := &storageLock{file: f}
	if err := l.acquire(unix.LOCK_SH); err != nil {
		_ = l.Close()
		return nil, false, err
	}
	prior, err := pathExists(cfg.DatabasePath)
	if err == nil && !prior {
		err = l.acquire(unix.LOCK_EX)
		if err == nil {
			prior, err = pathExists(cfg.DatabasePath)
		}
	}
	if err != nil {
		_ = l.Close()
		return nil, false, err
	}
	return l, prior, nil
}

func (l *storageLock) acquire(mode int) error {
	deadline := time.Now().Add(time.Second)
	for {
		err := unix.Flock(int(l.file.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EINTR {
			return fmt.Errorf("database access coordination: %w", err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("database access busy; initialization or maintenance is in progress; retry after it finishes: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (l *storageLock) share() error { return l.acquire(unix.LOCK_SH) }

func openCoordinationFile(cfg Config, suffix string) (*os.File, error) {
	if cfg.DatabasePath == "" {
		return nil, fmt.Errorf("database path is required")
	}
	dbPath, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	for _, path := range []string{cfg.OPAQUEKeyPath, cfg.RepositoryKeyPath} {
		if path == "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if absolute == dbPath+".daemon.lock" || absolute == dbPath+".access.lock" {
			return nil, fmt.Errorf("external keys must be separate from coordination lock files")
		}
	}
	dir, err := openCoordinationDirectory(filepath.Dir(dbPath))
	if err != nil {
		return nil, fmt.Errorf("unsafe coordination directory: %w", err)
	}
	defer unix.Close(dir)
	var parent unix.Stat_t
	if err := unix.Fstat(dir, &parent); err != nil {
		return nil, err
	}
	if parent.Uid != uint32(os.Geteuid()) || parent.Mode&0022 != 0 {
		return nil, fmt.Errorf("database directory must be owned by the current user and not writable by others")
	}
	// Daemon ownership must remain usable for marker-locked HTTP serving
	// without even inspecting the database. Validate its identity only at the
	// database-access boundary, after the separate marker preflight (#30).
	if suffix == ".access.lock" {
		var db unix.Stat_t
		if err := unix.Fstatat(dir, filepath.Base(dbPath), &db, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			if db.Mode&unix.S_IFMT != unix.S_IFREG || db.Nlink != 1 || db.Uid != uint32(os.Geteuid()) {
				return nil, fmt.Errorf("database must be an owned regular file without aliases")
			}
		} else if err != unix.ENOENT {
			return nil, err
		}
	}
	name := filepath.Base(dbPath) + suffix
	fd, err := unix.Openat(dir, name, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("open coordination lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), dbPath+suffix)
	var stat, entry unix.Stat_t
	err = unix.Fstat(fd, &stat)
	if err == nil {
		err = unix.Fstatat(dir, name, &entry, unix.AT_SYMLINK_NOFOLLOW)
	}
	if err == nil && (stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0077 != 0 || stat.Size != 0 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Dev != entry.Dev || stat.Ino != entry.Ino) {
		err = fmt.Errorf("coordination lock must be an empty owner-only regular file without aliases")
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Walk using directory descriptors: neither an existing nor a newly created
// ancestor may be a symlink. No path supplied here is removed or truncated.
func openCoordinationDirectory(path string) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if component == "" {
			continue
		}
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err == unix.ENOENT {
			if err = unix.Mkdirat(fd, component, 0700); err == nil || err == unix.EEXIST {
				next, err = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			}
		}
		_ = unix.Close(fd)
		if err != nil {
			return -1, err
		}
		var stat unix.Stat_t
		if err = unix.Fstat(next, &stat); err == nil && ((stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) || (stat.Mode&0022 != 0 && stat.Mode&unix.S_ISVTX == 0)) {
			err = fmt.Errorf("database directory ancestors must be trusted and not writable by others (except sticky directories)")
		}
		if err != nil {
			_ = unix.Close(next)
			return -1, err
		}
		fd = next
	}
	return fd, nil
}
