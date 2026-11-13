#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ "${DM_BUILD_FRONTEND_IN_DOCKER:-0}" == "1" ]]; then
	docker run --rm -v "$root:/workspace" -v deploy-manager-bun-1.4.4-cache:/root/.bun/install/cache -w /workspace/web oven/bun:1.4.4-alpine \
		sh -eu -c 'bun install --frozen-lockfile && bun run build'
else
	cd "$root/web"
	bun install --frozen-lockfile
	bun run build
fi
cd "$root"
CGO_ENABLED=1 go build -o deploy-manager .
