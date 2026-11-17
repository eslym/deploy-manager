package lib

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	entdialect "entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/bytemare/opaque"
	"github.com/eslym/deploy-manager/lib/ent"
	"github.com/eslym/deploy-manager/lib/ent/registrationgrant"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sys/unix"
)

// Config controls durable administrator bootstrap. OPAQUEKeyPath must be external to SQLite.
type Config struct {
	DatabasePath          string
	OPAQUEKeyPath         string
	RepositoryKeyPath     string
	CanonicalOrigin       string
	AllowHTTPOrigin       bool
	TrustedProxies        []netip.Prefix
	GrantTTL              time.Duration
	LoginTTL              time.Duration
	SessionTTL            time.Duration
	RecentAuthTTL         time.Duration
	AuthRequestsPerMinute int
	InsecureLocalHTTP     bool
	Now                   func() time.Time
	GitTimeout            time.Duration
	GitMaxTransferBytes   int64
	// GitMaxCacheBytes bounds persistent project-cache bytes. A refresh may
	// temporarily add one staged project cache of at most this same size.
	GitMaxCacheBytes            int64
	GitMaxRefs                  int
	GitMaxAdvertisementBytes    int64
	DeploymentRoot              string
	DeployMaxFiles              int
	DeployMaxBytes              int64
	DeployTimeout               time.Duration
	DeployConcurrency           int
	DeployGracePeriod           time.Duration
	JobRetention                time.Duration
	FetchRetryAttempts          int
	WebhookMaxBodyBytes         int64
	beforeRestoreDeploymentJobs func(*sql.DB) error
	// storageFault is a per-startup test seam, never configured by the CLI or HTTP.
	// "access" runs under the access guard before DB/key work, with a nil DB;
	// "verify" runs after preparation and before storage readiness.
	storageFault   func(string, *sql.DB) error
	upgradeContext context.Context
	// migrationFS is a deterministic test fixture seam; production migrations
	// remain the embedded immutable set.
	migrationFS          fs.FS
	upgradeTargetVersion int64
}

// WithUpgradeContext bounds and cancels protected startup maintenance when the
// daemon's serving process receives a termination signal.
func (c Config) WithUpgradeContext(ctx context.Context) Config {
	c.upgradeContext = ctx
	return c
}

type App struct {
	cfg                  Config
	db                   *sql.DB
	store                *ent.Client
	protocol             *opaque.Server
	des                  *opaque.Deserializer
	keyGeneration        string
	credentialGeneration int64
	mu                   sync.Mutex
	rate                 map[string][]time.Time
	logins               map[string]pendingLogin
	sessions             map[[32]byte]sessionState
	projects             *projectManager
	repositoryMaster     []byte
	registrationTxs      map[string]registrationTransaction
	deployQueue          chan string
	deployStop           chan struct{}
	deploySem            chan struct{}
	deploySlots          chan struct{}
	deployWG             sync.WaitGroup
	deployCloseOnce      sync.Once
	publicationRootID    dirIdentity
	publicationStateMu   sync.RWMutex
	publicationBlocked   string
	deployLocks          sync.Map
	deploymentJobsMu     sync.Mutex
	deploymentJobCancels map[string]activeDeploymentJob
	webhookFault         func(string) error
	daemonOwnership      *storageLock
	storageAccess        *storageLock
	storageCloseOnce     sync.Once
	storageCloseErr      error
}

