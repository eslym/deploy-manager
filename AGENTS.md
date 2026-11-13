# Repository guidance

## Requirements and domain

- Before implementation, read `docs/design.md`; it is the confirmed requirements authority, not authorization to implement, and supersedes alternatives in research notes. RFC 9807 OPAQUE dependencies are approved; preserve the remaining security, upgrade, and distribution requirements there.
- Use `GLOSSARY.md` for domain terms, especially deployment vs. release, frozen tag targets, and permanent rollback vs. commit targets.
- For Git compatibility/tree export, LFS, publication recovery, or OPAQUE implementation details, read the corresponding `docs/research/` notes; where they differ, `docs/design.md` governs.

## Implementation boundaries

- This is a scaffold: `main.go` is empty, `lib/` has only a package declaration, and no tests or combined frontend/Go build exist yet. The registration implementation mentioned in `web/README.md` was removed; do not assume it exists.
- The Go module is at the repository root; application logic belongs in `lib/`. `web/` owns all application UI; use a client-rendered static SPA, not Go templates or SvelteKit server endpoints.
- Ticket #3 owns bootstrap and the combined embedded build; #4 adds login and the protected dashboard. Build frontend assets before Go compilation; validate the resulting binary from an unrelated working directory without source assets or a frontend runtime. A standalone frontend build does not verify embedding.
- SvelteKit configuration lives in `web/vite.config.ts`, including the static adapter's `index.html` fallback and forced runes mode for application code. Read `web/AGENTS.md` before frontend work for required Svelte tooling.

## Checks

- Go (root): `go test ./...`; dependency vulnerabilities: `govulncheck ./...`.
- Frontend (from `web/`): `bun install --frozen-lockfile`, then `bun run check`, `bun run lint`, `bun run build`. `lint` is Prettier checking, not ESLint; there is no frontend test script yet.
- Container tests: `docker compose -f tests/compose.yaml run --build --rm test`. Read `tests/README.md` for custom runs and cleanup. Keep the `deploy-manager-tests` project name to share Bun and Go cache volumes across worktrees; ordinary cleanup preserves volumes. Use this environment when Playwright system dependencies are unavailable on the host.

## Shared workflow

- Name branches `<category>/<ticket-number>-<name>` (for example, `fix/123-...`, `feat/123-...`, or `refactor/123-...`); omit the ticket number when none is available.
- Use `gpt-6-luna` when delegating tasks to subagents.
- Publish or push the working branch only when explicitly instructed by the user.
- When working in a worktree, leave the main workspace's checked-out branch unchanged unless explicitly instructed by the user.
- Follow `docs/agents/issue-tracker.md` and `docs/agents/triage-labels.md` for GitHub issue/spec tracking; follow `docs/agents/domain.md` when changing the shared `GLOSSARY.md` or ADRs.
- Commit subjects use `<tag>: <message>` or `<category>/<tag>: <message>`.
- When merging fixes, prefer a fast-forward merge.
