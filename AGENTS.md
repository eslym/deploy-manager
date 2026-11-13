# Repository guidance

## Requirements and domain

- Before implementation, read `docs/design.md`; it is the confirmed requirements authority and supersedes alternatives in research notes. Dependency selection for RFC 9807 OPAQUE is approved; preserve the remaining security, upgrade, and distribution requirements there.
- Use `GLOSSARY.md` for domain terms, especially deployment vs. release, frozen tag targets, and permanent rollback vs. commit targets.
- For Git compatibility/tree export, LFS, publication recovery, or OPAQUE implementation details, read the corresponding `docs/research/` notes; where they differ, `docs/design.md` governs.

## Project layout and checks

- The Go module is at the repository root (`github.com/eslym/deploy-manager`); application logic belongs in `lib/`.
- `web/` is the Bun-managed SvelteKit SPA source for all application UI; embed its static production build in Go as required by `docs/design.md`. Ticket #3 owns bootstrap and the combined frontend/Go build; #4 extends it with login and the dashboard. Run frontend checks from `web/`: `bun run check`, `bun run lint`, `bun run build`. See `web/AGENTS.md` for Svelte-specific tooling instructions.
- Run Go checks from the repository root with `go test ./...`; run `govulncheck ./...` for Go dependency vulnerability checks.

## Shared workflow

- Follow `docs/agents/issue-tracker.md` and `docs/agents/triage-labels.md` for GitHub issue/spec tracking; follow `docs/agents/domain.md` when changing the shared `GLOSSARY.md` or ADRs.
- Commit subjects use `<tag>: <message>` or `<category>/<tag>: <message>`.
- When merging fixes, prefer a fast-forward merge.
