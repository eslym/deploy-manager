package lib

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const maxLFSBatchBytes = 1 << 20

// lfsTestRootCAs is an explicit test seam. Production leaves it nil and uses
// the platform trust store with normal certificate and hostname verification.
var lfsTestRootCAs *x509.CertPool

type lfsProjectSettings struct {
	Endpoint          string              `json:"endpoint"`
	ObjectOrigins     []string            `json:"objectOrigins"`
	AllowInternal     bool                `json:"allowInternal"`
	InternalAddresses map[string][]string `json:"internalAddresses,omitempty"`
}

type lfsHydrator struct {
	projectID    string
	settings     lfsProjectSettings
	cacheRoot    string
	cacheBase    string
	cacheBudget  int64
	cacheLock    *sync.Mutex
	cacheAllowed func() error
	maxObject    int64
	client       *http.Client
}

type lfsBatchRequest struct {
	Operation string           `json:"operation"`
	Transfers []string         `json:"transfers"`
	HashAlgo  string           `json:"hash_algo"`
	Objects   []lfsBatchObject `json:"objects"`
}
type lfsBatchObject struct {
	OID     string         `json:"oid"`
	Size    int64          `json:"size"`
	Error   *lfsBatchError `json:"error,omitempty"`
	Actions map[string]struct {
		Href   string            `json:"href"`
		Header map[string]string `json:"header"`
	} `json:"actions,omitempty"`
}
type lfsBatchError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func newLFSHydrator(projectID string, settings lfsProjectSettings, root string, maxObject int64, timeout time.Duration) (*lfsHydrator, error) {
	return newLFSHydratorWithBudget(projectID, settings, root, maxObject, 1<<40, timeout, &sync.Mutex{})
}

func newLFSHydratorWithBudget(projectID string, settings lfsProjectSettings, root string, maxObject, cacheBudget int64, timeout time.Duration, cacheLock *sync.Mutex) (*lfsHydrator, error) {
	if !validProjectID(projectID) || maxObject <= 0 {
		return nil, errors.New("invalid LFS project configuration")
	}
	if cacheBudget <= 0 {
		return nil, errors.New("invalid LFS cache budget")
	}
	endpoint, err := validateLFSEndpoint(settings.Endpoint)
	if err != nil {
		return nil, err
	}
	settings.Endpoint = endpoint.String()
	approved := make(map[string]bool)
	for _, raw := range settings.ObjectOrigins {
		origin, err := validateLFSOrigin(raw)
		if err != nil {
			return nil, errors.New("invalid approved LFS object origin")
		}
		approved[origin] = true
	}
	if !approved[originOf(endpoint)] {
		return nil, errors.New("the LFS endpoint origin is not approved for object downloads")
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	transport := &http.Transport{
		Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout: 30 * time.Second, MaxIdleConns: 8, MaxIdleConnsPerHost: 2,
		TLSClientConfig: &tls.Config{RootCAs: lfsTestRootCAs, MinVersion: tls.VersionTLS12},
		DialContext:     lfsPolicyDialer{allowInternal: settings.AllowInternal, internalAddresses: addressSets(settings.InternalAddresses)}.DialContext,
	}
	return &lfsHydrator{projectID: projectID, settings: settings, cacheRoot: filepath.Join(root, "lfs-cache", projectID), cacheBase: root, cacheBudget: cacheBudget, cacheLock: cacheLock, maxObject: maxObject, client: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func validateLFSEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, errors.New("LFS endpoint must be an HTTPS URL without credentials, query, or fragment")
	}
	if u.Port() != "" {
		port, e := strconv.Atoi(u.Port())
		if e != nil || port < 1 || port > 65535 {
			return nil, errors.New("invalid LFS endpoint port")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func validateLFSOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "\r\n\x00") {
		return "", errors.New("invalid HTTPS origin")
	}
	if u.Port() != "" {
		port, e := strconv.Atoi(u.Port())
		if e != nil || port < 1 || port > 65535 {
			return "", errors.New("invalid origin port")
		}
	}
	u.Host = strings.ToLower(u.Host)
	return "https://" + u.Host, nil
}

func originOf(u *url.URL) string { return "https://" + strings.ToLower(u.Host) }

type lfsPolicyDialer struct {
	allowInternal     bool
	internalAddresses map[string]map[string]bool
}

func (allowInternal lfsPolicyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid LFS destination")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("LFS destination could not be resolved")
	}
	origin := originForDialAddress(host, port)
	approved := allowInternal.internalAddresses[origin]
	var last error
	for _, ip := range ips {
		if !lfsAddressAllowed(ip, allowInternal.allowInternal, approved) {
			return nil, errors.New("LFS destination address is not permitted")
		}
		conn, dialErr := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		last = dialErr
	}
	return nil, last
}

func lfsAddressAllowed(ip netip.Addr, allowInternal bool, approvedInternal map[string]bool) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip == netip.MustParseAddr("255.255.255.255") {
		return false
	}
	internal := !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback()
	if internal {
		return allowInternal && approvedInternal[ip.String()]
	}
	if !ip.IsGlobalUnicast() {
		return false
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	for _, raw := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "2001::/23", "2001:db8::/32", "2002::/16"} {
		if netip.MustParsePrefix(raw).Contains(ip) {
			return false
		}
	}
	return true
}

