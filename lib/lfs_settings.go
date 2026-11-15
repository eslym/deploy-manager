package lib

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eslym/deploy-manager/lib/ent/projectsshkey"
)

const lfsApprovalPhrase = "I independently verified these LFS destinations"

func (a *App) lfsSettings(w http.ResponseWriter, r *http.Request) {
	mutation := r.Method == http.MethodPost
	if !a.projectAuthorized(w, r, mutation) {
		return
	}
	id := r.PathValue("id")
	var remote string
	if err := a.db.QueryRow(`SELECT remote_url FROM projects WHERE id=?`, id).Scan(&remote); err != nil {
		http.NotFound(w, r)
		return
	}
	if !mutation {
		settings := lfsProjectSettings{}
		var encoded string
		var internal int
		var encodedInternal string
		err := a.db.QueryRow(`SELECT endpoint,object_origins,allow_internal,internal_addresses FROM project_lfs_settings WHERE project_id=?`, id).Scan(&settings.Endpoint, &encoded, &internal, &encodedInternal)
		if err == nil {
			if json.Unmarshal([]byte(encoded), &settings.ObjectOrigins) != nil || json.Unmarshal([]byte(encodedInternal), &settings.InternalAddresses) != nil {
				http.Error(w, "LFS settings are unavailable", http.StatusInternalServerError)
				return
			}
			settings.AllowInternal = internal != 0
		} else if !errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "LFS settings are unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"settings": settings, "suggestedEndpoint": suggestedLFSEndpoint(remote)})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var input struct {
		Endpoint        string   `json:"endpoint"`
		ObjectOrigins   []string `json:"objectOrigins"`
		AllowInternal   bool     `json:"allowInternal"`
		Confirmation    string   `json:"confirmation"`
		InternalConfirm string   `json:"internalConfirmation"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.Confirmation != lfsApprovalPhrase || len(input.ObjectOrigins) == 0 || len(input.ObjectOrigins) > 16 {
		http.Error(w, "confirm independently verified HTTPS LFS destinations", http.StatusBadRequest)
		return
	}
	endpoint, err := validateLFSEndpoint(input.Endpoint)
	if err != nil {
		http.Error(w, "enter an HTTPS LFS endpoint without credentials, query, or fragment", http.StatusBadRequest)
		return
	}
	origins := make([]string, 0, len(input.ObjectOrigins))
	seen := map[string]bool{}
	for _, raw := range input.ObjectOrigins {
		origin, e := validateLFSOrigin(raw)
		if e != nil || seen[origin] {
			http.Error(w, "enter unique HTTPS origins without paths or credentials", http.StatusBadRequest)
			return
		}
		seen[origin] = true
		origins = append(origins, origin)
	}
	if !seen[originOf(endpoint)] {
		http.Error(w, "include the LFS endpoint origin in approved object origins", http.StatusBadRequest)
		return
	}
	if input.AllowInternal && input.InternalConfirm != lfsApprovalPhrase {
		http.Error(w, "internal destinations require separate independent confirmation", http.StatusBadRequest)
		return
	}
	internalIPs, err := approvedInternalAddresses(input.AllowInternal, append([]string{originOf(endpoint)}, origins...))
	if err != nil {
		http.Error(w, "internal destinations could not be safely resolved; verify the origins and try again", http.StatusBadRequest)
		return
	}
	encoded, _ := json.Marshal(origins)
	encodedInternal, _ := json.Marshal(internalIPs)
	_, err = a.db.Exec(`INSERT INTO project_lfs_settings(project_id,endpoint,object_origins,allow_internal,internal_addresses,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(project_id) DO UPDATE SET endpoint=excluded.endpoint,object_origins=excluded.object_origins,allow_internal=excluded.allow_internal,internal_addresses=excluded.internal_addresses,updated_at=excluded.updated_at`, id, endpoint.String(), string(encoded), boolInt(input.AllowInternal), string(encodedInternal), a.cfg.Now().UTC().UnixNano())
	if err != nil {
		http.Error(w, "LFS settings could not be saved", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func suggestedLFSEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return ""
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/info/lfs"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func (a *App) projectLFSHydrator(projectID string) (*lfsHydrator, error) {
	var settings lfsProjectSettings
	var encoded, encodedInternal string
	var internal int
	if err := a.db.QueryRow(`SELECT endpoint,object_origins,allow_internal,internal_addresses FROM project_lfs_settings WHERE project_id=?`, projectID).Scan(&settings.Endpoint, &encoded, &internal, &encodedInternal); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, errors.New("selected LFS content has no administrator-approved endpoint")
	}
	if err := json.Unmarshal([]byte(encoded), &settings.ObjectOrigins); err != nil {
		return nil, errors.New("administrator-approved LFS settings are invalid")
	}
	if err := json.Unmarshal([]byte(encodedInternal), &settings.InternalAddresses); err != nil {
		return nil, errors.New("administrator-approved LFS settings are invalid")
	}
	settings.AllowInternal = internal != 0
	hydrator, err := newLFSHydratorWithBudget(projectID, settings, a.projects.root, a.cfg.DeployMaxBytes, a.projects.maxCacheBytes, a.cfg.DeployTimeout, &a.projects.lfsCacheMu)
	if err != nil {
		return nil, err
	}
	hydrator.cacheAllowed = func() error {
		var pending int
		if err := a.db.QueryRow(`SELECT count(*) FROM project_deletions WHERE project_id=?`, projectID).Scan(&pending); err != nil {
			return errors.New("project LFS cache lifecycle is unavailable")
		}
		if pending != 0 {
			return errors.New("project deletion is in progress; LFS cache writes are blocked")
		}
		var exists int
		if err := a.db.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, projectID).Scan(&exists); err != nil || exists == 0 {
			return errors.New("project no longer exists; LFS cache writes are blocked")
		}
		return nil
	}
	var remote string
	if err := a.db.QueryRow(`SELECT remote_url FROM projects WHERE id=?`, projectID).Scan(&remote); err != nil {
		return nil, errors.New("project repository configuration is unavailable")
	}
	if strings.HasPrefix(remote, "ssh://") {
		if _, _, _, _, parseErr := safeSSHRemote(remote); parseErr != nil {
			return nil, errors.New("private SSH LFS requires an assigned repository key")
		}
		hydrator.now = a.cfg.Now
		hydrator.sshLifecycleLock = func() func() {
			a.projects.lfsAuthMu.RLock()
			return a.projects.lfsAuthMu.RUnlock
		}
		hydrator.sshAuth = func(ctx context.Context) (lfsAuthCredentials, error) {
			// hydrate holds the SSH-key lifecycle read lock across cache lookup,
			// authentication, and transfer. No signer is retained across calls.
			var currentRemote string
			if a.db.QueryRow(`SELECT remote_url FROM projects WHERE id=?`, projectID).Scan(&currentRemote) != nil || currentRemote != remote {
				return lfsAuthCredentials{}, errors.New("private SSH LFS project configuration changed")
			}
			assignment, assignmentErr := a.store.ProjectSSHKey.Query().Where(projectsshkey.IDEQ(projectID)).Only(ctx)
			if assignmentErr != nil {
				return lfsAuthCredentials{}, errors.New("private SSH LFS project configuration changed")
			}
			keyID := assignment.KeyID
			parsed, host, port, _, parseErr := safeSSHRemote(currentRemote)
			if parseErr != nil {
				return lfsAuthCredentials{}, errors.New("private SSH LFS repository URL is invalid")
			}
			user := "git"
			if parsed.User != nil {
				user = parsed.User.Username()
			}
			signer, signerErr := a.projectSSHSigner(keyID)
			if signerErr != nil {
				return lfsAuthCredentials{}, errors.New("private SSH LFS repository key is unavailable")
			}
			timeoutCtx, cancel := context.WithTimeout(ctx, a.projects.timeout)
			defer cancel()
			raw, authErr := remoteLFSAuthenticate(timeoutCtx, host, port, user, parsed.Path, signer, a.projectSSHHostKeyCallback(host, port), a.projects.timeout)
			if authErr != nil {
				return lfsAuthCredentials{}, authErr
			}
			defer clear(raw)
			return parseLFSAuthResponse(raw, settings.Endpoint, a.cfg.Now())
		}
	}
	return hydrator, nil
}

func approvedInternalAddresses(allow bool, origins []string) (map[string][]string, error) {
	if !allow {
		return nil, nil
	}
	approved := make(map[string][]string)
	seenIPs := make(map[string]map[string]bool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, raw := range origins {
		origin, err := validateLFSOrigin(raw)
		if err != nil {
			return nil, err
		}
		u, err := url.Parse(origin)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
		if err != nil || len(ips) == 0 {
			return nil, errors.New("destination DNS resolution failed")
		}
		seenForOrigin := seenIPs[origin]
		if seenForOrigin == nil {
			seenForOrigin = map[string]bool{}
			seenIPs[origin] = seenForOrigin
		}
		for _, ip := range ips {
			ip = ip.Unmap()
			if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				return nil, errors.New("destination resolves to a special-purpose address")
			}
			if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
				if !lfsAddressAllowed(ip, true, map[string]bool{ip.String(): true}) {
					return nil, errors.New("destination resolves to a forbidden address")
				}
				if !seenForOrigin[ip.String()] {
					seenForOrigin[ip.String()] = true
					approved[origin] = append(approved[origin], ip.String())
				}
			}
		}
	}
	return approved, nil
}
