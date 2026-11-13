# syntax=docker/dockerfile:1.7
FROM oven/bun:1.4.4-alpine AS frontend
WORKDIR /workspace
COPY go.mod go.sum main.go ./
COPY lib ./lib
COPY web ./web
RUN --mount=type=cache,id=deploy-manager-bun-1.4.4,target=/root/.bun/install/cache,sharing=locked \
    cd web && bun install --frozen-lockfile && bun run check && bun run lint && bun run build

FROM mcr.microsoft.com/playwright:v1.65.0-noble
ARG GO_VERSION=1.26.11
RUN --mount=type=cache,id=deploy-manager-apt-archives,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,id=deploy-manager-apt-lists,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gcc libc6-dev \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tar.gz \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz

ENV PATH="/usr/local/go/bin:${PATH}"
WORKDIR /workspace
COPY --from=frontend /workspace/go.mod /workspace/go.sum /workspace/main.go ./
COPY --from=frontend /workspace/lib ./lib
COPY tests/browser ./tests/browser
RUN --mount=type=cache,id=deploy-manager-npm,target=/root/.npm,sharing=locked \
    --mount=type=cache,id=deploy-manager-go-mod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=deploy-manager-go-build,target=/root/.cache/go-build,sharing=locked \
    npm install --prefix tests/browser --no-save --ignore-scripts --no-package-lock --no-audit --no-fund \
    && CGO_ENABLED=1 go build -o /opt/deploy-manager .
ENV DEPLOY_MANAGER_BINARY=/opt/deploy-manager
CMD ["node", "tests/browser/registration.mjs"]