// prepareStorage performs only schema/key preparation and validation. Runtime
// recovery and all background work belong to readyStorage.start.
func prepareStorage(cfg Config, daemonUpgrade bool) (*readyStorage, error) {
	if err := validateStoragePaths(cfg); err != nil {
		return nil, err
	}
	if err := CheckMaintenance(cfg); err != nil {
		return nil, err
	}
	if daemonUpgrade {
		if err := preflightRunningExecutablePath(); err != nil {
			return nil, err
		}
	}
	if cfg.DatabasePath == "" || cfg.OPAQUEKeyPath == "" {
		return nil, errors.New("database and external OPAQUE key paths are required")
	}
	if cfg.RepositoryKeyPath == "" {
		cfg.RepositoryKeyPath = cfg.DatabasePath + ".repository-master-key"
	}
	if cfg.CanonicalOrigin != "" {
		origin, err := url.Parse(cfg.CanonicalOrigin)
		if err != nil || (origin.Scheme != "https" && !(cfg.AllowHTTPOrigin && origin.Scheme == "http")) || origin.Hostname() == "" || origin.User != nil || origin.Opaque != "" || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
			return nil, errors.New("canonical origin must be an HTTPS origin without path, query, or credentials (HTTP requires allow-http-origin)")
		}
	}
	dbPath, keyPath := filepath.Clean(cfg.DatabasePath), filepath.Clean(cfg.OPAQUEKeyPath)
	if keyPath == dbPath || keyPath == dbPath+"-wal" || keyPath == dbPath+"-shm" {
		return nil, errors.New("OPAQUE key must be separate from SQLite")
	}
	masterPath := filepath.Clean(cfg.RepositoryKeyPath)
	if masterPath == dbPath || masterPath == dbPath+"-wal" || masterPath == dbPath+"-shm" || masterPath == keyPath {
		return nil, errors.New("repository key master file must be separate from SQLite and the OPAQUE key")
	}
	if cfg.GrantTTL == 0 {
		cfg.GrantTTL = 15 * time.Minute
	}
	if cfg.RecentAuthTTL < 0 || cfg.RecentAuthTTL > 24*time.Hour {
		return nil, errors.New("recent authentication lifetime is outside the supported range")
	}
	if cfg.LoginTTL == 0 {
		cfg.LoginTTL = 2 * time.Minute
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.RecentAuthTTL == 0 {
		cfg.RecentAuthTTL = 5 * time.Minute
	}
	if cfg.GrantTTL < time.Second || cfg.GrantTTL > 24*time.Hour || cfg.LoginTTL < time.Second || cfg.LoginTTL > 10*time.Minute || cfg.SessionTTL < time.Second || cfg.SessionTTL > 30*24*time.Hour {
		return nil, errors.New("grant, login, or session lifetime is outside the supported range")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AuthRequestsPerMinute == 0 {
		cfg.AuthRequestsPerMinute = 20
	}
	if cfg.AuthRequestsPerMinute < 1 || cfg.AuthRequestsPerMinute > 1000 {
		return nil, errors.New("authentication request limit must be between 1 and 1000 per minute")
	}
	if cfg.GitMaxTransferBytes == 0 {
		cfg.GitMaxTransferBytes = 32 << 20
	}
	if cfg.GitMaxCacheBytes == 0 {
		cfg.GitMaxCacheBytes = 1 << 30
	}
	if cfg.GitMaxRefs == 0 {
		cfg.GitMaxRefs = 10000
	}
	if cfg.GitMaxAdvertisementBytes == 0 {
		cfg.GitMaxAdvertisementBytes = 4 << 20
		if cfg.GitMaxAdvertisementBytes > cfg.GitMaxTransferBytes {
			cfg.GitMaxAdvertisementBytes = cfg.GitMaxTransferBytes
		}
	}
	if cfg.GitMaxTransferBytes < 1024 || cfg.GitMaxTransferBytes > 1<<30 || cfg.GitMaxCacheBytes < 1 || cfg.GitMaxCacheBytes > 1<<40 || cfg.GitMaxRefs < 1 || cfg.GitMaxRefs > 100000 || cfg.GitMaxAdvertisementBytes < 1024 || cfg.GitMaxAdvertisementBytes > 64<<20 || cfg.GitMaxAdvertisementBytes > cfg.GitMaxTransferBytes {
		return nil, errors.New("Git transfer, cache, or reference limit is outside the supported range")
	}
	access, prior, err := acquireStorageAccess(cfg)
	if err != nil {
		return nil, err
	}
	accessTransferred := false
	defer func() {
		if !accessTransferred {
			_ = access.Close()
		}
	}()
	if dbInfo, dbErr := os.Stat(dbPath); dbErr == nil {
		if keyInfo, keyErr := os.Stat(keyPath); keyErr == nil && os.SameFile(dbInfo, keyInfo) {
			return nil, errors.New("OPAQUE key must not share the SQLite database file")
		}
	}
	if cfg.storageFault != nil {
		if err := cfg.storageFault("access", nil); err != nil {
			return nil, err
		}
	}
	// Existing stores are opened read-only first. This prevents a DSN pragma
	// (notably journal_mode=WAL) from changing unsupported state before exact
	// migration history has been accepted.
	sqlitePath, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	dbURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(sqlitePath)}).String()
	if prior {
		if err := preflightExistingSchema(dbURL, cfg); err != nil {
			var pending pendingUpgrade
			if !errors.As(err, &pending) {
				return nil, err
			}
			if !daemonUpgrade {
				return nil, errors.New("CLI commands cannot upgrade an existing database; start the daemon to perform protected maintenance")
			}
			if err := access.acquire(unix.LOCK_EX); err != nil {
				return nil, err
			}
			if err := CheckMaintenance(cfg); err != nil {
				return nil, err
			}
			if err := preflightRunningExecutablePath(); err != nil {
				return nil, err
			}
			priorNow, err := inspectGuardedStorage(cfg)
			if err != nil || !priorNow {
				if err == nil {
					err = errors.New("database changed during maintenance transition")
				}
				return nil, err
			}
			err = preflightExistingSchema(dbURL, cfg)
			if err == nil {
				return nil, errors.New("database upgrade state changed during maintenance transition; refusing startup")
			}
			if !errors.As(err, &pending) {
				return nil, err
			}
			if err := prepareUpgradeBackup(cfg, pending.source, pending.target, dbURL); err != nil {
				return nil, err
			}
			prior = true
		}
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.OPAQUEKeyPath), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.RepositoryKeyPath), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dbURL+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=1000&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := ent.NewClient(ent.Driver(entsql.OpenDB(entdialect.SQLite, db)))
	if err = initializeSchemaWithConfig(db, !prior, cfg); err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(cfg.DatabasePath, 0600); err != nil {
		db.Close()
		return nil, err
	}
	if err = prepareRepositoryKeyStore(store, db, cfg.RepositoryKeyPath, prior); err != nil {
		db.Close()
		return nil, err
	}
	repositoryMaster, err := loadRepositoryMasterKey(cfg.RepositoryKeyPath, true)
	if err != nil {
		db.Close()
		return nil, err
	}
	masterTransferred := false
	defer func() {
		if !masterTransferred {
			clear(repositoryMaster)
		}
	}()
	for _, suffix := range []string{"-wal", "-shm"} {
		if path := cfg.DatabasePath + suffix; fileInfo(path) {
			if err = os.Chmod(path, 0600); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	c := opaque.DefaultConfiguration()
	material, keyGeneration, err := loadOPAQUEServerKey(c, cfg.OPAQUEKeyPath, prior)
	if err != nil {
		db.Close()
		return nil, err
	}
	protocol, err := opaque.NewServer(c)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = protocol.SetKeyMaterial(material); err != nil {
		db.Close()
		return nil, err
	}
	des, err := c.Deserializer()
	if err != nil {
		db.Close()
		return nil, err
	}
	const profile = "RFC9807-OPAQUE-3DH;ristretto255-SHA512;HKDF-SHA512;HMAC-SHA512;Argon2id-v19-i1-m65536-p4;context-empty;identities-default"
	if !prior {
		if _, err = store.OpaqueProfile.Create().SetID(1).SetProfileGeneration(1).SetKeyGeneration(keyGeneration).SetProfile(profile).Save(context.Background()); err != nil {
			db.Close()
			return nil, fmt.Errorf("could not initialize fresh OPAQUE profile binding; preserve key and database evidence and use isolated rebootstrap if interrupted: %w", err)
		}
	}
	var storedProfile string
	var profileGeneration int
	var storedKeyGeneration string
	profileRecord, profileErr := store.OpaqueProfile.Get(context.Background(), 1)
	if err = profileErr; err != nil {
		db.Close()
		return nil, err
	}
	profileGeneration, storedKeyGeneration, storedProfile = profileRecord.ProfileGeneration, profileRecord.KeyGeneration, profileRecord.Profile
	if profileGeneration != 1 || storedKeyGeneration != keyGeneration || storedProfile != profile {
		db.Close()
		return nil, errors.New("OPAQUE key/profile generation mismatch; refusing authentication startup")
	}
	var accountGeneration int
	adminRecord, adminErr := store.Administrator.Get(context.Background(), 1)
	err = adminErr
	if err != nil && !ent.IsNotFound(err) {
		db.Close()
		return nil, err
	}
	if adminRecord != nil {
		accountGeneration = adminRecord.ProfileGeneration
	}
	if adminRecord != nil && accountGeneration != profileGeneration {
		db.Close()
		return nil, errors.New("administrator/profile generation mismatch; refusing authentication startup")
	}
	var credentialGeneration int64
	if adminRecord != nil {
		credentialGeneration = adminRecord.CredentialGeneration
	}
	if masterInfo, masterErr := os.Lstat(cfg.RepositoryKeyPath); masterErr == nil {
		if dbInfo, dbErr := os.Stat(dbPath); dbErr == nil && os.SameFile(masterInfo, dbInfo) {
			db.Close()
			return nil, errors.New("repository key master file must not share the SQLite database file")
		}
		if opaqueInfo, opaqueErr := os.Stat(cfg.OPAQUEKeyPath); opaqueErr == nil && os.SameFile(masterInfo, opaqueInfo) {
			db.Close()
			return nil, errors.New("repository key master file must not share the OPAQUE key file")
		}
	}
	app := &App{cfg: cfg, db: db, store: store, protocol: protocol, des: des, keyGeneration: keyGeneration, credentialGeneration: credentialGeneration, rate: make(map[string][]time.Time), logins: make(map[string]pendingLogin), sessions: make(map[[32]byte]sessionState), repositoryMaster: repositoryMaster, registrationTxs: make(map[string]registrationTransaction)}
	masterTransferred = true
	ready := false
	defer func() {
		if !ready {
			_ = app.Close()
		}
	}()
	if app.cfg.WebhookMaxBodyBytes == 0 {
		app.cfg.WebhookMaxBodyBytes = 1 << 20
	}
	if app.cfg.WebhookMaxBodyBytes < 1024 || app.cfg.WebhookMaxBodyBytes > 16<<20 {
		db.Close()
		return nil, errors.New("webhook body limit must be between 1 KiB and 16 MiB")
	}
	if err := app.configureDeployments(); err != nil {
		return nil, err
	}
	if cfg.storageFault != nil {
		if err := cfg.storageFault("verify", db); err != nil {
			return nil, fmt.Errorf("storage verification: %w", err)
		}
	}
	ready = true
	if err := access.share(); err != nil {
		_ = app.Close()
		return nil, err
	}
	if daemonUpgrade {
		if err := writeProgramProvenance(cfg, db); err != nil {
			_ = app.Close()
			return nil, fmt.Errorf("record verified daemon program provenance: %w", err)
		}
	}
	app.storageAccess = access
	accessTransferred = true
	return &readyStorage{app: app}, nil
}

func loadOPAQUEServerKey(c *opaque.Configuration, path string, priorDatabase bool) (*opaque.ServerKeyMaterial, string, error) {
	keyInfo, keyStatErr := os.Lstat(path)
	if keyStatErr == nil && (!keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm()&0077 != 0) {
		return nil, "", errors.New("OPAQUE key file must be a regular file readable only by its owner")
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) < len(opaqueKeyMagic)+16 || string(data[:len(opaqueKeyMagic)]) != opaqueKeyMagic {
			return nil, "", errors.New("invalid OPAQUE key file format")
		}
		keyGeneration := base64.RawURLEncoding.EncodeToString(data[len(opaqueKeyMagic) : len(opaqueKeyMagic)+16])
		material, err := c.DecodeServerKeyMaterial(data[len(opaqueKeyMagic)+16:])
		if err != nil {
			return nil, "", fmt.Errorf("decode OPAQUE key: %w", err)
		}
		// Bytemare's current SKM decoder rejects empty trailing identity vectors.
		// The empty identity is the protocol default (server public key).
		material.Identity = nil
		return material, keyGeneration, nil
	}
	if !os.IsNotExist(err) {
		return nil, "", err
	}
	if priorDatabase {
		return nil, "", errors.New("OPAQUE key file is missing; refusing to regenerate authentication keys; preserve the database and key-file evidence, and use isolated rebootstrap only if this was an interrupted fresh initialization")
	}
	priv, pub := c.KeyGen()
	material := &opaque.ServerKeyMaterial{PrivateKey: priv, PublicKeyBytes: pub.Encode(), OPRFGlobalSeed: c.GenerateOPRFSeed(), Identity: []byte("deploy-manager-keyfile")}
	generation := make([]byte, 16)
	if _, err := rand.Read(generation); err != nil {
		return nil, "", err
	}
	keyGeneration := base64.RawURLEncoding.EncodeToString(generation)
	encoded := append(append(append([]byte(nil), opaqueKeyMagic...), generation...), material.Encode()...)
	material.Identity = nil
	if err := writeExclusive(path, encoded); err != nil {
		return nil, "", err
	}
	dir, openErr := os.Open(filepath.Dir(path))
	if openErr != nil {
		return nil, "", openErr
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return nil, "", err
	}
	if closeErr != nil {
		return nil, "", closeErr
	}
	return material, keyGeneration, nil
}

