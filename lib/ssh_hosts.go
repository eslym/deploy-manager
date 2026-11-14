package lib

import (
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/idna"
)

type sshHostWarning struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Fingerprint string `json:"fingerprint"`
	Warning     string `json:"warning"`
	Trusted     bool   `json:"trusted"`
}

func init() {
	// The application deliberately supports only the repository URL's explicit
	// host, port, and user. Do not inherit host aliases, proxies, agents, or
	// known_hosts trust from a service account's SSH configuration.
	gitssh.DefaultSSHConfig = nil
	gitclient.InstallProtocol("ssh", gitssh.NewClient(nil))
}

func (a *App) projectSSHAuth(keyID, host string, port int, user string) (transport.AuthMethod, error) {
	var ciphertext []byte
	if a.db.QueryRow(`SELECT ciphertext FROM repository_keys WHERE id=?`, keyID).Scan(&ciphertext) != nil {
		return nil, errors.New("managed repository key is unavailable")
	}
	plain, err := decryptRepositoryKey(a.repositoryMaster, keyID, ciphertext)
	if err != nil {
		return nil, errors.New("managed repository key is unavailable")
	}
	defer clear(plain)
	key, err := ssh.ParseRawPrivateKey(plain)
	if err != nil {
		key, err = x509.ParsePKCS8PrivateKey(plain)
	}
	if err != nil {
		return nil, errors.New("managed repository key is invalid")
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, errors.New("managed repository key is invalid")
	}
	auth := &gitssh.PublicKeys{User: user, Signer: signer}
	auth.HostKeyCallback = func(gotHost string, _ net.Addr, observed ssh.PublicKey) error {
		if canonicalSSHHost(gotHost) != host {
			return errors.New("SSH host does not match the configured repository host")
		}
		var trusted []byte
		err := a.db.QueryRow(`SELECT trusted_key FROM ssh_host_trust WHERE host=? AND port=?`, host, port).Scan(&trusted)
		if err == nil && len(trusted) > 0 && string(trusted) == string(observed.Marshal()) {
			return nil
		}
		fingerprint := ssh.FingerprintSHA256(observed)
		warning := "SSH host key changed; verify the new fingerprint independently before approval"
		if errors.Is(err, sql.ErrNoRows) || (err == nil && len(trusted) == 0) {
			warning = "SSH host key is not approved; verify the fingerprint independently before approval"
		}
		_, writeErr := a.db.Exec(`INSERT INTO ssh_host_trust(host,port,observed_key,observed_fingerprint,warning) VALUES(?,?,?,?,?) ON CONFLICT(host,port) DO UPDATE SET observed_key=excluded.observed_key,observed_fingerprint=excluded.observed_fingerprint,warning=excluded.warning`, host, port, observed.Marshal(), fingerprint, warning)
		if writeErr != nil {
			return errors.New("SSH host key could not be verified because trust storage is unavailable")
		}
		return errors.New("SSH host key is not approved")
	}
	return timedPublicKeys{PublicKeys: auth, timeout: a.projects.timeout}, nil
}

type timedPublicKeys struct {
	*gitssh.PublicKeys
	timeout time.Duration
}

func (a timedPublicKeys) ClientConfig() (*ssh.ClientConfig, error) {
	config, err := a.PublicKeys.ClientConfig()
	if err == nil {
		config.Timeout = a.timeout
	}
	return config, err
}

func canonicalSSHHost(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if normalized, err := normalizeSSHHostname(host); err == nil {
		return normalized
	}
	return host
}

func normalizeSSHHostname(host string) (string, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if address, err := netip.ParseAddr(host); err == nil {
		return address.String(), nil
	}
	if host == "" || strings.ContainsAny(host, "/\\\r\n\x00 :") {
		return "", errors.New("invalid SSH hostname")
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" {
		return "", errors.New("invalid SSH hostname")
	}
	return strings.ToLower(ascii), nil
}

func (a *App) listSSHHostWarnings(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, false) {
		return
	}
	rows, err := a.db.Query(`SELECT host,port,observed_fingerprint,warning,trusted_key FROM ssh_host_trust WHERE warning<>'' ORDER BY host,port`)
	if err != nil {
		http.Error(w, "SSH host trust is temporarily unavailable", 500)
		return
	}
	defer rows.Close()
	out := []sshHostWarning{}
	for rows.Next() {
		var x sshHostWarning
		var trusted []byte
		if rows.Scan(&x.Host, &x.Port, &x.Fingerprint, &x.Warning, &trusted) != nil {
			http.Error(w, "SSH host trust is temporarily unavailable", 500)
			return
		}
		x.Trusted = len(trusted) > 0
		out = append(out, x)
	}
	if rows.Err() != nil {
		http.Error(w, "SSH host trust is temporarily unavailable", 500)
		return
	}
	writeJSON(w, out)
}

func (a *App) approveSSHHost(w http.ResponseWriter, r *http.Request) {
	if !a.projectAuthorized(w, r, true) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	var input struct {
		Host        string `json:"host"`
		Port        int    `json:"port"`
		Fingerprint string `json:"fingerprint"`
		Confirm     bool   `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !input.Confirm {
		http.Error(w, "confirm independent verification before approving this SSH host key", 400)
		return
	}
	host, hostErr := normalizeSSHHostname(input.Host)
	if hostErr != nil || input.Port < 1 || input.Port > 65535 || !strings.HasPrefix(input.Fingerprint, "SHA256:") {
		http.Error(w, "invalid SSH host key approval", 400)
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "SSH host trust is temporarily unavailable", 500)
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE ssh_host_trust SET trusted_key=observed_key,warning='' WHERE host=? AND port=? AND observed_fingerprint=? AND length(observed_key)>0`, host, input.Port, input.Fingerprint)
	if err != nil {
		http.Error(w, "SSH host trust is temporarily unavailable", 500)
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		http.Error(w, "the observed SSH key has changed; refresh the warning and verify it again", 409)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "SSH host trust is temporarily unavailable", 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
