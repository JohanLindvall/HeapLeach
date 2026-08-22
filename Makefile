# HeapLeach — parallel bulk downloader
#
# The default build runs entirely inside Docker (no local Go or Node needed)
# and writes the resulting single, self-contained binary to ./bin/heapleach on
# the host. The TypeScript UI is compiled and embedded into that binary.

SHELL     := /bin/bash
.DEFAULT_GOAL := help

BIN_DIR   := bin
DIST_DIR  := dist
BINARY    := $(BIN_DIR)/heapleach

# What `make dist` cross-compiles. The program is pure Go with cgo off, so
# every one of these builds from whichever machine runs make.
RELEASE_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
IMAGE     ?= heapleach
TAG       ?= latest
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

PORT      ?= 8080
# `make run` takes any free port by default; the binary reports the real one.
ADDR      ?= 127.0.0.1:0
DOWNLOADS ?= $(HOME)/Downloads
CONCURRENCY ?= 4

DIST      := backend/internal/webui/dist

# What the exported binary is made of, so `make build` can tell a stale
# binary from a current one and do nothing when there is nothing to do.
#
# The frontend counts as much as the Go does: it is compiled into the binary,
# so a UI edit leaves it just as out of date as an extractor edit would. The
# embed directory itself is excluded — it is build output, rewritten by every
# frontend build, and treating it as a source would mean never settling.
GO_SOURCES := $(shell find backend -name '*.go' -not -path '$(DIST)/*' 2>/dev/null) \
              backend/go.mod backend/go.sum
UI_SOURCES := $(shell find frontend -type f \
                \( -name '*.ts' -o -name '*.tsx' -o -name '*.css' -o -name '*.html' -o -name '*.json' \) \
                -not -path 'frontend/node_modules/*' -not -path 'frontend/dist/*' 2>/dev/null)
BUILD_SOURCES := $(GO_SOURCES) $(UI_SOURCES) Dockerfile

# Optional helpers the service uses when present: yt-dlp resolves YouTube,
# ffmpeg rewraps and muxes. `make dependencies` puts static builds in ./bin,
# where the service looks before falling back to PATH.
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_M),aarch64)
YTDLP_ASSET  := yt-dlp_linux_aarch64
FFMPEG_ASSET := ffmpeg-master-latest-linuxarm64-gpl.tar.xz
else
YTDLP_ASSET  := yt-dlp_linux
FFMPEG_ASSET := ffmpeg-master-latest-linux64-gpl.tar.xz
endif
YTDLP_URL  := https://github.com/yt-dlp/yt-dlp/releases/latest/download/$(YTDLP_ASSET)
FFMPEG_URL := https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/$(FFMPEG_ASSET)
NODE_IMAGE := node:24-alpine
UID_GID   := $(shell id -u):$(shell id -g)

# Prefer the host toolchain when it exists, otherwise fall back to Docker.
HAVE_NPM  := $(shell command -v npm 2>/dev/null)
HAVE_GO   := $(shell command -v go 2>/dev/null)

.PHONY: help build binary image run run-image stop logs shell dev dev-backend dev-frontend \
        frontend frontend-clean screenshots dist tag native test test-live fmt vet tidy lock dependencies \
        clean distclean

## help: show this help
help:
	@echo "HeapLeach — parallel bulk downloader"
	@echo
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  make /' | column -t -s ':'
	@echo
	@echo "  Variables: ADDR=$(ADDR) (run) PORT=$(PORT) (run-image)"
	@echo "             DOWNLOADS=$(DOWNLOADS) IMAGE=$(IMAGE):$(TAG)"

## build: build in Docker and export the standalone binary to ./bin/heapleach
build: $(BINARY)

# The rule that does the work. Its prerequisites are the sources the binary
# is made of, which is what lets `build` be asked for freely: it is a no-op
# while the binary is newer than all of them, and it rebuilds the moment one
# changes. Anything that runs the binary depends on this rather than on the
# file existing, since a stale binary is exactly what looks like a bug.
$(BINARY): $(BUILD_SOURCES) | $(BIN_DIR)
	@echo ">> building in Docker, exporting binary to $(BINARY)"
	DOCKER_BUILDKIT=1 docker build \
	  --target export \
	  --build-arg VERSION=$(VERSION) \
	  --output type=local,dest=$(BIN_DIR) \
	  -f Dockerfile .
	@chmod +x $(BINARY)
	@# BuildKit writes the file with the timestamp it had in the image, which
	@# can predate the sources and would leave the target permanently stale.
	@touch $(BINARY)
	@echo ">> $(BINARY) ($$(du -h $(BINARY) | cut -f1)) — self-contained, UI embedded"

