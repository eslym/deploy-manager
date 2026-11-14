package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func boundedCommand(t *testing.T, binary string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return exec.CommandContext(ctx, binary, args...)
}

// Exercise the shipped executable from an unrelated directory, without source
// assets or a frontend runtime. Every subprocess owns isolated installation data.
func buildCommand(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "deploy-manager")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v\n%s", err, out)
	}
	return binary
}

func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	return address
}

func startCommand(t *testing.T, binary, data, address string, flags ...string) *http.Client {
	t.Helper()
	args := append([]string{"serve", "--data", data, "--listen", address}, flags...)
	cmd := exec.Command(binary, args...)
	cmd.Dir = t.TempDir()
	var diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &diagnostics, &diagnostics
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	client := &http.Client{Timeout: 200 * time.Millisecond}
	scheme := "http"
	for i, flag := range flags {
		if flag == "--tls-cert" {
			cert, err := os.ReadFile(flags[i+1])
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(cert) {
				t.Fatal("cannot trust test TLS certificate")
			}
			client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
			t.Cleanup(client.CloseIdleConnections)
			scheme = "https"
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			// Let cleanup consume the already completed process, too.
			done <- err
			t.Fatalf("server exited before listening: %v\n%s", err, &diagnostics)
		default:
		}
		res, err := client.Get(scheme + "://" + address + "/")
		if err == nil {
			res.Body.Close()
			return client
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not become available")
	return nil
}

func TestLockedCommandServesMaintenanceOverTLS(t *testing.T) {
	binary := buildCommand(t)
	// Reuse Go's local test certificate but serve it through the real binary,
	// with normal client certificate verification rather than an insecure client.
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	pair := fixture.TLS.Certificates[0]
	fixture.Close()
	data := t.TempDir()
	certPath, keyPath := filepath.Join(t.TempDir(), "cert.pem"), filepath.Join(t.TempDir(), "key.pem")
	key, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string][]byte{
		certPath: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}),
		keyPath:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
		filepath.Join(data, "state.sqlite.maintenance.json"): []byte("interrupted maintenance"),
	} {
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	address := freeAddress(t)
	client := startCommand(t, binary, data, address, "--tls-cert", certPath, "--tls-key", keyPath)
	res, err := client.Get("https://" + address + "/opaque/login/start")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 503 || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("TLS maintenance response: %d %v", res.StatusCode, res.Header)
	}
	entries, err := os.ReadDir(data)
	if err != nil || len(entries) != 2 || entries[0].Name() != "state.sqlite.daemon.lock" || entries[1].Name() != "state.sqlite.maintenance.json" {
		t.Fatalf("TLS locked start initialized state: entries=%v err=%v", entries, err)
	}
}

func TestLockedDaemonRetainsSingletonOwnership(t *testing.T) {
	binary := buildCommand(t)
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "state.sqlite.maintenance.json"), []byte("interrupted maintenance"), 0600); err != nil {
		t.Fatal(err)
	}
	startCommand(t, binary, data, freeAddress(t))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "serve", "--data", data, "--listen", freeAddress(t)).CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(out), "daemon ownership unavailable") {
		t.Fatalf("second locked daemon was not refused promptly: err=%v context=%v out=%q", err, ctx.Err(), out)
	}
	for _, command := range []string{"setup", "recover"} {
		out, err := boundedCommand(t, binary, command, "--data", data).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "maintenance-locked") || strings.Contains(string(out), "daemon ownership") {
			t.Fatalf("%s did not refuse maintenance independently of daemon ownership: %v %q", command, err, out)
		}
	}
	for _, name := range []string{"state.sqlite", "state.sqlite.access.lock", "opaque-server.key", "repository-key-master.key"} {
		if _, err := os.Lstat(filepath.Join(data, name)); !os.IsNotExist(err) {
			t.Errorf("locked daemon/CLI accessed application state %s: %v", name, err)
		}
	}
}

