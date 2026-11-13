//go:build integrationtest

package lib

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// This trust seam is compiled only into the isolated browser-test binary. It
// trusts an explicit CA and permits a private dial only to that exact HTTPS
// origin; it never disables certificate verification or follows redirects.
func init() {
	configureTestGitTransport = func() error {
		caPath := os.Getenv("DEPLOY_MANAGER_TEST_GIT_CA")
		origin := os.Getenv("DEPLOY_MANAGER_TEST_GIT_ORIGIN")
		if caPath == "" && origin == "" {
			return nil
		}
		if caPath == "" || origin == "" {
			return errors.New("test Git CA and exact test Git origin must be configured together")
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("test Git origin must be an HTTPS origin without credentials or path")
		}
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return errors.New("test Git CA file contains no certificates")
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		publicDial := publicGitDialContext(dialer)
		fixtureAddress := strings.ToLower(u.Host)
		transport := &http.Transport{
			Proxy:           nil,
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				if strings.EqualFold(address, fixtureAddress) {
					return dialer.DialContext(ctx, network, address)
				}
				return publicDial(ctx, network, address)
			},
		}
		installProjectHTTPSClient(&http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		})
		return nil
	}
}
