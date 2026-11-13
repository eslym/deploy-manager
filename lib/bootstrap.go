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
	DatabasePath      string
	OPAQUEKeyPath     string
	GrantTTL          time.Duration
	InsecureLocalHTTP bool
	Now               func() time.Time
}

type App struct {
	cfg           Config
	db            *sql.DB
	protocol      *opaque.Server
	des           *opaque.Deserializer
	keyGeneration string
	mu            sync.Mutex
	rate          map[string][]time.Time
}

func New(cfg Config) (*App, error) {
	if cfg.DatabasePath == "" || cfg.OPAQUEKeyPath == "" {
		return nil, errors.New("database and external OPAQUE key paths are required")
	}
	dbPath, keyPath := filepath.Clean(cfg.DatabasePath), filepath.Clean(cfg.OPAQUEKeyPath)
	if keyPath == dbPath || keyPath == dbPath+"-wal" || keyPath == dbPath+"-shm" {
		return nil, errors.New("OPAQUE key must be separate from SQLite")
	}
	if dbInfo, dbErr := os.Stat(dbPath); dbErr == nil {
		if keyInfo, keyErr := os.Stat(keyPath); keyErr == nil && os.SameFile(dbInfo, keyInfo) {
			return nil, errors.New("OPAQUE key must not share the SQLite database file")
		}
	}
	if cfg.GrantTTL == 0 {
		cfg.GrantTTL = 15 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	prior, err := pathExists(cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.OPAQUEKeyPath), 0700); err != nil {
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
	dbURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(cfg.DatabasePath)}).String()
	db, err := sql.Open("sqlite3", dbURL+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS registration_grants(token_hash BLOB PRIMARY KEY,expires_at INTEGER NOT NULL,created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS administrator(id INTEGER PRIMARY KEY CHECK(id=1),registration_record BLOB NOT NULL,profile_generation INTEGER NOT NULL,created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS opaque_profile(singleton INTEGER PRIMARY KEY CHECK(singleton=1),profile_generation INTEGER NOT NULL,key_generation TEXT NOT NULL,profile TEXT NOT NULL)`,
	} {
		if _, err = db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err = os.Chmod(cfg.DatabasePath, 0600); err != nil {
		db.Close()
		return nil, err
	}
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
	return &App{cfg: cfg, db: db, protocol: protocol, des: des, keyGeneration: keyGeneration, rate: make(map[string][]time.Time)}, nil
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
func (a *App) Close() error { return a.db.Close() }

// IssueRegistrationGrant returns a random one-time token; only its SHA-256 digest is stored.
func (a *App) IssueRegistrationGrant() (string, error) {
	tx, e := a.db.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	var n int
	if e = tx.QueryRow(`SELECT count(*) FROM administrator`).Scan(&n); e != nil {
		return "", e
	}
	if n != 0 {
		return "", errors.New("administrator already registered")
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
	if _, e = tx.Exec(`INSERT INTO registration_grants(token_hash,expires_at,created_at) VALUES(?,?,?)`, h[:], now.Add(a.cfg.GrantTTL).UnixNano(), now.UnixNano()); e != nil {
		return "", e
	}
	if e = tx.Commit(); e != nil {
		return "", e
	}
	return token, nil
}
func (a *App) RegistrationURL(base, token string) (string, error) {
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
	u.Path = "/register"
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
	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.TrimSuffix(r.URL.Path, "/") == "/register" && !a.secure(r) {
			http.Error(w, "HTTPS is required", http.StatusUpgradeRequired)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/opaque/") || strings.HasPrefix(r.URL.Path, "/webhooks/") {
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

func shellContentSecurityPolicy(html []byte) string {
	policy := "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'"
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
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
func (a *App) secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e == nil && isLoopback(ip) && strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")) == "https" {
		return true
	}
	host, _, hostErr := net.SplitHostPort(r.Host)
	if hostErr != nil {
		host = strings.Trim(r.Host, "[]")
	}
	return a.cfg.InsecureLocalHTTP && e == nil && isLoopback(ip) && isLoopback(host)
}
func (a *App) originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, e := url.Parse(origin)
	scheme := a.requestScheme(r)
	return e == nil && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host)
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
func (a *App) validGrant(token string) bool {
	if len(token) < 32 || len(token) > 128 {
		return false
	}
	h := sha256.Sum256([]byte(token))
	var exp int64
	e := a.db.QueryRow(`SELECT expires_at FROM registration_grants WHERE token_hash=?`, h[:]).Scan(&exp)
	return e == nil && exp >= a.cfg.Now().UnixNano()
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
	if len(old) >= 10 {
		a.rate[ip] = old
		return false
	}
	a.rate[ip] = append(old, now)
	return true
}

type protocolMessage struct {
	Grant string `json:"grant"`
	Value string `json:"value"`
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
	registered, err := a.HasAdministrator()
	if err != nil {
		http.Error(w, "registration is temporarily unavailable", http.StatusInternalServerError)
		return
	}
	if registered {
		http.Error(w, "registration is closed", http.StatusGone)
		return
	}
	if !a.validGrant(m.Grant) {
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
	jsonValue(w, response.Serialize())
}
func (a *App) registerFinish(w http.ResponseWriter, r *http.Request) {
	m, ok := a.decode(w, r)
	if !ok {
		return
	}
	token := m.Grant
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
	hash := sha256.Sum256([]byte(token))
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
	res, e := tx.Exec(`DELETE FROM registration_grants WHERE token_hash=? AND expires_at>=?`, hash[:], now)
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
	if e = tx.Commit(); e != nil {
		http.Error(w, "registration unavailable", 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (a *App) HasAdministrator() (bool, error) {
	var n int
	e := a.db.QueryRow(`SELECT count(*) FROM administrator`).Scan(&n)
	return n > 0, e
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.Handler().ServeHTTP(w, r) }