func addressSets(values map[string][]string) map[string]map[string]bool {
	sets := make(map[string]map[string]bool, len(values))
	for origin, addresses := range values {
		sets[origin] = make(map[string]bool, len(addresses))
		for _, address := range addresses {
			sets[origin][address] = true
		}
	}
	return sets
}

func originForDialAddress(host, port string) string {
	host = strings.Trim(host, "[]")
	if port == "443" {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return "https://" + strings.ToLower(host)
	}
	return "https://" + strings.ToLower(net.JoinHostPort(host, port))
}

func (h *lfsHydrator) hydrate(ctx context.Context, pointer lfsPointer) (string, error) {
	if pointer.Size < 0 || pointer.Size > h.maxObject || len(pointer.OID) != 64 {
		return "", errors.New("LFS object exceeds configured size or has invalid identity")
	}
	if _, err := hex.DecodeString(pointer.OID); err != nil || strings.ToLower(pointer.OID) != pointer.OID {
		return "", errors.New("invalid LFS object identity")
	}
	if h.cacheLock == nil {
		return "", errors.New("LFS cache lifecycle is unavailable")
	}
	h.cacheLock.Lock()
	defer h.cacheLock.Unlock()
	if h.cacheAllowed != nil {
		if err := h.cacheAllowed(); err != nil {
			return "", err
		}
	}
	if err := ensureCacheDirectory(h.cacheRoot); err != nil {
		return "", err
	}
	cachePath := filepath.Join(h.cacheRoot, pointer.OID)
	if validCachedLFS(cachePath, pointer) {
		return cachePath, nil
	}
	used, err := directorySize(h.cacheBase, h.cacheBudget)
	if err != nil || used > h.cacheBudget || pointer.Size > h.cacheBudget-used {
		return "", errors.New("verified LFS payload would exceed the configured project cache budget")
	}
	if err := h.fetch(ctx, pointer, cachePath, h.cacheBudget-used); err != nil {
		return "", err
	}
	return cachePath, nil
}

func ensureCacheDirectory(path string) error {
	root := filepath.Dir(filepath.Dir(path))
	parent := filepath.Dir(path)
	if err := ensureRealDirectory(root, 0700); err != nil {
		return err
	}
	if err := ensureRealDirectory(parent, 0700); err != nil {
		return err
	}
	return ensureRealDirectory(path, 0700)
}

func ensureRealDirectory(path string, mode os.FileMode) error {
	if err := os.Mkdir(path, mode); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("LFS cache directory is not a real directory")
	}
	return nil
}

func (p *projectManager) reconcileLFSCacheTemps() error {
	root := filepath.Join(p.root, "lfs-cache")
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("LFS cache root is not a real directory")
	}
	projects, err := os.ReadDir(root)
	if err != nil || len(projects) > 10000 {
		return errors.New("LFS cache project directory listing is unavailable or too large")
	}
	removed := 0
	for _, project := range projects {
		if !validProjectID(project.Name()) || !project.IsDir() || project.Type()&os.ModeSymlink != 0 {
			return errors.New("LFS cache contains an unrecognized project path")
		}
		dir := filepath.Join(root, project.Name())
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 10000 {
			return errors.New("LFS cache entry listing is unavailable or too large")
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".tmp-") {
				id := strings.TrimPrefix(name, ".tmp-")
				if !validProjectID(id) || !entry.Type().IsRegular() || entry.Type()&os.ModeSymlink != 0 {
					return errors.New("LFS cache contains an unsafe temporary file")
				}
				if removed >= 10000 {
					return errors.New("too many orphaned LFS cache temporary files")
				}
				if err := os.Remove(filepath.Join(dir, name)); err != nil {
					return fmt.Errorf("remove orphaned LFS cache temporary file: %w", err)
				}
				removed++
				continue
			}
			if len(name) != 64 || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return errors.New("LFS cache contains an unrecognized entry")
			}
			if _, err := hex.DecodeString(name); err != nil || strings.ToLower(name) != name {
				return errors.New("LFS cache contains an invalid object name")
			}
		}
		if err := syncDirectory(dir); err != nil {
			return fmt.Errorf("sync recovered LFS cache directory: %w", err)
		}
	}
	return nil
}

