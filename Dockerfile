# syntax=docker/dockerfile:1.7
#
# Build layout:
#   frontend -> compiles the TypeScript UI to static js/css/html
#   backend  -> embeds those assets with go:embed and links one static binary
#   export   -> scratch stage holding only the binary, for `--output`
#   helpers  -> yt-dlp, ffmpeg/ffprobe and deno, the same builds
#               `make dependencies` fetches for the host
#   captcha  -> freezes the local OCR model and its Python runtime
#   captcha-export -> optional OCR helper for `make captcha-helper`
#   runtime  -> the image that actually runs (default target)
#
# The app itself is one self-contained binary: the UI is inside it, so there
# is nothing to serve from disk. The helpers are what some hosts need beyond
# HTTP (see internal/tools); they sit beside the binary, which is the first
# place tools.Find looks.

# --------------------------------------------------------------- frontend
# The UI and Go stages run on the builder's own platform and cross-compile.
# The OCR helper freezes native libraries, so that stage runs on the target
# platform (under QEMU when cross-building the runtime image).
FROM --platform=$BUILDPLATFORM node:24-alpine AS frontend
WORKDIR /app/frontend

# Dependencies first, so edits to the source do not re-resolve the tree.
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund

COPY frontend/ ./
# tsc type-checks in strict mode, then vite emits to ../backend/.../dist.
RUN npm run build && ls -la /app/backend/internal/webui/dist

# ---------------------------------------------------------------- backend
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS backend
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

# ---------------------------------------------------------------- captcha
# A separate optional helper keeps the application itself pure Go. Bookworm
# sets the helper's glibc baseline to 2.36; the runtime below is newer. Only
# the beta recognition model is needed, not the detector or older model.
FROM python:3.12-slim-bookworm AS captcha
RUN apt-get update \
 && apt-get install -y --no-install-recommends binutils \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /build
COPY helpers/captcha/requirements.txt ./
RUN --mount=type=cache,target=/root/.cache/pip pip install -r requirements.txt
COPY helpers/captcha/recognize.py ./
RUN model_dir=$(python -c 'import pathlib, ddddocr; print(pathlib.Path(ddddocr.__file__).parent)') \
 && pyinstaller --noconfirm --onefile --name heapleach-ocr \
      --add-data "$model_dir/common.onnx:ddddocr" \
      --add-data "/usr/local/lib/python3.12/LICENSE.txt:python" \
      --collect-binaries onnxruntime --recursive-copy-metadata ddddocr \
      recognize.py

FROM scratch AS captcha-export
COPY --from=captcha /build/dist/heapleach-ocr /heapleach-ocr

# ---------------------------------------------------------------- runtime
FROM debian:trixie-slim AS runtime

# ca-certificates for TLS to the download hosts; tzdata for sane timestamps;
# wget for the health check.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata wget \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 10001 --home-dir /home/heapleach heapleach \
 && mkdir -p /downloads \
 && chown -R heapleach:heapleach /downloads

COPY --from=helpers /out/ /usr/local/bin/
COPY --from=captcha /build/dist/heapleach-ocr /usr/local/bin/heapleach-ocr
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