const opaqueKeyMagic = "DMOPAQUE1"

func pathExists(p string) (bool, error) {
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
func fileInfo(path string) bool { _, err := os.Stat(path); return err == nil }
func writeExclusive(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e != nil {
		_ = f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		_ = f.Close()
		return e
	}
	return f.Close()
}
func (a *App) Close() error {
	a.deployCloseOnce.Do(func() {
		if a.deployStop != nil {
			close(a.deployStop)
			a.deployWG.Wait()
		}
	})
	a.storageCloseOnce.Do(func() {
		clear(a.repositoryMaster)
		a.storageCloseErr = errors.Join(a.db.Close(), a.storageAccess.Close(), a.daemonOwnership.Close())
	})
	return a.storageCloseErr
}

// IssueRegistrationGrant returns a random one-time token; only its SHA-256 digest is stored.
func (a *App) IssueRegistrationGrant() (string, error) {
	return a.issueGrant("setup")
}

// IssueRecoveryGrant issues a fragment-only authorization to replace the existing administrator credential.
func (a *App) IssueRecoveryGrant() (string, error) { return a.issueGrant("recovery") }

func (a *App) issueGrant(purpose string) (string, error) {
	ctx := context.Background()
	tx, e := a.store.Tx(ctx)
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	n, e := tx.Administrator.Query().Count(ctx)
	if e != nil {
		return "", e
	}
	if (purpose == "setup" && n != 0) || (purpose == "recovery" && n != 1) {
		if purpose == "recovery" {
			return "", errors.New("administrator is not registered")
		}
		return "", errors.New("administrator already registered")
	}
	var generation int64
	if n == 1 {
		adminRecord, err := tx.Administrator.Get(ctx, 1)
		if err != nil {
			return "", err
		}
		generation = adminRecord.CredentialGeneration
	}
	now := a.cfg.Now()
	if _, e = tx.RegistrationGrant.Delete().Where(registrationgrant.ExpiresAtLT(now.UnixNano())).Exec(ctx); e != nil {
		return "", e
	}
	if n, e = tx.RegistrationGrant.Query().Count(ctx); e != nil {
		return "", e
	}
	if n >= 10 {
		return "", errors.New("too many outstanding setup links; wait for one to expire")
	}
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(token))
	if _, e = tx.RegistrationGrant.Create().SetID(h[:]).SetExpiresAt(now.Add(a.cfg.GrantTTL).UnixNano()).SetCreatedAt(now.UnixNano()).SetPurpose(purpose).SetCredentialGeneration(generation).Save(ctx); e != nil {
		return "", e
	}
	if e = tx.Commit(); e != nil {
		return "", e
	}
	return token, nil
}
func (a *App) RegistrationURL(base, token string) (string, error) {
	return a.grantURL(base, token, "/register")
}

