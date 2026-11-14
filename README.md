# Deploy Manager

A self-hosted, single-administrator dashboard for publishing static Git content
to stable directories on a Linux server.

Deploy Manager fetches repositories, prepares their tracked content, and switches
each publication atomically. Your existing web server serves the published
directories. Deploy Manager does **not** build sites, run repository scripts,
configure your web server, or transfer releases to remote servers.

The Go executable includes the Svelte dashboard and authentication assets. No
Node.js, Bun, frontend source tree, or separate frontend server is needed at
runtime.

## Features

- Shared repository caches with multiple named deployments per project.
- Branch, frozen tag, and fixed commit targets; manual Deploy and Hard re-deploy.
- Verified GitHub and Gitea webhooks with independent per-project secrets,
  durable acceptance, deduplication, and coalesced branch updates.
- Per-deployment Pause/Resume, including manual updates while paused.
- Searchable, paginated first-parent history and temporary rollback to an
  eligible branch ancestor without changing the configured branch.
- Managed SSH keys, encrypted private-key storage, and explicit SSH host trust.
- Git LFS hydration with approved HTTPS destinations and verified payload caches.
- Atomic real-directory publication, restart reconciliation, delayed cleanup,
  durable job status, and sanitized logs.
- Single-administrator registration, login, password changes, and recovery using
  RFC 9807 OPAQUE with browser-local password processing.

