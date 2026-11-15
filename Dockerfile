# syntax=docker/dockerfile:1.7

FROM oven/bun:1.4.4-alpine AS frontend
WORKDIR /workspace/web
COPY web/package.json web/bun.lock ./
RUN --mount=type=cache,id=deploy-manager-bun-1.4.4,target=/root/.bun/install/cache,sharing=locked \
    bun install --frozen-lockfile
COPY web ./
RUN bun run build

FROM golang:1.26.11-bookworm AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN --mount=type=cache,id=deploy-manager-go-mod,target=/go/pkg/mod,sharing=locked \
    go mod download
COPY . ./
COPY --from=frontend /workspace/lib/web ./lib/web
RUN --mount=type=cache,id=deploy-manager-go-build,target=/root/.cache/go-build,sharing=locked \
    mkdir -p /out /runtime/data /runtime/published \
    && CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/deploy-manager . \
    && chown -R 65532:65532 /runtime

FROM gcr.io/distroless/base-debian12:nonroot AS runtime
WORKDIR /data
COPY --from=builder /out/deploy-manager /usr/bin/deploy-manager
# Copy the trust store explicitly so outbound HTTPS (Git/LFS) works at runtime.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder --chown=65532:65532 /runtime/data /data
COPY --from=builder --chown=65532:65532 /runtime/published /published

ENV DM_DATA=/data \
    DM_LISTEN=0.0.0.0:8080 \
    DM_PUBLICATION_ROOT=/published

VOLUME ["/data", "/published"]
EXPOSE 8080
ENTRYPOINT ["/usr/bin/deploy-manager"]
CMD ["serve"]