## binary: alias for build
binary: build

## image: build the runnable Docker image
image:
	DOCKER_BUILDKIT=1 docker build \
	  --target runtime \
	  --build-arg VERSION=$(VERSION) \
	  -t $(IMAGE):$(TAG) \
	  -f Dockerfile .
	@echo ">> built $(IMAGE):$(TAG)"

## run: build if needed, serve on ~/Downloads on a free port, open a browser
run: $(BINARY)
	@mkdir -p "$(DOWNLOADS)"
	@# The binary binds the port, so it is the only thing that can know
	@# which one it got — it logs the URL and opens it itself.
	@exec $(BINARY) -addr "$(ADDR)" -open -concurrency $(CONCURRENCY) "$(DOWNLOADS)"

## run-image: run the container image instead of the host binary
run-image: image
	@mkdir -p "$(DOWNLOADS)"
	docker run --rm -it \
	  --name heapleach \
	  -p $(PORT):8080 \
	  -e HEAPLEACH_CONCURRENCY=$(CONCURRENCY) \
	  -v "$(DOWNLOADS)":/downloads \
	  --user $(UID_GID) \
	  $(IMAGE):$(TAG)

## stop: stop a detached container started by `make run-image`
stop:
	-docker stop heapleach

## logs: follow container logs
logs:
	docker logs -f heapleach

## shell: open a shell in the runtime image
shell: image
	docker run --rm -it --entrypoint sh $(IMAGE):$(TAG)

## native: build on the host (needs Go; uses Docker for the UI if Node is absent)
native: frontend $(BIN_DIR)
ifndef HAVE_GO
	$(error Go is not installed — use `make build` to build in Docker instead)
endif
	cd backend && CGO_ENABLED=0 go build -trimpath \
	  -ldflags "-s -w -X main.version=$(VERSION)" \
	  -o ../$(BINARY) ./cmd/heapleach
	@echo ">> $(BINARY)"

## dist: cross-compile the release archives into ./dist
dist: frontend
ifndef HAVE_GO
	$(error Go is not installed — a release build needs it on the host)
endif
	@rm -rf $(DIST_DIR) && mkdir -p $(DIST_DIR)
	@set -euo pipefail; \
	for target in $(RELEASE_TARGETS); do \
	  goos=$${target%%/*}; goarch=$${target##*/}; \
	  binary=heapleach; \
	  [ "$$goos" = windows ] && binary=heapleach.exe; \
	  stage=$$(mktemp -d); \
	  ( cd backend && CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -trimpath \
	      -ldflags "-s -w -X main.version=$(VERSION)" \
	      -o "$$stage/$$binary" ./cmd/heapleach ); \
	  cp README.md LICENSE "$$stage/"; \
	  base="heapleach_$(VERSION)_$${goos}_$${goarch}"; \
	  if [ "$$goos" = windows ]; then \
	    ( cd "$$stage" && zip -qr - . ) > "$(DIST_DIR)/$$base.zip"; \
	  else \
	    tar -czf "$(DIST_DIR)/$$base.tar.gz" -C "$$stage" .; \
	  fi; \
	  rm -rf "$$stage"; \
	  echo ">> $(DIST_DIR)/$$base"; \
	done; \
	cd $(DIST_DIR) && sha256sum * > SHA256SUMS
	@echo ">> $(DIST_DIR)/SHA256SUMS"

## tag: cut a release, e.g. `make tag V=v0.1.0` — CI builds and publishes it
tag:
	@test -n "$(V)" || { echo "usage: make tag V=v0.1.0" >&2; exit 1; }
	@case "$(V)" in v[0-9]*) ;; *) echo "tags look like v0.1.0" >&2; exit 1;; esac
	@git diff --quiet HEAD || { echo "the working tree is dirty" >&2; exit 1; }
	git tag -a "$(V)" -m "HeapLeach $(V)"
	git push origin "$(V)"
	@echo ">> pushed $(V); the release workflow takes it from here"

## screenshots: regenerate the pictures in docs/ (needs Chrome or Chromium)
screenshots: frontend
	bash docs/screenshots.sh

## frontend: compile the TypeScript UI into the Go embed directory
frontend:
ifdef HAVE_NPM
	cd frontend && npm install --no-audit --no-fund && npm run build
else
	@echo ">> npm not found — building the UI in Docker"
	docker run --rm \
	  -v "$(CURDIR)":/w -w /w/frontend \
	  -u $(UID_GID) \
	  -e npm_config_cache=/tmp/.npm -e HOME=/tmp \
	  $(NODE_IMAGE) \
	  sh -c 'npm install --no-audit --no-fund && npm run build'