func TestCLIRechecksMarkerAfterWaitingForStorageAccess(t *testing.T) {
	binary := buildCommand(t)
	data := t.TempDir()
	if out, err := boundedCommand(t, binary, "setup", "--data", data, "--url", "https://example.test").CombinedOutput(); err != nil {
		t.Fatalf("initialize installation: %v %s", err, out)
	}
	// Act as a maintenance owner. The CLI must pass its marker preflight and
	// reach the access guard before we create the marker and release exclusion.
	access, err := os.OpenFile(filepath.Join(data, "state.sqlite.access.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	if err := unix.Flock(int(access.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	arrival, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(arrival)
	if _, err := unix.InotifyAddWatch(arrival, access.Name(), unix.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	protected, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(protected)
	for _, name := range []string{"state.sqlite", "opaque-server.key", "repository-key-master.key"} {
		if _, err := unix.InotifyAddWatch(protected, filepath.Join(data, name), unix.IN_OPEN|unix.IN_MODIFY|unix.IN_ATTRIB); err != nil {
			t.Fatal(err)
		}
	}
	cmd := boundedCommand(t, binary, "setup", "--data", data, "--url", "https://example.test")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if n, err := unix.Poll([]unix.PollFd{{Fd: int32(arrival), Events: unix.POLLIN}}, 5000); err != nil || n == 0 {
		t.Fatalf("CLI did not reach storage access: n=%d err=%v", n, err)
	}
	if err := os.WriteFile(filepath.Join(data, "state.sqlite.maintenance.json"), []byte("upgrade interrupted while CLI waited"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(access.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "maintenance-locked") {
		t.Fatalf("CLI bypassed marker created while waiting: err=%v stdout=%q stderr=%q", err, &stdout, &stderr)
	}
	if n, err := unix.Read(protected, make([]byte, 65536)); n > 0 || err != unix.EAGAIN {
		t.Fatalf("CLI touched database or keys after new maintenance marker: events=%d err=%v", n, err)
	}
}

func TestLockedCommandServesOnlyMaintenance(t *testing.T) {
	binary := buildCommand(t)
	data := t.TempDir()
	// Deliberately invalid database and secrets: any normal application startup
	// would fail, so availability here proves the maintenance-only path exists.
	for _, name := range []string{"state.sqlite", "opaque-server.key", "repository-key-master.key"} {
		if err := os.WriteFile(filepath.Join(data, name), []byte("sensitive invalid state"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(data, "state.sqlite.maintenance.json"), []byte(`{"phase":"failed","backup":"secret-backup-path"}`), 0600); err != nil {
		t.Fatal(err)
	}
	address := freeAddress(t)
	startCommand(t, binary, data, address)
	paths := []string{"/", "/register", "/recover", "/login", "/projects/a/deployments/b", "/_app/immutable/missing.js", "/favicon.png", "/api/projects", "/api/deployments", "/api/repository-keys", "/api/ssh-hosts", "/api/ssh-hosts/approve", "/api/session", "/api/logout", "/opaque/login/start", "/opaque/login/finish", "/opaque/register/start", "/opaque/register/finish", "/opaque/password/change/start", "/opaque/password/change/finish", "/webhooks/github/a", "/webhooks/gitea/a", "/unknown"}
	// Include real embedded assets as well as missing assets, so the lock cannot
	// accidentally leave the SPA/crypto bundle available through a static router.
	for _, pattern := range []string{"lib/web/_app/immutable/entry/*.js", "lib/web/_app/immutable/assets/*.css", "lib/web/_app/immutable/chunks/*.js"} {
		assets, err := filepath.Glob(pattern)
		if err != nil || len(assets) == 0 {
			t.Fatalf("missing built frontend assets: %s: %v", pattern, err)
		}
		for _, asset := range assets {
			paths = append(paths, "/"+strings.TrimPrefix(asset, "lib/web/"))
		}
	}
	for _, path := range paths {
		for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "CUSTOM"} {
			t.Run(fmt.Sprintf("%s%s", method, path), func(t *testing.T) {
				req, _ := http.NewRequest(method, "http://"+address+path, strings.NewReader("private request"))
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				body, _ := io.ReadAll(res.Body)
				want := "Service unavailable: maintenance required.\n"
				if method == "HEAD" {
					want = ""
				}
				if res.StatusCode != 503 || string(body) != want || res.Header.Get("Content-Type") != "text/plain; charset=utf-8" || res.Header.Get("Cache-Control") != "no-store" {
					t.Fatalf("response: status=%d headers=%v body=%q", res.StatusCode, res.Header, body)
				}
			})
		}
	}
}

func TestLockedRestartsAndCLILeaveInstallationUntouched(t *testing.T) {
	binary := buildCommand(t)
	for _, marker := range []string{"valid", "malformed", "unreadable", "directory", "dangling-symlink", "fifo"} {
		t.Run(marker, func(t *testing.T) {
			data := t.TempDir()
			cmd := boundedCommand(t, binary, "setup", "--data", data, "--url", "https://example.test")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("initialize real installation: %v\n%s", err, out)
			}
			// These names are normally reconciled by daemon startup. Preserve both
			// publication content and pending cache/publication recovery evidence.
			for _, name := range []string{"state.sqlite-wal", "state.sqlite-shm", "state.sqlite-journal", "state.sqlite.projects/.git-stage-interrupted/content", "state.sqlite.publications/." + strings.Repeat("a", 32) + ".stage-" + strings.Repeat("b", 32) + "/content", "state.sqlite.publications/live/index.html"} {
				path := filepath.Join(data, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("preserved publication/recovery evidence"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(data, "state.sqlite.maintenance.json")
			var err error
			switch marker {
			case "directory":
				err = os.Mkdir(path, 0700)
			case "dangling-symlink":
				err = os.Symlink(filepath.Join(data, "missing-marker-target"), path)
			case "fifo":
				err = unix.Mkfifo(path, 0600)
			default:
				body, mode := []byte(`{"phase":"failed","operation":"fixture"}`), os.FileMode(0600)
				if marker == "malformed" {
					body = []byte("not json: secret metadata")
				}
				if marker == "unreadable" {
					mode = 0
				}
				err = os.WriteFile(path, body, mode)
			}
			if err != nil {
				t.Fatal(err)
			}
			// inotify catches even read-only opens (not merely changed bytes).
			// Metadata snapshots additionally catch chmod, replacement and unlink.
			fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			before := map[string]unix.Stat_t{}
			for _, name := range []string{"state.sqlite", "state.sqlite-wal", "state.sqlite-shm", "state.sqlite-journal", "opaque-server.key", "repository-key-master.key", "state.sqlite.projects", "state.sqlite.publications"} {
				err := filepath.WalkDir(filepath.Join(data, name), func(path string, entry os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					var stat unix.Stat_t
					if err := unix.Lstat(path, &stat); err != nil {
						return err
					}
					before[path] = stat
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			for path := range before {
				// Snapshot after traversal so our own directory reads are excluded.
				var stat unix.Stat_t
				if err := unix.Lstat(path, &stat); err != nil {
					t.Fatal(err)
				}
				before[path] = stat
				if _, err := unix.InotifyAddWatch(fd, path, unix.IN_OPEN|unix.IN_MODIFY|unix.IN_ATTRIB|unix.IN_CREATE|unix.IN_DELETE|unix.IN_DELETE_SELF|unix.IN_MOVE_SELF|unix.IN_MOVED_FROM|unix.IN_MOVED_TO); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				t.Run(fmt.Sprintf("restart-%d", i), func(t *testing.T) {
					address := freeAddress(t)
					startCommand(t, binary, data, address)
					res, err := http.Get("http://" + address + "/api/projects")
					if err != nil {
						t.Fatal(err)
					}
					res.Body.Close()
					if res.StatusCode != 503 {
						t.Fatalf("status=%d", res.StatusCode)
					}
				})
				for _, command := range []string{"setup", "recover"} {
					cmd := boundedCommand(t, binary, command, "--data", data, "--listen", freeAddress(t), "--url", "https://example.test")
					cmd.Dir = t.TempDir()
					var stdout, stderr bytes.Buffer
					cmd.Stdout, cmd.Stderr = &stdout, &stderr
					if err := cmd.Run(); err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "maintenance-locked") {
						t.Fatalf("%s: err=%v stdout=%q stderr=%q", command, err, &stdout, &stderr)
					}
					if strings.Contains(stderr.String(), data) || strings.Contains(stderr.String(), "secret metadata") {
						t.Fatalf("CLI leaked sensitive diagnostics: %s", &stderr)
					}
				}
			}
			for path, want := range before {
				var got unix.Stat_t
				if err := unix.Lstat(path, &got); err != nil || got != want {
					t.Errorf("locked command touched %s: err=%v\nbefore=%+v\nafter=%+v", path, err, want, got)
				}
			}
			buf := make([]byte, 65536)
			if n, err := unix.Read(fd, buf); n > 0 || err != unix.EAGAIN {
				t.Errorf("locked commands opened or modified protected state: event bytes=%d err=%v", n, err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Errorf("marker was cleared: %v", err)
			}
		})
	}
}

func TestListenerPrerequisitesFailBeforeApplicationWork(t *testing.T) {
	binary := buildCommand(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	for _, locked := range []bool{false, true} {
		for _, tc := range []struct {
			name, diagnostic string
			args             []string
		}{
			{"occupied", "bind", []string{"--listen", occupied.Addr().String()}},
			{"public-http", "HTTPS is required", []string{"--listen", "0.0.0.0:0"}},
			{"invalid-listen", "address", []string{"--listen", "not-an-address"}},
			{"missing-key", "both TLS", []string{"--tls-cert", "nonexistent-cert"}},
			{"missing-cert", "both TLS", []string{"--tls-key", "nonexistent-key"}},
			{"unreadable-tls", "cannot load TLS", []string{"--tls-cert", "nonexistent-cert", "--tls-key", "nonexistent-key"}},
		} {
			t.Run(fmt.Sprintf("%s/locked=%t", tc.name, locked), func(t *testing.T) {
				base := t.TempDir()
				data := filepath.Join(base, "fresh")
				if locked {
					if err := os.Mkdir(data, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(data, "state.sqlite.maintenance.json"), []byte("failed"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				args := append([]string{"serve", "--data", data}, tc.args...)
				cmd := boundedCommand(t, binary, args...)
				cmd.Dir = base
				out, err := cmd.CombinedOutput()
				if err == nil || !strings.Contains(string(out), tc.diagnostic) || strings.Contains(string(out), "maintenance-locked") {
					t.Fatalf("err=%v diagnostics=%q", err, out)
				}
				entries, err := os.ReadDir(data)
				if !locked && !os.IsNotExist(err) || locked && (err != nil || len(entries) != 1 || entries[0].Name() != "state.sqlite.maintenance.json") {
					t.Fatalf("transport failure touched application data: entries=%v err=%v", entries, err)
				}
			})
		}
	}
}

func TestUnsafeMaintenancePathsFailClosed(t *testing.T) {
	binary := buildCommand(t)
	for _, kind := range []string{"symlink-parent", "dangling-parent", "non-directory-parent", "inaccessible-parent"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			data := filepath.Join(base, "installation")
			var err error
			switch kind {
			case "symlink-parent":
				target := filepath.Join(base, "real-installation")
				if err = os.Mkdir(target, 0700); err == nil {
					err = os.Symlink(target, data)
				}
			case "dangling-parent":
				err = os.Symlink(filepath.Join(base, "missing-installation"), data)
			case "non-directory-parent":
				err = os.WriteFile(data, []byte("not a directory"), 0600)
			case "inaccessible-parent":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses filesystem permission denial")
				}
				err = os.Mkdir(data, 0)
				t.Cleanup(func() { _ = os.Chmod(data, 0700) })
			}
			if err != nil {
				t.Fatal(err)
			}
			// Unsafe installation directories cannot establish daemon ownership.
			// This infrastructure failure must refuse rather than serve without
			// singleton protection, even though the TCP listener itself can bind.
			out, err := boundedCommand(t, binary, "serve", "--data", data, "--listen", freeAddress(t)).CombinedOutput()
			if err == nil || !strings.Contains(string(out), "unsafe coordination directory") {
				t.Fatalf("unsafe installation did not fail closed: err=%v out=%q", err, out)
			}
			for _, command := range []string{"setup", "recover"} {
				out, err := boundedCommand(t, binary, command, "--data", data).CombinedOutput()
				if err == nil || !strings.Contains(string(out), "maintenance-locked") {
					t.Fatalf("%s bypassed unsafe path: err=%v out=%q", command, err, out)
				}
			}
		})
	}
}

func TestReservedMaintenancePathCannotBecomeApplicationState(t *testing.T) {
	binary := buildCommand(t)
	for _, command := range []string{"setup", "recover"} {
		for _, flag := range []string{"--repository-key-master", "--publication-root"} {
			for _, suffix := range []string{"", "/child"} {
				t.Run(command+flag+suffix, func(t *testing.T) {
					data := t.TempDir()
					marker := filepath.Join(data, "state.sqlite.maintenance.json")
					out, err := boundedCommand(t, binary, command, "--data", data, flag, marker+suffix, "--url", "https://example.test").CombinedOutput()
					if err == nil || !strings.Contains(string(out), "maintenance-locked") {
						t.Fatalf("reserved marker alias accepted: err=%v out=%q", err, out)
					}
					entries, err := os.ReadDir(data)
					if err != nil || len(entries) != 0 {
						t.Fatalf("unsafe marker configuration initialized state: entries=%v err=%v", entries, err)
					}
				})
			}
		}
	}
}

func TestCommandRejectsTraversalWithoutRetargetingInstallation(t *testing.T) {
	binary := buildCommand(t)
	for _, command := range []string{"setup", "recover", "serve"} {
		t.Run(command, func(t *testing.T) {
			base := t.TempDir()
			trusted, other := filepath.Join(base, "trusted"), filepath.Join(base, "other")
			if err := os.MkdirAll(filepath.Join(other, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(trusted, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(other, "nested"), filepath.Join(trusted, "link")); err != nil {
				t.Fatal(err)
			}
			// Do not filepath.Join this argument: preserving the original kernel
			// traversal is precisely what the command must do until validation.
			data := trusted + "/link/../installation"
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, binary, command, "--data", data, "--listen", freeAddress(t), "--url", "https://example.test").CombinedOutput()
			if err == nil || ctx.Err() != nil || !strings.Contains(string(out), "path components") {
				t.Fatalf("%s accepted or cleaned traversal: err=%v context=%v out=%q", command, err, ctx.Err(), out)
			}
			for _, parent := range []string{trusted, other} {
				if _, err := os.Lstat(filepath.Join(parent, "installation")); !os.IsNotExist(err) {
					t.Errorf("command touched installation under %s: %v", parent, err)
				}
			}
		})
	}
}
