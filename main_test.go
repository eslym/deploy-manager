package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eslym/deploy-manager/internal/testbuild"
	"golang.org/x/sys/unix"
)

var commandBuild struct {
	sync.Once
	directory, binary string
	err               error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if commandBuild.directory != "" {
		if err := os.RemoveAll(commandBuild.directory); err != nil {
			fmt.Fprintln(os.Stderr, "remove CLI test build:", err)
			code = 1
		}
	}
	os.Exit(code)
}

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
	// Only installation state is test-specific. All CLI scenarios can share one
	// immutable executable, including when tests run in parallel or with -count.
	commandBuild.Do(func() {
		commandBuild.directory, commandBuild.err = os.MkdirTemp("", "deploy-manager-cli-tests-")
		if commandBuild.err != nil {
			return
		}
		commandBuild.binary = filepath.Join(commandBuild.directory, "deploy-manager")
		args := []string{"build"}
		if testbuild.Race {
			args = append(args, "-race")
		}
		args = append(args, "-o", commandBuild.binary, ".")
		if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
			commandBuild.err = fmt.Errorf("build executable: %w\n%s", err, out)
		}
	})
	if commandBuild.err != nil {
		t.Fatal(commandBuild.err)
	}
	return commandBuild.binary
}

func TestBuiltExecutableRunsEmbeddedApplicationOutsideSourceTree(t *testing.T) {
	binary := buildCommand(t)
	installation := t.TempDir()
	workDir := t.TempDir()
	setup := boundedCommand(t, binary, "setup", "--data", installation, "--url", "https://manager.example")
	setup.Dir = workDir
	if output, err := setup.CombinedOutput(); err != nil || !strings.Contains(string(output), "https://manager.example/register#") {
		t.Fatalf("embedded executable setup failed outside source tree: %v\n%s", err, output)
	}
	db, err := sql.Open("sqlite3", filepath.Join(installation, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sqliteVersion string
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&sqliteVersion); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(sqliteVersion, ".")
	if len(parts) < 3 {
		t.Fatalf("unexpected SQLite version from shipped executable: %q", sqliteVersion)
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(sqliteVersion, "%d.%d.%d", &major, &minor, &patch); err != nil || major < 3 || major == 3 && (minor < 51 || minor == 51 && patch < 3) {
		t.Fatalf("shipped SQLite %q lacks the required WAL-reset fix: %v", sqliteVersion, err)
	}
	var version, applied int64
	if err := db.QueryRow(`SELECT version_id,is_applied FROM goose_db_version WHERE is_applied=1 ORDER BY version_id DESC LIMIT 1`).Scan(&version, &applied); err != nil || version != 1 || applied != 1 {
		t.Fatalf("shipped migration history = %d/%d, err=%v; want embedded v1", version, applied, err)
	}
	if _, err := os.Stat(filepath.Join(installation, "opaque-server.key")); err != nil {
		t.Fatalf("production executable did not create external OPAQUE key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installation, "repository-key-master.key")); err != nil {
		t.Fatalf("production executable did not create external repository master key: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	address := freeAddress(t)
	client := startCommand(t, binary, installation, address)
	request, err := http.NewRequest(http.MethodGet, "http://"+address+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "text/html")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "_app/immutable/entry/start.") {
		t.Fatalf("embedded SPA unavailable from unrelated working directory: status=%d err=%v body=%q", response.StatusCode, err, body)
	}
}

func TestBuiltExecutableRefusesOlderAndNewerCLIStorageWithoutMutation(t *testing.T) {
	binary := buildCommand(t)
	for _, tc := range []struct {
		name, update string
	}{
		{"older", `UPDATE goose_db_version SET is_applied=0 WHERE version_id=1`},
		{"newer", `INSERT INTO goose_db_version(version_id,is_applied) VALUES(2,1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := t.TempDir()
			setup := boundedCommand(t, binary, "setup", "--data", data, "--url", "https://manager.example")
			if output, err := setup.CombinedOutput(); err != nil {
				t.Fatalf("initialize fixture installation: %v\n%s", err, output)
			}
			dbPath := filepath.Join(data, "state.sqlite")
			db, err := sql.Open("sqlite3", dbPath)
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
			paths := []string{dbPath, filepath.Join(data, "opaque-server.key"), filepath.Join(data, "repository-key-master.key")}
			before := make(map[string][]byte, len(paths))
			for _, path := range paths {
				before[path], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, command := range []string{"setup", "recover"} {
				cmd := boundedCommand(t, binary, command, "--data", data, "--url", "https://manager.example")
				cmd.Dir = t.TempDir()
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				if err := cmd.Run(); err == nil || stdout.Len() != 0 || !(strings.Contains(stderr.String(), "schema version mismatch") || strings.Contains(stderr.String(), "cannot upgrade an existing database")) {
					t.Fatalf("%s accepted unsupported schema: err=%v stdout=%q stderr=%q", command, err, &stdout, &stderr)
				}
			}
			for path, want := range before {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != string(want) {
					t.Fatalf("CLI mismatch changed %s: %v", path, err)
				}
			}
			if _, err := os.Lstat(dbPath + ".maintenance.json"); !os.IsNotExist(err) {
				t.Fatalf("CLI mismatch created maintenance marker: %v", err)
			}
			if _, err := os.Lstat(dbPath + ".upgrade-backups"); !os.IsNotExist(err) {
				t.Fatalf("CLI mismatch created backup artifacts: %v", err)
			}
		})
	}
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

func TestServePublishesMaintenanceUntilApplicationStartupCompletes(t *testing.T) {
	address := freeAddress(t)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		finished <- serveWithStartup(ctx, address, nil, func(context.Context) (http.Handler, func(), error) {
			close(entered)
			<-release
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), func() {}, nil
		})
	}()
	<-entered
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		response, err := (&http.Client{Timeout: time.Second}).Get("http://" + address + "/webhooks/gitea")
		if method == http.MethodHead {
			request, _ := http.NewRequest(method, "http://"+address+"/webhooks/github", nil)
			response, err = (&http.Client{Timeout: time.Second}).Do(request)
		}
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s during protected startup = %d, want 503", method, response.StatusCode)
		}
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get("http://" + address + "/")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusNoContent {
				cancel()
				select {
				case err := <-finished:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("serve returned %v after cancellation", err)
					}
				case <-time.After(time.Second):
					t.Fatal("serve did not stop after cancellation")
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ready handler was not published after startup succeeded")
}

func TestServeStartupProcessFixture(t *testing.T) {
	if os.Getenv("DM_SERVE_STARTUP_FIXTURE") != "1" {
		return
	}
	address, release := os.Getenv("DM_SERVE_FIXTURE_ADDRESS"), os.Getenv("DM_SERVE_FIXTURE_RELEASE")
	err := serveWithStartup(context.Background(), address, nil, func(context.Context) (http.Handler, func(), error) {
		for {
			if _, err := os.Stat(release); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), func() {}, nil
	})
	if err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestRealProcessServes503DuringProtectedStartupThenSwitchesReady(t *testing.T) {
	address, release := freeAddress(t), filepath.Join(t.TempDir(), "release-startup")
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeStartupProcessFixture$")
	cmd.Env = append(os.Environ(), "DM_SERVE_STARTUP_FIXTURE=1", "DM_SERVE_FIXTURE_ADDRESS="+address, "DM_SERVE_FIXTURE_RELEASE="+release)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := &http.Client{Timeout: time.Second}
	request := func(method, path string) int {
		r, _ := http.NewRequest(method, "http://"+address+path, nil)
		response, err := client.Do(r)
		if err != nil {
			return 0
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && request(http.MethodGet, "/webhooks/gitea") != http.StatusServiceUnavailable {
		time.Sleep(10 * time.Millisecond)
	}
	if got := request(http.MethodHead, "/webhooks/github"); got != http.StatusServiceUnavailable {
		t.Fatalf("HEAD while startup blocked returned %d, want 503", got)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := request(http.MethodGet, "/"); got == http.StatusNoContent {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("process did not switch to ready handler after initialization succeeded")
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
			if res.StatusCode != http.StatusServiceUnavailable {
				return client
			}
			if _, markerErr := os.Lstat(filepath.Join(data, "state.sqlite.maintenance.json")); markerErr == nil {
				return client
			}
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

func TestLockedCommandServesMaintenanceForRawOptionsStar(t *testing.T) {
	binary := buildCommand(t)
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	pair := fixture.TLS.Certificates[0]
	fixture.Close()
	key, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(t.TempDir(), "cert.pem"), filepath.Join(t.TempDir(), "key.pem")
	for path, content := range map[string][]byte{
		certPath: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}),
		keyPath:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
	} {
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%t", secure), func(t *testing.T) {
			data := t.TempDir()
			for _, name := range []string{"state.sqlite", "opaque-server.key", "repository-key-master.key", "state.sqlite.maintenance.json"} {
				if err := os.WriteFile(filepath.Join(data, name), []byte("sensitive invalid state: secret-backup-path"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			address := freeAddress(t)
			var flags []string
			if secure {
				flags = []string{"--tls-cert", certPath, "--tls-key", keyPath}
			}
			client := startCommand(t, binary, data, address, flags...)
			var conn net.Conn
			dialer := &net.Dialer{Timeout: 5 * time.Second}
			if secure {
				conn, err = tls.DialWithDialer(dialer, "tcp", address, client.Transport.(*http.Transport).TLSClientConfig)
			} else {
				conn, err = dialer.Dial("tcp", address)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			// Send the actual asterisk-form request target, not a URL path that
			// an HTTP client or router might normalize before it reaches net/http.
			if _, err := fmt.Fprintf(conn, "OPTIONS * HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", address); err != nil {
				t.Fatal(err)
			}
			res, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Content-Type") != "text/plain; charset=utf-8" || string(body) != "Service unavailable: maintenance required.\n" {
				t.Fatalf("raw OPTIONS * response: status=%d headers=%v body=%q", res.StatusCode, res.Header, body)
			}
			for _, secret := range []string{data, "sensitive invalid state", "secret-backup-path", "SELECT", "PRIVATE KEY"} {
				if strings.Contains(fmt.Sprint(res.Header)+string(body), secret) {
					t.Errorf("raw OPTIONS * leaked %q", secret)
				}
			}
		})
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
