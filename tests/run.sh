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
GOFLAGS='-tags=integrationtest' ./scripts/build.sh
go build -o "$workspace/smart-https-fixture" ./tests/fixtures/smart-https
go build -o "$workspace/ssh-git-fixture" ./tests/fixtures/ssh-git
go build -o "$workspace/repository-key-state" ./tests/fault/repository-key-state
if [[ "${LIFECYCLE_OUTCOMES_ONLY:-false}" == "true" || "${PROJECT_OUTCOMES_ONLY:-false}" == "true" ]]; then
	cd "$workspace/tests/browser"
	bun install --frozen-lockfile
	DEPLOY_MANAGER_BINARY="$workspace/deploy-manager" \
		DEPLOY_MANAGER_FIXTURE_BINARY="$workspace/smart-https-fixture" \
		DEPLOY_MANAGER_SSH_FIXTURE_BINARY="$workspace/ssh-git-fixture" \
		timeout --kill-after=10s 300s node projects.mjs
	exit
fi
go test ./...
go test -race ./...
go vet ./...

cd tests/browser
bun install --frozen-lockfile
DEPLOY_MANAGER_BINARY="$workspace/deploy-manager" REPOSITORY_KEY_STATE_BINARY="$workspace/repository-key-state" timeout --kill-after=10s 180s node registration.mjs
DEPLOY_MANAGER_BINARY="$workspace/deploy-manager" DEPLOY_MANAGER_FIXTURE_BINARY="$workspace/smart-https-fixture" DEPLOY_MANAGER_SSH_FIXTURE_BINARY="$workspace/ssh-git-fixture" timeout --kill-after=10s 180s node projects.mjs
