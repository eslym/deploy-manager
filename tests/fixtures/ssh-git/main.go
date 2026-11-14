// Command ssh-git serves a controlled in-process SSH/upload-pack repository for
// browser acceptance tests. It intentionally does not execute Git or SSH tools.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
	"golang.org/x/crypto/ssh"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18442", "listen address")
	lfsHref := flag.String("lfs-href", "https://127.0.0.1:18441/private-lfs", "approved private LFS endpoint returned by the remote service")
	flag.Parse()
	root, err := os.MkdirTemp("", "deploy-manager-ssh-fixture-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(root)
	repository := filepath.Join(root, "repo.git")
	if err := makeRepository(repository, root); err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		log.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return &ssh.Permissions{}, nil }}
	config.AddHostKey(signer)
	repo, err := git.PlainOpen(repository)
	if err != nil {
		log.Fatal(err)
	}
	endpoint, err := transport.NewEndpoint("ssh://git@" + *listen + "/repo.git")
	if err != nil {
		log.Fatal(err)
	}
	backend := gitserver.NewServer(gitserver.MapLoader{endpoint.String(): repo.Storer})
	log.Printf("controlled SSH Git fixture listening at %s", *listen)
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go serveConnection(connection, config, endpoint, backend, *lfsHref)
	}
}

func makeRepository(repository, root string) error {
	bare, err := git.PlainInit(repository, true)
	if err != nil {
		return err
	}
	workPath := filepath.Join(root, "work")
	work, err := git.PlainInit(workPath, false)
	if err != nil {
		return err
	}
	wt, err := work.Worktree()
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(workPath, "index.html"), []byte("SSH fixture site\n"), 0600); err != nil {
		return err
	}
	payload := []byte("fixture hydrated LFS asset\n")
	digest := sha256.Sum256(payload)
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", hex.EncodeToString(digest[:]), len(payload))
	if err = os.WriteFile(filepath.Join(workPath, ".gitattributes"), []byte("*.dat filter=lfs\n"), 0600); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(workPath, "asset.dat"), []byte(pointer), 0600); err != nil {
		return err
	}
	for _, name := range []string{"index.html", ".gitattributes", "asset.dat"} {
		if _, err = wt.Add(name); err != nil {
			return err
		}
	}
	commit, err := wt.Commit("SSH fixture", &git.CommitOptions{Author: &object.Signature{Name: "Fixture", Email: "fixture@example.test", When: time.Now().UTC()}})
	if err != nil {
		return err
	}
	objects, err := work.Storer.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return err
	}
	if err = objects.ForEach(func(value plumbing.EncodedObject) error { _, e := bare.Storer.SetEncodedObject(value); return e }); err != nil {
		return err
	}
	if err = bare.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), commit)); err != nil {
		return err
	}
	return bare.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main")))
}

func serveConnection(connection net.Conn, config *ssh.ServerConfig, endpoint *transport.Endpoint, backend transport.Transport, lfsHref string) {
	defer connection.Close()
	client, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		return
	}
	defer client.Close()
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			_ = incoming.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		channel, requests, err := incoming.Accept()
		if err != nil {
			continue
		}
		go serveSession(channel, requests, endpoint, backend, lfsHref)
	}
}

func serveSession(channel ssh.Channel, requests <-chan *ssh.Request, endpoint *transport.Endpoint, backend transport.Transport, lfsHref string) {
	defer channel.Close()
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		var command struct{ Value string }
		if ssh.Unmarshal(request.Payload, &command) != nil {
			_ = request.Reply(false, nil)
			return
		}
		if command.Value == "git-lfs-authenticate '/repo.git' download" {
			_ = request.Reply(true, nil)
			_, _ = fmt.Fprintf(channel, `{"href":%q,"header":{"Authorization":"Bearer ssh-fixture-private-lfs"},"expires_in":60}`, lfsHref)
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		}
		if !strings.HasPrefix(command.Value, "git-upload-pack ") {
			_ = request.Reply(false, nil)
			return
		}
		_ = request.Reply(true, nil)
		session, err := backend.NewUploadPackSession(endpoint, nil)
		if err != nil {
			_, _ = fmt.Fprintln(channel.Stderr(), "upload service unavailable")
			return
		}
		advertisement, err := session.AdvertisedReferencesContext(context.Background())
		if err != nil {
			_, _ = fmt.Fprintln(channel.Stderr(), "references unavailable")
			return
		}
		if err = advertisement.Encode(channel); err != nil {
			return
		}
		request := packp.NewUploadPackRequest()
		if err = request.Decode(channel); err != nil {
			return
		}
		response, err := session.UploadPack(context.Background(), request)
		if err != nil {
			_, _ = io.WriteString(channel.Stderr(), "upload failed")
			return
		}
		_ = response.Encode(channel)
		return
	}
}
