# Container development and test environment

The Compose image is a reusable development tools environment as well as the
full browser-test runner. Use the interactive form to prepare or inspect the
project in a pinned Bun/Go/Playwright environment without launching the suite:

```sh
docker compose -f tests/compose.yaml run --build --rm test bash
```

The interactive shell starts in `/workspace`; `/source` is the read-only
checkout. Copy files to `/workspace` before running commands that generate or
modify project output. For example:

```sh
cp -a /source/. /workspace/
cd /workspace
cd web
bun install --frozen-lockfile
bun run check
```

The shell has shared Bun and Go caches mounted at `/cache`. Run any needed
development commands there; stop any servers before exiting. To run the complete
browser/Go test suite instead, use:

From any checkout or worktree root:

```sh
docker compose -f tests/compose.yaml run --build --rm test
```

This installs locked Bun dependencies, checks/lints/builds the frontend, builds
the combined Go executable, runs Go tests/race tests/vet, and runs Playwright
against that binary from a separate temporary working directory. Chromium and
its system dependencies are supplied by the pinned Playwright image. Run
`govulncheck ./...` separately after generating frontend assets; it is not
installed in this environment.

CLI behavior tests build one shared executable per package run; each scenario
still uses isolated installation data and runs outside the source directory.
With `go test -race`, this executable and the versioned upgrade/recovery fixture
executables are also built with `-race`, so child-process behavior is actually
instrumented. The recovery rehearsal retains two distinct fixture executables
because it verifies restoring the exact prior artifact, not just a version label.

The source bind mount is read-only. Each run copies source into temporary
container storage, excluding Git metadata, installed dependencies, and generated
frontend/binary output. No test state or root-owned build output is written into
the checkout. Only dependency and compilation caches persist.

## Shared caches and worktrees

The Compose project name is fixed to `deploy-manager-tests`, so its named Bun,
Go module, and Go build cache volumes are reused across worktrees. Use the file
from the worktree you want to test: its relative source mount selects that
checkout, while the cache names remain stable. `run --rm` creates a separate
one-off container for each invocation, including concurrent invocations.

Keep the project name unchanged (including `COMPOSE_PROJECT_NAME` and `-p`) to
reuse these caches. The environment image layers and Dockerfile's BuildKit cache
mounts also persist independently of a checkout.

Remove leftover Compose containers/networks without deleting caches:

```sh
docker compose -f tests/compose.yaml down
```

Use `down --volumes` only when deliberately discarding the shared caches; it
affects every worktree using this project name.
