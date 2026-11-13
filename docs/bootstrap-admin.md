# Administrator bootstrap

The `web/` SvelteKit client-rendered SPA is the only application UI. It is built
with the pinned Bun lockfile and embedded into the Go executable. The combined
production build is:

```sh
./scripts/build.sh
```

It runs `bun install --frozen-lockfile`, builds the static SPA into `lib/web`,
then compiles the SQLite-enabled Go executable. The `go:embed` directive makes
missing build output a compile error; no Bun/Node runtime or external asset
directory is used when the executable runs. On Linux hosts where the Vite native
binding is incompatible with the CPU, `DM_BUILD_FRONTEND_IN_DOCKER=1
./scripts/build.sh` runs the frontend build in the pinned Bun Alpine image and
then compiles Go on the host.

Issue a setup link and run the server:

```sh
./deploy-manager setup --data ./data --listen 127.0.0.1:8080
./deploy-manager serve --data ./data --listen 127.0.0.1:8080
```

`setup` prints a short-lived, single-use registration URL once. Open it in a
browser while the server is running. Loopback-only HTTP is available for local
setup; non-loopback serving requires `--tls-cert` and `--tls-key`. Behind a
local reverse proxy, pass an HTTPS setup URL with `--url` and proxy to a
loopback listener; the application trusts `X-Forwarded-Proto: https` only from
a loopback peer and checks the request Origin.

The setup grant is placed after `#` in the URL. The SPA removes the fragment
from the visible URL before any protocol request and sends the grant only in
same-origin, JSON POST bodies. The pinned `@serenity-kit/opaque@1.1.0` browser
module is split into its own content-hashed chunk and loaded only when the user
submits the registration form. The password and local OPAQUE state remain in
the browser; initialization errors fail closed. Registration does not create a
login session.

The database stores only grant hashes and the administrator registration
record/profile generation. Keep `opaque-server.key` with the matching SQLite
backup generation. It is a separate mode-0600 file. Missing or mismatched
OPAQUE key/profile generations fail closed. The repository-key encryption
master key is independent. There is no key-rotation or recovery command in this
bootstrap-only slice.

The Go server serves the SPA shell and hashed assets from its embedded files.
Direct frontend-route navigation uses the shell only for HTML GET/HEAD
requests; missing assets and backend paths retain error semantics. The shell is
revalidated, hashed assets are immutable, and the shell's inline boot script is
authorized by a per-build CSP hash.

## Validation

Run frontend checks from `web/`:

```sh
bun install --frozen-lockfile
bun run check
bun run lint
bun run build
```

Then run Go checks from the repository root:

```sh
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
```

For the production-binary browser regression test, build and run the supplied
Docker image. It builds the SPA, compiles the combined executable, and runs
Chromium with its supported system dependencies; the browser launches the
binary from a temporary working directory without source-tree asset access:

```sh
docker build -t deploy-manager-browser-tests -f tests/browser/Dockerfile .
docker run --rm deploy-manager-browser-tests
```

The browser/HTTP checks cover client-route reloads, hashed assets and missing
asset/backend statuses, lazy OPAQUE loading and initialization failure,
fragment cleanup, grant expiry/reuse/concurrent consumption, no password or
local-state transmission, no session cookie, registration disablement, and
durability across restart.

This implements administrator bootstrap only. Login, sessions, password
changes, recovery, dashboard authorization and management remain outside this
ticket. Preserve the dependency upgrade and license/notice follow-ups tracked
by #25 and #26; see [`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md).
