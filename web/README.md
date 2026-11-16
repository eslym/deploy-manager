# Deploy Manager frontend

`web/` is the SvelteKit/TypeScript source for the application SPA, including
authentication and the protected dashboard. The [agreed design](../docs/design.md#frontend-delivery-and-go-integration)
requires client rendering and a static production build embedded in the Go
executable, not a separately deployed frontend server.

## Authentication and hosting

The SPA contains browser-side administrator setup and OPAQUE login, plus a
session-guarded dashboard. OPAQUE JavaScript/WASM loads only when an
authentication form is submitted. Go verifies protocol messages and protects
session endpoints; the static shell itself contains no protected data.

Serve login and dashboard pages over HTTPS, either directly with `--tls-cert`
and `--tls-key` or through a trusted loopback reverse proxy. Configure the
external HTTPS origin with `--origin https://manager.example`. The explicit
`--allow-http-origin`/`DM_ALLOW_HTTP_ORIGIN=true` opt-in supports an HTTP origin
end-to-end, including login, authenticated requests, and a non-Secure session
cookie. This is insecure: although OPAQUE does not send the password to the
server, HTTP traffic can be intercepted or modified, including grants, OPAQUE
exchanges, and sessions. HTTPS remains the default and recommendation. For local
Vite development, configure both `setup` and `serve` with `--origin
http://localhost:5173 --allow-http-origin`, then use `http://localhost:5173` in
the browser. The default
session lifetime is 12 hours and can be changed with `--session-ttl`; sessions
are intentionally invalidated on server restart, while the administrator
credential remains stored durably.

The combined build is performed by `scripts/build.sh`; it builds and embeds the
static frontend before compiling Go. Do not treat a standalone frontend build
as proof that the Go binary uses this frontend.

## Development and checks

Run from `web/`:

```sh
bun install --frozen-lockfile
bun run dev
```

Before integrating frontend changes:

```sh
bun run check
bun run lint
bun run build
```

Vite development/preview servers are development tools only. The migration
must document and test the combined build: build this SPA first, then compile
Go with its static output embedded. The resulting executable must serve the
frontend and internal Go endpoints on one origin, work independently of the
source tree, and require neither Bun nor Node.js at runtime. See the design
for routing, security, lazy authentication assets, and distribution requirements.
