package lib

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	maxLFSAuthResponseBytes = 64 << 10
	maxLFSAuthCredentialTTL = 15 * time.Minute
)

// lfsSSHAuth is deliberately ephemeral: it is returned by the remote
// git-lfs-authenticate service and is never persisted or included in errors.
type lfsSSHAuth struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header"`
	ExpiresIn json.RawMessage   `json:"expires_in"`
	ExpiresAt json.RawMessage   `json:"expires_at"`
}

type lfsAuthCredentials struct {
	endpoint *url.URL
	headers  map[string]string
	expires  time.Time
}

func quoteSSHRemotePath(path string) (string, error) {
	if path == "" || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") || strings.HasPrefix(path, "-") {
		return "", errors.New("SSH repository path is not supported for LFS authentication")
	}
	// POSIX single-quote escaping. The path has already been parsed from the
	// configured SSH URL; quoting keeps shell metacharacters data, not syntax.
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'", nil
}

func parseLFSAuthResponse(raw []byte, approvedEndpoint string, now time.Time) (lfsAuthCredentials, error) {
	var result lfsSSHAuth
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) == 0 || len(raw) > maxLFSAuthResponseBytes || decoder.Decode(&result) != nil {
		return lfsAuthCredentials{}, errors.New("SSH LFS authentication service returned invalid data")
	}
	defer clearLFSStringHeaders(result.Header)
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return lfsAuthCredentials{}, errors.New("SSH LFS authentication service returned invalid data")
	}
	endpoint, err := validateLFSEndpoint(approvedEndpoint)
	if err != nil {
		return lfsAuthCredentials{}, errors.New("approved LFS endpoint is invalid")
	}
	if result.Href != "" {
		provided, parseErr := validateLFSEndpoint(result.Href)
		if parseErr != nil || originOf(provided) != originOf(endpoint) || provided.EscapedPath() != endpoint.EscapedPath() {
			return lfsAuthCredentials{}, errors.New("SSH LFS authentication endpoint is not approved")
		}
		endpoint = provided
	}
	if len(result.Header) > 32 {
		return lfsAuthCredentials{}, errors.New("SSH LFS authentication service returned invalid headers")
	}
	headers := make(map[string]string, len(result.Header))
	keepHeaders := false
	defer func() {
		if !keepHeaders {
			clearLFSStringHeaders(headers)
		}
	}()
	for name, value := range result.Header {
		if len(name) > 128 || len(value) > 8192 || !validLFSActionHeader(name, value) {
			return lfsAuthCredentials{}, errors.New("SSH LFS authentication service returned invalid headers")
		}
		canonical := httpCanonicalHeader(name)
		if canonical == "" {
			return lfsAuthCredentials{}, errors.New("SSH LFS authentication service returned invalid headers")
		}
		for existing := range headers {
			if strings.EqualFold(existing, canonical) {
				return lfsAuthCredentials{}, errors.New("SSH LFS authentication service returned duplicate headers")
			}
		}
		headers[canonical] = value
	}
	expires, err := lfsAuthExpiry(result.ExpiresIn, result.ExpiresAt, now)
	if err != nil {
		return lfsAuthCredentials{}, err
	}
	keepHeaders = true
	return lfsAuthCredentials{endpoint: endpoint, headers: headers, expires: expires}, nil
}

func httpCanonicalHeader(name string) string {
	return http.CanonicalHeaderKey(name)
}

// lfsAuthExpiry interprets expires_in as seconds from response receipt and
// expires_at as RFC3339. Relative expiry takes precedence; credentials are
// accepted only when live and no more than 15 minutes into the future. No
// clock-skew grace is applied; expired values fail closed.
func lfsAuthExpiry(relative, absolute json.RawMessage, now time.Time) (time.Time, error) {
	if len(relative) == 0 && len(absolute) == 0 {
		return time.Time{}, errors.New("SSH LFS authentication service omitted credential expiry")
	}
	var expiry time.Time
	if len(relative) > 0 {
		var seconds int64
		if json.Unmarshal(relative, &seconds) != nil || seconds <= 0 || seconds > int64(maxLFSAuthCredentialTTL/time.Second) {
			return time.Time{}, errors.New("SSH LFS authentication service returned invalid expiry")
		}
		expiry = now.Add(time.Duration(seconds) * time.Second)
	} else {
		var text string
		if json.Unmarshal(absolute, &text) != nil || len(text) > 64 {
			return time.Time{}, errors.New("SSH LFS authentication service returned invalid expiry")
		}
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return time.Time{}, errors.New("SSH LFS authentication service returned invalid expiry")
		}
		expiry = parsed
	}
	if !expiry.After(now) || expiry.After(now.Add(maxLFSAuthCredentialTTL)) {
		return time.Time{}, errors.New("SSH LFS authentication credentials are expired or expire too far in the future")
	}
	return expiry, nil
}

// remoteLFSAuthenticate invokes only the fixed remote LFS service over an
// in-process SSH session. It never invokes a local Git, SSH, or LFS executable.
func remoteLFSAuthenticate(ctx context.Context, host string, port int, user, path string, signer ssh.Signer, callback ssh.HostKeyCallback, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		return nil, errors.New("SSH LFS authentication timeout is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	quoted, err := quoteSSHRemotePath(path)
	if err != nil || signer == nil || callback == nil || port < 1 || port > 65535 || user == "" || len(user) > 256 || strings.ContainsAny(user, "\r\n\x00 /\\") {
		return nil, errors.New("SSH LFS authentication configuration is invalid")
	}
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, errors.New("SSH LFS authentication connection failed")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	config := &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: callback, Timeout: timeout}
	clientConn, channels, requests, err := ssh.NewClientConn(conn, net.JoinHostPort(host, strconv.Itoa(port)), config)
	if err != nil {
		return nil, errors.New("SSH LFS authentication failed; verify the repository key and approved host key")
	}
	client := ssh.NewClient(clientConn, channels, requests)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return nil, errors.New("SSH LFS authentication service is unavailable")
	}
	defer session.Close()
	var stdout, stderr limitedBuffer
	stdout.limit = maxLFSAuthResponseBytes
	stderr.limit = 4096
	stdout.onLimit = func() { _ = conn.Close() }
	stderr.onLimit = func() { _ = conn.Close() }
	defer func() {
		clear(stdout.Bytes())
		clear(stderr.Bytes())
	}()
	session.Stdout = &stdout
	session.Stderr = &stderr
	err = session.Run("git-lfs-authenticate " + quoted + " download")
	if ctx.Err() != nil {
		return nil, errors.New("SSH LFS authentication timed out")
	}
	if stdout.overflow {
		return nil, errors.New("SSH LFS authentication response exceeded the size limit")
	}
	if err != nil {
		return nil, errors.New("SSH LFS authentication service is unsupported or denied the request")
	}
	response := append([]byte(nil), stdout.Bytes()...)
	return response, nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
	onLimit  func()
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.overflow = true
		if b.onLimit != nil {
			b.onLimit()
		}
		return 0, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(p)
}

var _ io.Writer = (*limitedBuffer)(nil)
