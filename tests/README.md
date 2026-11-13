# Container test environment

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

To open the tools environment without running the suite:

```sh
docker compose -f tests/compose.yaml run --build --rm test bash
```

The source is at `/source`; use a disposable copy when running commands that
write generated files. Stop any development servers you start before exiting.

Remove leftover Compose containers/networks without deleting caches:

```sh
docker compose -f tests/compose.yaml down
```

Use `down --volumes` only when deliberately discarding the shared caches; it
affects every worktree using this project name.
