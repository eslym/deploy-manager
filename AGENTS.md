# Repository guidance

## Requirements and layout

- Read `docs/design.md` before feature or security work; it is the confirmed requirements source. Use `GLOSSARY.md` for deployment/release and rollback terminology. Read relevant `docs/research/` notes for Git/tree export, LFS, publication recovery, or OPAQUE details; the design doc takes precedence.
- `main.go` wires CLI/server commands; application logic belongs in `lib/`. `web/` is the client-rendered SvelteKit SPA; Go owns persistence, auth, and HTTP endpoints. Do not add SvelteKit server routes for application behavior.
- Frontend build output is embedded from `lib/web` (`lib/assets.go`). Build the frontend before compiling Go; `scripts/build.sh` performs the combined build. A missing frontend build intentionally fails Go compilation. The shipped binary must work without frontend source or runtime assets.

## Development and checks

- Frontend instructions live in `web/AGENTS.md`. From `web/`, run `bun install --frozen-lockfile`, then `bun run check`, `bun run lint`, and `bun run build`; `lint` is Prettier.
- For local full-stack debugging, start the Go backend at the root with `go run . serve --listen 127.0.0.1:8080 --data ./data --origin http://localhost:5173 --allow-http-origin`, then run `bun run dev` from `web/` and open `http://localhost:5173`. Vite proxies `/api`, `/opaque`, and `/webhooks`; `GO_BACKEND_URL` overrides its default `http://127.0.0.1:8080`. Issue a local setup link with `go run . setup --listen 127.0.0.1:8080 --data ./data --origin http://localhost:5173 --allow-http-origin`; use the same opt-in for `recover` when needed. HTTPS remains the default for canonical `--origin`/`DM_ORIGIN`. This explicit opt-in permits HTTP authentication and dashboard requests on the configured HTTP origin and uses a session cookie without `Secure`; although OPAQUE does not send the password to the server, HTTP traffic can be intercepted or modified (including OPAQUE exchanges, grants, and sessions). Use only on a trusted isolated network. For safe loopback setup without the Vite frontend, leave origin unset to use the loopback HTTP default.
- Run Go tests from the root with `go test ./...` after frontend assets exist; run `govulncheck ./...` after the same build prerequisite. `scripts/build.sh` builds the frontend and executable together.
- Compose provides the pinned Bun/Go/Playwright development shell and browser test runner. Use `docker compose -f tests/compose.yaml run --build --rm test bash` for the shell or omit `bash` to run the full suite. The checkout is mounted read-only at `/source`; copy it into `/workspace` before commands that generate files. See `tests/README.md` for cache sharing and cleanup.

### Test selection and merge gate

- Documentation-only changes require no test run.
- For frontend-only changes, run frontend checks and relevant unit/browser tests; do not run Go tests or Go race tests. Use the browser-only Compose selectors in `tests/README.md` when combined-binary browser coverage is needed; compiling the Go backend for those tests is allowed.
- Before merging frontend-only changes into `main`, both full browser scenarios (registration and projects) must pass; focused browser slices alone do not satisfy this merge gate. Go tests and race tests remain excluded.
- If the changes being merged include Go code, the full Compose test suite must pass before merging into `main`, including Go tests, race tests, vet, frontend checks/build, and both browser scenarios.
- Reuse passing results for the exact tested commit; a fast-forward merge that preserves its hash requires no post-merge rerun.

## Shared workflow

- Follow `docs/agents/issue-tracker.md` and `docs/agents/triage-labels.md` when managing GitHub issue/spec tracking; read `docs/agents/domain.md` when changing `GLOSSARY.md` or ADRs.
- Name branches `<category>/<ticket-number>-<name>`; omit the ticket number when none applies. Commit subjects use `<tag>: <message>` or `<category>/<tag>: <message>`. Publish branches only when explicitly asked.