func validCachedLFS(name string, pointer lfsPointer) bool {
	f, err := openLFSCacheFile(name)
	if err != nil {
		return false
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != pointer.Size {
		_ = f.Close()
		return false
	}
	hash := sha256.New()
	n, copyErr := io.CopyN(hash, f, pointer.Size)
	var extra [1]byte
	extraN, extraErr := f.Read(extra[:])
	closeErr := f.Close()
	return copyErr == nil && extraN == 0 && extraErr == io.EOF && closeErr == nil && n == pointer.Size && hex.EncodeToString(hash.Sum(nil)) == pointer.OID
}

// verifiedLFSPayloadPath is the narrow project-cache read seam for publication
// and project lifecycle operations. It never trusts the cache key without
// checking the file's exact size and digest.
func (p *projectManager) verifiedLFSPayloadPath(projectID string, pointer lfsPointer) (string, error) {
	if !validProjectID(projectID) || len(pointer.OID) != 64 {
		return "", errors.New("invalid project LFS cache identity")
	}
	if _, err := hex.DecodeString(pointer.OID); err != nil || strings.ToLower(pointer.OID) != pointer.OID {
		return "", errors.New("invalid LFS object identity")
	}
	root := filepath.Join(p.root, "lfs-cache")
	for _, dir := range []string{root, filepath.Join(root, projectID)} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("LFS cache directory is not a real directory")
		}
	}
	path := filepath.Join(root, projectID, pointer.OID)
	if !validCachedLFS(path, pointer) {
		return "", errors.New("LFS cache entry failed integrity verification")
	}
	return path, nil
}

// removeProjectLFSPayloads is called only after a project tombstone is durable
// and its jobs are canceled/superseded. The lock prevents a concurrent cache
// commit from recreating the project directory during cleanup.
func (p *projectManager) removeProjectLFSPayloads(projectID string, want dirIdentity) error {
	if !validProjectID(projectID) {
		return errors.New("invalid project identifier for LFS cache cleanup")
	}
	p.lfsCacheMu.Lock()
	defer p.lfsCacheMu.Unlock()
	root := filepath.Join(p.root, "lfs-cache")
	rootInfo, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("LFS cache root is not a real directory")
	}
	path := filepath.Join(root, projectID)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("project LFS cache path is not a real directory")
	}
	actual, err := identity(path)
	if err != nil || actual != want || want == (dirIdentity{}) {
		return errors.New("project LFS cache identity changed; cleanup blocked")
	}
	if err = os.RemoveAll(path); err != nil {
		return errors.New("project LFS payload cache could not be removed")
	}
	return syncDirectory(root)
}

func openLFSCacheFile(name string) (*os.File, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("LFS cache entry is not a regular file")
	}
	return f, nil
}

