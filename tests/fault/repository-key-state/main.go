// Command repository-key-state reports non-secret state for browser durability checks.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/ssh"
)

type keyState struct {
	ID               string `json:"id"`
	CiphertextSHA256 string `json:"ciphertextSha256"`
	Usable           bool   `json:"usable"`
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: repository-key-state <database> <master-key>")
		os.Exit(2)
	}
	master, err := os.ReadFile(os.Args[2])
	fail(err)
	masterHash := sha256.Sum256(master)
	db, err := sql.Open("sqlite3", os.Args[1]+"?mode=ro")
	fail(err)
	defer db.Close()
	rows, err := db.Query(`SELECT id,ciphertext FROM repository_keys ORDER BY id`)
	fail(err)
	defer rows.Close()
	keys := []keyState{}
	for rows.Next() {
		var id string
		var sealed []byte
		fail(rows.Scan(&id, &sealed))
		sum := sha256.Sum256(sealed)
		state := keyState{ID: id, CiphertextSHA256: hex.EncodeToString(sum[:])}
		block, e := aes.NewCipher(master)
		if e == nil {
			var gcm cipher.AEAD
			gcm, e = cipher.NewGCM(block)
			if e == nil && len(sealed) >= gcm.NonceSize()+gcm.Overhead() {
				plain, openErr := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], []byte("repository-key:"+id))
				if openErr == nil {
					key, parseErr := ssh.ParseRawPrivateKey(plain)
					if parseErr != nil {
						key, parseErr = x509.ParsePKCS8PrivateKey(plain)
					}
					if private, ok := key.(*ed25519.PrivateKey); ok {
						key = *private
					}
					if parseErr == nil {
						signer, signErr := ssh.NewSignerFromKey(key)
						if signErr == nil {
							_, signErr = signer.Sign(rand.Reader, []byte("durability proof"))
						}
						state.Usable = signErr == nil
					}
					clear(plain)
				}
			}
		}
		keys = append(keys, state)
	}
	fail(rows.Err())
	fail(json.NewEncoder(os.Stdout).Encode(struct {
		MasterSHA256 string     `json:"masterSha256"`
		Keys         []keyState `json:"keys"`
	}{hex.EncodeToString(masterHash[:]), keys}))
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "repository key state check failed")
		os.Exit(1)
	}
}
