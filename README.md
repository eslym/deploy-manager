# Deploy Manager

> [!IMPORTANT]
> **AI usage disclosure:** This app is vibe-coded. AI tools have been used
> extensively to write and modify its code and documentation. Tests and the
> security features described here are not an independent security audit.
> Review and assess the code before trusting it with credentials or production
> infrastructure.

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
- Searchable, paginated first-parent history with temporary and permanent
  rollback to an eligible branch ancestor, without changing the configured branch.
- Managed SSH keys, encrypted private-key storage, and explicit SSH host trust.
- Git LFS hydration with approved HTTPS destinations and verified payload caches.
- Per-project Git and LFS cache-size reporting.
- Atomic real-directory publication, restart reconciliation, delayed cleanup,
  durable job history with sanitized logs, and bounded transient-fetch retries.
- Single-administrator registration, login, password changes, and recovery using
  RFC 9807 OPAQUE with browser-local password processing.

The project is under active development. The [agreed design](docs/design.md)
defines the full v1 requirements; dependency upgrades and distribution notice
work remain open. See [Security and distribution](#security-and-distribution)
and the [current cache-clearing limitation](#caches-and-job-history).

## Requirements

**Runtime**

- Linux and a writable, manager-owned data directory.
- A supported local filesystem with atomic directory exchange
  (`renameat2(RENAME_EXCHANGE)`). Publication capability is checked at startup;
  unsupported filesystems fail closed rather than falling back to copying.
- HTTPS for login, dashboard operations, and webhooks by default, provided
  directly or by a trusted local reverse proxy. The explicit insecure HTTP-origin
  opt-in is an exception for isolated/trusted networks; see below.
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

### Container builds

```sh
docker build -t deploy-manager .
```

The default Docker build compiles the frontend on the native build platform,
then embeds it in the target-platform Go executable. For multi-platform CI,
the workflow builds the frontend once on the runner, moves `lib/web` to
`web/build`, and passes `--build-arg FRONTEND_SOURCE=prebuilt` to the same
Dockerfile. Both paths share the Go builder and runtime stages; Vite never
runs under QEMU.

## First run

Use the same executable, data directory, and external-key configuration for
every command. The following example serves the dashboard directly over TLS;
replace the hostname and certificate paths:

```sh
./deploy-manager setup \
  --data ./data \
  --origin https://manager.example.com

./deploy-manager serve \
  --data ./data \
  --listen 0.0.0.0:8443 \
  --origin https://manager.example.com \
  --tls-cert /path/to/fullchain.pem \
  --tls-key /path/to/privkey.pem
```

This assumes external HTTPS traffic for `manager.example.com` reaches port 8443.
If you access that port directly, include `:8443` in `--origin`.

HTTPS is required by default. An HTTP origin requires `--allow-http-origin` on
each relevant `setup`, `recover`, and `serve` command; `DM_ALLOW_HTTP_ORIGIN=true`
is the environment equivalent, with explicit flags taking precedence. This
opt-in permits HTTP authentication and dashboard requests only on the configured
HTTP origin, using a session cookie without the `Secure` attribute. Although
OPAQUE does not send the password to the server, HTTP traffic can be intercepted
or modified, including grants, OPAQUE exchanges, and sessions. Use it only on a
trusted isolated network; HTTPS remains the recommended/default mode.

Open the setup URL printed by the first command while the server is running.
It expires after 15 minutes by default and can be used only once. Registration
is disabled after the administrator account is created. Then sign in at
`/login`; registration does not sign you in automatically.

Treat setup and recovery links as secrets. Their grants are carried in URL
fragments, not query strings; do not share them or put them in logs.

Only one daemon may use an installation. Setup/recovery commands can run beside
it when their executable supports the exact database schema and maintenance is
not in progress. See [process coordination](docs/process-coordination.md) for
locking, contention, compatibility limitations, and offline-restore requirements.

### Behind an HTTPS reverse proxy

For a proxy on the same host, bind the application to loopback and configure
the external origin:

```sh
./deploy-manager serve \
  --data ./data \
  --listen 127.0.0.1:8080 \
  --origin https://manager.example.com
```

For a proxy on another container or host, bind to a private interface and
explicitly trust **only the proxy's source IP or CIDR** (multiple entries are
comma-separated):

```sh
./deploy-manager serve \
  --data ./data \
  --listen 0.0.0.0:8080 \
  --origin https://manager.example.com \
  --trusted-proxy 172.18.0.5/32,172.19.0.0/24
```

The proxy must preserve the external Host and Origin headers and **replace**
any client-supplied `X-Forwarded-Proto` with `https`. The application trusts
that header from loopback or an explicitly trusted proxy peer; other peers
cannot claim HTTPS. Proxy the dashboard, `/api`, `/opaque`, and `/webhooks` on
the same origin. Restrict access to the unencrypted backend to the proxy with
network/firewall rules; never expose it directly to untrusted clients. Avoid
trusting an entire shared container network if clients can reach it directly.

The Docker image listens on HTTP port 8080 by default, with no certificate or
key configured. Put it behind an HTTPS-terminating proxy, set `DM_ORIGIN` to
the public HTTPS origin, and set `DM_TRUSTED_PROXY` to the proxy's source IP or
CIDR if it is not loopback. TLS can still be enabled directly by setting both
`DM_TLS_CERT` and `DM_TLS_KEY` (or passing the corresponding flags). The image
runs `serve` by default; pass `setup` or `recover` as the container command to
use those CLI modes with the same mounted data and key paths. All three commands
honor `DM_DATA`, `DM_REPOSITORY_KEY_MASTER`, `DM_ORIGIN`, and
`DM_ALLOW_HTTP_ORIGIN`, so
the same container environment works for grant issuance and the running daemon.
For example:

```sh
docker run --rm -v deploy-manager-data:/data \
  -e DM_ORIGIN=https://manager.example.com \
  deploy-manager:latest setup
```

The printed link uses the configured public origin; without `DM_ORIGIN`, a
loopback listener produces a local HTTP link and non-loopback HTTP remains
disallowed for public setup links.

Loopback HTTP is available for local setup. Without the explicit insecure
opt-in, HTTP is **not** allowed for login or authenticated dashboard use.

## Publish a site

1. Add a repository in the dashboard. Public smart-HTTPS repositories work
   anonymously. For SSH, generate or import a repository key, authorize its
   public key at the Git host, and assign it to the project.
2. For unknown or changed SSH host keys, independently verify the displayed
   fingerprint before approving it. Trust is scoped to hostname and port.
3. Select a cached branch or tag, or a full commit ID, and create a deployment.
   Choose a publication folder directly beneath `--publication-root`; the dashboard
   previews its absolute path before creation. Use a unique name beginning with a
   letter or number, followed by letters, numbers, dots, hyphens, or underscores
   (at most 100 characters). Nested paths are not supported. Creating a deployment
   or changing its target queues publication automatically.
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
- **Permanent rollback:** preserves the branch and pauses automation as soon as
  the request is accepted, then publishes the selected ancestor. The pause remains
  even if publication fails; Resume explicitly catches up to the branch tip.

Changing a paused deployment to another branch preserves the pause and queues
one publication. Changing it to a tag or commit clears branch-only pause state.
Branch deletion leaves the live site unchanged.

Rollback uses Git objects and LFS payloads, not retained release directories.
Replaced directories are retained only for the reader grace period (60 seconds
by default), then cleaned up. Atomic switching does not guarantee that every
request from a browser sees the same release.

### Caches and job history

The dashboard shows each project's Git and verified LFS cache sizes. There is no
automatic Git garbage collection or LFS payload eviction. Clearing caches requires
confirmation and preserves deployment configuration and published content, but
can permanently lose unreachable commits or payloads no longer available upstream.

**Current limitation:** clearing caches removes the Git repository cache without
re-cloning it. Refresh and deployment fetches currently require that cache to
exist, so they do not automatically rebuild it afterward. Avoid clearing a cache
you still need for deployment or rollback.

Each deployment shows its latest 100 jobs with status and expandable logs. Job
and log retention defaults to 30 days; active work and pending recovery/cleanup
records are preserved. Interrupted jobs are marked failed on restart, separately
from publication reconciliation. Only transient deployment fetch failures are
retried, with a bounded number of attempts; authentication and host-trust failures
require corrective action instead.

## Configuration and data

Run `./deploy-manager serve -h` for all flags. Common settings include:

- `--data`: `./data`.
- `--listen`: `127.0.0.1:8080`.
- `--tls-cert` / `--tls-key`: optional pair for direct HTTPS; without both,
  the listener uses HTTP on any configured address.
- `--trusted-proxy`: comma-separated proxy source IPs or CIDRs allowed to set
  `X-Forwarded-Proto: https` (loopback peers are trusted by default).
- `--publication-root`: `<data>/state.sqlite.publications`.
- `--repository-key-master`: `<data>/repository-key-master.key`.
- `--git-timeout` / `--deploy-timeout`: `2m` each.
- `--deploy-concurrency`: `2`.
- `--deploy-max-files`: `10000`.
- `--deploy-max-bytes`: `1073741824` (1 GiB).
- `--deploy-grace-period`: `1m`.
- `--job-retention`: `720h` (30 days).
- `--fetch-retry-attempts`: `3` total attempts, including the initial fetch
  (supported range: `1`–`5`).
- `--login-ttl`: `2m` for a pending OPAQUE login handshake.
- `--session-ttl`: `12h`.
- `--grant-ttl`: `15m`.
- `--recent-auth-ttl`: `5m` for password changes.
- `--auth-rate-per-minute`: `20` authentication requests per client per minute.

Keep the publication root manager-owned. Live publication paths must be real
directories, not symlinks; staging and live content must be on the same mount.
Do not manually rename or remove publication/recovery state while the manager
is running.
Deployments created before publication folders were selectable retain their
existing ID-named directories without relocation. Their displayed path remains
the document root to configure in your web server.

### Startup and upgrades

The default data directory contains `state.sqlite`, `opaque-server.key`, and
`repository-key-master.key`, plus caches and publication/recovery state. Keep
both key files with their matching backup generation; neither is password-derived.
If `--repository-key-master` points elsewhere, supply the same path to setup,
recovery, and serving commands.

The server returns HTTP 503 maintenance responses until startup is ready. For a
supported database upgrade, the daemon excludes other database users, creates a
verified database-only backup, and applies embedded migrations before normal
workers start. Setup/recovery commands never migrate an existing database.

A failed upgrade leaves a durable maintenance marker and requires operator
recovery; restarting does not retry or clear it. Do not delete locks, key files,
or the maintenance marker to force startup. See the
[upgrade recovery procedure](docs/operations/upgrade-recovery.md).

### Recovery and backups

To replace a forgotten administrator password:

```sh
./deploy-manager recover --data ./data --origin https://manager.example.com
```

Open the printed single-use link and complete browser registration of the new
password. Recovery replaces the existing administrator credential; it does not
create another account. Password replacement revokes existing sessions and
pending logins without changing repository-key encryption. Server restart also
invalidates sessions.

Follow the [full-installation backup and restore procedure](docs/operations/backup-restore.md).
It requires a stopped installation and one consistent, identity-preserving
snapshot of SQLite, matching external keys, caches, and publication/recovery
state. Ordinary file copying can invalidate publication directory identities.
Protect backups as secrets. Missing or mismatched key files fail closed; do not
delete them to repair startup. Git/LFS caches are reconstructible only when the
required objects and payloads remain available upstream.

For a startup migration failure, follow the dedicated [operator upgrade recovery
procedure](docs/operations/upgrade-recovery.md). It restores the exact prior
program with its verified database-only upgrade backup and must not be confused
with deployment rollback or whole-installation recovery.

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

The full race suite is currently slow and can exceed Go's default per-package
test timeout. For a complete run, choose an explicit `-timeout` appropriate to
your environment; use `-run` to narrow investigations. Repeated executable hashing
is tracked in [#42](https://github.com/eslym/deploy-manager/issues/42); increasing
the deadline does not fix that bottleneck. CLI tests share one executable build
per package run and also instrument child executables when run with `-race`.

The pinned container suite builds the combined executable and tests it from an
unrelated working directory, including real Chromium authentication and
dashboard workflows:

```sh
docker compose -f tests/compose.yaml run --build --rm test
```

See [tests/README.md](tests/README.md) for the development shell, shared caches,
and cleanup. The [frontend README](web/README.md) describes the SPA. Vite's
development server (`bun run dev` in `web/`) proxies backend paths to
`http://127.0.0.1:8080`; `GO_BACKEND_URL` overrides that destination. To use the
default `http://localhost:5173` Vite origin for authenticated development, both
`serve` and `setup` need `--origin http://localhost:5173 --allow-http-origin`.
This deliberately uses insecure HTTP and a non-Secure session cookie; OPAQUE
keeps the password out of the request, but HTTP traffic remains interceptable
and modifiable. Use a trusted isolated network. HTTPS remains the default for
non-development deployments.

## Security and distribution

OPAQUE authentication requires first-party JavaScript/WASM. Crypto initialization
failures do not fall back to posting passwords. HTTPS, Secure server-side
sessions, Origin checks, and rate limits remain the default security model. The
explicit HTTP-origin opt-in is an insecure exception: its session cookie cannot
be Secure, and HTTP can expose or alter OPAQUE exchanges, grants, and sessions.

The pinned cryptographic dependencies have explicitly accepted residual risks;
tests and vulnerability scans are not an independent cryptographic audit. Read
the [accepted risks](docs/research/opaque-accepted-risks.md) and
[gate findings](docs/research/opaque-gate-findings.md). The mandatory browser
dependency upgrade ([#25](https://github.com/eslym/deploy-manager/issues/25)) and
dependency/license inventory work
([#26](https://github.com/eslym/deploy-manager/issues/26)) remain follow-ups.
Review [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) before distribution;
it is not a complete distribution clearance.

The [current notice index](third_party/DEPENDENCIES.md) records pinned versions
and retained upstream legal files. Builds embed a readable bundle at
`/third-party-notices.txt`. Refresh and verify it after dependency changes with
`bun scripts/update-notices.mjs` and `bun scripts/update-notices.mjs --check`
after installing the pinned Go and Bun dependencies, then rebuild the combined
binary. This maintenance does not close the unresolved WASM license inventory.

For requirements, terminology, and design rationale, see
[docs/design.md](docs/design.md), [GLOSSARY.md](GLOSSARY.md), and
[research notes](docs/research/).

## License

Deploy Manager is licensed under the [MIT License](LICENSE). Third-party
components retain their own licenses and notice requirements; see
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