func (h *lfsHydrator) fetch(ctx context.Context, pointer lfsPointer, destination string, writeBudget int64) (retErr error) {
	endpoint, _ := url.Parse(h.settings.Endpoint)
	batchURL := *endpoint
	batchURL.Path = strings.TrimRight(batchURL.Path, "/") + "/objects/batch"
	body, err := json.Marshal(lfsBatchRequest{Operation: "download", Transfers: []string{"basic"}, HashAlgo: "sha256", Objects: []lfsBatchObject{{OID: pointer.OID, Size: pointer.Size}}})
	if err != nil {
		return errors.New("could not prepare LFS request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, batchURL.String(), strings.NewReader(string(body)))
	if err != nil {
		return errors.New("could not prepare LFS request")
	}
	req.Header.Set("Accept", "application/vnd.git-lfs+json")
	req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
	response, err := h.client.Do(req)
	if err != nil {
		return errors.New("LFS batch request failed")
	}
	batch, readErr := io.ReadAll(io.LimitReader(response.Body, maxLFSBatchBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || len(batch) > maxLFSBatchBytes || response.StatusCode != http.StatusOK {
		return errors.New("LFS batch response was invalid")
	}
	var result struct {
		Transfer string           `json:"transfer"`
		Objects  []lfsBatchObject `json:"objects"`
	}
	decoderErr := json.Unmarshal(batch, &result)
	if decoderErr != nil || result.Transfer != "" && result.Transfer != "basic" || len(result.Objects) != 1 {
		return errors.New("LFS batch response was invalid")
	}
	object := result.Objects[0]
	if object.OID != pointer.OID || object.Size != pointer.Size || object.Error != nil {
		return errors.New("LFS object is unavailable")
	}
	action, ok := object.Actions["download"]
	if !ok {
		return errors.New("LFS download action is missing")
	}
	objectURL, err := validateLFSActionURL(action.Href, h.settings)
	if err != nil {
		return errors.New("LFS download origin is not approved")
	}
	if originOf(objectURL) != originOf(endpoint) && len(action.Header) != 0 {
		return errors.New("LFS action headers cannot be sent to a different object origin")
	}
	get, err := http.NewRequestWithContext(ctx, http.MethodGet, objectURL.String(), nil)
	if err != nil {
		return errors.New("could not prepare LFS download")
	}
	for key, value := range action.Header {
		if !validLFSActionHeader(key, value) {
			return errors.New("LFS download action contains unsafe headers")
		}
		get.Header.Set(key, value)
	}
	response, err = h.client.Do(get)
	if err != nil {
		return errors.New("LFS payload download failed")
	}
	defer func() {
		if err := response.Body.Close(); retErr == nil && err != nil {
			retErr = errors.New("LFS payload could not be closed")
		}
	}()
	if response.StatusCode != http.StatusOK {
		return errors.New("LFS payload download failed")
	}
	tmpID, idErr := randomID()
	if idErr != nil {
		return errors.New("LFS cache temporary name could not be generated")
	}
	tmp, err := os.OpenFile(filepath.Join(h.cacheRoot, ".tmp-"+tmpID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("LFS cache temporary file could not be created")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hash := sha256.New()
	writer := &lfsCacheWriter{file: tmp, hash: hash, remaining: writeBudget}
	n, copyErr := io.Copy(writer, io.LimitReader(response.Body, pointer.Size+1))
	if copyErr != nil || n != pointer.Size || hex.EncodeToString(hash.Sum(nil)) != pointer.OID {
		_ = tmp.Close()
		return errors.New("LFS payload failed exact size or SHA-256 verification")
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return errors.New("LFS cache could not be synchronized")
	}
	if err = tmp.Close(); err != nil {
		return errors.New("LFS cache could not be closed")
	}
	if err = os.Rename(tmpName, destination); err != nil {
		return errors.New("verified LFS payload could not be committed to cache")
	}
	if err = syncDirectory(h.cacheRoot); err != nil {
		return errors.New("LFS cache directory could not be synchronized")
	}
	if !validCachedLFS(destination, pointer) {
		return fmt.Errorf("verified LFS cache entry failed post-commit validation")
	}
	return nil
}

type lfsCacheWriter struct {
	file      *os.File
	hash      io.Writer
	remaining int64
}

func (w *lfsCacheWriter) Write(p []byte) (int, error) {
	requested := len(p)
	if int64(len(p)) > w.remaining {
		p = p[:w.remaining]
		if len(p) == 0 {
			return 0, errors.New("LFS cache write budget exceeded")
		}
	}
	n, err := w.file.Write(p)
	if n > 0 {
		_, _ = w.hash.Write(p[:n])
		w.remaining -= int64(n)
	}
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	if len(p) < requested {
		return n, errors.New("LFS cache write budget exceeded")
	}
	return n, nil
}

func validateLFSActionURL(raw string, settings lfsProjectSettings) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, errors.New("invalid action URL")
	}
	approved := false
	for _, rawOrigin := range settings.ObjectOrigins {
		origin, err := validateLFSOrigin(rawOrigin)
		if err == nil && origin == originOf(u) {
			approved = true
		}
	}
	if !approved {
		return nil, errors.New("unapproved action origin")
	}
	return u, nil
}

func validLFSActionHeader(key, value string) bool {
	key = http.CanonicalHeaderKey(key)
	switch key {
	case "", "Host", "Connection", "Proxy-Authorization", "Proxy-Authenticate", "Transfer-Encoding", "Upgrade", "Trailer", "Content-Length":
		return false
	}
	return !strings.ContainsAny(key+value, "\r\n\x00")
}
