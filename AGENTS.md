# Repository guidance

## Requirements and terminology

- Read `docs/design.md` before implementation; it is the confirmed requirements authority. Its implementation-stage direction supersedes historical alternatives in the research notes.
- RFC 9807 OPAQUE is mandatory for registration and login. Before approving dependencies, complete the security and Go/browser interoperability feasibility gate in `docs/design.md`; the candidate pairing is not yet proven.
- Use `GLOSSARY.md` for domain terms, especially deployment vs. release, frozen tag targets, and permanent rollback vs. commit targets.
- For Git fetching/tree export/concurrency, LFS selection/reuse/transport, or publication recovery, read the corresponding `docs/research/` note. These record upstream research, not proven application interoperability; follow confirmed scope in `docs/design.md` where it differs.

## Project state and validation

- The web package is a Bun-managed SvelteKit scaffold under `web/`; it is not yet the designed embedded dashboard. Run its checks from `web/` using `bun run check`, `bun run lint`, and `bun run build`.
- No Go module exists yet. Once added, run `govulncheck ./...` from its module root. If unavailable, install with `go install golang.org/x/vuln/cmd/govulncheck@latest` and put the Go binary directory on `PATH`; report findings or inability to run it.
- `web/AGENTS.md` provides additional Svelte-specific instructions for work under `web/`.

## Git commits

- Format commit subjects as `<tag>: <message>` or `<category>/<tag>: <message>`.

## Repository workflow

- Track issues and specs in GitHub Issues with `gh`; follow `docs/agents/issue-tracker.md` and `docs/agents/triage-labels.md`.
- Follow `docs/agents/domain.md` for shared domain docs: root `GLOSSARY.md` and shared ADRs.
