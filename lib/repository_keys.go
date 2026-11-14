package lib

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

const maxRepositoryPrivateKeyBytes = 64 << 10
const repositoryKeyVerifierContext = "deploy-manager repository key encryption verifier v1"

type repositoryKey struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Algorithm   string    `json:"algorithm"`
	Bits        int       `json:"bits"`
	Fingerprint string    `json:"fingerprint"`
	PublicKey   string    `json:"publicKey"`
	CreatedAt   time.Time `json:"createdAt"`
}

// prepareRepositoryKeyStore consumes the pre-schema classification. The mere
// existence of freshly created tables must never authorize key regeneration.
func prepareRepositoryKeyStore(db *sql.DB, path string, priorStore bool) error {
	key, err := loadRepositoryMasterKey(path, priorStore)
	if err != nil {
		return err
	}
	defer clear(key)
	if !priorStore {
		tx, txErr := db.Begin()
		if txErr != nil {
			return errors.New("could not initialize repository key store")
		}
		defer tx.Rollback()
		if txErr = createRepositoryKeySchema(tx); txErr != nil {
			return errors.New("could not initialize repository key store")
		}
		if txErr = initializeRepositoryKeyBinding(tx, key); txErr != nil {
			return errors.New("could not initialize repository key store")
		}
		if txErr = tx.Commit(); txErr != nil {
			return errors.New("could not initialize repository key store")
		}
	}
	return validateRepositoryKeyStore(db, key)
}

// Structural DDL is deliberately independent from the external key/binding.
// Fresh preparation keeps DDL and binding insertion in one transaction.
func createRepositoryKeySchema(tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE TABLE repository_key_store(singleton INTEGER PRIMARY KEY CHECK(singleton=1),verifier BLOB NOT NULL)`,
		`CREATE TABLE repository_keys(id TEXT PRIMARY KEY,name TEXT NOT NULL,algorithm TEXT NOT NULL,bits INTEGER NOT NULL,fingerprint TEXT NOT NULL,public_key TEXT NOT NULL,ciphertext BLOB NOT NULL,created_at INTEGER NOT NULL)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func repositoryKeyVerifier(key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(repositoryKeyVerifierContext))
	return mac.Sum(nil)
}

func initializeRepositoryKeyBinding(tx *sql.Tx, key []byte) error {
	_, err := tx.Exec(`INSERT INTO repository_key_store(singleton,verifier) VALUES(1,?)`, repositoryKeyVerifier(key))
	return err
}

