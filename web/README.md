# Deploy Manager frontend

`web/` is the SvelteKit/TypeScript source for the application SPA, including
authentication and the protected dashboard. The [agreed design](../docs/design.md#frontend-delivery-and-go-integration)
requires client rendering and a static production build embedded in the Go
executable, not a separately deployed frontend server.

## Current status

This directory is still a scaffold. The static adapter has a fallback configured,
but the combined frontend/Go build and SPA application flows are not implemented.
Current registration lives outside `web/` in Go-defined HTML and standalone JS.
Existing ticket [#3](https://github.com/eslym/deploy-manager/issues/3) tracks its
migration and build/embedding integration; #4 adds login and the protected
dashboard. Do not treat a successful standalone frontend build as proof that
the Go binary uses this frontend.

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
