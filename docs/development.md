# Development reference

[Documentation](README.md) · [Project home](../README.md)

Start with [CONTRIBUTING.md](../CONTRIBUTING.md) for prerequisites, setup and a first contribution.

The [2026-10-03 service audit](audit-2026-10-03.md) records the hardening
changes, validation results and remaining operating limits.

```bash
make run          # build the standalone binary, serve locally and open the browser
make run-image    # build and run the container image
make dev          # Go API on :8080 + Vite dev server on :5173 (hot reload)
make dev-backend  # API only
make test         # Go unit tests, with the race detector
make check        # formatting, vet, host inventory, Go, UI, OCR and release tests
make test-live    # include local, gitignored live extractor tests (when present)
make frontend     # build the UI into the Go embed directory
make lock         # regenerate frontend/package-lock.json
make dist         # package this host into ./dist (native cgo toolchain required)
make tag          # bump the patch and build a release; V=v0.1.0 overrides (requires gh)
make help         # every target
```

`make build`, `make run` and `make image` build through Docker. Frontend
builds and tests use local npm when available and Docker otherwise, with
locked dependencies installed through `npm ci`. Go tests, vet, host-list
generation and release cross-compilation need local Go; Go downloads the
toolchain pinned in `backend/go.mod` automatically. `make dev` needs both
Go and Node locally.

The checked-in tests use synthetic fixtures and local HTTP servers.
Live extractor tests are `*live_test.go` files behind the `live` build tag.
They are kept in a separate private repository and symlinked into the
checkout, where they are gitignored; they take passwords from environment
variables. A fresh clone has no live-site fixtures.

## Layout

```
backend/
  cmd/heapleach/        entry point
  internal/config/  settings and every shared tunable constant
  internal/util/    shared helpers (no dependencies)
  internal/httpx/   HTTP client: browser headers, redirects, retry/backoff
  internal/tools/   locates the optional yt-dlp, ffmpeg and deno
  internal/captcha/ in-process OCR, image preprocessing and ranked decoding
  internal/extractor/  one file per host + a direct-link fallback
  internal/download/   worker pool, resumable transfers, progress
  internal/server/     JSON API, SSE, embedded-asset serving
  internal/cli/        the headless run's terminal display
  internal/webui/dist/ the compiled frontend (go:embed)
frontend/           React + TypeScript (strict), built by Vite
```

## Adding a host

Implement `extractor.Extractor` (`Name`, `Shape`, `Recognise`, `Recognises`,
`Extract`) and register it in `NewRegistry`. No extractor claims a domain:
`Recognise` reads the probed page for the link's form and the markup the
extractor parses, `Shape` gives the link scanner the form alone, and
`Recognises` is its row in the [generated inventory](../README.md#every-supported-site).
See [the extractor guide](agents/extractors.md#the-contract). Return a `File`
per download; set `Resolve` instead of `URL` when the host issues links that
expire.

## Releases

Every push to `main` and pull request runs the shared checks, including the
built UI, Go and frontend tests, formatting, vet and the real OCR model on
Linux. After a main push passes, CI publishes Linux amd64/arm64 Docker images
as `latest` and `sha-<full SHA>`. The binary and OCI version label use the
highest stable tag reachable from that commit plus its Git SHA, for example
`v1.2.3-g0123456789ab`. This does not increment a version or create a release.
Without a stable tag, the version starts with `v0.0.0`.

Run **Update version** manually on `main` to create a version and build its
release. Leave `version` blank to increment the highest stable tag's patch
(for example, `v1.2.3` becomes `v1.2.4`), or enter an explicit version such
as `v1.3.0`. With no stable tags, the first default is `v0.0.1`.

The workflow requires successful CI on its selected commit, creates an
annotated tag, then automatically runs the Release pipeline. That pipeline
runs shared checks and native builds on all five platforms, tests the OCR
model from each archive, and publishes the complete release with checksums.
`make tag` dispatches the default patch update; `make tag V=v1.3.0` chooses
an explicit version. Both require GitHub CLI and a clean checkout matching
remote main.

The equivalent commands are:

```bash
# Bump the patch and build/publish the release.
gh workflow run version.yml --ref main
# Or choose the version explicitly.
gh workflow run version.yml --ref main -f version=v1.3.0
# Rebuild an existing tag without creating a new version.
gh workflow run release.yml --ref main -f tag=v1.2.3
```

A tag push alone does not build a release. Update version calls the release
workflow directly. Version updates are serialized, and rerunning an update
reuses its original tag even if another version has since been created.
A new dispatch with no version specified always increments the patch,
including at the same commit. **Release** remains available for rebuilding
an existing tag; publication is serialized across both entry points.
Manual releases also publish versioned Docker tags (`vX.Y.Z`, `X.Y.Z`,
`vX.Y`, `X.Y`) using the verified released Linux binaries. They leave
`latest` with the main image publisher. The native build cache retains
compilation output between releases.

Native Linux, macOS and Windows runners build and test these archives with cgo:

| Archive | For |
|---|---|
| `heapleach_<tag>_linux_amd64.tar.gz` | Linux, x86-64 |
| `heapleach_<tag>_linux_arm64.tar.gz` | Linux, arm64 |
| `heapleach_<tag>_darwin_amd64.tar.gz` | macOS, Intel |
| `heapleach_<tag>_darwin_arm64.tar.gz` | macOS, Apple silicon |
| `heapleach_<tag>_windows_amd64.zip` | Windows, x86-64 |

Each carries the binary, README, licence and the complete `ocr/` directory.
`SHA256SUMS` covers the set. `make dist` builds the matching archive for the
current host. Cross-compiling requires `RELEASE_TARGETS` and a suitable `CC`;
CI uses native runners and tests OCR from the extracted archive on every target.