func (a *App) RecoveryURL(base, token string) (string, error) {
	return a.grantURL(base, token, "/recover")
}

func (a *App) grantURL(base, token, route string) (string, error) {
	u, e := url.Parse(base)
	if e != nil {
		return "", e
	}
	if u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("registration URL must have a valid host and no user info or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (a.cfg.AllowHTTPOrigin || a.cfg.InsecureLocalHTTP && isLoopback(u.Hostname()))) {
		return "", errors.New("registration URL must use HTTPS; HTTP requires allow-http-origin except on loopback")
	}
	u.Path = route
	u.RawQuery = ""
	u.Fragment = token
	return u.String(), nil
}
func isLoopback(h string) bool {
	ip := net.ParseIP(h)
	return (ip != nil && ip.IsLoopback()) || strings.EqualFold(h, "localhost")
}

func (a *App) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /opaque/register/start", a.registerStart)
	m.HandleFunc("POST /opaque/register/finish", a.registerFinish)
	m.HandleFunc("POST /opaque/password/change/start", a.passwordChangeStart)
	m.HandleFunc("POST /opaque/password/change/finish", a.passwordChangeFinish)
	m.HandleFunc("POST /opaque/login/start", a.loginStart)
	m.HandleFunc("POST /opaque/login/finish", a.loginFinish)
	m.HandleFunc("GET /api/session", a.sessionStatus)
	m.HandleFunc("POST /api/logout", a.logout)
	m.HandleFunc("GET /api/projects", a.listProjects)
	m.HandleFunc("POST /api/projects", a.createProject)
	m.HandleFunc("GET /api/projects/{id}/lfs", a.lfsSettings)
	m.HandleFunc("POST /api/projects/{id}/lfs", a.lfsSettings)
	m.HandleFunc("GET /api/projects/{id}/github-webhook", a.githubWebhookSettings)
	m.HandleFunc("POST /api/projects/{id}/github-webhook", a.githubWebhookSettings)
	m.HandleFunc("POST /webhooks/github/{id}", a.githubWebhook)
	m.HandleFunc("GET /api/projects/{id}/gitea-webhook", a.giteaWebhookSettings)
	m.HandleFunc("POST /api/projects/{id}/gitea-webhook", a.giteaWebhookSettings)
	m.HandleFunc("POST /webhooks/gitea/{id}", a.giteaWebhook)
	m.HandleFunc("POST /api/projects/{id}/delete", a.deleteProject)
	m.HandleFunc("GET /api/projects/{id}/refs", a.projectRefs)
	m.HandleFunc("POST /api/projects/{id}/refresh", a.refreshProject)
	m.HandleFunc("POST /api/projects/{id}/cache/recreate", a.recreateProjectCache)
	m.HandleFunc("POST /api/projects/{id}/key", a.assignProjectKey)
	m.HandleFunc("GET /api/deployments", a.listDeployments)
	m.HandleFunc("GET /api/deployments/root", a.deploymentRoot)
	m.HandleFunc("POST /api/deployments", a.createDeployment)
	m.HandleFunc("POST /api/deployments/{id}/deploy", a.deployNow)
	m.HandleFunc("GET /api/deployments/{id}/history", a.rollbackHistory)
	m.HandleFunc("POST /api/deployments/{id}/rollback", a.temporaryRollback)
	m.HandleFunc("POST /api/deployments/{id}/pause", a.pauseDeployment)
	m.HandleFunc("POST /api/deployments/{id}/resume", a.resumeDeployment)
	m.HandleFunc("POST /api/deployments/{id}/hard-redeploy", a.hardRedeploy)
	m.HandleFunc("POST /api/deployments/{id}/delete", a.deleteDeployment)
	m.HandleFunc("POST /api/deployments/{id}/target", a.updateDeploymentTarget)
	m.HandleFunc("GET /api/deployments/{id}/jobs", a.deploymentJobs)
	m.HandleFunc("GET /api/repository-keys", a.listRepositoryKeys)
	m.HandleFunc("POST /api/repository-keys", a.addRepositoryKey)
	m.HandleFunc("POST /api/repository-keys/{id}/delete", a.deleteRepositoryKey)
	m.HandleFunc("GET /api/ssh-hosts", a.listSSHHostWarnings)
	m.HandleFunc("POST /api/ssh-hosts/approve", a.approveSSHHost)
	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.CanonicalOrigin != "" {
			origin, _ := url.Parse(a.cfg.CanonicalOrigin)
			if !strings.EqualFold(r.Host, origin.Host) {
				http.Error(w, "request host is not the configured application origin", http.StatusMisdirectedRequest)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/opaque/") || strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/webhooks/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if (r.URL.Path == "/login" || r.URL.Path == "/dashboard" || strings.HasPrefix(r.URL.Path, "/dashboard/") || r.URL.Path == "/account/password" || strings.HasPrefix(r.URL.Path, "/account/")) && !a.authSecure(r) {
			http.Error(w, "HTTPS is required", http.StatusUpgradeRequired)
			return
		}
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && (strings.TrimSuffix(r.URL.Path, "/") == "/register" || strings.TrimSuffix(r.URL.Path, "/") == "/recover") && !a.secure(r) {
			http.Error(w, "HTTPS is required", http.StatusUpgradeRequired)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/opaque/") || strings.HasPrefix(r.URL.Path, "/webhooks/") || strings.HasPrefix(r.URL.Path, "/api/") {
			m.ServeHTTP(w, r)
			return
		}
		serveSPA(w, r, m)
	}))
}

