# Fresh initialization and rebootstrap

The executable embeds goose schema version 1. A genuinely new data path gets
the complete schema automatically. An existing empty, partial, unversioned,
malformed, or unsupported database is refused without schema repair, baseline
stamping, key generation, or reset. Current-version startup checks migration
history and key bindings without running migrations. CLI setup/recovery commands
also require an exact supported schema version; they never upgrade an existing
database. Existing-store upgrades remain unavailable until the protected
upgrade lifecycle is implemented.

## Explicit rebootstrap

Use rebootstrap only when intentionally creating a separate installation. It
does not migrate or clean up an old installation. Keep the old database, its
external keys, cache, publication root, and evidence untouched. Choose entirely
new paths for the database/keys **and** publication/cache state; do not point
the new database at an old cache or publication root.

For example, with a newly installed executable at `/usr/local/bin/deploy-manager`:

```sh
install -d -m 700 /srv/deploy-manager-rebootstrap-20261115/data
install -d -m 700 /srv/deploy-manager-rebootstrap-20261115/sites
/usr/local/bin/deploy-manager setup \
  --data /srv/deploy-manager-rebootstrap-20261115/data \
  --origin https://deploy.example.net
/usr/local/bin/deploy-manager serve \
  --data /srv/deploy-manager-rebootstrap-20261115/data \
  --publication-root /srv/deploy-manager-rebootstrap-20261115/sites \
  --listen 127.0.0.1:8080 \
  --origin https://deploy.example.net
```

Use a fresh, owner-controlled directory and verify that every path is new before
running these commands. Setup creates the independent OPAQUE and repository-key
master files on first initialization and prints a one-use registration URL.
Never copy or regenerate keys to make an existing database open. If a fresh
initialization is interrupted after schema creation, preserve all created files
and logs and investigate; do not point the executable at the old installation
or clear state to force startup. Rebootstrap again only in another isolated,
confirmed-new location. Recreate projects, repository credentials, host trust,
webhook configuration, and deployments deliberately.

This procedure is not recovery for an existing installation and does not
include its published sites or Git/LFS cache. Do not remove or overwrite those
paths as part of rebootstrap.
