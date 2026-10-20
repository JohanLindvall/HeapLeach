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
make tag V=v0.1.0 # manually choose a release version (main pushes bump the patch)
make help         # every target
```

`make build`, `make run` and `make image` build through Docker. Frontend
builds and tests use local npm when available and Docker otherwise, with
locked dependencies installed through `npm ci`. Go tests, vet, host-list
generation and release cross-compilation need local Go; Go downloads the
toolchain pinned in `backend/go.mod` automatically. `make dev` needs both
Go and Node locally.

The checked-in tests use synthetic fixtures and local HTTP servers.
Live extractor tests belong in gitignored `*live_test.go` files behind the
`live` build tag and take their source URLs and passwords from environment
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

Implement `extractor.Extractor` (`Name`, `Match`, `Extract`) and register it
in `NewRegistry`. Embed a `hostSet` naming the domains it claims: that one
declaration supplies `Match` and the [generated inventory](../README.md#every-supported-site) entry both, so the two
cannot disagree, and a `Match` of your own is only for what a host list
cannot say. Return a `File` per download; set `Resolve` instead of `URL` when
the host issues links that expire.

## Releases

Every push to `main` and pull request runs the tests, `go vet` and a `gofmt` check, with
the UI compiled first so the binary is built against the real embedded
frontend rather than the placeholder. Native builds and real-model OCR tests run on all five release platforms
alongside the main checks; all must pass before a release starts.

After a push to `main` passes CI, it automatically tags the tested commit
with the next patch version and publishes the archives and container images.
Versions follow the highest stable `vX.Y.Z` tag. Publishing is queued, and a
rerun reuses its tag or skips a commit already included in a newer release.
`make tag V=vX.Y.Z` remains available for choosing a version explicitly;
pushing that tag uses the same publisher.

Automatic releases reuse the caller's tests after verifying the checked-out
commit; standalone tags run the Go suite themselves. A separate release cache
retains compilation output for all five targets between commits. Container
images use the published Linux binaries after checking their checksums and
version, so the archives and images carry the same executable and embedded UI.

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
