//go:build integrationtest

package lib

import (
	"crypto/x509"
	"errors"
	"os"
)

// This explicit trust seam is included only in the isolated browser-test binary.
// It adds a supplied CA to normal certificate verification; it never disables
// TLS verification or changes the endpoint/origin/IP policy.
func init() {
	path := os.Getenv("DEPLOY_MANAGER_TEST_LFS_CA")
	if path == "" {
		return
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		panic(errors.New("test LFS CA file contains no certificates"))
	}
	lfsTestRootCAs = roots
}