func validateRepositoryKeyStore(db *sql.DB, key []byte) error {
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('repository_key_store','repository_keys')`).Scan(&tables); err != nil || tables != 2 {
		return errors.New("repository key store schema is incomplete; refusing startup")
	}
	var stored []byte
	if err := db.QueryRow(`SELECT verifier FROM repository_key_store WHERE singleton=1`).Scan(&stored); err != nil {
		return errors.New("repository key master-key verifier is missing or unavailable; refusing startup")
	}
	if !hmac.Equal(stored, repositoryKeyVerifier(key)) {
		return errors.New("repository key master key does not match this database; refusing startup")
	}
	rows, err := db.Query(`SELECT id,ciphertext FROM repository_keys`)
	if err != nil {
		return errors.New("repository keys could not be validated")
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ciphertext []byte
		if err := rows.Scan(&id, &ciphertext); err != nil {
			return errors.New("repository keys could not be validated")
		}
		plain, err := decryptRepositoryKey(key, id, ciphertext)
		if err != nil {
			return errors.New("repository key ciphertext is invalid; refusing startup")
		}
		clear(plain)
	}
	return rows.Err()
}

func loadRepositoryMasterKey(path string, priorKeyStore bool) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		defer f.Close()
		info, statErr := f.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, errors.New("repository key master file must be a regular mode-0600 file")
		}
		b, readErr := io.ReadAll(io.LimitReader(f, 33))
		if readErr != nil {
			return nil, errors.New("repository key master file is unavailable")
		}
		if len(b) != 32 {
			return nil, errors.New("repository key master file has invalid length")
		}
		return b, nil
	}
	if !os.IsNotExist(err) {
		return nil, errors.New("repository key master file is unavailable")
	}
	if priorKeyStore {
		return nil, errors.New("repository key master file is missing; refusing to regenerate it")
	}
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return nil, errors.New("could not initialize repository key master file")
	}
	if err = writeExclusive(path, b); err != nil {
		return nil, errors.New("could not create repository key master file exclusively")
	}
	if err = syncParent(path); err != nil {
		return nil, errors.New("could not durably initialize repository key master file")
	}
	return b, nil
}

var syncParent = func(path string) error {
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

func encryptRepositoryKey(master []byte, id string, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(master)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, []byte("repository-key:"+id)), nil
}
func decryptRepositoryKey(master []byte, id string, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(master)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("invalid ciphertext")
	}
	return gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], []byte("repository-key:"+id))
}

func parseRepositoryPrivateKey(data []byte) (any, ssh.PublicKey, string, int, error) {
	if len(data) == 0 || len(data) > maxRepositoryPrivateKeyBytes {
		return nil, nil, "", 0, errors.New("invalid key")
	}
	key, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		return nil, nil, "", 0, errors.New("invalid key")
	}
	if private, ok := key.(*ed25519.PrivateKey); ok {
		key = *private
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, nil, "", 0, errors.New("invalid key")
	}
	alg, bits := "", 0
	switch k := key.(type) {
	case *rsa.PrivateKey:
		alg = "RSA"
		bits = k.N.BitLen()
		if bits < 3072 {
			return nil, nil, "", 0, errors.New("unsupported key")
		}
	case *ecdsa.PrivateKey:
		if k.Curve != elliptic.P256() && k.Curve != elliptic.P384() && k.Curve != elliptic.P521() {
			return nil, nil, "", 0, errors.New("unsupported key")
		}
		alg = "ECDSA-" + k.Curve.Params().Name
		bits = k.Curve.Params().BitSize
	case ed25519.PrivateKey:
		alg = "Ed25519"
		bits = 256
	default:
		return nil, nil, "", 0, errors.New("unsupported key")
	}
	return key, signer.PublicKey(), alg, bits, nil
}

func (a *App) createRepositoryKey(name string, private []byte, generated bool) (repositoryKey, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return repositoryKey{}, errors.New("invalid name")
	}
	var privateKey any
	var pub ssh.PublicKey
	var alg string
	var bits int
	var err error
	if generated {
		pubkey, privkey, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return repositoryKey{}, errors.New("key generation failed")
		}
		privateKey = privkey
		pub, err = ssh.NewPublicKey(pubkey)
		alg = "Ed25519"
		bits = 256
	} else {
		privateKey, pub, alg, bits, err = parseRepositoryPrivateKey(private)
	}
	if err != nil {
		return repositoryKey{}, errors.New("unsupported private key; use unencrypted Ed25519, RSA (3072 bits or stronger), or ECDSA P-256/P-384/P-521 OpenSSH or PEM")
	}
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		return repositoryKey{}, errors.New("key creation failed")
	}
	id := hex.EncodeToString(idBytes)
	privatePEM := []byte{}
	if generated {
		privatePEM, err = x509.MarshalPKCS8PrivateKey(privateKey)
	} else {
		privatePEM = append(privatePEM, private...)
	}
	if err != nil {
		return repositoryKey{}, errors.New("key creation failed")
	}
	ciphertext, err := encryptRepositoryKey(a.repositoryMaster, id, privatePEM)
	clear(privatePEM)
	if err != nil {
		return repositoryKey{}, errors.New("key creation failed")
	}
	public := ssh.MarshalAuthorizedKey(pub)
	fingerprint := ssh.FingerprintSHA256(pub)
	created := a.cfg.Now().UTC()
	_, err = a.db.Exec(`INSERT INTO repository_keys(id,name,algorithm,bits,fingerprint,public_key,ciphertext,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, name, alg, bits, fingerprint, string(public), ciphertext, created.UnixNano())
	if err != nil {
		return repositoryKey{}, errors.New("could not save repository key")
	}
	return repositoryKey{ID: id, Name: name, Algorithm: alg, Bits: bits, Fingerprint: fingerprint, PublicKey: strings.TrimSpace(string(public)), CreatedAt: created}, nil
}

