package lib

import (
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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bytemare/opaque"
	_ "github.com/mattn/go-sqlite3"
)

// Config controls durable administrator bootstrap. OPAQUEKeyPath must be external to SQLite.
type Config struct {
	DatabasePath          string
	OPAQUEKeyPath         string
	RepositoryKeyPath     string
	CanonicalOrigin       string
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
	WebhookMaxBodyBytes         int64
	beforeRestoreDeploymentJobs func(*sql.DB) error
	// storageFault is a per-startup test seam, never configured by the CLI or HTTP.
	// "access" runs under the access guard before DB/key work, with a nil DB;
	// "verify" runs after preparation and before storage readiness.
	storageFault func(string, *sql.DB) error
}

type App struct {
	cfg                  Config
	db                   *sql.DB
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
func prepareStorage(cfg Config) (*readyStorage, error) {
	if err := validateStoragePaths(cfg); err != nil {
		return nil, err
	}
	if err := CheckMaintenance(cfg); err != nil {
		return nil, err
	}
	if cfg.DatabasePath == "" || cfg.OPAQUEKeyPath == "" {
		return nil, errors.New("database and external OPAQUE key paths are required")
	}
	if cfg.RepositoryKeyPath == "" {
		cfg.RepositoryKeyPath = cfg.DatabasePath + ".repository-master-key"
	}
	if cfg.CanonicalOrigin != "" {
		origin, err := url.Parse(cfg.CanonicalOrigin)
		if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
			return nil, errors.New("canonical origin must be an HTTPS origin without path, query, or credentials")
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
	if dbInfo, dbErr := os.Stat(dbPath); dbErr == nil {
		if keyInfo, keyErr := os.Stat(keyPath); keyErr == nil && os.SameFile(dbInfo, keyInfo) {
			return nil, errors.New("OPAQUE key must not share the SQLite database file")
		}
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
	if cfg.storageFault != nil {
		if err := cfg.storageFault("access", nil); err != nil {
			return nil, err
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
	c := opaque.DefaultConfiguration()
	material, keyGeneration, err := loadOPAQUEServerKey(c, cfg.OPAQUEKeyPath, prior)
	if err != nil {
		return nil, err
	}
	protocol, err := opaque.NewServer(c)
	if err != nil {
		return nil, err
	}
	if err = protocol.SetKeyMaterial(material); err != nil {
		return nil, err
	}
	des, err := c.Deserializer()
	if err != nil {
		return nil, err
	}
	// Parent traversal was rejected before any filesystem work, and access
	// coordination validated the database directory without symlink ancestors.
	// An absolute URL path selects that same file; a relative URL path would be
	// encoded as a file://authority and rejected by SQLite.
	sqlitePath, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	dbURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(sqlitePath)}).String()
	db, err := sql.Open("sqlite3", dbURL+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=1000&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS registration_grants(token_hash BLOB PRIMARY KEY,expires_at INTEGER NOT NULL,created_at INTEGER NOT NULL,purpose TEXT NOT NULL DEFAULT 'setup',credential_generation INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS administrator(id INTEGER PRIMARY KEY CHECK(id=1),registration_record BLOB NOT NULL,profile_generation INTEGER NOT NULL,credential_generation INTEGER NOT NULL DEFAULT 1,created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS opaque_profile(singleton INTEGER PRIMARY KEY CHECK(singleton=1),profile_generation INTEGER NOT NULL,key_generation TEXT NOT NULL,profile TEXT NOT NULL)`,
	} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := ensureColumn(db, "registration_grants", "purpose", `TEXT NOT NULL DEFAULT 'setup'`); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureColumn(db, "registration_grants", "credential_generation", `INTEGER NOT NULL DEFAULT 0`); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureColumn(db, "administrator", "credential_generation", `INTEGER NOT NULL DEFAULT 1`); err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(cfg.DatabasePath, 0600); err != nil {
		db.Close()
		return nil, err
	}
	var priorRepositoryKeyStore int
	if err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('repository_key_store','repository_keys')`).Scan(&priorRepositoryKeyStore); err != nil {
		db.Close()
		return nil, err
	}
	if err = prepareRepositoryKeyStore(db, cfg.RepositoryKeyPath, priorRepositoryKeyStore != 0); err != nil {
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
	const profile = "RFC9807-OPAQUE-3DH;ristretto255-SHA512;HKDF-SHA512;HMAC-SHA512;Argon2id-v19-i1-m65536-p4;context-empty;identities-default"
	if _, err = db.Exec(`INSERT OR IGNORE INTO opaque_profile(singleton,profile_generation,key_generation,profile) VALUES(1,1,?,?)`, keyGeneration, profile); err != nil {
		db.Close()
		return nil, err
	}
	var storedProfile string
	var profileGeneration int
	var storedKeyGeneration string
	if err = db.QueryRow(`SELECT profile_generation,key_generation,profile FROM opaque_profile WHERE singleton=1`).Scan(&profileGeneration, &storedKeyGeneration, &storedProfile); err != nil {
		db.Close()
		return nil, err
	}
	if profileGeneration != 1 || storedKeyGeneration != keyGeneration || storedProfile != profile {
		db.Close()
		return nil, errors.New("OPAQUE key/profile generation mismatch; refusing authentication startup")
	}
	var accountGeneration int
	err = db.QueryRow(`SELECT profile_generation FROM administrator WHERE id=1`).Scan(&accountGeneration)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		db.Close()
		return nil, err
	}
	if err == nil && accountGeneration != profileGeneration {
		db.Close()
		return nil, errors.New("administrator/profile generation mismatch; refusing authentication startup")
	}
	var credentialGeneration int64
	if err = db.QueryRow(`SELECT credential_generation FROM administrator WHERE id=1`).Scan(&credentialGeneration); err != nil && !errors.Is(err, sql.ErrNoRows) {
		db.Close()
		return nil, err
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
	app := &App{cfg: cfg, db: db, protocol: protocol, des: des, keyGeneration: keyGeneration, credentialGeneration: credentialGeneration, rate: make(map[string][]time.Time), logins: make(map[string]pendingLogin), sessions: make(map[[32]byte]sessionState), repositoryMaster: repositoryMaster, registrationTxs: make(map[string]registrationTransaction)}
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
	if err := initProjectSchema(db); err != nil {
		return nil, err
	}
	if err := app.initWebhooks(); err != nil {
		db.Close()
		return nil, err
	}
	if err := app.initDeploymentSchema(); err != nil {
		db.Close()
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
	app.storageAccess = access
	accessTransferred = true
	return &readyStorage{app: app}, nil
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
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
		return nil, "", errors.New("OPAQUE key file is missing; refusing to regenerate authentication keys")
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
	tx, e := a.db.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	var n int
	if e = tx.QueryRow(`SELECT count(*) FROM administrator`).Scan(&n); e != nil {
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
		if e = tx.QueryRow(`SELECT credential_generation FROM administrator WHERE id=1`).Scan(&generation); e != nil {
			return "", e
		}
	}
	now := a.cfg.Now()
	if _, e = tx.Exec(`DELETE FROM registration_grants WHERE expires_at<?`, now.UnixNano()); e != nil {
		return "", e
	}
	if e = tx.QueryRow(`SELECT count(*) FROM registration_grants`).Scan(&n); e != nil {
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
	if _, e = tx.Exec(`INSERT INTO registration_grants(token_hash,expires_at,created_at,purpose,credential_generation) VALUES(?,?,?,?,?)`, h[:], now.Add(a.cfg.GrantTTL).UnixNano(), now.UnixNano(), purpose, generation); e != nil {
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
	if u.Scheme != "https" && !(a.cfg.InsecureLocalHTTP && isLoopback(u.Hostname())) {
		return "", errors.New("registration URL must use HTTPS; HTTP is allowed only for loopback")
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
	m.HandleFunc("POST /api/projects/{id}/key", a.assignProjectKey)
	m.HandleFunc("GET /api/deployments", a.listDeployments)
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
		if (r.URL.Path == "/login" || r.URL.Path == "/dashboard" || strings.HasPrefix(r.URL.Path, "/dashboard/") || r.URL.Path == "/account/password" || strings.HasPrefix(r.URL.Path, "/account/")) && !a.https(r) {
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
	if a.https(r) {
		return true
	}
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	host, _, hostErr := net.SplitHostPort(r.Host)
	if hostErr != nil {
		host = strings.Trim(r.Host, "[]")
	}
	return a.cfg.InsecureLocalHTTP && e == nil && isLoopback(ip) && isLoopback(host)
}

func (a *App) https(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e == nil && isLoopback(ip) && strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")) == "https" {
		return true
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
	return e == nil && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.Scheme == scheme && strings.EqualFold(u.Host, host)
}
func (a *App) requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && isLoopback(ip) && strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")) == "https" {
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
	var expires int64
	if err := a.db.QueryRow(`SELECT expires_at FROM registration_grants WHERE token_hash=? AND purpose=? AND credential_generation=?`, grantHash[:], purpose, generation).Scan(&expires); err != nil || expires < now.UnixNano() {
		return "", errors.New("invalid or expired grant")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	handle := base64.RawURLEncoding.EncodeToString(raw[:])
	deadline := now.Add(registrationTransactionTTL)
	grantDeadline := time.Unix(0, expires)
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
	tx, e := a.db.Begin()
	if e != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	defer tx.Rollback()
	var count int
	if e = tx.QueryRow(`SELECT count(*) FROM administrator`).Scan(&count); e != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	if count != 0 {
		http.Error(w, "registration is closed", http.StatusGone)
		return
	}
	now := a.cfg.Now().UnixNano()
	res, e := tx.Exec(`DELETE FROM registration_grants WHERE token_hash=? AND purpose='setup' AND credential_generation=? AND expires_at>=?`, pending.grantHash[:], pending.generation, now)
	if e != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		http.Error(w, "grant expired or already used", 410)
		return
	}
	if _, e = tx.Exec(`INSERT INTO administrator(id,registration_record,profile_generation,created_at) VALUES(1,?,1,?)`, b, now); e != nil {
		http.Error(w, "administrator already registered", 409)
		return
	}
	if _, e = tx.Exec(`DELETE FROM registration_grants`); e != nil {
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
	var exp, storedGeneration int64
	err := a.db.QueryRow(`SELECT expires_at,credential_generation FROM registration_grants WHERE token_hash=? AND purpose=?`, h[:], purpose).Scan(&exp, &storedGeneration)
	return err == nil && exp >= a.cfg.Now().UnixNano() && storedGeneration == generation
}
func (a *App) HasAdministrator() (bool, error) {
	var n int
	e := a.db.QueryRow(`SELECT count(*) FROM administrator`).Scan(&n)
	return n > 0, e
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.Handler().ServeHTTP(w, r) }
