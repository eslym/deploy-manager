package lib

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/eslym/deploy-manager/lib/ent"
	"github.com/eslym/deploy-manager/lib/ent/sshhosttrust"
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
	signer, err := a.projectSSHSigner(keyID)
	if err != nil {
		return nil, err
	}
	auth := &gitssh.PublicKeys{User: user, Signer: signer}
	auth.HostKeyCallback = a.projectSSHHostKeyCallback(host, port)
	return timedPublicKeys{PublicKeys: auth, timeout: a.projects.timeout}, nil
}

func (a *App) projectSSHSigner(keyID string) (ssh.Signer, error) {
	record, err := a.store.RepositoryKey.Get(context.Background(), keyID)
	if err != nil {
		return nil, errors.New("managed repository key is unavailable")
	}
	plain, err := decryptRepositoryKey(a.repositoryMaster, keyID, record.Ciphertext)
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
	return signer, nil
}

func (a *App) projectSSHHostKeyCallback(host string, port int) ssh.HostKeyCallback {
	return func(gotHost string, _ net.Addr, observed ssh.PublicKey) error {
		if canonicalSSHHost(gotHost) != host {
			return errors.New("SSH host does not match the configured repository host")
		}
		ctx := context.Background()
		row, err := a.store.SSHHostTrust.Query().Where(sshhosttrust.IDEQ(host), sshhosttrust.PortEQ(port)).Only(ctx)
		trusted := []byte(nil)
		if err == nil {
			trusted = row.TrustedKey
		}
		if err == nil && len(trusted) > 0 && string(trusted) == string(observed.Marshal()) {
			return nil
		}
		fingerprint := ssh.FingerprintSHA256(observed)
		warning := "SSH host key changed; verify the new fingerprint independently before approval"
		if ent.IsNotFound(err) || (err == nil && len(trusted) == 0) {
			warning = "SSH host key is not approved; verify the fingerprint independently before approval"
		}
		tx, writeErr := a.store.Tx(ctx)
		if writeErr == nil {
			query := tx.SSHHostTrust.Query().Where(sshhosttrust.IDEQ(host), sshhosttrust.PortEQ(port))
			exists, queryErr := query.Exist(ctx)
			if queryErr != nil {
				writeErr = queryErr
			} else if exists {
				_, writeErr = tx.SSHHostTrust.Update().Where(sshhosttrust.IDEQ(host), sshhosttrust.PortEQ(port)).SetObservedKey(observed.Marshal()).SetObservedFingerprint(fingerprint).SetWarning(warning).Save(ctx)
			} else {
				_, writeErr = tx.SSHHostTrust.Create().SetID(host).SetPort(port).SetObservedKey(observed.Marshal()).SetObservedFingerprint(fingerprint).SetWarning(warning).Save(ctx)
			}
			if writeErr == nil {
				writeErr = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
		if writeErr != nil {
			return errors.New("SSH host key could not be verified because trust storage is unavailable")
		}
		return errors.New("SSH host key is not approved")
	}
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
	rows, err := a.store.SSHHostTrust.Query().Where(sshhosttrust.WarningNEQ("")).Order(sshhosttrust.ByID(), sshhosttrust.ByPort()).All(r.Context())
	if err != nil {
		http.Error(w, "SSH host trust is temporarily unavailable", 500)
		return
	}
	out := []sshHostWarning{}
	for _, row := range rows {
		out = append(out, sshHostWarning{Host: row.ID, Port: row.Port, Fingerprint: row.ObservedFingerprint, Warning: row.Warning, Trusted: len(row.TrustedKey) > 0})
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
	tx, err := a.store.Tx(r.Context())
	if err != nil {
		http.Error(w, "SSH host trust is temporarily unavailable", 500)
		return
	}
	defer tx.Rollback()
	row, err := tx.SSHHostTrust.Query().Where(sshhosttrust.IDEQ(host), sshhosttrust.PortEQ(input.Port), sshhosttrust.ObservedFingerprintEQ(input.Fingerprint), sshhosttrust.ObservedKeyNotNil()).Only(r.Context())
	if err != nil {
		http.Error(w, "the observed SSH key has changed; refresh the warning and verify it again", 409)
		return
	}
	res, err := tx.SSHHostTrust.Update().Where(sshhosttrust.IDEQ(host), sshhosttrust.PortEQ(input.Port), sshhosttrust.ObservedFingerprintEQ(input.Fingerprint), sshhosttrust.ObservedKeyEQ(row.ObservedKey)).SetTrustedKey(row.ObservedKey).SetWarning("").Save(r.Context())
	if err != nil {
		http.Error(w, "SSH host trust is temporarily unavailable", 500)
		return
	}
	if res != 1 {
		http.Error(w, "the observed SSH key has changed; refresh the warning and verify it again", 409)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "SSH host trust is temporarily unavailable", 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
