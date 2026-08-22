# syntax=docker/dockerfile:1.7
#
# Build layout:
#   frontend -> compiles the TypeScript UI to static js/css/html
#   backend  -> embeds those assets with go:embed and links one static binary
#   export   -> scratch stage holding only the binary, for `--output`
#   runtime  -> the image that actually runs (default target)
#
# The whole app is a single self-contained binary: the UI is inside it, so
# there is nothing to serve from disk and no second process.

# --------------------------------------------------------------- frontend
FROM node:24-alpine AS frontend
WORKDIR /app/frontend

# Dependencies first, so edits to the source do not re-resolve the tree.
COPY frontend/package.json frontend/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm \
    if [ -f package-lock.json ]; then npm ci --no-audit --no-fund; \
    else npm install --no-audit --no-fund; fi

COPY frontend/ ./
# tsc type-checks in strict mode, then vite emits to ../backend/.../dist.
RUN npm run build && ls -la /app/backend/internal/webui/dist

# ---------------------------------------------------------------- backend
FROM golang:1.27-alpine AS backend
WORKDIR /src

COPY backend/go.mod backend/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY backend/ ./
# Replace the checked-in placeholder with the real build.
RUN rm -rf ./internal/webui/dist
COPY --from=frontend /app/backend/internal/webui/dist ./internal/webui/dist

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
# CGO off and -trimpath give a portable, reproducible static binary.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/heapleach ./cmd/heapleach

# ----------------------------------------------------------------- export
# `docker build --target export --output type=local,dest=bin .` drops the
# binary straight onto the host without running a container.
FROM scratch AS export
COPY --from=backend /out/heapleach /heapleach

# ---------------------------------------------------------------- runtime
FROM alpine:3.24 AS runtime

# ca-certificates for TLS to the download hosts; tzdata for sane timestamps.
RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -u 10001 -h /home/heapleach heapleach \
 && mkdir -p /downloads \
 && chown -R heapleach:heapleach /downloads

COPY --from=backend /out/heapleach /usr/local/bin/heapleach

ENV HEAPLEACH_ADDR=:8080 \
    HEAPLEACH_DIR=/downloads \
    HEAPLEACH_CONCURRENCY=4

USER heapleach
WORKDIR /home/heapleach
VOLUME ["/downloads"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=3s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/api/health >/dev/null 2>&1 || exit 1

ENTRYPOINT ["heapleach"]
