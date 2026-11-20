package lib

import (
	"context"
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

	"github.com/eslym/deploy-manager/lib/ent"
	"github.com/eslym/deploy-manager/lib/ent/projectsshkey"
	"github.com/eslym/deploy-manager/lib/ent/repositorykey"
	"golang.org/x/crypto/ssh"
)

const maxRepositoryPrivateKeyBytes = 64 << 10
const repositoryKeyVerifierContext = "deploy-manager repository key encryption verifier v1"

type repositoryKey struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Algorithm    string    `json:"algorithm"`
	Bits         int       `json:"bits"`
	Fingerprint  string    `json:"fingerprint"`
	PublicKey    string    `json:"publicKey"`
	CreatedAt    time.Time `json:"createdAt"`
	ProjectCount int       `json:"projectCount"`
}

// prepareRepositoryKeyStore consumes the pre-schema classification. The mere
// existence of freshly created tables must never authorize key regeneration.
func prepareRepositoryKeyStore(store *ent.Client, db *sql.DB, path string, priorDatabase bool) error {
	key, err := loadRepositoryMasterKey(path, priorDatabase)
	if err != nil {
		return err
	}
	defer clear(key)
	if !priorDatabase {
		tx, txErr := store.Tx(context.Background())
		if txErr != nil {
			return errors.New("could not initialize repository key store")
		}
		defer tx.Rollback()
		if _, txErr = tx.RepositoryKeyStore.Create().SetID(1).SetVerifier(repositoryKeyVerifier(key)).Save(context.Background()); txErr != nil {
			return errors.New("could not initialize repository key store")
		}
		if txErr = tx.Commit(); txErr != nil {
			return errors.New("could not initialize repository key store")
		}
	}
	return validateRepositoryKeyStore(store, db, key)
}

func repositoryKeyVerifier(key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(repositoryKeyVerifierContext))
	return mac.Sum(nil)
}

func validateRepositoryKeyStore(store *ent.Client, db *sql.DB, key []byte) error {
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('repository_key_store','repository_keys')`).Scan(&tables); err != nil || tables != 2 {
		return errors.New("repository key store schema is incomplete; refusing startup")
	}
	binding, err := store.RepositoryKeyStore.Get(context.Background(), 1)
	if err != nil {
		return errors.New("repository key master-key verifier is missing or unavailable; refusing startup")
	}
	if !hmac.Equal(binding.Verifier, repositoryKeyVerifier(key)) {
		return errors.New("repository key master key does not match this database; refusing startup")
	}
	rows, err := store.RepositoryKey.Query().All(context.Background())
	if err != nil {
		return errors.New("repository keys could not be validated")
	}
	for _, row := range rows {
		plain, err := decryptRepositoryKey(key, row.ID, row.Ciphertext)
		if err != nil {
			return errors.New("repository key ciphertext is invalid; refusing startup")
		}
		clear(plain)
	}
	return nil
}

func loadRepositoryMasterKey(path string, priorDatabase bool) ([]byte, error) {
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
	if priorDatabase {
		return nil, errors.New("repository key master file is missing; refusing to regenerate it; preserve the database and key-file evidence, and use isolated rebootstrap only if this was an interrupted fresh initialization")
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
	_, err = a.store.RepositoryKey.Create().SetID(id).SetName(name).SetAlgorithm(alg).SetBits(bits).SetFingerprint(fingerprint).SetPublicKey(string(public)).SetCiphertext(ciphertext).SetCreatedAt(created.UnixNano()).Save(context.Background())
	if err != nil {
		return repositoryKey{}, errors.New("could not save repository key")
	}
	return repositoryKey{ID: id, Name: name, Algorithm: alg, Bits: bits, Fingerprint: fingerprint, PublicKey: strings.TrimSpace(string(public)), CreatedAt: created}, nil
}

func (a *App) listRepositoryKeys(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, false) {
		return
	}
	keys, err := a.store.RepositoryKey.Query().Order(repositorykey.ByCreatedAt(), repositorykey.ByID()).All(r.Context())
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	out := []repositoryKey{}
	for _, record := range keys {
		var projectCount int
		if err := a.db.QueryRowContext(r.Context(), `SELECT count(*) FROM project_ssh_keys WHERE key_id=?`, record.ID).Scan(&projectCount); err != nil {
			writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
			return
		}
		out = append(out, repositoryKey{ID: record.ID, Name: record.Name, Algorithm: record.Algorithm, Bits: record.Bits, Fingerprint: record.Fingerprint, PublicKey: strings.TrimSpace(record.PublicKey), CreatedAt: time.Unix(0, record.CreatedAt).UTC(), ProjectCount: projectCount})
	}
	writeOutcome(w, http.StatusOK, "repository_key.listed", "Repository keys loaded.", out)
}

func (a *App) addRepositoryKey(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRepositoryPrivateKeyBytes+2048)
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeOutcomeError(w, http.StatusUnsupportedMediaType, "request.json_required", "JSON content type required.")
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
			writeOutcomeError(w, http.StatusRequestEntityTooLarge, "request.too_large", "Request exceeds the allowed limit.")
			return
		}
		writeOutcomeError(w, http.StatusBadRequest, "request.invalid_json", "Invalid JSON request.")
		return
	}
	if d.Decode(new(any)) != io.EOF || input.Generate == (input.PrivateKey != "") {
		writeOutcomeError(w, http.StatusBadRequest, "request.invalid", "Check the request and try again.")
		return
	}
	if len(input.PrivateKey) > maxRepositoryPrivateKeyBytes {
		writeOutcomeError(w, http.StatusRequestEntityTooLarge, "request.too_large", "Request exceeds the allowed limit.")
		return
	}
	var private []byte
	if !input.Generate {
		private = []byte(input.PrivateKey)
		defer clear(private)
	}
	key, err := a.createRepositoryKey(input.Name, private, input.Generate)
	if err != nil {
		writeOutcomeError(w, http.StatusBadRequest, "request.invalid", "Check the request and try again.")
		return
	}
	writeOutcome(w, http.StatusCreated, "repository_key.created", "Repository key created.", key)
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
	tx, err := a.store.Tx(r.Context())
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	defer tx.Rollback()
	if _, err := tx.RepositoryKey.Get(r.Context(), id); err != nil {
		writeOutcomeError(w, http.StatusNotFound, "request.not_found", "Resource not found.")
		return
	}
	assignments, err := tx.ProjectSSHKey.Query().Where(projectsshkey.KeyIDEQ(id)).Count(r.Context())
	if err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	if assignments > 0 {
		writeOutcomeError(w, http.StatusConflict, "repository_key.in_use", "Repository key is assigned to a project.")
		return
	}
	if _, err = tx.RepositoryKey.Delete().Where(repositorykey.IDEQ(id)).Exec(r.Context()); err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	if err = tx.Commit(); err != nil {
		writeOutcomeError(w, http.StatusInternalServerError, "service.internal_error", "Operation could not be completed.")
		return
	}
	writeOutcome(w, http.StatusOK, "repository_key.deleted", "Repository key deleted.", nil)
}

func (a *App) repositoryKeyCanSign(id string, challenge []byte) bool {
	record, err := a.store.RepositoryKey.Get(context.Background(), id)
	if err != nil {
		return false
	}
	plain, err := decryptRepositoryKey(a.repositoryMaster, id, record.Ciphertext)
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
