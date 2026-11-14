# Repository SSH keys

Repository keys are reusable credentials for reading repositories. The dashboard
can generate Ed25519 keys or import unencrypted OpenSSH/PEM private keys using
Ed25519, RSA with at least 3072 bits, or ECDSA on P-256, P-384, or P-521. Other
algorithms, weak RSA keys, malformed keys, and passphrase-protected keys are
rejected. The dashboard only returns authorized public keys and public metadata;
there is no private-key export endpoint.

Private material is encrypted with AES-256-GCM in SQLite. Its independent
32-byte master key is stored outside SQLite at `--repository-key-master` (by
default `<data>/repository-key-master.key`), with exclusive creation and mode
0600. For an upgraded database without repository-key-store metadata, the first
successful startup initializes this new store and master file. Once the store
has metadata, a missing, truncated, replaced, symlinked, or incorrectly
permissioned master file prevents startup; it is never silently replaced.
Back up the database and repository master key together. Keep the repository
master key separate from the OPAQUE server key.

Changing or recovering the administrator password does not rotate this master
key. Startup checks the key verifier and authenticates/decrypts each stored key
before serving requests. Future SSH connections must use an application-owned
in-process boundary; private-key material must not be added to an HTTP response,
CLI output, logs, or frontend assets.