func (a *App) listRepositoryKeys(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, false) {
		return
	}
	rows, err := a.db.Query(`SELECT id,name,algorithm,bits,fingerprint,public_key,created_at FROM repository_keys ORDER BY created_at,id`)
	if err != nil {
		http.Error(w, "repository keys are temporarily unavailable", 500)
		return
	}
	defer rows.Close()
	out := []repositoryKey{}
	for rows.Next() {
		var k repositoryKey
		var created int64
		if rows.Scan(&k.ID, &k.Name, &k.Algorithm, &k.Bits, &k.Fingerprint, &k.PublicKey, &created) != nil {
			http.Error(w, "repository keys are temporarily unavailable", 500)
			return
		}
		k.PublicKey = strings.TrimSpace(k.PublicKey)
		k.CreatedAt = time.Unix(0, created).UTC()
		out = append(out, k)
	}
	if rows.Err() != nil {
		http.Error(w, "repository keys are temporarily unavailable", 500)
		return
	}
	writeJSON(w, out)
}

func (a *App) addRepositoryKey(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRepositoryPrivateKeyBytes+2048)
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		http.Error(w, "JSON content type required", 415)
		return
	}
	var input struct {
		Name       string `json:"name"`
		PrivateKey string `json:"privateKey"`
		Generate   bool   `json:"generate"`
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	decodeErr := d.Decode(&input)
	if decodeErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(decodeErr, &tooLarge) {
			http.Error(w, "private key request exceeds the size limit", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid key request", 400)
		return
	}
	if d.Decode(new(any)) != io.EOF || input.Generate == (input.PrivateKey != "") {
		http.Error(w, "invalid key request", 400)
		return
	}
	if len(input.PrivateKey) > maxRepositoryPrivateKeyBytes {
		http.Error(w, "private key exceeds the 64 KiB limit", 413)
		return
	}
	var private []byte
	if !input.Generate {
		private = []byte(input.PrivateKey)
		defer clear(private)
	}
	key, err := a.createRepositoryKey(input.Name, private, input.Generate)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSONStatus(w, http.StatusCreated, key)
}

func (a *App) deleteRepositoryKey(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	a.projects.mu.Lock()
	defer a.projects.mu.Unlock()
	a.projects.lfsAuthMu.Lock()
	defer a.projects.lfsAuthMu.Unlock()
	id := r.PathValue("id")
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "repository key is temporarily unavailable", 500)
		return
	}
	defer tx.Rollback()
	var found int
	if tx.QueryRow(`SELECT 1 FROM repository_keys WHERE id=?`, id).Scan(&found) != nil {
		http.NotFound(w, r)
		return
	}
	var assignments int
	if tx.QueryRow(`SELECT count(*) FROM project_ssh_keys WHERE key_id=?`, id).Scan(&assignments) != nil {
		http.Error(w, "repository key is temporarily unavailable", 500)
		return
	}
	if assignments > 0 {
		http.Error(w, "remove this key from every SSH project before deleting it", http.StatusConflict)
		return
	}
	if _, err = tx.Exec(`DELETE FROM repository_keys WHERE id=?`, id); err != nil {
		http.Error(w, "repository key is temporarily unavailable", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "repository key is temporarily unavailable", 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) repositoryKeyCanSign(id string, challenge []byte) bool {
	var ciphertext []byte
	if a.db.QueryRow(`SELECT ciphertext FROM repository_keys WHERE id=?`, id).Scan(&ciphertext) != nil {
		return false
	}
	plain, err := decryptRepositoryKey(a.repositoryMaster, id, ciphertext)
	if err != nil {
		return false
	}
	defer clear(plain)
	key, err := ssh.ParseRawPrivateKey(plain)
	if err != nil { // Generated PKCS#8 is supported by crypto/x509.
		key, err = x509.ParsePKCS8PrivateKey(plain)
	}
	if err != nil {
		return false
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return false
	}
	_, err = signer.Sign(rand.Reader, challenge)
	return err == nil
}
