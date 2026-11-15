package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/eslym/deploy-manager/lib"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: deploy-manager <setup|recover|serve> [flags]")
		os.Exit(2)
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	data := flags.String("data", "./data", "application data directory")
	listen := flags.String("listen", "127.0.0.1:8080", "HTTP(S) listen address")
	base := flags.String("url", "", "externally reachable HTTPS base URL for setup link")
	origin := flags.String("origin", "", "canonical HTTPS origin used by the application")
	cert := flags.String("tls-cert", "", "TLS certificate (required for non-loopback serving)")
	key := flags.String("tls-key", "", "TLS private key")
	repositoryMaster := flags.String("repository-key-master", "", "external repository-key encryption key file (defaults inside the data directory)")
	loginTTL := flags.Duration("login-ttl", 2*time.Minute, "maximum lifetime of an OPAQUE login handshake")
	sessionTTL := flags.Duration("session-ttl", 12*time.Hour, "maximum lifetime of an authenticated session")
	gitTimeout := flags.Duration("git-timeout", 2*time.Minute, "maximum duration of a repository fetch")
	grantTTL := flags.Duration("grant-ttl", 15*time.Minute, "maximum lifetime of a setup or recovery link")
	recentAuthTTL := flags.Duration("recent-auth-ttl", 5*time.Minute, "maximum age of OPAQUE authentication for credential changes")
	authRate := flags.Int("auth-rate-per-minute", 20, "maximum authentication requests per client per minute")
	publicationRoot := flags.String("publication-root", "", "manager-owned directory for published sites (defaults inside the data directory)")
	deployMaxFiles := flags.Int("deploy-max-files", 10000, "maximum tracked files per publication")
	deployMaxBytes := flags.Int64("deploy-max-bytes", 1<<30, "maximum expanded tracked bytes per publication")
	deployTimeout := flags.Duration("deploy-timeout", 2*time.Minute, "maximum duration of one publication")
	deployConcurrency := flags.Int("deploy-concurrency", 2, "maximum publications running across deployments")
	deployGrace := flags.Duration("deploy-grace-period", 60*time.Second, "minimum time before replaced files are cleaned up")
	jobRetention := flags.Duration("job-retention", 30*24*time.Hour, "how long publication job details and logs are retained")
	fetchRetryAttempts := flags.Int("fetch-retry-attempts", 3, "maximum attempts for transient Git fetch failures (1-5)")
	if command == "serve" {
		if err := applyEnvironmentDefaults(flags, os.LookupEnv); err != nil {
			log.Fatal(err)
		}
	}
	_ = flags.Parse(os.Args[2:])
	// Preserve caller traversal until the storage boundary validates it. Join
	// would erase "link/.." and silently select a different installation.
	dataDir := *data
	if dataDir == "" {
		dataDir = "."
	}
	dataPath := func(name string) string { return dataDir + string(os.PathSeparator) + name }
	repositoryMasterPath := *repositoryMaster
	if repositoryMasterPath == "" {
		repositoryMasterPath = dataPath("repository-key-master.key")
	}
	cfg := lib.Config{DatabasePath: dataPath("state.sqlite"), OPAQUEKeyPath: dataPath("opaque-server.key"), RepositoryKeyPath: repositoryMasterPath, CanonicalOrigin: *origin, InsecureLocalHTTP: true, GrantTTL: *grantTTL, LoginTTL: *loginTTL, SessionTTL: *sessionTTL, RecentAuthTTL: *recentAuthTTL, AuthRequestsPerMinute: *authRate, GitTimeout: *gitTimeout, DeploymentRoot: *publicationRoot, DeployMaxFiles: *deployMaxFiles, DeployMaxBytes: *deployMaxBytes, DeployTimeout: *deployTimeout, DeployConcurrency: *deployConcurrency, DeployGracePeriod: *deployGrace, JobRetention: *jobRetention, FetchRetryAttempts: *fetchRetryAttempts}
	switch command {
	case "setup", "recover":
		app, err := lib.NewGrantIssuer(cfg)
		if err != nil {
			log.Fatal(err)
		}
		defer app.Close()
		var token string
		if command == "recover" {
			token, err = app.IssueRecoveryGrant()
		} else {
			token, err = app.IssueRegistrationGrant()
		}
		if err != nil {
			log.Fatal(err)
		}
		url := *base
		if url == "" {
			host, _, e := net.SplitHostPort(*listen)
			if e != nil {
				log.Fatal(e)
			}
			scheme := "https"
			if *cert == "" {
				if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
					log.Fatal("HTTPS is required unless listening on loopback")
				}
				scheme = "http"
			}
			url = scheme + "://" + net.JoinHostPort(host, port(*listen))
		}
		var link string
		if command == "recover" {
			link, err = app.RecoveryURL(url, token)
		} else {
			link, err = app.RegistrationURL(url, token)
		}
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(link)
	case "serve":
		if err := serve(cfg, *listen, *cert, *key); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown command %q", command)
	}
}

