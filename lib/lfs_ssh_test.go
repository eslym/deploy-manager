package lib

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestQuoteSSHRemotePathPreservesDataWithoutShellSyntax(t *testing.T) {
	quoted, err := quoteSSHRemotePath("/team/a repo/'$(touch nope).git")
	if err != nil {
		t.Fatal(err)
	}
	if quoted != "'/team/a repo/'\\''$(touch nope).git'" {
		t.Fatalf("quoted path = %q", quoted)
	}
	for _, path := range []string{"", "-leading-option", "x\ncommand", "x\x00y"} {
		if _, err := quoteSSHRemotePath(path); err == nil {
			t.Errorf("accepted unsafe SSH path %q", path)
		}
	}
}

func TestParseLFSAuthResponseValidatesApprovedEndpointHeadersAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	body := `{"href":"https://lfs.example.test/team/repo.git/info/lfs","header":{"authorization":"Bearer ephemeral"},"expires_in":60}`
	got, err := parseLFSAuthResponse([]byte(body), "https://lfs.example.test/team/repo.git/info/lfs", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.headers["Authorization"] != "Bearer ephemeral" || !got.expires.Equal(now.Add(time.Minute)) {
		t.Fatalf("unexpected credentials metadata: headers=%v expiry=%s", got.headers, got.expires)
	}
	absolute := `{"expires_at":"2026-11-14T12:01:00Z"}`
	got, err = parseLFSAuthResponse([]byte(absolute), "https://lfs.example.test/team/repo.git/info/lfs", now)
	if err != nil || !got.expires.Equal(now.Add(time.Minute)) {
		t.Fatalf("absolute expiry was not accepted: expiry=%s err=%v", got.expires, err)
	}
}

func TestParseLFSAuthResponseRejectsUnsafeOrExpiredCredentials(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for name, body := range map[string]string{
		"unapproved endpoint":    `{"href":"https://attacker.example.test/lfs","expires_in":60}`,
		"endpoint path override": `{"href":"https://lfs.example.test/other","expires_in":60}`,
		"expired":                `{"expires_in":0}`,
		"too long":               `{"expires_in":3600}`,
		"no expiry":              `{}`,
		"routing header":         `{"header":{"Host":"attacker.example.test"},"expires_in":60}`,
		"cookie header":          `{"header":{"Cookie":"session=value"},"expires_in":60}`,
		"proxy header":           `{"header":{"Proxy-Authorization":"secret"},"expires_in":60}`,
		"oversized header":       `{"header":{"Authorization":"` + strings.Repeat("x", 8193) + `"},"expires_in":60}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLFSAuthResponse([]byte(body), "https://lfs.example.test/team/repo.git/info/lfs", now); err == nil {
				t.Fatal("unsafe authentication response was accepted")
			}
		})
	}
}

func TestParseLFSAuthResponseRejectsOversizeAndMalformedJSON(t *testing.T) {
	for _, body := range [][]byte{[]byte(`{"expires_in":"soon"}`), bytes.Repeat([]byte("x"), maxLFSAuthResponseBytes+1)} {
		if _, err := parseLFSAuthResponse(body, "https://lfs.example.test/info/lfs", time.Now()); err == nil {
			t.Fatal("malformed or oversized service response was accepted")
		}
	}
}

func TestPrivateLFSAuthenticationSecretsAreSanitizedFromPublicationLogs(t *testing.T) {
	secret := "Bearer never-log-this-token"
	message := sanitizePublicationError(errors.New("private SSH LFS authentication failed: " + secret))
	if strings.Contains(message, secret) || strings.Contains(message, "never-log-this-token") {
		t.Fatalf("publication log leaked LFS credential: %q", message)
	}
	if !strings.Contains(message, "git-lfs-authenticate") || !strings.Contains(message, "Pure-SSH LFS transfers are not supported") {
		t.Fatalf("private LFS failure omitted actionable capability guidance: %q", message)
	}
}

func TestRemoteLFSAuthenticateUsesInProcessSSHAndFixedService(t *testing.T) {
	clientKey, clientSigner := testLFSSigner(t)
	hostKey, hostSigner := testLFSSigner(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), clientKey.Marshal()) {
			return nil, errors.New("unexpected client key")
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(hostSigner)
	commandSeen := make(chan string, 1)
	serverDone := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		serverConn, chans, requests, err := ssh.NewServerConn(conn, serverConfig)
		if err != nil {
			return
		}
		defer serverConn.Close()
		go ssh.DiscardRequests(requests)
		for newChannel := range chans {
			if newChannel.ChannelType() != "session" {
				_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported")
				continue
			}
			channel, requests, err := newChannel.Accept()
			if err != nil {
				continue
			}
			for request := range requests {
				if request.Type != "exec" {
					_ = request.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				_ = ssh.Unmarshal(request.Payload, &payload)
				commandSeen <- payload.Command
				_ = request.Reply(true, nil)
				_, _ = io.WriteString(channel, `{"header":{"Authorization":"Bearer test-only"},"expires_in":60}`)
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = channel.Close()
				once.Do(func() { close(commandSeen) })
				return
			}
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	callback := func(_ string, _ net.Addr, got ssh.PublicKey) error {
		if !bytes.Equal(got.Marshal(), hostKey.Marshal()) {
			return errors.New("unexpected host key")
		}
		return nil
	}
	response, err := remoteLFSAuthenticate(ctx, "127.0.0.1", port, "git", "/team/a repo/'$(touch nope).git", clientSigner, callback, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if json.Unmarshal(response, &parsed) != nil {
		t.Fatalf("service output was not returned: %q", response)
	}
	command := <-commandSeen
	want := "git-lfs-authenticate '/team/a repo/'\\''$(touch nope).git' download"
	if command != want {
		t.Fatalf("remote command = %q, want %q", command, want)
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("SSH test server did not stop")
	}
}

func TestRemoteLFSAuthenticateSanitizesSSHKeyDenial(t *testing.T) {
	_, clientSigner := testLFSSigner(t)
	_, hostSigner := testLFSSigner(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		return nil, errors.New("denied Bearer private-test-secret")
	}}
	config.AddHostKey(hostSigner)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _, _, _ = ssh.NewServerConn(conn, config)
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = nil
	_, err = remoteLFSAuthenticate(ctx, "127.0.0.1", port, "git", "/repo.git", clientSigner, func(string, net.Addr, ssh.PublicKey) error { return nil }, time.Second)
	if err == nil || strings.Contains(err.Error(), "private-test-secret") || !strings.Contains(err.Error(), "approved host key") {
		t.Fatalf("SSH key denial was not sanitized: %v", err)
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("SSH denial fixture did not stop")
	}
}

func testLFSSigner(t *testing.T) (ssh.PublicKey, ssh.Signer) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey(), signer
}