func serveSPA(w http.ResponseWriter, r *http.Request, backend http.Handler) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if isFrontendRoute(r.URL.Path) {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		backend.ServeHTTP(w, r)
		return
	}
	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		http.Error(w, "application assets unavailable", http.StatusInternalServerError)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "." {
		name = "index.html"
	}
	if name != ".." && !strings.HasPrefix(name, "../") && (name != "index.html" || strings.Contains(r.Header.Get("Accept"), "text/html")) {
		if data, readErr := fs.ReadFile(assets, name); readErr == nil {
			serveAsset(w, r, name, data)
			return
		}
	}
	if isReservedPath(r.URL.Path) || !isFrontendRoute(r.URL.Path) || !strings.Contains(r.Header.Get("Accept"), "text/html") {
		backend.ServeHTTP(w, r)
		return
	}
	data, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		http.Error(w, "application shell unavailable", http.StatusInternalServerError)
		return
	}
	serveAsset(w, r, "index.html", data)
}

func isReservedPath(p string) bool {
	for _, prefix := range []string{"/opaque/", "/webhooks/", "/api/", "/assets/"} {
		if p == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func isFrontendRoute(p string) bool {
	if isReservedPath(p) || strings.Contains(path.Base(p), ".") {
		return false
	}
	return strings.HasPrefix(p, "/")
}

func serveAsset(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	ext := filepath.Ext(name)
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		switch ext {
		case ".js", ".mjs":
			contentType = "text/javascript; charset=utf-8"
		case ".wasm":
			contentType = "application/wasm"
		case ".css":
			contentType = "text/css; charset=utf-8"
		case ".html":
			contentType = "text/html; charset=utf-8"
		default:
			contentType = "application/octet-stream"
		}
	}
	w.Header().Set("Content-Type", contentType)
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", shellContentSecurityPolicy(data))
	} else if hashedAsset(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// SvelteKit's route announcer has this fixed style attribute. Permit only its
// exact hash; arbitrary inline styles remain blocked.
const baseContentSecurityPolicy = "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; style-src-attr 'unsafe-hashes' 'sha256-S8qMpvofolR8Mpjy4kQvEm7m1q8clzU4dfDH0AmvZjo='; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'"

func shellContentSecurityPolicy(html []byte) string {
	policy := baseContentSecurityPolicy
	text := string(html)
	for len(text) > 0 {
		start := strings.Index(text, "<script")
		if start < 0 {
			break
		}
		openEnd := strings.IndexByte(text[start:], '>')
		if openEnd < 0 {
			break
		}
		openEnd += start
		opening := text[start:openEnd]
		closeAt := strings.Index(text[openEnd+1:], "</script>")
		if closeAt < 0 {
			break
		}
		closeAt += openEnd + 1
		if !strings.Contains(opening, "src=") {
			digest := sha256.Sum256([]byte(text[openEnd+1 : closeAt]))
			policy = strings.Replace(policy, "script-src 'self' 'wasm-unsafe-eval'", "script-src 'self' 'wasm-unsafe-eval' 'sha256-"+base64.StdEncoding.EncodeToString(digest[:])+"'", 1)
		}
		text = text[closeAt+len("</script>"):]
	}
	return policy
}

func hashedAsset(name string) bool {
	for _, part := range strings.Split(filepath.Base(name), ".") {
		if len(part) < 8 || len(part) > 64 {
			continue
		}
		allHashCharacters := true
		for _, c := range part {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-", c) {
				allHashCharacters = false
				break
			}
		}
		if allHashCharacters {
			return true
		}
	}
	return false
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", baseContentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}
func (a *App) secure(r *http.Request) bool {
	if a.authSecure(r) {
		return true
	}
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	host, _, hostErr := net.SplitHostPort(r.Host)
	if hostErr != nil {
		host = strings.Trim(r.Host, "[]")
	}
	return a.cfg.InsecureLocalHTTP && e == nil && isLoopback(ip) && isLoopback(host)
}

// authSecure permits cleartext only when explicitly configured for this origin.
func (a *App) authSecure(r *http.Request) bool {
	if a.https(r) {
		return true
	}
	if !a.cfg.AllowHTTPOrigin || r.TLS != nil || a.cfg.CanonicalOrigin == "" {
		return false
	}
	canonical, err := url.Parse(a.cfg.CanonicalOrigin)
	return err == nil && canonical.Scheme == "http" && sameOrigin(canonical, requestOrigin(r, "http"))
}

func (a *App) https(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil || strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")) != "https" {
		return false
	}
	peer, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	if peer.IsLoopback() {
		return true
	}
	for _, prefix := range a.cfg.TrustedProxies {
		if prefix.Contains(peer.Unmap()) || prefix.Contains(peer) {
			return true
		}
	}
	return false
}
func (a *App) originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, e := url.Parse(origin)
	scheme, host := a.requestScheme(r), r.Host
	if a.cfg.CanonicalOrigin != "" {
		canonical, parseErr := url.Parse(a.cfg.CanonicalOrigin)
		if parseErr != nil {
			return false
		}
		scheme, host = canonical.Scheme, canonical.Host
	}
	canonical, parseErr := url.Parse(scheme + "://" + host)
	return e == nil && parseErr == nil && sameOrigin(u, canonical)
}

