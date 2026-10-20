# Contributing to HeapLeach

You can help with a clear bug report, a documentation correction, a UI
improvement or an extractor. Small, focused contributions are welcome.

[Get help](SUPPORT.md) · [Open an issue](https://github.com/JohanLindvall/HeapLeach/issues/new/choose) ·
[Code of conduct](CODE_OF_CONDUCT.md) · [Architecture](docs/architecture.md)

## Find a starting point

- Try the [quick start](README.md#quick-start) on your platform and fix any
  unclear instructions.
- Improve an error message or a keyboard interaction in the UI.
- Add a synthetic regression fixture for a reproducible parser bug.
- Check [open issues](https://github.com/JohanLindvall/HeapLeach/issues) for
  something you can reproduce. Describe your intended approach on the issue
  before starting a substantial change.

For a new feature or host, explain the use case first. For a small typo or
isolated bug fix, a contribution can go straight to review.

## Development setup

Run `make` commands from the repository root and `go` commands from `backend/`.
Toolchain versions are defined in [Dockerfile](Dockerfile) and
[backend/go.mod](backend/go.mod).

| Workflow | Requirements | Command |
| --- | --- | --- |
| Build a Linux executable with the UI embedded | Git, GNU Make, Docker with BuildKit | `make build` |
| Build and run the Linux container | Git, GNU Make, Docker | `make run-image` |
| Build for your own OS | Git, GNU Make, Go, a C compiler, Node.js 24 or Docker for the UI | `make native` |
| Develop with frontend hot reload | Git, GNU Make, Go, a C compiler, Node.js 24 and npm | `make dev` after the setup below |
| Run all local checks | Go, a C compiler, GNU Make, Bash, Node.js 24 or Docker, and OCR assets | `make check` |

Go automatically downloads the version in `go.mod` when needed. Initial
builds need network access for dependencies and OCR assets. On Windows,
the Makefile needs a Unix-like shell and a cgo-capable C toolchain; the
prebuilt release or Docker image is the easiest way to try the application.

Clone your fork if you plan to submit a pull request, or clone the main
repository to explore:

```bash
git clone https://github.com/JohanLindvall/HeapLeach.git
cd HeapLeach
```

For a Linux build without local Go or Node.js:

```bash
make build
./bin/heapleach
```

For development with hot reload:

```bash
cd frontend
npm ci
cd ..
make ocr-assets
make dev
```

Open **http://localhost:5173**. Vite proxies API requests to the Go backend
on port 8080. Frontend edits reload automatically; restart `make dev` after
changing Go code. Ctrl-C stops both processes.

For a native build, run `make native` and launch the application in `bin/`.
The UI is compiled into the Go executable: use `make native` or `make build`
after frontend changes. A plain `go build` may embed old assets or the
checked-in placeholder.

Media helpers are optional for most development. On Linux, `make dependencies`
installs them in `bin/`. See [helpers and platform limits](docs/helpers.md)
for other systems and external-download workflows.

## Find the code you need

| Change | Starting point | Read first |
| --- | --- | --- |
| Web UI | `frontend/src/` | [Frontend guide](docs/agents/frontend.md) |
| Site support | `backend/internal/extractor/` | [Extractor guide](docs/agents/extractors.md) and the host notes it links |
| Queue and files on disk | `backend/internal/download/` | [Download guide](docs/agents/download.md) |
| Transfer speed, retry or resume | `backend/internal/download/` | [Transfer guide](docs/agents/transfers.md) |
| API, settings or CLI | `backend/internal/server/`, `config/`, `cli/`, `backend/cmd/heapleach/` | [Server guide](docs/agents/server.md) |
| CI or packaging | `.github/`, `Makefile`, `Dockerfile` | [Release guide](docs/agents/releases.md) |

[AGENTS.md](AGENTS.md) contains the shared invariants and the complete guide
index. These rules apply to human and automated contributions alike.

## Check your change

Use the checks relevant to your work while iterating:

```bash
make test             # Go tests with the race detector
make test-frontend    # TypeScript checks and Vitest; Docker fallback
make fmt             # Format Go sources
make hosts           # Regenerate the site inventory after registry changes
```

Before submitting code, run:

```bash
make check
```

This checks Go formatting, vet, the generated inventory, license headers,
Go and frontend tests, real-model OCR and release-tag logic. If a check
cannot run on your system, name it and the reason in your contribution;
do not report it as passing. CI also builds the UI and verifies packaged
applications on all five release platforms.

For documentation-only changes, check relative links, examples, spelling
and `git diff --check`; run `make hosts-check` if you edit the README.
Do not hand-edit the generated inventory between `BEGIN HOSTS` and `END HOSTS`.

Checked-in tests use synthetic fixtures and local HTTP servers. Live-site
tests are optional, gitignored `*live_test.go` files with the `live` build
tag; `make test-live` runs those present locally. A fresh clone contains no
live sources. See the [live-test guide](docs/agents/extractors.md#live-tests).

## Keep fixtures and reports shareable

Never commit specific content URLs, signed media links, real hosted
filenames, passwords, cookies or proxy credentials. Use reserved domains
such as `example.test`, placeholder IDs and invented filenames. Site roots,
API bases and documented URL shapes with placeholders are fine.

Scrub logs and screenshots before sharing them. For a host bug, the host
name, URL shape, error text and a synthetic reproducer are useful starting
points. Security problems belong in [private vulnerability reports](SECURITY.md).

## Submit a contribution

External contributors can fork the repository, make a focused change on a
branch in their fork, and open a pull request against `main`. Explain the
problem, the resulting behavior and the checks you ran. Add a screenshot
for a visible UI change, using invented sample data.

Maintainers and coding agents with write access work directly on `main`,
as specified in [AGENTS.md](AGENTS.md). A passing push automatically creates
a release; ordinary contributions do not need a version bump or manual tag.

Keep commit messages generic: describe the behavior, without site names,
domains, content URLs or real hosted filenames. Update the relevant area
guide in the same change when its rules or behavior change.

Follow existing formatting and package boundaries. Put shared tunables in
`internal/config/limits.go` and HTTP header constants in `internal/httpx`.
New source files carry `SPDX-License-Identifier: MIT`. Contributions are
made under the project's [MIT license](LICENSE).
