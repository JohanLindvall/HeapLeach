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
make tag V=v0.1.0 # dispatch the manual version workflow (requires gh)
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

Every push to `main` and pull request runs the shared checks, including the
built UI, Go and frontend tests, formatting, vet and the real OCR model on
Linux. After a main push passes, CI publishes Linux amd64/arm64 Docker images
as `latest` and `sha-<full SHA>`. The binary and OCI version label use the
highest stable tag reachable from that commit plus its Git SHA, for example
`v1.2.3-g0123456789ab`. This does not increment a version or create a release.
Without a stable tag, the version starts with `v0.0.0`.

Versions and releases are separate manual workflows in GitHub Actions. Run
both on the `main` branch:

1. Run **Update version** with the desired `version`, such as `v1.2.3`.
   It requires successful CI on the selected main commit and creates that
   exact annotated tag. `make tag V=v1.2.3` is a shortcut using GitHub CLI
   from a clean checkout matching remote main.
2. After the tag exists, run **Release** with that `tag`. This runs the
   shared checks and native builds on all five platforms, tests the OCR
   model from each archive, and publishes the complete release with checksums.

The equivalent commands are:

```bash
gh workflow run version.yml --ref main -f version=v1.2.3
# Wait for Update version to complete before starting the release.
gh workflow run release.yml --ref main -f tag=v1.2.3
```

A tag push alone does not build a release. Version creation and publication
are serialized separately; retries reuse a matching tag or partial draft.
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
