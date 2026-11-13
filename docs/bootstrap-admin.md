# Administrator bootstrap

Build with the pinned Go baseline (`go 1.26.11`) and CGO enabled for SQLite.
The executable has two commands:

```sh
./deploy-manager setup --data ./data --listen 127.0.0.1:8080
./deploy-manager serve --data ./data --listen 127.0.0.1:8080
```

`setup` prints a short-lived, single-use registration URL once. Open it in a
browser while the server is running. Loopback-only HTTP is available for local
setup; non-loopback serving requires `--tls-cert` and `--tls-key`. Behind a
local reverse proxy, pass an HTTPS setup URL with `--url` and proxy to a
loopback listener; the application trusts `X-Forwarded-Proto: https` only from
a loopback peer and still checks the request Origin.

Keep `opaque-server.key` with the SQLite database's backup generation. It is a
separate mode-0600 file and is not stored in SQLite. Missing or mismatched
OPAQUE key/profile generations fail closed. Back up both the data file and its
external key together; there is no key-rotation or recovery command in this
bootstrap-only slice.

The setup grant is placed after `#` in the registration URL, so the browser
does not send it in the initial navigation or subsequent resource request URLs.
The registration script removes the fragment from the visible URL before any
POST and includes the grant only in the protected JSON request body. It never
falls back to an unprotected registration request.

The browser checks are in `tests/browser/`. Install the pinned test package
with `cd tests/browser && bun install --frozen-lockfile`; run
`node registration.mjs` where Go 1.26.11, CGO, and the Playwright 1.65.0
Chromium dependencies are available. To use the pinned browser container from
the repository root:

```sh
docker build -t deploy-manager-browser-tests -f tests/browser/Dockerfile .
docker run --rm -v "$PWD:/workspace" deploy-manager-browser-tests
```

The browser test covers fragment-only CLI grant issuance and absence from all
browser request URLs, missing-grant and initialization failure, real
Chromium-to-Go OPAQUE registration,
concurrent grant consumption, restart durability, registration disablement,
and absence of a session cookie/password transmission.

This implements bootstrap only. It does not implement login, sessions,
password change, recovery, dashboard authorization, or an authenticated
dashboard. See [`design.md`](design.md) for the remaining product requirements
and [`../THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md) for unresolved
distribution/license follow-ups (#25 and #26).