The project is under active development; the [agreed design](docs/design.md)
also describes work not yet implemented. Dependency upgrades and distribution
notice work remain open; see [Security and distribution](#security-and-distribution).

## Requirements

**Runtime**

- Linux and a writable, manager-owned data directory.
- A supported local filesystem with atomic directory exchange
  (`renameat2(RENAME_EXCHANGE)`). Publication capability is checked at startup;
  unsupported filesystems fail closed rather than falling back to copying.
- HTTPS for login, dashboard operations, and webhooks, provided directly or by a
  trusted local reverse proxy.
- A separate web server if you want to serve the published sites.

**Building from source**

- Go 1.26.11 or a compatible newer toolchain.
- Bun 1.4.4, as pinned in the container development environment.
- A C compiler and libc development headers: SQLite uses CGO.

Git, SSH, and LFS operations run in-process; no local `git`, `ssh`, or `git-lfs`
executable is required by the application.

## Build

From the repository root:

```sh
./scripts/build.sh
```

This installs frontend dependencies from `web/bun.lock`, builds the static SPA,
and compiles `./deploy-manager` with those assets embedded. Build the frontend
before invoking Go directly: missing frontend output intentionally fails Go
compilation.

To build the frontend using the pinned Docker image instead of a host Bun
installation:

```sh
DM_BUILD_FRONTEND_IN_DOCKER=1 ./scripts/build.sh
```

This alternative still requires the Go toolchain and CGO prerequisites on the
host.

## First run

Use the same data directory for every command. The following example serves the
dashboard directly over TLS; replace the hostname and certificate paths:

```sh
./deploy-manager setup \
  --data ./data \
  --url https://manager.example.com

./deploy-manager serve \
  --data ./data \
  --listen 0.0.0.0:8443 \
  --origin https://manager.example.com \
  --tls-cert /path/to/fullchain.pem \
  --tls-key /path/to/privkey.pem
```

This assumes external HTTPS traffic for `manager.example.com` reaches port 8443.
If you access that port directly, include `:8443` in both `--url` and `--origin`.

Open the setup URL printed by the first command while the server is running.
It expires after 15 minutes by default and can be used only once. Registration
is disabled after the administrator account is created. Then sign in at
`/login`; registration does not sign you in automatically.

Treat setup and recovery links as secrets. Their grants are carried in URL
fragments, not query strings; do not share them or put them in logs.

Only one daemon may use an installation. Setup/recovery commands can run beside
it; see [process coordination](docs/process-coordination.md) for locking,
contention, compatibility limitations, and offline-restore requirements.

### Behind a local HTTPS reverse proxy

Bind the application to loopback and configure the external origin:

```sh
./deploy-manager serve \
  --data ./data \
  --listen 127.0.0.1:8080 \
  --origin https://manager.example.com
```

The proxy must preserve the external Host and Origin headers and set
`X-Forwarded-Proto: https`, replacing any client-supplied value. The application
trusts that header only from a loopback peer. Proxy the dashboard, `/api`,
`/opaque`, and `/webhooks` on the same origin. Do not expose the unencrypted
backend listener to the network.

Loopback HTTP is allowed for initial local registration, but it is **not** a
substitute for HTTPS login and authenticated dashboard use.

## Publish a site

1. Add a repository in the dashboard. Public smart-HTTPS repositories work
   anonymously. For SSH, generate or import a repository key, authorize its
   public key at the Git host, and assign it to the project.
2. For unknown or changed SSH host keys, independently verify the displayed
   fingerprint before approving it. Trust is scoped to hostname and port.
3. Select a cached branch or tag, or a full commit ID, and create a deployment.
   Creating a deployment or changing its target queues publication automatically.
4. If the repository uses LFS, approve its LFS server and object-download origins.
   Internal destinations require explicit approval. Missing or invalid payloads
   fail publication without replacing the live content.
5. Point your web server at the deployment's displayed publication path. Grant
   that server read/traverse access as needed, not write access to manager state.
6. Optionally configure the project's GitHub or Gitea webhook using the URL and
   secret shown in the dashboard. Select push events; secret rotation invalidates
   the old secret.

### Deployment behavior

- **Branch target:** fetches and follows the latest branch tip. Verified matching
  pushes queue updates.
- **Tag target:** freezes the resolved commit; ordinary Deploy does not follow a
  moved tag.
- **Commit target:** remains fixed to its selected commit.
- **Deploy:** fetches branch targets; uses saved tag/commit resolution. Records a
  no-op if already live.
- **Hard re-deploy:** fetches and re-resolves branches/tags, then recreates content
  even if unchanged. Commit targets remain fixed.
- **Pause:** persists the branch deployment's automation pause. Matching pushes
  fetch updates without preparing or publishing content.
- **Deploy while paused:** publishes a one-off branch update without clearing
  the pause.
- **Resume:** clears the pause and immediately queues the latest branch tip.
- **Temporary rollback:** recreates a selected branch ancestor while preserving
  the branch and automation state. Later Deploy/webhook work may replace it.

Changing a paused deployment to another branch preserves the pause and queues
one publication. Changing it to a tag or commit clears branch-only pause state.
Branch deletion leaves the live site unchanged.

Rollback uses Git objects and LFS payloads, not retained release directories.
Replaced directories are retained only for the reader grace period (60 seconds
by default), then cleaned up. Atomic switching does not guarantee that every
request from a browser sees the same release.

## Configuration and data

Run `./deploy-manager serve -h` for all flags. Common settings include:

- `--data`: `./data`.
- `--listen`: `127.0.0.1:8080`.
- `--publication-root`: `<data>/state.sqlite.publications`.
- `--repository-key-master`: `<data>/repository-key-master.key`.
- `--git-timeout` / `--deploy-timeout`: `2m` each.
- `--deploy-concurrency`: `2`.
- `--deploy-max-files`: `10000`.
- `--deploy-max-bytes`: `1073741824` (1 GiB).
- `--deploy-grace-period`: `1m`.
- `--job-retention`: `720h` (30 days).
- `--session-ttl`: `12h`.
- `--grant-ttl`: `15m`.

Keep the publication root manager-owned. Live publication paths must be real
directories, not symlinks; staging and live content must be on the same mount.
Do not manually rename or remove publication/recovery state while the manager
is running.

### Recovery and backups

To replace a forgotten administrator password:

```sh
./deploy-manager recover --data ./data --url https://manager.example.com
```

Open the printed single-use link and complete browser registration of the new
password. Recovery replaces the existing administrator credential; it does not
create another account. Password replacement revokes existing sessions and
pending logins without changing repository-key encryption. Server restart also
invalidates sessions.

For a consistent backup, stop the server and back up the entire data directory,
any external repository master-key file, and any separately configured
publication root together. Important default paths include:

- `state.sqlite` and any SQLite WAL/SHM sidecars.
- `opaque-server.key`: long-term OPAQUE server key, paired with the database's
  authentication records and profile generation.
- `repository-key-master.key`: independent encryption key for repository keys
  and webhook secrets.
- `state.sqlite.projects`: Git and LFS caches.
- `state.sqlite.publications`: publication content and recovery/cleanup state.

Protect backups as secrets. Missing or mismatched key files fail closed; do not
delete them to repair startup. Retain repository/LFS caches if you need rollback
material that may no longer be available upstream. Explicit cache recreation
can lose that material.

## Supported scope

Supports conventional SHA-1 repositories over smart HTTPS or SSH. Private HTTPS
credentials, local/file URLs, dumb HTTP, partial clones, and SHA-256 repositories
are not supported. Symlinks and submodules in published trees are rejected.
The full tracked repository root, including dotfiles but excluding Git metadata,
is published without builds, hooks, or arbitrary checkout filters.

LFS supports basic HTTPS downloads, including private SSH repositories whose
servers provide `git-lfs-authenticate`. Pure-SSH LFS transfer and private HTTPS
Git credentials are out of scope. Redirects and unapproved LFS destinations are
rejected.

There are no teams, roles, public signup, public API tokens, or remote deployment
targets. The private HTTP endpoints exist to support the embedded dashboard.

## Development and tests

Frontend commands, from `web/`:

```sh
bun install --frozen-lockfile
bun run check
bun run lint
bun run build
```

After frontend assets exist, run from the repository root:

```sh
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
```

The pinned container suite builds the combined executable and tests it from an
unrelated working directory, including real Chromium authentication and
dashboard workflows:

```sh
docker compose -f tests/compose.yaml run --build --rm test
```

See [tests/README.md](tests/README.md) for the development shell, shared caches,
and cleanup. The [frontend README](web/README.md) describes the SPA. Vite's
development server (`bun run dev` in `web/`) proxies backend paths to
`http://127.0.0.1:8080`; `GO_BACKEND_URL` overrides that destination. Authenticated
browser development still needs HTTPS and the same Origin/cookie protections
as production.

## Security and distribution

OPAQUE authentication requires first-party JavaScript/WASM. Crypto initialization
failures do not fall back to posting passwords. HTTPS, secure server-side
sessions, Origin checks, and rate limits remain required.

The pinned cryptographic dependencies have explicitly accepted residual risks;
tests and vulnerability scans are not an independent cryptographic audit. Read
the [accepted risks](docs/research/opaque-accepted-risks.md) and
[gate findings](docs/research/opaque-gate-findings.md). The mandatory browser
dependency upgrade ([#25](https://github.com/eslym/deploy-manager/issues/25)) and
dependency/license inventory work
([#26](https://github.com/eslym/deploy-manager/issues/26)) remain follow-ups.
Review [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) before distribution;
it is not a complete distribution clearance.

For requirements, terminology, and design rationale, see
[docs/design.md](docs/design.md), [GLOSSARY.md](GLOSSARY.md), and
[research notes](docs/research/).

## License

Deploy Manager is licensed under the [MIT License](LICENSE). Third-party
components retain their own licenses and notice requirements; see
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
