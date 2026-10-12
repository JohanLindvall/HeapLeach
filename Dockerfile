# syntax=docker/dockerfile:1.7
# SPDX-License-Identifier: MIT
#
# Build layout:
#   frontend -> compiles the TypeScript UI to static js/css/html
#   backend  -> embeds those assets with go:embed and links the cgo-enabled application
#   export   -> scratch stage holding the binary and OCR data, for `--output`
#   helpers  -> yt-dlp, ffmpeg/ffprobe and deno, the same builds
#               `make dependencies` fetches for the host
#   ocr-assets -> downloads the pinned model and native CPU runtime
#   captcha-test -> tests the Go OCR reader against synthetic challenges
#   ocr-export -> exports the OCR data for source builds
#   released-binary -> verified release archive, used by the publishing job
#   runtime  -> the image that actually runs (default target)
#
# The UI is inside the application binary. OCR runs in-process using the
# adjacent ocr/ model and native library. Helpers are what some hosts need beyond
# HTTP (see internal/tools); they sit beside the binary, which is the first
# place tools.Find looks.

# Local builds compile from source; release images reuse the published binary.
ARG BINARY_STAGE=backend

# --------------------------------------------------------------- frontend
# UI, Go and asset downloads run on the builder platform. Only the runtime
# package install needs QEMU when producing an image for another architecture.
FROM --platform=$BUILDPLATFORM node:24-alpine AS frontend
WORKDIR /app/frontend

# Dependencies first, so edits to the source do not re-resolve the tree.
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund

COPY frontend/ ./
# tsc type-checks in strict mode, then vite emits to ../backend/.../dist.
RUN npm run build && ls -la /app/backend/internal/webui/dist

# -------------------------------------------------------------- OCR assets
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS ocr-assets
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl unzip \
 && rm -rf /var/lib/apt/lists/*
COPY helpers/captcha/install.sh /install.sh
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/heapleach/ocr \
    bash /install.sh "${TARGETOS:-linux}/${TARGETARCH:-amd64}" /out/ocr

FROM scratch AS ocr-export
COPY --from=ocr-assets /out/ /

# ---------------------------------------------------------------- backend
# glibc matches the bundled runtime; cgo cannot load it from a static musl
# executable. Bookworm gives local builds a glibc 2.36 baseline.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS backend-source
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY backend/ ./

FROM backend-source AS captcha-test
COPY --from=ocr-assets /out/ocr /ocr
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    HEAPLEACH_OCR_DIR=/ocr CGO_ENABLED=1 go test -race -count=1 ./internal/captcha

FROM backend-source AS backend
# Replace the checked-in placeholder with the real build.
RUN rm -rf ./internal/webui/dist
COPY --from=frontend /app/backend/internal/webui/dist ./internal/webui/dist
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
ARG BUILDARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    if [ "${TARGETARCH}" != "${BUILDARCH}" ]; then \
      case "${TARGETARCH}" in \
        arm64) compiler=aarch64-linux-gnu ;; \
        amd64) compiler=x86_64-linux-gnu ;; \
        *) echo "unsupported target ${TARGETARCH}" >&2; exit 1 ;; \
      esac; \
      apt-get update; apt-get install -y --no-install-recommends "gcc-${compiler}" "libc6-dev-${TARGETARCH}-cross"; \
      export CC="${compiler}-gcc"; \
    fi; \
    CGO_ENABLED=1 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/heapleach ./cmd/heapleach
COPY --from=ocr-assets /out/ocr /out/ocr

# ----------------------------------------------------------------- export
FROM scratch AS export
COPY --from=backend /out/ /

# ---------------------------------------------------------------- helpers
# The upstream builds are linked against glibc, which is why the runtime
# below is Debian rather than Alpine: musl cannot load them, and the distro
# packages trail yt-dlp by months — long enough for a host to have changed
# under it. These are "latest" downloads, so the layer is only as fresh as
# the build cache lets it be: `make image` after `docker builder prune`, or
# with --no-cache, picks up new releases.
FROM --platform=$BUILDPLATFORM debian:trixie-slim AS helpers
ARG TARGETARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl xz-utils unzip \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /out
RUN set -eu; \
    case "${TARGETARCH:-amd64}" in \
      amd64) ytdlp=yt-dlp_linux;         ffmpeg=ffmpeg-master-latest-linux64-gpl;    deno=deno-x86_64-unknown-linux-gnu ;; \
      arm64) ytdlp=yt-dlp_linux_aarch64; ffmpeg=ffmpeg-master-latest-linuxarm64-gpl; deno=deno-aarch64-unknown-linux-gnu ;; \
      *) echo "no helper builds for ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 3 -o yt-dlp "https://github.com/yt-dlp/yt-dlp/releases/latest/download/${ytdlp}"; \
    curl -fsSL --retry 3 "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/${ffmpeg}.tar.xz" \
      | tar -xJ --strip-components=2 --wildcards '*/bin/ffmpeg' '*/bin/ffprobe'; \
    curl -fsSL --retry 3 -o deno.zip "https://github.com/denoland/deno/releases/latest/download/${deno}.zip"; \
    unzip -q deno.zip deno && rm deno.zip; \
    chmod 0755 yt-dlp ffmpeg ffprobe deno

# --------------------------------------------------------- release binary
# Only reached with BINARY_STAGE=released-binary. The release workflow checks
# the archives' checksums before placing the two Linux binaries here.
FROM scratch AS released-binary
ARG TARGETARCH
COPY dist/runtime/${TARGETARCH}/ /out/

FROM ${BINARY_STAGE} AS app

# ---------------------------------------------------------------- runtime
FROM debian:trixie-slim AS runtime

# ca-certificates for TLS to the download hosts; tzdata for sane timestamps;
# wget for the health check.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata wget libstdc++6 \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 10001 --home-dir /home/heapleach heapleach \
 && mkdir -p /downloads \
 && chown -R heapleach:heapleach /downloads

COPY --from=helpers /out/ /usr/local/bin/
COPY --from=app /out/ /usr/local/bin/

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
