# AGENTS.md

Guidance for coding agents working in this repository. This file covers what
applies everywhere. Each guide in [`docs/agents/`](docs/agents/) covers one
area of the code, with the invariants and traps the code alone does not show:
read the matching guide before changing that area. The
[table at the end](#area-guides) says which.

## What this is

`HeapLeach` is a bulk downloader: a Go backend with per-host extractors and a
parallel worker pool, plus a React + TypeScript UI that is **compiled into the
Go binary**. Releases also carry an `ocr/` directory with the pinned CAPTCHA
model, native ONNX Runtime and license notices.

## User and contributor documentation

`README.md` is the project's front page and quick start. Keep detailed user
reference material in `docs/`, indexed by `docs/README.md`, and contributor
setup in `CONTRIBUTING.md`. `SUPPORT.md` is the troubleshooting and reporting
entry point; GitHub issue forms and the pull-request template live in
`.github/`. Update the relevant user guide when behavior changes.

The generated supported-site inventory stays in README.md inside a collapsed
section, with its markers intact, so `make hosts` and CI remain its source
of truth. Preserve the README's existing section anchors when moving detail:
published links can still point readers to the fuller guide.

## Commands

All `make` targets run from the repo root; `go` commands run from `backend/`.

```bash
make build          # Docker builds everything, exports ./bin/heapleach to the host
make run            # build if needed, serve on ~/Downloads on a free port, open a browser
make image          # build the runnable container image
make run-image      # run the container image (passes --user so bind mounts work)
make native         # build on the host (needs Go; uses Docker for the UI if Node is absent)
make dev            # Go API on :8080 + Vite dev server on :5173 (needs Go and Node)
make frontend       # compile the UI into the Go embed dir (Docker if npm is absent)
make hosts          # regenerate README's supported-site inventory from the registry
make check          # CI checks: formatting, vet, hosts, Go, UI, OCR and release tests
make test-frontend  # type-check the UI and run its vitest suite (Docker if npm is absent)
make test-live      # extractor tests against the real sites (docs/agents/extractors.md)
make dependencies   # install yt-dlp, ffmpeg, deno and local OCR into ./bin
make ocr-assets     # install the pinned OCR model and native runtime into ./bin/ocr
make dist           # package this host (RELEASE_TARGETS + CC for cross-compilation)
make tag            # bump the patch and build a release; V=v1.2.3 overrides (requires gh)
make help           # every target
```

`make build` is incremental: `./bin/heapleach` is a real target whose
prerequisites are the Go and frontend sources, so it does nothing when nothing
changed, and `make run` depends on it rather than on the file merely existing.
A binary that silently predates your edits looks exactly like a bug in the
code you just wrote.

Go work:

```bash
cd backend
go test ./... -race           # or: make test
go vet ./... && go vet -tags live ./...
gofmt -w ./internal ./cmd

go test ./internal/download/ -run TestSafeName -v          # single test
go test ./internal/download/ -run 'Enqueue|DoubleRuns' -v   # subset
```

The frontend's vitest suite covers formatting, progress, filtering, snapshot
merging, API errors and connection recovery, plus jsdom submission tests. CI
runs it after the build.

Node is **not** installed on this machine — the frontend builds via Docker.
`make build` and `make frontend` both handle that automatically.

Toolchain versions live in the Dockerfile (Go 1.27 on Debian bookworm, Node 24
LTS on Alpine for the UI, Debian trixie-slim for the runtime image, which
carries the glibc-linked helpers and complete OCR data beside the binary) and
in `backend/go.mod`. The local Go is older than the `go.mod` directive, so
`go` commands auto-download the pinned toolchain on first use.

## Committing

Work goes straight to `main`: commit and push there rather than opening a
branch or a pull request, and without asking first. That is this repository's
settled workflow, not an oversight.

Keep site names and domains out of commit messages. Describe the changed
behaviour in generic terms instead.

A push to `main` that passes CI publishes only Docker images, stamped with
the latest stable tag plus the Git SHA. Manual Update version runs choose a
version and build its release; Release can rebuild an existing tag. Release
changes and manual publication have a checklist in [releases.md](docs/agents/releases.md).

## No live URLs or real filenames in the repository

Nothing tracked may contain a link to a specific piece of content — no video
pages, albums, profiles, threads or signed media URLs — **nor the name of a
real file on any of these hosts**. That second half is the one that gets
forgotten: a fixture built by viewing a live page tends to arrive with the
page's own filenames still in it, and a test asserting on a real release or
track name is the same mistake wearing different clothes. Both are as stale,
and as much a catalogue of somebody's content, as a link would be.

This applies to code, tests, README.md, these guides and commit messages
alike.

Invent the names. A fixture proves the parser reads `data-title`; it does not
matter whose title it is, and `First Song` tests the code exactly as well as a
real one does. Where the *shape* is what is under test — typographic quotes
around a release, an apostrophe inside a word — invent something with that
shape rather than reaching for the example that prompted it.

What is fine, and necessary:

- site roots and API bases (`https://api.gofile.io`, `https://kemono.cr`)
- documented URL shapes with placeholders (`https://gofile.io/d/<code>`)
- URLs the code builds at runtime from an id
- `example.test` and similar reserved domains in fixtures

Where a test needs realistic data — the KVS permutation in `kvs_test.go`, say
— use a **synthetic fixture**. The algorithm is what is under test and does
not care whose host a path belongs to; real-world correctness belongs in the
live tests. Generate the expected value with the implementation once, then pin
it.

**Standing rule: live tests and real URLs live in the private
[`hl-priv`](https://github.com/JohanLindvall/hl-priv) repository, and only
there.** That covers every `*live_test.go`, and every list or note of URLs or
filenames taken from real pages (verification corpora included). Clone it
beside this checkout and run its `link.sh`, which symlinks the live tests into
place; `.gitignore` carries `*live_test.go`, so the links are never committed
here. Create a new live test in `hl-priv` and link it, rather than writing it
into this checkout, and commit it there. Before committing here, check that
nothing slipped in:

```bash
git ls-files -z | xargs -0 grep -nE 'gofile\.io/d/[A-Za-z0-9]|thisvid\.com/videos/|youtube\.com/watch\?v=|mega\.nz/(file|folder)/[A-Za-z0-9_-]{4}'
```

## Architecture

Packages depend one way only, enforced by convention:

```
config, util                        (no internal deps)
  └─ httpx, tools, captcha, logstore
       └─ extractor, proxy
            └─ download
                 └─ server, cli
```

`webui` stands alone and only carries the embedded assets; `cmd/heapleach`
wires everything together.

### The single-binary coupling

Vite's `outDir` points **directly into the Go package** that embeds it
(`frontend/vite.config.ts` → `backend/internal/webui/dist`), and
`webui/embed.go` uses `//go:embed all:dist`. Consequences worth knowing before
editing either side:

- A checked-in placeholder `dist/index.html` keeps `go build` working before
  any frontend build. `go:embed` fails outright on a missing directory, so
  **never delete `dist/` without restoring the placeholder** — `make clean`
  does this via its `frontend-clean` target.
- `webui.Built()` distinguishes the placeholder from a real build; the server
  logs a warning when only the placeholder is embedded.
- A plain `cd backend && go build` embeds whatever is in `dist/` right now,
  which may be stale. Use `make build` or `make native`.
- `make build` never writes into the host's `backend/internal/webui/dist`: the
  UI is built inside the container and only `bin/heapleach` is exported, so a
  build cannot disturb the tracked placeholder.

### Conventions that span packages

- **Every tunable constant lives in `internal/config/limits.go`**, and HTTP
  header names and common values in `internal/httpx/headers.go`
  (`httpx.Header`, `httpx.Referer`, `httpx.RefererOrigin`,
  `httpx.SiteOrigin`). Add new ones there rather than scattering literals. Shared dependency-free helpers belong in
  `internal/util`.
- **Byte counts are SI and read the same everywhere.** `cli.formatBytes`
  mirrors `frontend/src/format.ts`, and their test tables
  (`internal/cli/cli_test.go` and `frontend/src/format.test.ts`) mirror each
  other digit for digit, so the same transfer cannot read "1.5 GB" in the
  terminal and "1.6 GB" in the browser. Change them together or not at all.
- **Everything the event stream carries is rendered somewhere.** A field the
  UI stops reading comes out of the payload rather than being left to
  accumulate.

## Area guides

| Before changing | Read |
| --- | --- |
| CI workflows, release scripts or Dependabot, or before tagging by hand | [releases.md](docs/agents/releases.md) |
| Extractors in general: the registry, the `File` contract, the generic fallback, listings, live tests | [extractors.md](docs/agents/extractors.md) |
| One host's extractor | its notes in [`hosts/`](docs/agents/hosts/), indexed at the end of extractors.md |
| `internal/httpx`: retries, rate limits, TLS, proxy errors, challenge pages | [http.md](docs/agents/http.md) |
| Proxy routes and routed transfers, `internal/proxy`, WAF recovery | [proxies.md](docs/agents/proxies.md) |
| The download manager: jobs, the queue, persistence, scheduling, names on disk | [download.md](docs/agents/download.md) |
| The transfer engine: retries, stalls, multiple connections, host admission | [transfers.md](docs/agents/transfers.md) |
| Playlists, joining audio and subtitles, remuxing, yt-dlp | [media.md](docs/agents/media.md) |
| Configuration, the API and its event stream, logs, profiling, shutdown, headless mode | [server.md](docs/agents/server.md) |
| The web UI in `frontend/` | [frontend.md](docs/agents/frontend.md) |

Keep these guides current in the same commit as the code. They describe the
code as it is and why: state a rule with the reason it holds, not the story of
how it was found.
