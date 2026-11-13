#!/usr/bin/env bash
set -euo pipefail

# Work on a private copy: generated assets and dependencies never touch the host.
workspace="$(mktemp -d /tmp/deploy-manager-tests.XXXXXX)"
trap 'rm -rf "$workspace"' EXIT
tar -C /source \
	--exclude=.git --exclude=node_modules --exclude=.svelte-kit \
	--exclude=./lib/web --exclude=./deploy-manager \
	-cf - . | tar -C "$workspace" -xf -

cd "$workspace/web"
bun install --frozen-lockfile
bun run check
bun run lint
cd "$workspace"
./scripts/build.sh
go test ./...
go test -race ./...
go vet ./...

cd tests/browser
bun install --frozen-lockfile
DEPLOY_MANAGER_BINARY="$workspace/deploy-manager" node registration.mjs