func requestOrigin(r *http.Request, scheme string) *url.URL {
	u, _ := url.Parse(scheme + "://" + r.Host)
	return u
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil || a.User != nil || b.User != nil || a.Opaque != "" || b.Opaque != "" || a.Path != "" || b.Path != "" || a.RawQuery != "" || b.RawQuery != "" || a.ForceQuery || b.ForceQuery || a.Fragment != "" || b.Fragment != "" || a.Hostname() == "" || b.Hostname() == "" || !strings.EqualFold(a.Scheme, b.Scheme) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "http") {
			return "80"
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return ""
	}
	return port(a) == port(b)
}
func (a *App) requestScheme(r *http.Request) string {
	if a.https(r) {
		return "https"
	}
	return "http"
}
func (a *App) allow(r *http.Request) bool {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := a.cfg.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.rate[ip]; !ok && len(a.rate) >= 4096 {
		for client, hits := range a.rate {
			active := false
			for _, hit := range hits {
				if now.Sub(hit) < time.Minute {
					active = true
					break
				}
			}
			if !active {
				delete(a.rate, client)
			}
		}
		if len(a.rate) >= 4096 {
			return false
		}
	}
	old := a.rate[ip][:0]
	for _, t := range a.rate[ip] {
		if now.Sub(t) < time.Minute {
			old = append(old, t)
		}
	}
	if len(old) >= a.cfg.AuthRequestsPerMinute {
		a.rate[ip] = old
		return false
	}
	a.rate[ip] = append(old, now)
	return true
}