endif
	@echo ">> UI built into $(DIST)"

## dependencies: fetch static yt-dlp and ffmpeg into ./bin for the service
dependencies: $(BIN_DIR)
	@echo ">> yt-dlp  ($(YTDLP_ASSET))"
	@curl -fsSL --retry 3 -o "$(BIN_DIR)/yt-dlp.tmp" "$(YTDLP_URL)"
	@chmod +x "$(BIN_DIR)/yt-dlp.tmp" && mv "$(BIN_DIR)/yt-dlp.tmp" "$(BIN_DIR)/yt-dlp"
	@echo ">> ffmpeg  ($(FFMPEG_ASSET))"
	@tmp=$$(mktemp -d) && \
	  curl -fsSL --retry 3 -o "$$tmp/ffmpeg.tar.xz" "$(FFMPEG_URL)" && \
	  tar -xJf "$$tmp/ffmpeg.tar.xz" -C "$$tmp" --strip-components=2 \
	      --wildcards '*/bin/ffmpeg' '*/bin/ffprobe' && \
	  mv "$$tmp/ffmpeg" "$$tmp/ffprobe" "$(BIN_DIR)/" && \
	  rm -rf "$$tmp"
	@chmod +x "$(BIN_DIR)/ffmpeg" "$(BIN_DIR)/ffprobe"
	@printf '   yt-dlp %s\n' "$$($(BIN_DIR)/yt-dlp --version 2>/dev/null)"
	@$(BIN_DIR)/ffmpeg -version 2>/dev/null | head -1 | sed 's/^/   /'
	@echo ">> ready in $(BIN_DIR)/ — the service prefers these over PATH"

## lock: generate frontend/package-lock.json (reproducible installs)
lock:
	docker run --rm \
	  -v "$(CURDIR)":/w -w /w/frontend \
	  -u $(UID_GID) \
	  -e npm_config_cache=/tmp/.npm -e HOME=/tmp \
	  $(NODE_IMAGE) \
	  npm install --package-lock-only --no-audit --no-fund
	@echo ">> wrote frontend/package-lock.json"

## dev: run the Go API and the Vite dev server together (hot reload)
dev:
	@echo ">> API on :8080, UI on http://localhost:5173"
	@trap 'kill 0' EXIT INT TERM; \
	( cd backend && go run ./cmd/heapleach -debug "$(DOWNLOADS)" ) & \
	( cd frontend && npm run dev ) & \
	wait

## dev-backend: run only the Go API on port 8080
dev-backend:
	cd backend && go run ./cmd/heapleach -debug "$(DOWNLOADS)"

## dev-frontend: run only the Vite dev server on port 5173
dev-frontend:
	cd frontend && npm run dev

## test: run the Go unit tests
test:
	cd backend && go test ./...

## test-live: run the extractor tests against the real sites (needs network)
test-live:
	cd backend && go test -tags live -v ./internal/extractor/

## fmt: format the Go sources
fmt:
	cd backend && gofmt -w .

## vet: run go vet
vet:
	cd backend && go vet ./...

## tidy: tidy the Go module
tidy:
	cd backend && go mod tidy

$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

## clean: remove build output and restore the embed placeholder
clean: frontend-clean
	# Only the binary: running `./bin/heapleach` from inside bin/ puts downloads
	# in bin/downloads, and `rm -rf bin` would take them with it.
	rm -f $(BINARY)
	-rmdir $(BIN_DIR) 2>/dev/null || true
	-docker rmi $(IMAGE):$(TAG) 2>/dev/null || true

frontend-clean:
	rm -rf $(DIST)
	@mkdir -p $(DIST)
	@printf '%s\n' \
	  '<!doctype html>' \
	  '<html lang="en" data-heapleach-placeholder>' \
	  '  <head><meta charset="utf-8" /><title>HeapLeach</title></head>' \
	  '  <body><h1>Frontend not built</h1>' \
	  '  <p>Run <code>make frontend</code> or <code>make build</code>.</p></body>' \
	  '</html>' > $(DIST)/index.html
	@echo ">> restored the $(DIST) placeholder"

## distclean: clean plus node_modules and the downloaded helper tools
distclean: clean
	rm -rf frontend/node_modules
	rm -f $(BIN_DIR)/yt-dlp $(BIN_DIR)/ffmpeg $(BIN_DIR)/ffprobe
	@echo ">> left $(DOWNLOADS) alone; remove it by hand if you want to"
