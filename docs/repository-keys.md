# Repository SSH keys

Repository keys are reusable credentials for reading repositories. The dashboard
can generate Ed25519 keys or import unencrypted OpenSSH/PEM private keys using
Ed25519, RSA with at least 3072 bits, or ECDSA on P-256, P-384, or P-521. Other
algorithms, weak RSA keys, malformed keys, and passphrase-protected keys are
rejected. The dashboard only returns authorized public keys and public metadata;
there is no private-key export endpoint.

Key management displays each key's current project usage count. A project counts
once when it is assigned to that key, regardless of its deployments; unused keys
show zero. Reassignment and project deletion change the count because it reflects
current assignments, not historical use.

Selecting a key's count opens the project list filtered to current projects using
that key. The selected key is shown in the list, and **Clear filter** returns to
all projects. The filter is encoded in the browser URL, so it remains active on
refresh. Each result links to its project details; an unused or unknown key has
no matching projects.

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