type protocolMessage struct {
	Grant       string  `json:"grant"`
	Transaction *string `json:"transaction"`
	Value       string  `json:"value"`
}

type registrationTransaction struct {
	grantHash  [32]byte
	purpose    string
	generation int64
	origin     string
	expiresAt  time.Time
}

const registrationTransactionTTL = 5 * time.Minute
const maxRegistrationTransactions = 128

func (a *App) newRegistrationTransactionLocked(grant, purpose, origin string, generation int64) (string, error) {
	now := a.cfg.Now()
	for handle, pending := range a.registrationTxs {
		if !pending.expiresAt.After(now) {
			delete(a.registrationTxs, handle)
		}
	}
	if len(a.registrationTxs) >= maxRegistrationTransactions {
		return "", errors.New("registration is busy; try again")
	}
	grantHash := sha256.Sum256([]byte(grant))
	grantRecord, err := a.store.RegistrationGrant.Get(context.Background(), grantHash[:])
	if err != nil || grantRecord.Purpose != purpose || grantRecord.CredentialGeneration != generation || grantRecord.ExpiresAt < now.UnixNano() {
		return "", errors.New("invalid or expired grant")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	handle := base64.RawURLEncoding.EncodeToString(raw[:])
	deadline := now.Add(registrationTransactionTTL)
	grantDeadline := time.Unix(0, grantRecord.ExpiresAt)
	if grantDeadline.Before(deadline) {
		deadline = grantDeadline
	}
	a.registrationTxs[handle] = registrationTransaction{grantHash: grantHash, purpose: purpose, generation: generation, origin: origin, expiresAt: deadline}
	return handle, nil
}

func (a *App) pendingRegistrationLocked(handle, origin, purpose string) (registrationTransaction, bool) {
	pending, ok := a.registrationTxs[handle]
	if !ok || !pending.expiresAt.After(a.cfg.Now()) || pending.origin != origin || pending.purpose != purpose || pending.generation != a.credentialGeneration {
		return registrationTransaction{}, false
	}
	return pending, true
}

func (a *App) decode(w http.ResponseWriter, r *http.Request) (protocolMessage, bool) {
	var m protocolMessage
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	if r.Method != "POST" || !a.secure(r) || !a.originOK(r) || !a.allow(r) {
		http.Error(w, "request rejected", http.StatusForbidden)
		return m, false
	}
	contentType, _, contentTypeErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentTypeErr != nil || contentType != "application/json" {
		http.Error(w, "JSON content type required", http.StatusUnsupportedMediaType)
		return m, false
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if e := decoder.Decode(&m); e != nil || len(m.Value) == 0 || len(m.Value) > 60000 {
		http.Error(w, "invalid protocol message", 400)
		return m, false
	}
	if e := decoder.Decode(new(any)); e != io.EOF {
		http.Error(w, "invalid protocol message", 400)
		return m, false
	}
	return m, true
}
func decodeValue(s string) ([]byte, error) {
	if len(s) > 60000 {
		return nil, errors.New("message too large")
	}
	return base64.RawURLEncoding.DecodeString(s)
}
func jsonValue(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"value": base64.RawURLEncoding.EncodeToString(b)})
}
func (a *App) registerStart(w http.ResponseWriter, r *http.Request) {
	m, ok := a.decode(w, r)
	if !ok {
		return
	}
	if m.Transaction != nil {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	registered, err := a.HasAdministrator()
	if err != nil {
		http.Error(w, "registration is temporarily unavailable", http.StatusInternalServerError)
		return
	}
	purpose := "setup"
	if registered {
		purpose = "recovery"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.refreshCredentialGenerationLocked(); err != nil {
		http.Error(w, "registration is temporarily unavailable", http.StatusInternalServerError)
		return
	}
	generation := a.credentialGeneration
	if !a.grantValid(m.Grant, purpose, generation) {
		http.Error(w, "invalid or expired grant", 410)
		return
	}
	b, e := decodeValue(m.Value)
	if e != nil {
		http.Error(w, "invalid protocol message", 400)
		return
	}
	request, e := a.des.RegistrationRequest(b)
	if e != nil {
		http.Error(w, "invalid protocol message", 400)
		return
	}
	response, e := a.protocol.RegistrationResponse(request, []byte("administrator"), nil)
	if e != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	handle, e := a.newRegistrationTransactionLocked(m.Grant, purpose, r.Header.Get("Origin"), generation)
	if e != nil {
		http.Error(w, "registration is temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"value": base64.RawURLEncoding.EncodeToString(response.Serialize()), "transaction": handle})
}
func (a *App) registerFinish(w http.ResponseWriter, r *http.Request) {
	m, ok := a.decode(w, r)
	if !ok {
		return
	}
	if m.Transaction == nil || *m.Transaction == "" || m.Grant != "" {
		http.Error(w, "invalid protocol message", http.StatusBadRequest)
		return
	}
	handle := *m.Transaction
	b, e := decodeValue(m.Value)
	if e != nil {
		http.Error(w, "invalid protocol message", 400)
		return
	}
	record, e := a.des.RegistrationRecord(b)
	if e != nil || record == nil {
		http.Error(w, "invalid protocol message", 400)
		return
	}
	registered, e := a.HasAdministrator()
	if e != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.refreshCredentialGenerationLocked(); err != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	purpose := "setup"
	if registered {
		purpose = "recovery"
	}
	pending, valid := a.pendingRegistrationLocked(handle, r.Header.Get("Origin"), purpose)
	if !valid {
		http.Error(w, "registration transaction expired or invalid; start again", http.StatusGone)
		return
	}
	if registered {
		a.replaceCredential(w, pending.grantHash[:], b, "recovery", [32]byte{}, handle, r.Header.Get("Origin"))
		return
	}
	ctx := r.Context()
	tx, e := a.store.Tx(ctx)
	if e != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	defer tx.Rollback()
	count, e := tx.Administrator.Query().Count(ctx)
	if e != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	if count != 0 {
		http.Error(w, "registration is closed", http.StatusGone)
		return
	}
	now := a.cfg.Now().UnixNano()
	res, e := tx.RegistrationGrant.Delete().Where(registrationgrant.IDEQ(pending.grantHash[:]), registrationgrant.PurposeEQ("setup"), registrationgrant.CredentialGenerationEQ(pending.generation), registrationgrant.ExpiresAtGTE(now)).Exec(ctx)
	if e != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	if res != 1 {
		http.Error(w, "grant expired or already used", 410)
		return
	}
	if _, e = tx.Administrator.Create().SetID(1).SetRegistrationRecord(b).SetProfileGeneration(1).SetCredentialGeneration(1).SetCreatedAt(now).Save(ctx); e != nil {
		http.Error(w, "administrator already registered", 409)
		return
	}
	if _, e = tx.RegistrationGrant.Delete().Exec(ctx); e != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	if e = tx.Commit(); e != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	a.credentialGeneration = 1
	a.registrationTxs = make(map[string]registrationTransaction)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) grantValid(token, purpose string, generation int64) bool {
	if len(token) < 32 || len(token) > 128 {
		return false
	}
	h := sha256.Sum256([]byte(token))
	grant, err := a.store.RegistrationGrant.Get(context.Background(), h[:])
	return err == nil && grant.ExpiresAt >= a.cfg.Now().UnixNano() && grant.Purpose == purpose && grant.CredentialGeneration == generation
}
func (a *App) HasAdministrator() (bool, error) {
	n, e := a.store.Administrator.Query().Count(context.Background())
	return n > 0, e
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.Handler().ServeHTTP(w, r) }