// applyEnvironmentDefaults maps flag names to DM_* variables. Setting values
// before parsing arguments lets explicit command-line flags take precedence.
func applyEnvironmentDefaults(flags *flag.FlagSet, lookup func(string) (string, bool)) error {
	var err error
	flags.VisitAll(func(option *flag.Flag) {
		if err != nil {
			return
		}
		name := "DM_" + strings.ToUpper(strings.ReplaceAll(option.Name, "-", "_"))
		if value, ok := lookup(name); ok {
			if setErr := flags.Set(option.Name, value); setErr != nil {
				err = fmt.Errorf("invalid %s value: %w", name, setErr)
			}
		}
	})
	return err
}

// Establish transport prerequisites before constructing anything that can open
// application data. A locked installation needs only this transport and a tiny
// handler; it must not construct the normal application to report maintenance.
func serve(cfg lib.Config, address, cert, key string) error {
	var tlsConfig *tls.Config
	if cert != "" || key != "" {
		if cert == "" || key == "" {
			return errors.New("both TLS certificate and key are required")
		}
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return errors.New("cannot load TLS certificate/key pair")
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	} else {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if !(ip != nil && ip.IsLoopback() || host == "localhost") {
			return errors.New("HTTPS is required unless listening on loopback")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveWithStartup(ctx, address, tlsConfig, func(startupCtx context.Context) (http.Handler, func(), error) {
		daemon, err := lib.OpenDaemon(cfg.WithUpgradeContext(startupCtx))
		if err != nil {
			return nil, nil, err
		}
		if daemon.Locked() {
			log.Print(daemon.Diagnostic())
		}
		return daemon.Handler(), func() { _ = daemon.Close() }, nil
	})
}

// serveWithStartup makes maintenance the only reachable application state until
// protected initialization has completed. Listener errors and cancellation are
// observed while initialization is running; a late successful initializer is
// immediately closed rather than published after the server has stopped.
func serveWithStartup(ctx context.Context, address string, tlsConfig *tls.Config, startup func(context.Context) (http.Handler, func(), error)) error {
	startupCtx, cancelStartup := context.WithCancel(ctx)
	defer cancelStartup()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	var active atomic.Pointer[handlerBox]
	active.Store(&handlerBox{handler: lib.MaintenanceHandler()})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Load().handler.ServeHTTP(w, r)
	}), DisableGeneralOptionsHandler: true, TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 16}
	serveErr := make(chan error, 1)
	go func() {
		if tlsConfig != nil {
			serveErr <- server.ServeTLS(listener, "", "")
		} else {
			serveErr <- server.Serve(listener)
		}
	}()
	type startupResult struct {
		handler http.Handler
		close   func()
		err     error
	}
	started := make(chan startupResult, 1)
	go func() {
		h, closeFn, startErr := startup(startupCtx)
		started <- startupResult{handler: h, close: closeFn, err: startErr}
	}()
	select {
	case result := <-started:
		if result.err != nil {
			_ = server.Close()
			<-serveErr
			return result.err
		}
		if ctx.Err() != nil {
			if result.close != nil {
				result.close()
			}
			_ = server.Close()
			<-serveErr
			return ctx.Err()
		}
		active.Store(&handlerBox{handler: result.handler})
		select {
		case err := <-serveErr:
			if result.close != nil {
				result.close()
			}
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			_ = server.Close()
			<-serveErr
			if result.close != nil {
				result.close()
			}
			return ctx.Err()
		}
	case err := <-serveErr:
		cancelStartup()
		_ = server.Close()
		result := <-started
		if result.close != nil {
			result.close()
		}
		return err
	case <-ctx.Done():
		cancelStartup()
		_ = server.Close()
		result := <-started
		if result.close != nil {
			result.close()
		}
		<-serveErr
		return ctx.Err()
	}
}

type handlerBox struct{ handler http.Handler }

func port(address string) string { _, p, _ := net.SplitHostPort(address); return p }
