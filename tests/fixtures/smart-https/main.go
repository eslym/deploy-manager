package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
)

var lfsPayload = []byte("fixture hydrated LFS asset\n")

func main() {
	listen := flag.String("listen", "127.0.0.1:18441", "HTTPS listen address")
	cert := flag.String("tls-cert", "", "fixture TLS certificate")
	key := flag.String("tls-key", "", "fixture TLS key")
	data := flag.String("data", "", "fixture repository directory")
	flag.Parse()
	if *cert == "" || *key == "" || *data == "" {
		fmt.Fprintln(os.Stderr, "--tls-cert, --tls-key, and --data are required")
		os.Exit(2)
	}
	if err := serve(*listen, *cert, *key, *data); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serve(address, cert, key, data string) error {
	if err := os.MkdirAll(data, 0700); err != nil {
		return err
	}
	repoPath := filepath.Join(data, "repo.git")
	repo, err := git.PlainInitWithOptions(repoPath, &git.PlainInitOptions{InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")}})
	if err != nil {
		return err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return err
	}
	commit := func(message string) (plumbing.Hash, error) {
		if err := os.WriteFile(filepath.Join(repoPath, "index.html"), []byte("<!doctype html><title>"+message+"</title>\n"), 0600); err != nil {
			return plumbing.ZeroHash, err
		}
		if _, err := worktree.Add("index.html"); err != nil {
			return plumbing.ZeroHash, err
		}
		if message == "feature search" {
			digest := sha256.Sum256(lfsPayload)
			pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", hex.EncodeToString(digest[:]), len(lfsPayload))
			if err := os.WriteFile(filepath.Join(repoPath, ".gitattributes"), []byte("*.dat filter=lfs\n"), 0600); err != nil {
				return plumbing.ZeroHash, err
			}
			if err := os.WriteFile(filepath.Join(repoPath, "asset.dat"), []byte(pointer), 0600); err != nil {
				return plumbing.ZeroHash, err
			}
			for _, name := range []string{".gitattributes", "asset.dat"} {
				if _, err := worktree.Add(name); err != nil {
					return plumbing.ZeroHash, err
				}
			}
		}
		return worktree.Commit(message, &git.CommitOptions{Author: &object.Signature{Name: "Fixture", Email: "fixture@example.test", When: time.Now().UTC()}})
	}
	if _, err := commit("main"); err != nil {
		return err
	}
	feature := plumbing.NewBranchReferenceName("feature/search")
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: feature, Create: true}); err != nil {
		return err
	}
	featureCommit, err := commit("feature search")
	if err != nil {
		return err
	}
	if _, err := repo.CreateTag("v1.0", featureCommit, nil); err != nil {
		return err
	}
	if err := worktree.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("main")}); err != nil {
		return err
	}
	ordinaryPath := filepath.Join(data, "ordinary.git")
	ordinaryRepo, err := git.PlainInitWithOptions(ordinaryPath, &git.PlainInitOptions{InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")}})
	if err != nil {
		return err
	}
	ordinaryWorktree, err := ordinaryRepo.Worktree()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ordinaryPath, "index.html"), []byte("ordinary fixture site\n"), 0600); err != nil {
		return err
	}
	if _, err := ordinaryWorktree.Add("index.html"); err != nil {
		return err
	}
	if _, err := ordinaryWorktree.Commit("ordinary repository", &git.CommitOptions{Author: &object.Signature{Name: "Fixture", Email: "fixture@example.test", When: time.Now().UTC()}}); err != nil {
		return err
	}
	endpoint, err := transport.NewEndpoint("https://" + address + "/repo.git")
	if err != nil {
		return err
	}
	ordinaryEndpoint, err := transport.NewEndpoint("https://" + address + "/ordinary.git")
	if err != nil {
		return err
	}
	backend := gitserver.NewServer(gitserver.MapLoader{endpoint.String(): repo.Storer, ordinaryEndpoint.String(): ordinaryRepo.Storer})
	var fail atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /__fixture/fail", func(w http.ResponseWriter, _ *http.Request) { fail.Store(true); w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /__fixture/recover", func(w http.ResponseWriter, _ *http.Request) { fail.Store(false); w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /repo.git/info/lfs/objects/batch", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Operation string `json:"operation"`
			Objects   []struct {
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			} `json:"objects"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil || request.Operation != "download" || len(request.Objects) != 1 {
			http.Error(w, "invalid LFS batch request", http.StatusBadRequest)
			return
		}
		digest := sha256.Sum256(lfsPayload)
		oid := hex.EncodeToString(digest[:])
		if request.Objects[0].OID != oid || request.Objects[0].Size != int64(len(lfsPayload)) {
			w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
			_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":%d,"error":{"code":404,"message":"not found"}}]}`, request.Objects[0].OID, request.Objects[0].Size)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
		_, _ = fmt.Fprintf(w, `{"transfer":"basic","objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":%q,"header":{"X-LFS-Action":"approved"}}}}]}`, oid, len(lfsPayload), "https://"+address+"/lfs/object?signature=fixture-secret")
	})
	mux.HandleFunc("GET /lfs/object", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("signature") != "fixture-secret" || r.Header.Get("X-LFS-Action") != "approved" {
			http.Error(w, "invalid fixture action", http.StatusForbidden)
			return
		}
		_, _ = w.Write(lfsPayload)
	})
	// Separate authenticated LFS endpoint used by the SSH-backed private
	// repository workflow. Its bearer token is issued only by the SSH fixture.
	mux.HandleFunc("POST /private-lfs/objects/batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ssh-fixture-private-lfs" {
			http.Error(w, "private LFS authentication required", http.StatusUnauthorized)
			return
		}
		var request struct {
			Operation string `json:"operation"`
			Objects   []struct {
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			} `json:"objects"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil || request.Operation != "download" || len(request.Objects) != 1 {
			http.Error(w, "invalid private LFS batch request", http.StatusBadRequest)
			return
		}
		digest := sha256.Sum256(lfsPayload)
		oid := hex.EncodeToString(digest[:])
		if request.Objects[0].OID != oid || request.Objects[0].Size != int64(len(lfsPayload)) {
			w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
			_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":%d,"error":{"code":404,"message":"not found"}}]}`, request.Objects[0].OID, request.Objects[0].Size)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
		_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":%d,"actions":{"download":{"href":%q,"header":{"X-LFS-Private-Action":"approved"}}}}]}`, oid, len(lfsPayload), "https://"+address+"/private-lfs/object?signature=private-fixture-secret")
	})
	mux.HandleFunc("GET /private-lfs/object", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.URL.Query().Get("signature") != "private-fixture-secret" || r.Header.Get("X-LFS-Private-Action") != "approved" {
			http.Error(w, "invalid private fixture action", http.StatusForbidden)
			return
		}
		_, _ = w.Write(lfsPayload)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "controlled fixture failure", http.StatusServiceUnavailable)
			return
		}
		repositoryEndpoint := endpoint
		if strings.HasPrefix(r.URL.Path, "/ordinary.git/") {
			repositoryEndpoint = ordinaryEndpoint
		}
		session, err := backend.NewUploadPackSession(repositoryEndpoint, nil)
		if err != nil {
			http.Error(w, "upload-pack unavailable", 500)
			return
		}
		defer session.Close()
		switch {
		case r.Method == http.MethodGet && (r.URL.Path == "/repo.git/info/refs" || r.URL.Path == "/ordinary.git/info/refs") && r.URL.Query().Get("service") == "git-upload-pack":
			advertisement, err := session.AdvertisedReferencesContext(r.Context())
			if err != nil {
				http.Error(w, "advertisement failed", 500)
				return
			}
			advertisement.Prefix = [][]byte{[]byte("# service=git-upload-pack"), pktline.Flush}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			if err := advertisement.Encode(w); err != nil {
				return
			}
		case r.Method == http.MethodPost && (r.URL.Path == "/repo.git/git-upload-pack" || r.URL.Path == "/ordinary.git/git-upload-pack"):
			if _, err := session.AdvertisedReferencesContext(r.Context()); err != nil {
				http.Error(w, "advertisement failed", 500)
				return
			}
			request := packp.NewUploadPackRequest()
			scanner := pktline.NewScanner(r.Body)
			for scanner.Scan() {
				line := string(scanner.Bytes())
				if strings.HasPrefix(line, "want ") {
					want := strings.SplitN(strings.TrimPrefix(line, "want "), "\x00", 2)[0]
					request.Wants = append(request.Wants, plumbing.NewHash(strings.TrimSpace(want)))
				} else if strings.HasPrefix(line, "have ") {
					request.Haves = append(request.Haves, plumbing.NewHash(strings.TrimSpace(strings.TrimPrefix(line, "have "))))
				}
			}
			if scanner.Err() != nil {
				http.Error(w, "malformed upload-pack request", 400)
				return
			}
			response, err := session.UploadPack(context.Background(), request)
			if err != nil {
				http.Error(w, "upload-pack failed", 500)
				return
			}
			defer response.Close()
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_ = response.Encode(w)
		default:
			http.NotFound(w, r)
		}
	})
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return server.ListenAndServeTLS(cert, key)
}
