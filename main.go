package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
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
	cfg := lib.Config{DatabasePath: dataPath("state.sqlite"), OPAQUEKeyPath: dataPath("opaque-server.key"), RepositoryKeyPath: repositoryMasterPath, CanonicalOrigin: *origin, InsecureLocalHTTP: true, GrantTTL: *grantTTL, LoginTTL: *loginTTL, SessionTTL: *sessionTTL, RecentAuthTTL: *recentAuthTTL, AuthRequestsPerMinute: *authRate, GitTimeout: *gitTimeout, DeploymentRoot: *publicationRoot, DeployMaxFiles: *deployMaxFiles, DeployMaxBytes: *deployMaxBytes, DeployTimeout: *deployTimeout, DeployConcurrency: *deployConcurrency, DeployGracePeriod: *deployGrace, JobRetention: *jobRetention}
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
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	daemon, err := lib.OpenDaemon(cfg)
	if err != nil {
		return err
	}
	defer daemon.Close()
	if daemon.Locked() {
		log.Print(lib.ErrMaintenance)
	}
	server := &http.Server{Handler: daemon.Handler(), TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 16}
	if tlsConfig != nil {
		err = server.ServeTLS(listener, "", "")
	} else {
		err = server.Serve(listener)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func port(address string) string { _, p, _ := net.SplitHostPort(address); return p }
