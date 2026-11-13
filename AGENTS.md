# Repository guidance

## Requirements and domain

- Before implementation, read `docs/design.md`; it is the confirmed requirements authority and supersedes alternatives in research notes. Dependency selection for RFC 9807 OPAQUE is approved; preserve the remaining security, upgrade, and distribution requirements there.
- Use `GLOSSARY.md` for domain terms, especially deployment vs. release, frozen tag targets, and permanent rollback vs. commit targets.
- For Git compatibility/tree export, LFS, publication recovery, or OPAQUE implementation details, read the corresponding `docs/research/` notes; where they differ, `docs/design.md` governs.

## Project layout and checks

- The Go module is at the repository root (`github.com/eslym/deploy-manager`); `main.go` is currently a stub and application logic belongs in `lib/`.
- `web/` is a separate Bun-managed SvelteKit scaffold, not yet the designed embedded dashboard. Run its checks from `web/`: `bun run check`, `bun run lint`, `bun run build`. See `web/AGENTS.md` for Svelte-specific tooling instructions.
- Run Go checks from the repository root with `go test ./...`; run `govulncheck ./...` for Go dependency vulnerability checks.

## Shared workflow

- Follow `docs/agents/issue-tracker.md` and `docs/agents/triage-labels.md` for GitHub issue/spec tracking; follow `docs/agents/domain.md` when changing the shared `GLOSSARY.md` or ADRs.
- Commit subjects use `<tag>: <message>` or `<category>/<tag>: <message>`.
- When merging fixes, prefer a fast-forward merge.
