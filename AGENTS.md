# AGENTS.md

This file provides guidance to coding agents working in this repository.

## What this is

`HeapLeach` is a bulk downloader: a Go backend with per-host extractors and a
parallel worker pool, plus a React + TypeScript UI that is **compiled into
the Go binary**. Releases also carry an `ocr/` directory with the pinned
CAPTCHA model, native ONNX Runtime and license notices.

## Commands

All `make` targets run from the repo root; `go` commands run from `backend/`.

`make build` is incremental: `./bin/heapleach` is a real target whose
prerequisites are the Go and frontend sources, so asking for it when nothing
changed does nothing, and `make run` depends on it rather than on the file
merely existing — a binary that silently predates your edits is exactly what
looks like a bug in the code you just wrote.

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
make dependencies   # install yt-dlp, ffmpeg, deno and local OCR into ./bin
make ocr-assets     # install the pinned OCR model and native runtime into ./bin/ocr
make dist           # package this host (RELEASE_TARGETS + CC for cross-compilation)
make tag            # choose a version manually: make tag V=v1.2.3 (main pushes bump the patch)
make help           # every target
```

Go work:

```bash
cd backend
go test ./... -race           # or: make test
go vet ./... && go vet -tags live ./...
gofmt -w ./internal ./cmd

go test ./internal/download/ -run TestSafeName -v          # single test
go test ./internal/download/ -run 'Enqueue|DoubleRuns' -v   # subset
```

Frontend tests are vitest over formatting, progress, filtering, snapshot
merging, API errors and connection recovery, plus jsdom submission tests.
`make test-frontend` type-checks
and runs them, in Docker when npm is absent; CI runs them after the build.
The formatBytes cases mirror
`internal/cli/cli_test.go` digit for digit on purpose: the two sides render
the same numbers, and the paired tables are what hold them together.

Node is **not** installed on this machine — the frontend builds via Docker.
`make build` and `make frontend` both handle that automatically.

Toolchain versions live in the Dockerfile (Go 1.27 on Debian bookworm, Node 24
LTS on Alpine for the UI, Debian trixie-slim for the runtime image, which carries
the glibc-linked helpers and complete OCR data beside the binary)
and in `backend/go.mod`. The local Go is older than the `go.mod` directive, so
`go` commands auto-download the pinned toolchain on first use.

### Committing

Work goes straight to `main`: commit and push there rather than opening a
branch or a pull request, and without asking first. That is this repository's
settled workflow, not an oversight.

Keep site names and domains out of commit messages. Describe the changed
behavior in generic terms instead.

### Releases

Every push to `main` that passes CI calls `.github/workflows/release.yml`,
which creates an annotated tag for that exact commit by incrementing the
highest stable tag's patch version. Publication is serialized, retries reuse
the current tag, and late CI runs for commits already in a newer release
skip publishing. PR runs perform checks only. The caller invokes the
publisher directly: tags pushed with `GITHUB_TOKEN` do not trigger another
workflow. `.github/scripts/release-tag.sh` handles version selection, tested
against a temporary Git remote by `make test-release`.

Native archive builds and OCR model tests run on all five platforms alongside
the main checks; all gate publication. The caller passes its tested SHA, and
the publisher verifies the checkout against it before
reusing those results. Standalone tag pushes still run the Go suite. Release
compilation has a separate cache keyed by toolchain/dependencies and commit,
restoring the previous commit's output and saving the new one. Sharing CI's
immutable `go.sum` cache left every cross-platform build cold on every push.

Dependabot (`.github/dependabot.yml`) proposes the Go modules' updates every
Monday at 05:00 Stockholm time, a quiet hour, grouped into one pull request,
and the pull request lands by itself.
Once its CI passes, `dependabot-merge.yml` squashes it into main. A merge made
with the workflow's own token starts no workflow, so it then starts CI on main
itself (`workflow_dispatch`, which CI's release job accepts as it does a
push). That run releases it. Only Dependabot's own pull requests are merged,
only at the commit CI passed, and nothing from them is checked out or run
with that token. Each merge is a release and a restart, which is why the
updates come as one a week rather than one per module. `onnxruntime_go` is
left out: it must match the native runtime that `helpers/captcha/install.sh`
pins, so the two move together by hand.

`make tag V=v1.2.3` still writes and pushes an explicit annotated tag;
the release workflow also fires on `v*`. Native runners build the five
archives, then the publisher uploads them alongside `SHA256SUMS`. The image
job waits for publication, then pushes the runtime image for amd64 and arm64 to
`ghcr.io/johanlindvall/heapleach` as `vX.Y.Z` and `X.Y.Z` (the git tag's
spelling and Docker's), `vX.Y` and `X.Y`, and `latest`. The image job downloads
the published Linux archives, verifies their checksums and amd64 version, and
selects Docker's `released-binary` stage with `BINARY_STAGE`. Thus both release
formats carry the same executable and UI. Local image builds default to the
`backend` stage: the UI and Go stages run on the builder's platform
(`--platform=$BUILDPLATFORM`) and Go uses a target C compiler for cross-builds.
QEMU emulates the runtime package install, so an arm64 image needs binfmt
locally, supplied by the QEMU setup step in CI. The tag target refuses a dirty
tree, which is what makes step 3 below necessary rather than tidy.

Worth doing for release-related changes and manual tags, because each step
has caught something:

1. `gh run list` — CI green on the **exact commit** being tagged, not merely
   somewhere on the branch. Check who authored anything unfamiliar in
   `git log <last tag>..main` rather than assuming it is yours. Read the
   plain list rather than filtering: `gh run list --commit <sha>` prints
   nothing when it matches nothing, which looks exactly like a commit CI
   never ran. An empty result is not evidence of a green build. Automatic
   releases enforce this through the CI job dependency.
2. `make dist`, then extract the linux/amd64 archive, run `-version` and
   `sha256sum -c`. This is a rehearsal on the host, so its binaries are
   stamped `<last tag>-N-g<sha>-dirty` — that is `git describe` before the
   tag exists, not a fault.
3. `make frontend-clean && rm -rf dist` — restores the embed placeholder and
   clears the staging, so `make tag`'s clean-tree check passes honestly
   instead of being bypassed.
4. Once the workflow finishes, download the **published** artifact and check
   its checksum and `-version`. Verifying the local rehearsal proves nothing
   about what CI actually uploaded. Where a release exists for one
   identifiable change, grep the binary for it — the embedded UI is not
   compressed, so `grep -c scrollbar-gutter heapleach` settles whether the
   fix actually shipped instead of trusting that the build picked it up.

`make build` never writes into the host's `backend/internal/webui/dist` — the
UI is built inside the container and only `bin/heapleach` is exported — so a
build cannot disturb the tracked placeholder. Release notes are GitHub's
auto-generated changelog link; the `tag` target authors no body.

### No live URLs or real filenames in the repository

Nothing tracked may contain a link to a specific piece of content — no
video pages, albums, profiles, threads or signed media URLs — **nor the name
of a real file on any of these hosts**. That second half is the one that gets
forgotten: a fixture built by viewing a live page tends to arrive with the
page's own filenames still in it, and a test asserting on a real release or
track name is the same mistake wearing different clothes. Both are as stale,
and as much a catalogue of somebody's content, as a link would be.

This applies to code, tests, README.md, this file, and commit messages alike.

Invent the names. A fixture proves the parser reads `data-title`; it does not
matter whose title it is, and `First Song` tests the code exactly as well as
a real one does. Where the *shape* is what is under test — typographic quotes
around a release, an apostrophe inside a word — invent something with that
shape rather than reaching for the example that prompted it.

What is fine, and necessary:

- site roots and API bases (`https://api.gofile.io`, `https://kemono.cr`)
- documented URL shapes with placeholders (`https://gofile.io/d/<code>`)
- URLs the code builds at runtime from an id
- `example.test` and similar reserved domains in fixtures

Where a test needs realistic data — the KVS permutation in
`kvs_test.go`, say — use a **synthetic fixture**. The algorithm is what is
under test and does not care whose host a path belongs to; real-world
correctness belongs in the live tests. Generate the expected value with the
implementation once, then pin it.

`.gitignore` carries `*live_test.go` to keep the live tests out — which is
also where a URL or filename taken from a real page belongs, since those
files never reach the repository. Before committing, it is worth checking
that nothing new slipped in:

```bash
git ls-files -z | xargs -0 grep -nE 'gofile\.io/d/[A-Za-z0-9]|thisvid\.com/videos/|youtube\.com/watch\?v=|mega\.nz/(file|folder)/[A-Za-z0-9_-]{4}'
```

### Live tests

Extractor tests that hit the real sites sit behind a build tag, because
these hosts change their plumbing without warning. They live in one
untracked file, `internal/extractor/live_test.go`, so a fresh clone will not
have them — write what you need and leave it untracked:

```bash
make test-live                                              # all hosts
cd backend && go test -tags live ./internal/extractor/ -run 'TestLiveExtract/gofile' -v
```

Run these after touching anything under `internal/extractor/`. They are the
fastest way to find out which host broke.

Two conventions worth keeping to when writing one:

- A check that only calls `Extract` proves the API answered, not that the
  file is reachable. Fetch a range of the first file as well and look at the
  content type — gofile serves the site's HTML shell when a storage server
  is busy, with a `Content-Length` that agrees, and that records as a
  complete download. A `206` also confirms the host honours ranges, so the
  segmented engine will work on it.
- Take the URL, and any password, from the environment rather than writing
  it into the file. `*live_test.go` is gitignored so a literal would not be
  committed either way, but a file free of it can be pasted into a report or
  handed to someone else without leaking anything.

## Architecture

Packages depend one way only, enforced by convention:

```
config, util  (no internal deps)
  └─ httpx  └─ extractor, proxy  └─ download  └─ server, cli
```

`webui` stands alone and only carries the embedded assets.

### The single-binary coupling

Vite's `outDir` points **directly into the Go package** that embeds it
(`frontend/vite.config.ts` → `backend/internal/webui/dist`), and
`webui/embed.go` uses `//go:embed all:dist`.

Consequences worth knowing before editing either side:

- A checked-in placeholder `dist/index.html` keeps `go build` working before
  any frontend build. `go:embed` fails outright on a missing directory, so
  **never delete `dist/` without restoring the placeholder** — `make clean`
  does this via its `frontend-clean` target.
- `webui.Built()` distinguishes the placeholder from a real build; the server
  logs a warning when only the placeholder is embedded.
- A plain `cd backend && go build` embeds whatever is in `dist/` right now,
  which may be stale. Use `make build` or `make native`.

### HTTP client

`httpx.Client.Do` retries with backoff; `DoOnce` does not. One status is
handled apart from the rest: **429 is waited out, not counted as a failure.**
A rate limit is the server saying *later*, not *no*, so `Do` honours
`Retry-After` when the response states one and otherwise backs off from
`config.RateLimitRetryBase` towards `RateLimitRetryMax`, then decrements the
attempt counter so waiting never spends the retry budget. It gives up after
`config.RateLimitRetries` waits. This matters most to paginated listings,
where reading a 429 as the end of the results silently truncates a job
instead of failing it.

**A short certificate chain is completed, not refused** (`aia.go`). Some
servers send only their own certificate and leave out the intermediate that
links it to a trusted root — vids.st does — which browsers complete by
fetching the issuer from the certificate's own AIA address, and Go and curl
do not. Both TLS paths (the standard transport and the uTLS dial) therefore
set `InsecureSkipVerify` and verify in `VerifyConnection` through
`chains.verify`: crypto/x509's own check against the system roots, hostname
and validity, run on every connection, and only on an unknown-authority
failure the issuer fetched (http/https only, three hops, 64 KiB, cached per
address) and the check run again. A fetched intermediate completes a chain
only if it really signed the certificate and leads to a trusted root.
`TestBothTLSPathsVerifyOnTheirOwn` holds each path to refusing a chain that
leads nowhere — keep it, since a regression there would accept anything.

Two traps that cost time here:

- `retryAfter` returns `(time.Duration, bool)`. It has to separate "the
  header says 0" from "there is no header", because a stated delay is
  authoritative while an absent one means back off. Returning a bare
  duration made every fixture sit out the full backoff.
- Tests whose fixtures serve a **permanent** 429 now sit out that patience
  too, and burn the whole budget unless the fixture states `Retry-After: 0`.
  A suite that suddenly takes a minute instead of three seconds is this, not
  a hang.

Proxy connection and response-header timeouts both penalise the route and
cool its endpoint across services. An unanswered explicit proxy connection
also returns `httpx.ProxyConnectError`, so it does not spend the file's retry
budget. Go's client timeout otherwise hides the underlying dial/CONNECT
error behind "request canceled while waiting for connection". A CONNECT
answered with anything but 200 returns `httpx.ProxyRefusedError` with the
status, from `OnProxyConnectResponse`, which net/http calls before its own
check; left to net/http it is the status text alone ("Bad Request"), which
says neither that a proxy answered nor what it said. A 502 or 504 refusal
is scored as the route refusing that service, not as a dead proxy
(`reachesNoHost`): the proxy works, and cannot reach that host. Scored as
dead, one CDN out of public proxies' reach shelved them for Keep2Share as
well. Every other refusal is the proxy's own and sets it aside for every
service: a Tor port's "501 Tor is not an HTTP Proxy", a server that knows
no CONNECT ("503 unknown method"), a plain 500. For a while every 5xx was
taken for the host's. Since the exit probe scores nothing but a success or a
dead connection, those routes were never set aside at all. They came back
after a minute's rest, fourteen thousand times in four hours, and took
nearly every extra stream's turn. Request traces
reset on each redirect; a CONNECT reply or completed SOCKS negotiation ends
proxy setup, so later TLS and response timeouts retain their bounded retry
budget. Caller cancellation and caller deadlines are never proxy penalties.

### WAF recovery

Cloudflare's `cf-mitigated: challenge` header becomes `httpx.ChallengeError`,
including HTTP 200 challenge pages. `CaptureChallenges` keeps platform probes
from swallowing it and stops further requests in that extraction attempt.
The manager installs `extractor.WithChallengeRecovery` for source extraction;
the registry reaches nested sources with the same hook. A recovery reruns the
whole extraction on one lease. `File.Refresh` re-reads the smallest known
source on the worker's route, matching a file by name and directory rather
than by its position in a listing, before minting fresh media URLs. External
downloaders own their network requests and cannot use this native recovery.

Any native host can enter the `cloudflare` proxy group after a challenge.
Existing per-route services retain their group and address limits, adding
`Pace.WAF` to route selection. `entry.WAF` stores challenge outcomes separately:
a WAF failure changes no ordinary service counters, speed, transport health
or cooldown. Recovery combines that penalty with ordinary reliability and
unthrottled speed measurements; known free-host caps are not shared as an
estimate of unrestricted capacity. A challenge through a route costs the
file or source nothing (`routeAtFault`): finding an address the host does
not challenge is what recovery is for, and each challenged route is held
back by its own WAF record. Other failures use the existing proxy budget;
source lease acquisition is cancelable and bounded by the request timeout.

### Extractors

`extractor.Extractor` is `Name/Match/Extract`. `Registry.Find` walks the
registered hosts and falls back to `Direct`, which matches everything — so it
**never returns nil**. Register new hosts in `NewRegistry`.

The supported-site table in README.md is **generated** from the registry by
`make hosts`, between two markers, and `make hosts-check` fails CI when the
file and the code disagree — so edit the extractor, never that table. Adding
a host is then one line in `NewRegistry` plus `make hosts` in the same
commit. Note the optional half of the contract, `SiteLister`. An extractor
that claims a plain list of domains embeds a `hostSet` (`hosts.go`): one
declaration that answers both `Match` and `Sites`, so the inventory cannot
disagree with the matcher, and `catalogue_test.go` checks that every host so
declared routes back to the extractor declaring it. Only an extractor whose
`Match` is more than a host list — a section of a domain, a wildcard TLD, a
document shape — writes `Sites()` by hand, in `catalogue.go`; and one that
skips it altogether must be named for its domain, since the catalogue then
uses the name as the site. `hosts-check` regenerates into a copy and
compares it with README.md as it stands, so it judges the working tree
whatever else is in flight there; in CI the tree is the commit, which is
what makes a regeneration that was never committed fail the build.

**bandzoogle** is the third platform family, and the first with an empty host
list: it is website software musicians rent, so every install answers on its
owner's own domain and there is nothing to compile in. It is reached entirely
through the direct-fallback sniff, with `HEAPLEACH_EXTRA_HOSTS`'s
`bandzoogle` family for naming one outright. The marker is `data-zoogle-track`
on the track anchors, which the player script binds to rather than a theme
supplying, and the same anchor carries the title, the artist and the audio
path — so nothing is read from the surrounding markup, which differs from one
band's site to the next. The audio path redirects to storage signed for a
couple of hours, minted per request, so the player link is what goes on the
File; the signed one would go stale in a long queue.

The important subtlety is `File.Resolve`:

- Most hosts set `File.URL` at extraction time.
- Hosts that mint **short-lived signed URLs** (bunkr, turbo — roughly 20
  minutes) leave `URL` empty and set `Resolve` instead. The downloader calls
  it immediately before transferring, and again before each retry. Resolving
  those eagerly would mean a long queue starts failing partway down.
- That same per-attempt cadence is what `Mirrored` (`mirror.go`) is built
  on: a file offered from several equivalent links gets a resolver that
  hands out the next one each time. Nothing tracks which mirror failed —
  distinguishing a dead mirror from a cancelled job, a paused queue and a
  stall, without ever leaving a file with no source left, buys nothing over
  rotating blindly. `MirrorHosts` builds the list when the alternatives
  differ only by host, which is how gofile's storage servers are expressed.
- A resolver whose links are bound to the address that obtained them sets
  `Target.Discard`. The downloader calls it when the host refuses the link
  for another address (409), so that the next resolve mints a new one
  rather than handing back what is cached. See keep2share below.
- **bunkr cannot use it, because bunkr does not mirror.** Its metadata
  endpoint (`_001_v2`) names exactly one storage server per file in a plain
  `mediafiles` string — never a list — and the signed path is scoped to that
  one server: a live probe against a file on `no5.scdn.st` got 404 from
  `no1`–`no4` (the file is genuinely not there) and NXDOMAIN from `no6`+ (the
  hosts do not exist). So there is no alternate to rotate to, blind `noN`
  guessing only manufactures 404s, and a file whose one server is down fails
  rather than failing over — which is correct, since nowhere else has it. The
  resolver re-signs against that same host each attempt for the fresh token,
  not for a new location. To re-verify if bunkr's storage ever changes, POST
  `{"id": <fileID>}` to `<dlHost>/api/_001_v2` and read `mediafiles`: more
  than one server there would be the first sign rotation had become possible.

  A server that is *down* rather than missing is the case worth knowing,
  because bunkr's three steps disagree about it: the page still carries the
  file id, `_001_v2` still answers with a real storage path, and the signing
  service still mints a token — only the storage host knows, and it says so
  with a bare nginx 403 twenty minutes later, once per file in the album.
  The file page has already said it in words, so `bunkrUnavailable` reads it
  there and the item fails at resolve time instead. What is read is the
  download control being present and **disabled**, not any particular
  sentence: on a servable page that control is the link to the download host
  that `pageInfo` already parses, and on a refused one it is a button with
  `disabled` and a title saying why. The wording is then passed through
  untouched, since which server is down and for how long is bunkr's to say.
  The state is per storage server and an album spans several, so it is read
  per file — which is also the only place bunkr publishes it. Album listing
  is untouched: it reads names and sizes off the cards and never opens a
  file page.

`File.Check` asks about the file itself before the downloader spends a
route on it, over the caller's own connection (`transferRouted`, ahead of
the exit probe). An error fails the file; a question that cannot be answered
returns nil and leaves the deciding to `Resolve`. Keep2Share asks there
whether the site still offers the file free.

`File.Cipher` is the other addition to the contract: a host that serves
ciphertext sets it, and the downloader decrypts on the way to disk. Only
mega does today, and see the download-manager section for why the mode
matters.

A resolver that is told "later" rather than "no" returns
`extractor.WaitError` with the time to come back, and does not sleep it out.
`deferWaitLocked` puts the item back in the queue as `queued`, which the UI
shows as Waiting, `nextLocked` passes over it until then (`Item.notBefore`),
and a timer wakes the dispatcher when the time comes. The note's time left
is rendered per snapshot (`itemNoteLocked`), rounded up to the minute so the
row changes once a minute, not every frame. Sleeping in the resolver was
how a Keep2Share file read "Downloading" for an hour in which nothing moved,
while holding a worker the rest of the queue could have used.

A route that failed a file is deferred the same way, and says as little about
the file. So a file whose part file holds bytes (`startedOnDisk`, statted by
`runItem` outside `mu` through `os.Root`) goes back at the front of the queue,
not the back, and the next route that frees up finishes what is started. At
the back, one 184 MB into a 2.9 GB transfer waited behind 270 others for each
new route: twenty-two tries in its first hour, none on a route that worked.
The part file is the evidence because nothing else is: a job re-read after a
restart has items that have never reached the transfer, as that one had not.
`partNames` names the part files for both, so they cannot drift apart. It
cannot spin: `nextLocked` leases a route before starting an item, and passes
over one it cannot. Stalls still go to the back, on purpose (see
`deferStalledLocked`).

The `Direct` fallback checks ordinary HTML pages for every registered host's
links before using a generic embedded player. `filelinks.go` shares the
`links:` harvester's bounded expansion, but never follows links assigned to
the generic fallback: a plain HTML navigation link cannot start another
scan. Named hosts and recognised platforms retain their own extractors.
K2S/FileBoom links are deduplicated by service and file ID across aliases;
other links keep their full URLs, including Mega keys. The fetch checks HTML
headers before reading a bounded body, and supports extensionless pages and
common HTML/script extensions. Supported links that all fail are reported as
an error rather than saved as an HTML shell. Partial results report counts.

Unregistered platform installs are identified in that fallback too.
`platform.go` supplies bounded, single-attempt JSON probes and page-marker
detection. Pixeldrain file/list/filesystem metadata, booru original-media
records, FoolFuuka archive records, Kemono creator profiles and MediaWiki
siteinfo select the existing extractors at the pasted URL's origin. KVS
player signatures and asynchronous listing controls identify its pages,
including members and searches; a generic videos container alone does not.
MediaWiki's generator and EditURI link also locate custom API directories.
Recognised page content is tried before guessed APIs, so an ordinary blog
post with an embedded album does not probe unrelated services.

Keep these broad URL shapes out of `Registry.Known`: its answer controls
which links the ordinary page scanner follows. Host registrations still
provide that discovery boundary, the generated inventory, canonical aliases
and per-site settings (including booru filters). Dropping a registration
because direct input now autodetects would silently lose embedded-link
discovery. Positive identification commits to an extractor, whose errors
must be returned rather than falling back to saving the page shell. The
fediverse's existing `/u/` and `/post/` path matches overlap two of these
platforms; if nodeinfo does not identify the site, their APIs get a turn.

**DarkGram albums** are recognised in that same fetched HTML, by the player's
`darkgram-album` class and JSON `data-album` attribute. The first player supplies
its own videos (`hls_url`) and full-size photos (`photo_url`); thumbnails and
other players are not album items. Discovery makes no media requests. Each
video resolves its playlist through `File.Resolve`, refreshing segments and
keys per attempt and preserving the final page URL as referer. File names keep
the original item positions when missing entries or file caps shorten an album.

**WordPress categories** are recognised in that same fetched document, before
the ordinary link scan, rather than claimed by domain. `wordpress.go` reads
classic post permalinks or block-theme query loops, follows the category's
own next control, and opens only the listed posts. The category heading and
pager may sit outside `main`, so the primary content area is kept around it.
Post content supplies supported host links, embedded media and JPEG images;
original-image links and metadata outrank thumbnails. Registered extractors
still resolve the host links, retaining their deferred resolvers and pacing.
Without those links, the same album and player parsers used by the generic
fallback read the post's content and title, keeping sidebar players and head
metadata out. DarkGram videos retain their deferred playlist resolvers; player
artwork is excluded, and independent JPEGs are kept without duplicates. The
shared source expansion also accepts a post-reading function, so folder names,
concurrency and caps use the existing implementation. No host-specific
WordPress category scraper is maintained beside this one. Tests use invented
classic/block-theme pages; saved browser archives stay untracked and can be
parsed offline without contacting their source sites.

Host-specific notes:

- **filejump** public shares use `/s/<id>`, with `?access=download` on some
  pasted links. The original filename is in the share's `.title`; `.meta`
  rounds the size and includes thousands separators, so strip commas and
  mark it `SizeApprox`. Keep the matching `/s/<id>/download` button URL,
  which redirects to freshly signed storage at request time. The native
  downloader follows it and supports ranges; no eager signing or external
  helper is needed. A player preview alone is not a download permission:
  require the public download button and reject unavailable shares.
- **doodstream** includes the `d000d.com` and `playmogo.com` aliases. An
  ordinary page embedding one is reached by the generic link scanner; its
  player goes through the existing native token resolver and range engine.
  This is how linked players work without a dedicated extractor for every
  blog embedding them. Cloudflare challenges use the shared WAF recovery
  path; Doodstream has no special proxy registration. Its own Turnstile
  player gate returns HTTP 200 without `cf-mitigated`: the `captcha-player`
  containing `turnstile-container`, without a media token, identifies it.
  `httpx.GetPage` checks that HTML before challenge capture and route scoring,
  so a challenged proxy is retried and is not credited with a successful
  transfer. A background Cloudflare script alone is not a gate. Cloudflare's
  interstitial is also recognised by its `_cf_chl_opt` script loading
  `orchestrate/chl_page`, if its usual header is absent. Check this before
  accepting a token path: a challenge can echo that path in its options.
  Debug diagnostics record response status/type, final player URL, route,
  attempt and gate
  markers, keeping raw HTML, cookies and successful media tokens out of the
  logs. Both source discovery and deferred refreshes inherit the job logger.
  The final page URL after redirects supplies both the token endpoint's origin
  and the media referer, refreshed together on each attempt. The token endpoint
  can also answer `RELOAD`, which the player handles with `location.reload()`.
  Repeat the whole page/token exchange on the same route, keeping its cookies
  and re-reading the token path from the final page. The shared extraction
  retry budget bounds this, with cancelable backoff; persistent `RELOAD`
  becomes a transient failure. Doodstream opts sources and files into
  `ProxyFallback`, so temporary network/HTTP failures and persistent reloads
  try the shared recovery pool after the normal connection fails. The
  manager classifies errors and owns reservations, penalties and random
  unmeasured draws; a missing video or parser error cannot start a proxy
  search. Source recovery exempts unreachable/dropped/refused proxies from
  the retry budget just as file workers do, while other temporary failures
  remain bounded. A challenge still records its separate WAF penalty.
  Some addresses are refused outright, and that is not a missing video
  either: the player is answered, with a 200 and no gate, by one bare
  sentence saying the resource "has been removed or is temporarily
  unavailable", for a video every other address is shown and for one that
  does not exist alike. A video that really has gone is shown the host's own
  HTML "Video not found" page. So `doodRefused` reads the missing markup, not
  the sentence, and returns a `RefusedError`: the route is held back for the
  site and the file tries another, outside the retry budget. Taken at its
  word, it failed the file on the first address that met it.
  That not-found page is recognised by its title (`doodGone`) and reported as
  the host having removed the video. Read as a page without a token, it was
  "no media token (the video may have been removed, or the page layout
  changed)", which an hour of one site's posts produced forty-four times for
  forty-four removed videos.
  `ResolveNote` reports player/token stages. Recovery adds the connection
  attempt during source discovery; workers use the existing item counter.
  `JobView.Note` carries source progress to the browser and terminal, guarded
  by the source generation and cleared from the view after resolving ends.
- **voyeurking** is an index of K2S and FileBoom files. Category and collection
  pages expose their video links and `rel="next"` pager in HTML. Pagination stays within the
  same listing type and slug. A video's file link lives in the
  flattened React Router data table. Read only the primary route's
  `video.file`: the same table also contains related videos and player
  assets. Expansion goes through the registry so the host's deferred
  resolver and per-route pacing reach the downloader intact. The listing
  page itself stays on the ordinary HTTP client; only K2S and FileBoom
  transfers use the configured proxy pool.
- **keep2share** uses the public `/api/v2` free-download flow, on both
  `k2s.cc` and `keep2share.cc`. Metadata does not need a CAPTCHA; the
  resolver requests one, reads it with the in-process CAPTCHA model, and waits
  for the API's timer before redeeming the accepted key.
  That timer is usually half a minute and is sat out in place, holding the
  key. The host's wait *between* free downloads, given when there is no key,
  is the best part of an hour and covers every file from the address. So it
  goes back to the queue as a `WaitError`, and is kept on the `Keep2Share`
  value so other files return the same wait without solving a CAPTCHA to be
  told it. That same wait also arrives on a ticket (a `time_wait` of 4300
  has been seen, where a fresh address gets 30), so a ticket timer longer
  than `Keep2ShareInPlaceWait` goes back to the queue too, keeping its key
  for the same route.

  "Download is not available" (errorCode 42) with no reason given is about
  the address, not the file. Asked for one file through twenty-two public
  proxies, six got exactly that answer while sixteen got tickets. The
  resolver returns it as `RefusedError`. Through a proxy route,
  `transferRouted` then holds the route back from the service with the
  ordinary refusal backoff and retries the file through another route. The
  API's answer is an HTTP 406 the route layer counts as a success, so
  nothing else would. On the direct route it is a failure saying so.

  Except that a file the site keeps for premium users gets the same bare
  refusal, from every address. The older API's `getFileStatus` went on
  calling such files free, while the site's own download dialog said "Free
  download: unavailable" from the newer API its pages use
  (`api.k2s.cc/v1/files/<id>`, `isAvailableForFree`). Read as refusals of
  the addresses, one job of such files spent a CAPTCHA on every route it met
  and set each aside for an hour, until 59,690 of 62,945 routes were cooling.
  Three hundred routes, 170 distinct exits and the home address among them,
  were all refused before any CAPTCHA was asked. In that sample only files
  under about 500 MB were still free. So `checkFree` asks the newer API, as
  the file's `File.Check` before any route is used and again on a bare
  refusal. A file the site keeps for premium users then fails at once, with
  no route penalty, and a site that cannot answer leaves the flow as it was.
  The question goes over this machine's own connection
  (`httpx.WithoutRoute`), since the answer is the same whoever asks. Asked
  through a route, it timed out on a failing proxy and a CAPTCHA was spent
  before the refusal came. And before the check moved ahead of the route,
  each such file first met the dead routes a feed is full of, five seconds
  apiece, one file at a time. errorCode 43 is that answer in
  the API's own words. 33 asks for a reCAPTCHA, and is the address's. Asking
  for a link without a CAPTCHA would tell the two refusals apart more
  cheaply, but for a free file it is answered "Invalid captcha code", and
  the site keeps a brute-force ban.

  "Traffic limit exceed" and "Download count files exceed" (detail 2 and 1,
  under errorCode 21 or 42) are the address's free-download limits, not the
  file's. For one such 6.5 GB file, seven of twelve public proxies answered
  with the traffic limit, while five and the direct address got tickets. Read
  as plain failures they ended 375 files of one job. `limitsAddress` now
  turns them into the route's wait (`Keep2ShareAddressLimitWait`, six hours,
  since the host says nothing of when the limit lifts). Through a proxy the
  route is set aside for that long and the file goes on through another; on
  the direct route it waits in the queue.

  With proxy routes, many free downloads reach a CAPTCHA at once. The shared
  `internal/captcha` pool bounds native sessions to the CAPTCHA concurrency
  (`HEAPLEACH_CAPTCHA_CONCURRENCY`, `-captcha-concurrency`, default four, at
  most 64; `captcha.SetConcurrency`, called by `main` before anything is
  read), each with two ONNX Runtime threads and spinning disabled, and its
  own copy of the 54 MB model. Two kept up with one file at a time; hundreds
  of files, each with extra streams on routes of their own, queued behind
  them. The budget
  starts after acquiring a slot. Caller cancellation stays cancellation;
  exhausting the OCR budget is `errKeep2ShareOCR`, so another image can be tried.
  Waiting, model loading and reading have separate progress notes. Network
  requests keep their own deadlines and proxy scoring.

  Sessions are reused and released after `Keep2ShareOCRIdle`. Native inference
  is terminated with per-run options on cancellation. Session construction
  cannot be interrupted, so a cancelled caller returns while the worker keeps
  its slot until construction finishes, then frees the unused session. This
  prevents repeated cancellations from creating unbounded native work.

  The ddddocr beta model is unchanged. Go matches Pillow's image conversion,
  contrast stretching and Lanczos resize, then runs the model through cgo.
  CTC decoding pools letter cases, enforces six characters and ranks readings
  jointly across the cropped and whole-image views. Synthetic fixtures pin
  both the preprocessing tensors and all five readings against the previous
  Python reader. `make test-captcha` tests real native inference and cancellation.

  Through a proxy route, a failure that the next attempt may well not meet
  is retried on the best route free rather than failing the file:
  `transientRouteFailure` covers an expired deadline, a timeout, a
  connection refused, reset or cut short, and `extractor.TransientError`,
  which a run of unread CAPTCHAs comes back as. Nothing is charged to the
  route for these, since the route layer has already scored what the
  network did. Those attempts spend the file's `ProxyRetries` like any route
  failure. A failure that is the proxy's does not (`routeAtFault`): a proxy
  that could not be reached at all (`deadProxy`: a `proxyconnect` or
  `socks connect` around a failed dial), a connection it dropped (a bare or
  unexpected EOF, a reset), an address the host refuses (`RefusedError`), or
  a proxy that answered and showed it cannot carry the request at all
  (`proxyUnusable`): a SOCKS proxy wanting credentials, a tunnel refused
  below 500, plain HTTP where TLS was expected (an `https://` proxy that is
  not one, or HTTP inside the tunnel), or a destination certificate that
  does not verify. The proxy's own TLS hop accepts any certificate, so that
  last one came through the tunnel: a certificate expired in 2021 was
  presented for a host whose own is current. Public lists are mostly such
  proxies, so twenty in a row used to fail the file with
  `proxyconnect tcp: dial tcp …: i/o timeout`, `unexpected EOF`, "free
  downloads are refused from this address", or one of those four, before it
  met one that worked. The pool already shelves each such route. The cost, accepted on
  purpose, is that a host refusing every proxy would have files trying
  routes for as long as the pool offers any, then waiting in the queue,
  rather than failing; each refusal also costs a CAPTCHA. Four more are the
  route's alone and cost the file nothing: a tunnel the proxy refuses at 502
  or 504 (it cannot reach the host from where it is), a TLS handshake that
  never completes through the tunnel, a tunnel answered in another protocol
  (a SOCKS proxy listed as an HTTP one, which net/http calls a malformed
  response), and a challenge from the host. Counted,
  three files failed in an hour on a CDN that other routes were serving,
  each after forty route attempts. A host down for every route leaves its
  files waiting for one, as for refusals. Other timeouts after proxy setup,
  HTTP refusals, stalls and a SOCKS "host unreachable" still count.

  The key survives cancellation within the process, and an unexpired
  storage URL is reused on retries: a new free transfer spends the host's
  hourly allowance even when only a range was fetched. `Pace.Group` keeps
  the one-file limit shared across aliases and storage hosts, and
  `WithResolveNote` carries the CAPTCHA/wait status to the existing item
  note. The model and runtime are installed by `make ocr-assets` (also part of
  `make dependencies`) and bundled in release archives and the Docker image.
  `helpers/captcha/install.sh` pins and verifies every download and includes
  upstream license notices. `HEAPLEACH_OCR_DIR` overrides the default `ocr/`
  directory alongside the executable. Official builds enable cgo; deliberately
  cgo-free source builds still run other hosts and explain that OCR is disabled.

  The storage link is bound to an address (`ip_access_policy=first`), and
  one route can present more than one. Both hosts are dual-stack, so a new
  connection can leave by the other address family, and a public proxy can
  leave by another of its provider's exits. A 2.9 GB file failed that way
  184 MB in: twenty minutes of serving, then `409 Conflict`, "assigned with
  another IP address", read as a client error and final. So the resolver
  hands each link out with `Target.Discard`, which drops the link and its
  ticket as a change of route does. The downloader calls it on a 409 for
  such a link. Through a proxy that is an address refusal (`RefusedError`):
  the route is held back for the service and the file goes to another,
  free, as with the API's own address refusals. On the direct route the
  next attempt mints a link from the address in use now, within the
  ordinary retry budget. Either way the part file carries what was fetched.
  A late discard of a link already replaced keeps the replacement.

  **fileboom** on `fboom.me` shares this implementation with its own
  `/api/v2` base, label and `Pace.Group`. The API namespaces are separate:
  a FileBoom file ID cannot be resolved through K2S. Each instance keeps its
  own tickets and address cooldowns. Proxy statistics and admission limits
  are per service too, with one transfer per address per service under the
  manager's shared live concurrency limit.

  API calls and CAPTCHA images use a client capped at
  `Keep2ShareRequestTimeout` (20 seconds), retaining a stricter configured
  timeout. A general one-minute timeout made every silent proxy occupy a
  worker for a minute before another route could try. Set this on the HTTP
  client, not a child request context: route accounting excludes caller
  cancellation, so a child deadline there would lose the proxy penalty.
  Media transfers still use their streaming client and stall watchdog; OCR
  and the host's ticket timer keep their own budgets. The item counts route
  attempts independently of its retry budget, including dead proxies that
  cost no retry, and appends the connection attempt to active notes so
  successive `requesting CAPTCHA (1/3)` attempts visibly differ.

  The connection to a proxy itself is bounded tighter still
  (`ProxyDialTimeout`, 5 seconds, set in `ThroughProxy`). Most routes in a
  public feed are dead and most dead ones never answer, so the dial timeout
  is what each costs a file. After a restart, 250 files spent nearly all
  their time in 15-second dials, at about 22 dead routes a second.

  Once a minute a goroutine of its own (`summarizeRoutes`) logs `route
  attempts` per service (`routeSummaryLocked`): how routed attempts ended
  (dead, shared, refused, waited, transient, stalled, failed, finished), and
  how many files are moving bytes or still trying. On the dispatcher's tick
  it went quiet exactly when wanted, with files failing routes as fast as
  they were started. A queue that is all dead routes or all refusals says
  so in one line.

  Each attempt also says how it ended (`logRouteAttempt`, `route attempt
  ended`): the route's address without credentials, its exit, the outcome,
  the cause, and what the file does next (another route, a wait, done or
  fail). Dead routes and taken exits are logged at debug level, since they
  can come twenty a second and say nothing about the host. Everything the
  host answered is logged at info. A file sent on to another route used to
  be logged as the host asking for a wait until that very moment, which
  named neither the route nor the cause. `deferWaitLocked` now says that for
  real waits alone, with their reason. The resolver logs the steps of the
  free flow: a CAPTCHA served, a wrong reading or a refusal (debug), and a
  ticket or link granted or a file kept for premium users (info).

  The helper prints ranked readings, not one answer. The challenge is six
  letters and digits compared without regard to case, and a wrong answer
  leaves it open, so `resolve` tries up to `Keep2ShareCaptchaGuesses`
  readings on one image before asking for another — and drops the rest
  the moment anything but a wrong answer comes back, since an accepted or
  expired challenge must not be answered again. Decoding the model's
  probabilities under those rules is what made the reader usable: its own
  best string dropped pale and hairline characters and came back short,
  which is how three images in a row could all be "unreadable". Two traps
  when checking answers against the live API: the file is checked before
  the CAPTCHA, so a made-up id is no oracle; and once a free download is
  pending for a file, `getUrl` hands out the link whatever the answer, so
  every guess against that file "succeeds".

  The persistent egress pool is enabled by default (`internal/proxy` adapts
  `github.com/JohanLindvall/proxytron/lease`, backed by bbolt), with
  `-proxies=false` or `HEAPLEACH_PROXIES=0` to disable it. The library owns
  discovery, scoring, leases, exit probing and claims, and their defaults.
  Application scope policy, health epoch, cached probe-outage policy and HTTP
  identity stay here.
  `Pace.PerRoute` marks the service's addressing rule;
  the manager allows the `keep2share`, `fileboom` and `cloudflare` groups to use proxies. The
  dispatcher leases a route before resolving and holds it through the
  transfer, one file per address per service across aliases and proxy protocols.
  The HTTP context pins CAPTCHA, ticket redemption, redirects and bytes to one
  isolated client. Changing routes invalidates an IP-bound ticket and cached
  target; a retry on the same healthy route may reuse them. Cooldowns belong
  to the service and route, never the entire service's queue. Transport failures
  return the item to the queue to try another route, with their own bounded retry
  budget. The score estimates useful bytes per second for the remaining
  file size, including reliability and observed setup delay. Unmeasured
  endpoints share one exploration candidate, drawn at random, first an
  address and then one of its ports, so discovery neither walks adjacent
  addresses in URL order nor spends itself on one gateway's thousands of
  ports. Selection and reservation share the pool lock, including for these
  unrated routes. The inventory lives beside the queue file wherever that
  is put (`defaultProxyDB`), and in the default state directory when no
  queue is kept. It used to sit beside the default queue path alone, so a
  container with only its configured state directory mounted forgot every
  route at every restart, and the next half hour went on rediscovering
  dead ones.

  **A route never tried for a service answers for itself alone**
  (`entry.untested`): what the other ports on its address are doing says
  nothing about it. Feeds list gateways of thousands of ports on one
  address, and those ports leave through public addresses of their own:
  two on one gateway came out at two different addresses, neither the
  gateway's. Held to its address, nearly every untested route in the
  inventory, 46,000 on 57 addresses, sat "busy" or "cooling" because one
  sibling port was leased or had met a host's limit. So an untested route
  is eligible unless it is dead, leased itself, or cooling itself, and its
  own lease still counts against its address, so the address's tried ports
  stay busy. Once tried, a route shares its address's leases and cooldowns
  like any other. Most gateway ports turn out dead, so they are tried and
  set aside: a dead proxy costs a file no retry, and an extra stream no
  backoff (`routeBroken`). `Lease.Cooldown` writes the host's timer on its
  own route alone, and tried ports pick it up where routes are chosen
  (`addressCooling`). Writing it on every port was a synced bbolt
  transaction per port under the pool's lock: thousands for one Keep2Share
  wait on a gateway, with the dispatcher waiting on that lock and holding
  the manager's.

  **Route health can be forgotten wholesale** (`config.ProxyHealthEpoch`).
  The database records the epoch its verdicts were reached under, and one
  opened under another has every route's health dropped, keeping the
  inventory: shelving, cooldowns, answers, speeds, exits and the priors
  (`forgetHealth`). Raise the epoch when a change means the recorded
  verdicts were reached wrongly. Epoch 1 forgot a day in which downloads
  could share an exit address, so that the host's refusals of the second
  download were held against routes that were fine, and in which every
  restart had started the inventory over. Epoch 2 forgot the refusals that
  were really files kept for premium users (see above). It has a cost: the
  dead routes are forgotten with the rest, and are found again by files,
  five seconds a route. For hours after epoch 2, 88% of attempts met dead
  proxies, and files read "Checking the download route — connection
  attempt 55". Raise it only when the verdicts of a whole kind are wrong.

  **No two downloads share an exit address** (`proxytron/lease/exit.go`). The address a
  host counts free downloads by is the one a route's requests leave from,
  and that need not be the one the proxy listens on. Of eleven live ports
  on one gateway, nine distinct exits answered, three ports on one of them;
  and three exits turned up behind more than one gateway. So no rule about
  listening addresses can keep two downloads off one exit. Leasing ports of
  untried gateways freely, about a thousand free downloads a minute were
  refused while one to three files moved. Before a routed attempt's first
  request to the host, `claimExit` delegates to `proxy.ExitProber`: it asks
  through the lease's existing client, bounded by `ProxyExitProbeTimeout`,
  and calls `Lease.ClaimExit` to claim the answer. There is one lease per
  exit per service, as for an address. A route whose exit is taken, or
  cooling for the service, is handed back at no cost to the file
  (`errExitInUse`), and selection passes over a route whose known exit is
  held or cooling (`exitFree`). The host's timers and refusals met through
  a route cool its exit, so every route known to leave by it waits too.
  Exits are kept on the routes (`Exit`, `ExitAt`, trusted for
  `ProxyExitTTL`), and exit cooldowns are seeded from them on load.

  The echo is also how a route shows it works, before a CAPTCHA is spent
  on it. One that cannot reach the echo service fails as it would have
  failed the host's first request. One that answers each echo service in
  turn (`ProxyExitProbeURL`, then `ProxyExitProbeFallbackURL`) with
  something other than an address, a page or a refusal of its own, is not
  carrying requests as a proxy should (`proxy.ErrExitProbeFailed`): it is
  shelved, and the file goes on at no cost. Unless the echo services are
  down over the normal connection too (`echoReachable`, asked at most once
  a `ProxyEchoRecheck`): then the exit stays unknown and the download goes
  ahead, since their outage must not shelve every route. Their answers are
  not scored as the host's (`proxy.ExitProber`): only the route's failure
  to carry the request counts against it there, or one rate-limited answer
  would have put the route on the refusal backoff. The shared prober retains
  the lease's client and browser identity, and `proxytron.Detector` parses
  bounded plain-text or JSON IP answers for both routed and normal checks.
  Cancellation stops both checks without caching an outage or penalizing
  the route. Extra streams check and claim their routes the same way.
  `Config.ProxyExitProbes` carries the URLs and is empty in a Config built
  by hand, so tests opt in.

  **Each route of a per-route service is one browser** (`clientAdapter.agent`,
  `httpx.WithRouteAgent`). It draws a User-Agent from
  `config.BrowserUserAgents`, over thirty real desktop Chrome, Edge,
  Firefox and Safari strings, when its client is made, and every request
  it carries sends that one, whichever client built the request: the
  CAPTCHA, the ticket, the echo and the bytes. Hundreds of addresses all
  naming one browser to a host counting free downloads per address is a
  signature of its own. WAF recovery keeps the client's User-Agent, since
  some hosts sign requests with it (gofile does).

  **An address refused outright waits an hour** (`ProxyAddressRefusedWait`),
  its exit with it. The host's bare "Download is not available" came to
  nearly every address at once one day, the direct connection's included,
  for every file asked, whatever the User-Agent and however long the
  CAPTCHA took to answer. On the minute-long refusal backoff each address
  asked again within minutes, after another CAPTCHA, about a thousand times
  a minute in all.

  Ranking never walks the inventory (`proxytron/lease/index.go`): a public feed's tens of
  thousands of routes, walked on every lease and every upgrade check, were
  most of the service's CPU. The index lists every route, the routes by
  address and by ID, and the routes with a speed on some site. It is rebuilt
  when routes are added or removed, and a route's first speed adds it where
  that speed is written, in `observe` and `Progress`, the only ways a speed
  leaves zero. Everything else is read from the route itself when it is
  considered, so a speed set anywhere else, as tests do, is missed unless set
  before the index is built. `eligible` asks of one route what `available`
  asks of all of them. An address's leases are a count in the index, kept by
  `lease` and `Release` and recounted on every rebuild. Its cooldown is
  found by walking its ports, since some providers list a thousand on one
  address, and an address found cooling is remembered until the cooldown it
  found (`addressCooling`). Production only ever lengthens a cooldown, so the
  memory can be wrong only in the safe direction. An address found free is
  never remembered, since a cooldown can start at any moment. Among
  crowded, mostly cooling addresses that took a lease from 21 to 7 µs. An
  upgrade rates measured routes only. The exploration ticket draws an
  address and then one of its ports, drawing again until the port
  qualifies, so each address is drawn in proportion to the share of its
  ports that could take the transfer. After a quarter of the library's draw limit
  misses it draws by route instead: once the single proxies have been
  tried, what qualifies is a sliver of a few gateways' ports, which an
  address drawn at random almost never is. Without that stage the
  dispatcher listed all 60,000 routes for nearly every file it started,
  under the manager's lock, at two and a half cores. After the library's draw limit
  misses more it lists the qualifying routes and draws an address and a
  port among them, for when few qualify. `leased` counts each route's leases,
  which is where selection finds an unmeasured transfer in progress. Tests
  hold each against a scan of the whole inventory.

  A route keeps its HTTP client between leases, except one shelved as dead
  or broken (`e.Until` ahead), which gives it up when its last lease ends.
  It will not be leased again for a while, a day for one that never
  worked, and route hunting tries thousands like it an hour: kept, their
  clients were the one part of the heap that grew without end. A lease holds
  the client it was taken with, so dropping the route's own never pulls one
  from a request in flight, and a later lease makes a new one.

  Five-second progress windows train throughput before a long
  file finishes, with the resumed prefix, pauses and speed caps excluded.
  Those samples never write Bolt from `sampleLocked`; the background task
  flushes changed rows. Bolt stores
  inventory and each health outcome immediately, while active leases remain
  in memory. The manager owns the pool, opens it lazily on enable, and closes
  it after workers finish. Explicit routes must never
  fall back to direct or disable destination certificate verification.
  An HTTPS proxy's outer TLS connection accepts self-signed certificates;
  `DialTLSContext` handles that hop alone. TLS inside CONNECT still uses the
  original certificate verification, tested with both transport modes.

  A slow, range-capable transfer can move to a proven faster route after
  sustained evidence that the remaining time saved covers setup costs.
  `Lease.Upgrade` reserves the replacement atomically. Only the attempt's
  child context is cancelled: `transferRouted` waits for the old response and
  part file to close, then changes leases and resumes. A user cancellation
  wins, and a settings change that invalidates the reservation resumes on
  the old lease. Unknown routes never preempt working transfers. Feed
  attempts are 24 hours apart unless runnable demand for either service
  exceeds distinct addresses with sufficient scores; early top-ups still wait five minutes.

  **A file can also read through several routes at once** (`multiroute.go`).
  A free link allows one connection, from the address that obtained it,
  and carries its own speed cap, so each extra stream leases a route of
  its own and runs a free download of its own there. The resolver keeps a
  ticket and link per route (`keep2ShareDownload.routes`), so one route's
  ticket timer holds up no other. Asked for one file through two routes at
  once, the API issued each its own link, and the second served a range
  64 MiB in. Extra streams take idle routes only: none starts while a file
  of the same service is due to start and not running (`routeWanted`, a
  queue scan kept for `RouteWantedRecheck`), and one still being obtained
  is given up. A file its job holds back is not due, for this or for the
  pool's demand: counted, one paused job kept every other file of the
  service from extra streams, and asked the feeds for routes. Nor is any
  file while the queue can start none, at its limit on files at once,
  paused or short of room. Such a file waits for a slot, not a route, and
  the transfer whose end frees the slot gives its route back with it.
  Counted, fifty files queued behind the limit kept 254 running ones on a
  stream apiece. The queue sets up only `RouteExtraAcquisitions` (64) extra
  streams at a time (`extraAcquiring`). That is no bound on how many run,
  which is files at once times streams per file. Each set-up is a hunt and
  a CAPTCHA, and the files' own routes need the reader first. Asking whether the dispatcher found a route was the first
  rule, and it stopped meaning anything once untried routes counted as
  free: the dispatcher nearly always finds one, mostly dead, and the files
  cycling through them would be racing extra streams for the few that
  work. A running extra keeps its route: its address has spent its free
  download, and a waiting file given it would only be told to come back in
  an hour. So this engages as a queue drains, for its last files.

  Two things here were each got wrong first. Opening the next range on a
  route while the host may still count the last one is a second
  connection, so every request asks for at most `RouteRangeSize` and is
  read to its end, and the next goes out on the same connection. The
  first request in `transferOnce` is bounded the same way, and
  `splitUnrequested` never cuts into a range already asked for: a split
  that did made the connection abandon its response and open another,
  which the test rig counts. And the main route's speed estimate is fed
  only its own bytes. `sampleLocked` subtracts `extraBytes`, each extra
  trains its own lease through `Lease.Progress`, and no upgrade runs while
  extras do, since aborting the attempt would end them too. An extra that
  gets nowhere has cost a CAPTCHA, so attempts back off towards
  `RouteExtraBackoffMax`. One whose route goes silent for the stall
  timeout is dropped alone, because the transfer's own watchdog waits for
  every connection to go quiet. A range left unread, by a stopped extra or
  a restored sidecar, is claimed by the next connection to finish its own;
  the main route reads one at a time.

  Proxy settings can change while workers run. `settingsMu` serializes
  preparation with shutdown, and a failing database open leaves the whole
  update unapplied. Disabling keeps the pool and its active leases alive;
  normal admission for each service waits for that service's transfers to drain.
  Enabling waits for its unleased direct transfer before the pool may lease
  that address. Both modes use `httpx.DirectRoute` for ticket and cooldown identity.
  Queued direct cooldowns are cleared on enable so new routes can run, while
  the resolver retains the address's actual timer. Source edits preserve
  active leases, and late results from removed feeds are discarded.
  Discovery demand shares the global worker budget across both services;
  `SetDemand` publishes their allocations atomically so an obsolete demand
  cannot trigger an early feed reload.
- **gofile** signs every API call with
  `sha256(userAgent :: language :: accountToken :: floor(unix/14400) :: secret)`
  sent as `X-Website-Token`. The user agent mixed into that hash **must** be
  the one actually sent, so signing reads `httpx.Client.UserAgent()` rather
  than a separate constant. Gofile rotates the secret server-side; it is
  configurable via `HEAPLEACH_GOFILE_SECRET`. Folders may be password
  protected — `Options.Password` exists for this host — and the gate refuses
  with `ErrPasswordRequired` rather than resolving a partial listing. Note
  that not every folder is offered from several storage servers: one that
  names a single server gets no resolver at all, so there is no mirror to
  rotate to and a busy server stops at `config.BusyRetryLimit`.
- **pixeldrain** counts connections rather than files. An anonymous caller
  is allowed only so many downloads open at once and the API says so
  outright, 403 with `max_concurrent_downloads` where the bytes would be —
  so the eight-way split spends that whole allowance on one file, and a
  queue of four is refused before it starts. `pixeldrainPace` therefore asks
  for `Streams: 1`, which costs nothing here because free downloads are
  speed-capped per account rather than per connection, and which makes the
  number of connections the number of transfers. `Files` is deliberately
  left unset: what the host allows is not published, so nothing guesses at
  it and the downloader learns the rest by being refused. nova.storage runs
  the same software and gets the same treatment for free.
- **mega** encrypts everything client-side, so `mega.go` cannot ask the API
  for a filename — it decrypts one. `megacrypt.go` holds the primitives
  (mega's base64, the key split, ECB key unwrapping, CBC attributes) and is
  kept apart from the API plumbing the way `kvs.go` is kept apart from its
  hosts. Two things are worth knowing: the `"MEGA"` prefix on decrypted
  attributes is what makes a wrong key recognisable, and it is also how a
  folder node's key is found — every candidate is tried and the one whose
  attributes carry the prefix is the right one. The per-file MAC is
  deliberately not verified; see the comment on `megaFileKey`.
- **Kernel Video Sharing** is a product, not a site. `kvs.go` holds the
  reusable parts (flashvars parsing, license-code permutation) and
  `kvssites.go` builds one extractor per known install, so each is matched
  and named like a hand-written host. Because a compiled-in list can only
  ever trail a platform sold to hundreds of sites, there are two escapes:
  the `kvs` family of `HEAPLEACH_EXTRA_HOSTS` adds installs at runtime (the
  older `HEAPLEACH_KVS_HOSTS` still works and folds into that family), and
  `kvsSniff` — called from the `Direct` fallback — recognises the
  `/videos/<id>/<slug>/` shape and tries the page before treating it as a
  file. A page that turns out not to be one falls through to what would have
  happened anyway.

  Installs vary more than one product suggests, and three differences have
  each looked like working code. The flashvars object is **scanned** rather
  than matched to a closing pattern: some installs put the brace on its own
  line, others pack the object onto one line and close it with `};` behind
  the last value, and a brace inside a quoted value must not end it early.
  A member listing's section is **kept, not rewritten** — an unknown member
  section is not a 404 here, it answers with a generic block of the site's
  newest videos under the member's own URL, which reads as a listing and
  downloads a stranger's work. And where an install pages the listing in
  script, leaving "next" pointing at `#`, `kvsAsyncPage` asks for the block
  directly; note that the `from` parameter this platform also accepts on a
  member listing is *ignored*, handing back page one forever, so the walk
  deduplicates and stops on a page that adds nothing either way.

  A search, `/search/<query>/`, is the same walk (`kvslisting.go`) from a
  different first page, and so is every other listing an install lays out:
  a category, a model, a tag, a channel, the site's own latest list. Those
  are not recognised by path — an install may rename its sections, and one
  keeps its models under `/onlyfans-models/` — but by shape: on a registered
  host a URL that is neither a profile nor a search is fetched once
  (`kvsBrowse`), a page with a player is a video, a page with no player but
  a videos block is walked as the listing it looks like, from the document
  already in hand, and the front page is refused outright. A trailing page
  number means the whole listing, the way a member's section means the
  member; a section whose own name is a number is caught by that, so a
  canonical listing that lists nothing is retried as pasted.

  The pager is read, never guessed at. Which parameter pages a block is the
  block's own business — `from` on a category, `from_videos` on a member's
  videos, `from_videos` and `from_albums` together on a search — and the
  platform does not refuse a wrong one, it ignores it and serves page one
  again, which reads as the end of a one-page listing. So `kvsAsyncNext`
  builds the block request from the "next" control's own `data-block-id`
  and `data-parameters`, exactly as the site's script does, and the older
  `from_videos` guess is the last resort for a control that carries none.
  Every install seen pages a search in script; the last page announces
  itself in the pager — the "next" item rendered with a span where the
  anchor was — which `kvsListingEnds` reads so the walk stops without
  asking for a page that is a 404. A search page also lists the albums
  matching the query, in a block with a pager of its own, so
  `kvsListingBlock` and `kvsPager` choose by name rather than by position;
  and `kvsNextPage` treats an anchor at `#` as no page at all, since
  following it resolves straight back to page one and ended the walk there.
  The query keeps the escaping it arrived with, because the site writes a
  plus as `%2B` and re-encoding a decoded segment would hand its routing a
  literal plus.

  Not every install scrambles. moannest signs its links instead — a plain
  `get_file` path with a `?v-acctoken=` that the path is refused without —
  and `kvsRealURL` passes anything without the `function/0/` prefix through
  untouched, so the link is kept whole, query and all. The token is stable
  across renders and carries no expiry; the storage host it redirects to
  mints its own two-hour token per request, which the downloader picks up
  by following the redirect, so the URL is stored directly like any other
  install's rather than resolved at download time.
- **vimeo** is read from the embed player's own configuration
  (`window.playerConfig` on `player.vimeo.com/video/<id>`), and that one door
  is what makes it work: `vimeo.com/<id>` answers a non-browser client with
  a bot check and the player's JSON config endpoint answers 403, while the
  player page is served to anyone with a browser's TLS. curl's handshake is
  turned away from it ("We couldn't verify the security of your
  connection"), the client's Chrome-shaped one is not. The configuration's
  HLS master, AVC first and on its default CDN, keeps video, audio and
  subtitles in playlists of their own, which `mux.go` joins; it used to go to
  yt-dlp for the muxing. Its links are signed for an hour, so `Resolve` reads
  the page again, holding to the kind of stream the first reading chose. The
  tallest progressive MP4 is taken only when the HLS cannot be read: modern
  uploads mostly have none. Some videos are DRM-protected in every form they
  are offered in, FairPlay HLS and Widevine DASH (`request.drm`), which
  yt-dlp cannot fetch either, and those are refused in those words. A `view`
  other than 1 is a private or password-protected video. An unlisted link's
  `<hash>` becomes `?h=`.
- **arte** reads ARTE's player API (`api.arte.tv/api/player/v2`), which
  answers anyone in each of the six languages the site is published in, the
  first path segment of every page. A programme's configuration names one
  HLS master for every version of it: each audio language, clear speech and
  audio description as renditions of their own, every subtitle set as
  WebVTT, the video in H.264 and HEVC, its parts byte ranges of one file per
  rendition, on links that are not signed. So one MP4 carries every version.
  It used to go to yt-dlp for the muxing. The configuration also says
  whether the programme is shown where it is asked from
  (`restriction.geoblocking`, `error`), and a refusal is passed on in
  ARTE's words. A collection (`RC-…`) is the playlist endpoint, which names
  each programme and its title. A season nested in a series is read into a
  folder of its own. ARTE shows some programmes of a collection in fewer
  countries than others and the listing does not say which, so each
  programme's configuration is read as the collection is (`FanOut`). One not
  shown here is left out, the note giving the reason once (`arteRefused`);
  one that could not be read for another cause is kept, for its transfer to
  try again. Their streams are resolved when the transfers start.
- **orf** reads ORF ON's own API (`api-tvthek.orf.at`) with the credentials
  the site hands every visitor: its runtime configuration
  (`window.__NUXT__.config`) names the API's base, user and password, and
  the site's player sends them as Basic authorisation. They are read off the
  page, never written down here, and read again when the API refuses them
  (a rotated build). An older public endpoint takes the episode id base64'd
  behind a fixed prefix; the site's own code uses another prefix for
  something else, which that endpoint does not take, so it is not used. An
  episode names one adaptive HLS master among fixed qualities, its DRM
  flag, the rights it is shown under, and its subtitles as files, WebVTT
  among them (a one-file `Track`). The master keeps its audio apart, the
  audio description one rendition. What ORF shows in Austria alone is not
  refused by the API but by the stream: asked from elsewhere, the master
  redirects to a notice (`insert_geoprotection`) that would download as a
  short clip of something else. It is recognised and refused in words, and a
  series' Austria-only episodes are left out once one has shown it. A series
  (`/sendereihe/<id>`, `profile/<id>/episodes`) lists newest first and is
  filed oldest first. Anything else on orf.at, its radio and news pages,
  still goes to yt-dlp through the `Handoff` the extractor keeps.
- **yle** reads Areena through the player API's preview
  (`player.api.yle.fi/v1/preview/<id>.json`), asked with the static
  player's app id and key, the pair Yle's own player script sends (a chunk
  `player-v2.yle.fi/embed.js` loads). Areena's pages publish another pair
  (`appIdFrontend`), for its UI API, which the preview refuses with a 403.
  The preview is in one of the states the player knows (`yleStates`); one
  that plays names its title and series in both languages, its region, and
  an HLS master in MPEG-TS with WebVTT subtitle renditions, or for audio one
  MP3 (`media_url`). Both are signed for some hours and minted again when
  the transfer starts. Three things here each looked like something else.
  `is_content_protected` is not DRM: the programmes it marks that are shown
  worldwide were plain HLS with no key anywhere, and refusing on it dropped
  21 of the 37 worldwide programmes on the front page. A stream that really
  is encrypted is the playlist reader's to refuse. What Yle shows in Finland
  alone (`region` "Finland") is handed a stream like any other, whatever
  `countryCode` says, and the CDN refuses it from elsewhere with a 403, the
  MP3s as well as the masters (`refusedHere` asks an MP3 for one byte). The
  player sends `countryCode` FI until it has located the viewer; since it
  changes nothing, it is not sent. And a series id asked of the preview
  answers with one of its episodes, so the item's page says what an id is
  (`meta.item.type`: `TVSeries`, `RadioSeries`, `TVSeason`).

  A series is its page's first tab's untitled list, read through the UI API
  (`areena.api.yle.fi/v1/ui/content/list?token=…`, signed per list) with the
  page's own pair, `v=10` and the UI's spelling of the language, `fi` or
  `sv`; a two-letter cut of the preview's `swe` is refused. Seasons are the
  filter's options (`path.season`), read in the site's order, each into a
  folder. A season's own page carries the same filter with itself as the
  default, and lists itself alone. A list is followed by the `count` it
  states and by what each page adds, not by the size asked for. Each episode
  is asked of the preview as the listing is read (`FanOut`): one Areena does
  not offer is left out with its reason once, as are the Finland-only ones
  once one of them has been refused here. An episode keeps the name it is
  listed under, the card's title with its number, so that a listing read
  again finds what was saved: `finishedLocked` keys on the name an item
  finished with, the `.ts`, not the MP4 it was remuxed into.
- **drtv** reads DR TV (`dr.dk/drtv/...`) through the API its player calls
  (`production-cdn.dr-massive.com/api`), asked as the anonymous subscriber
  (`sub=Anonymous`), as yt-dlp asks. The site now asks viewers to sign in
  before it plays anything, and treats a signed-out browser as another
  subscriber (`Anonymous2`) that is offered nothing, but the older code is
  still served what DR offers free. A programme the API offers no files
  (`8009`, "Ingen filer") is reported as needing a login: that is what every
  programme would answer if the code were retired. An item says what it is
  (`type`: episode, program, season, show, channel), so the id after the
  link's last `_` is dispatched on that; slugs have underscores of their
  own. A programme's videos are a master per version, and the standard one
  is taken. Its master is CMAF with video-only variants and the audio apart,
  every part a byte range of one MP4 per rendition, with no key and no
  signature.

  Two things in it are not what they say. The audio group holds one track
  at two or three bitrates (`English` and `English_2`, or a group per
  variant), so only its default is kept: joined whole, the MP4 carried the
  same sound twice. And every rendition is tagged English, Danish
  programmes included. The API's `language` replaces the tag: `da`, or
  `foreign`, which marks foreign speech in the programme and names no
  language, so the track is left unnamed. What DR shows in Denmark alone
  (`IsGeoRestricted`) its CDN refuses from elsewhere with a 403, sent as an
  HTML page under a playlist's content type; the API does not refuse it.
  DRM is marked per version (`drm`: `PlayReady`). A show lists its seasons
  and a season its episodes (`/children`, 200 a page, `paging.total`
  counting pages). Each entry carries its `contextualTitle`, the episode as
  its season names it, number and all, and its geo flag, so a listing needs
  no request per episode. A season whose title is only the show's is filed
  as "Sæson N". Anything else on dr.dk still goes to yt-dlp through the
  `Handoff` the extractor keeps.
- **niconico** is read through the site's own player API. A watch page
  describes its video in the JSON of a `<meta name="server-response">`: the
  qualities on offer (`media.domand`, video and audio apart, each marked
  available or not without an account), a key to ask with
  (`accessRightKey`), and the player's track id. POSTed one video and one
  audio quality, the access-rights API mints an HLS master for the pair and
  sets a `domand_bid` cookie. The segments sit on signed links anyone can
  fetch, but the AES-128 key answers 403 without that cookie, so it rides in
  the transfer's headers rather than being left to the jar. The audio is a
  rendition of its own, and the parts are fragmented MP4 under AES-128 with
  the initialisation segment in the clear and an explicit IV
  (`SegmentKey.Clear`). The grant and the cookie last a day, and a queue can
  wait longer, so `Resolve` mints them when the transfer starts, from a
  fresh reading of the watch page. The quality taken is the tallest
  available, then the highest bitrate. An error page, deleted or missing, is
  a 400 or 404 carrying the same description, read off `StatusError.Body`
  so the refusal can name its reason. Mylists (`/v2/mylists`), series
  (`/v2/series`) and a user's uploads (`/v3/users/<id>/videos`) list each
  video's id and title, so a listing costs a request a page and no watch
  page until a transfer starts. Entries gone from the site, or paid, are
  left out and counted in the note apart from what the cap cut. Any other
  page (tags, rankings, channels) still goes to yt-dlp, through a `Handoff`
  the extractor keeps for it. It used to go to yt-dlp whole, for the muxing.
- **yandex** is a redirector rather than a host. Its preview pages are
  search output wrapped around somebody else's video, so the extractor finds
  the source link the page carries and hands that to yt-dlp. Note that
  yt-dlp's own yandex extractor is broken at the time of writing while its
  extractor for the site the preview pointed at works fine — following the
  link ourselves is what routes around that.
- **pixhost** resolves a gallery from its thumbnails, since a thumbnail's
  URL differs from the full image's only by host prefix and one path
  segment. That is one request instead of one per image, and the failure
  mode if the mapping ever changes is a visible 404 rather than a directory
  full of thumbnails. Names keep the site's id prefix because two images in
  one gallery can share an original filename.
- **pornone** reads its renditions from `<source>` elements and ranks them
  by the dimensions in the filename, not by the `res` and `label`
  attributes: a page's largest file is routinely marked 720p while its name
  says 1920x1080.
- **balbums** indexes somebody else's albums rather than hosting any, so it
  resolves each result through the registry the way the `links:` harvester
  does — and the parts they share now live in `sources.go` rather than in
  one of them: the supported-link filter, the fan-out, the per-source folder
  and the "N of M" note that admits a truncated answer. Two things are
  worth knowing. The walk stops **at** the cap rather than collecting
  everything and truncating, because a two-word search states twenty
  thousand pages and walking them to throw all but five away is a lot of
  requests for the same answer. And `per=100` is the most the site honours:
  asking for five hundred is not refused, it is served as twenty under a
  page count for twenty, so the walk trusts the stated page count and a page
  that adds nothing, never the parameter. Self-exclusion is by extractor
  *name*, not pointer, since the recursion is in the kind and not the
  instance — without it the site's own navigation turns one search into a
  walk of the whole catalogue. Each source gets a folder named after its title,
  and titles are not unique — one search turned up 356 albums under 345 —
  so `expandSources` counts the names first and gives a repeated one the
  source's own id (`sourceTag`). A title that stands alone keeps its plain
  name, which is what anything already downloaded under it is filed as.
  Merged folders were how two different files of one name came to sit side
  by side as "(2)" and "(3)".

  `expandSources` applies the file cap **a source at a time**: an album is
  taken whole or not at all, resolving stops once the cap is reached, and
  only a first album larger than the cap on its own is cut. It used to
  resolve every album and truncate the files afterwards, which ended the job
  halfway through one album and resolved hundreds more to throw their files
  away — "500 of 2700 albums, 20000 of 54403 files" was a job holding 224
  albums. The note now counts the albums actually in the job.

- **coomerfans** fetches concurrently, as several extractors now do (pbs, svt,
  rai, zdf, rtve, rtp, peertube, pornpics). A creator's
  page lists posts but keeps the media on each post's own page, and the
  listing cannot be worked from instead: a post holding a video shows no
  thumbnail there at all. So every post is opened, bounded by
  `config.PageFetchConcurrency`, with results collected by index so the job
  lists files in the creator's own order however the requests interleave. A
  post that will not load is skipped rather than failing a job of hundreds.

- **celeb.st** image galleries carry their first page in Inertia's
  `data-page` attribute and the rest at `/api/v1/creator/<slug>/media`, with
  `mediaType=media` and the selected sort on every request. The `src` field
  is the full-size image; thumbnails, avatars and single-image pages'
  `moreMedia` recommendations are separate. URLs are base64 plus repeating
  XOR, as decoded by the site's player, and the resulting image endpoint
  is stable rather than signed. File names use the image id and extension,
  since the URL's basename is an opaque token. A pasted later gallery page
  starts from page one; file/page caps and repeated pages are reported in
  `Result.Note`, leaving the creator's folder name stable.

- **erome** paginates a profile, and two details decide whether it comes back
  complete. The tab lives in the query string (`?t=posts`, `?t=reposts`) and
  selects a different listing, so `eromeProfilePage` carries it onto every
  page; dropping it walks the default tab instead and quietly returns the
  wrong set at the right-looking count. And albums are foldered by title,
  which is not unique — two albums sharing a title collided into one folder
  and looked like lost files that had in fact downloaded. `eromeFolders`
  appends the album id only to titles that actually repeat, so the ordinary
  case keeps clean names. A walk that hits a rate limit it cannot outwait
  returns what it got, with `eromeProfileTitle` marking the job
  "(partial — rate limited)" rather than letting it pass for complete.
- **eporner** keeps its renditions out of the page. The page carries a video
  id and a 32-character hash, and the endpoint refuses that hash as it
  stands: each group of eight hexadecimal digits has to be re-expressed in
  base 36 and the groups joined, with no padding, so a group of zeroes
  collapses to one digit. What comes back is signed with an expiry *and* the
  requesting address, so it is minted through `Resolve` at download time —
  a profile of a hundred videos would otherwise start failing partway down
  its own queue.

  Two traps, both of which look like working code. The listing lives under
  `streamevents`; the `plister` block beside it is the tab's header and holds
  no tiles, so anything choosing a container by what precedes a tile rather
  than what contains it finds nothing. And a profile also shows a sidebar of
  the same tiles plus unrelated ones, so scraping every `/video-` link on the
  page collects entries the tab does not own — twelve reads as fourteen. The
  tab states its own total, which is what `epornerProfileTitle` compares
  against so a short walk says so instead of passing for a whole one.

  Titles need `epornerTitle` rather than the shared `trimSiteSuffix`, which
  cuts at the *first* `" - "`. Titles here carry that separator themselves in
  a numbered series, and cutting there leaves five videos sharing one name —
  on disk, one file that five downloads take turns overwriting. The page's
  JSON-LD states the exact name, and its breadcrumb list is in the same form,
  so the type is checked rather than taking the first name found.

- **ok.ru** reads the embed page rather than the watch page. Served to a
  logged-out caller the watch page carries an empty rendition list, and the
  player's own metadata endpoint answers the same way; the embed page, which
  exists to be framed by other sites, carries the identical structure filled
  in. No session, token or player JavaScript is involved.
- **vids.st** states everything in one `playerConfig` object on `/v/<id>`:
  the file's address, usually the uploaded MP4 or MKV itself, tokenless
  and rangeable, and its original name. An `.m3u8` address is followed
  natively, as the player does. A removed video answers 404 with "Video not
  found", which is reported as such. The site's certificate chain is short;
  see the HTTP client section.
- **knuthing** watch pages frame Bunny Stream. Read only the `bunnyframe`
  embed and its `bunny-stream-video` element's `content-src`; thumbnails and
  sharing embeds are not media. Free-video confirmation follows the page's
  own link for that same video once. Refresh the page, embed, HLS segments
  and AES-128 key on each download attempt, keeping the final embed URL as
  media referer. `/videos/<section>` listings start at page one, read only
  their primary video cards, and follow their own pager within the selected
  section and filters. Cards supply queued titles without opening players.
  Source/file caps and incomplete pagination are reported in the job note;
  recovery reopens the individual video rather than walking the listing.
- **lulustream** (`lulustream.com`, `luluvdo.com`, `luluvido.com`) packs its
  JW Player setup with p,a,c,k,e,d, and `unpackJS` reads the HLS master out
  of it: signed for eight hours, one muxed rendition, AES-128 segments under
  one key. The page is re-read at transfer time, segments and key with it,
  through `Target.Segments`. Its key server refuses Go's default HTTP client
  outright — a live test that fetched the key with `http.Get` got 403 where
  the downloader's own client is served — so anything checking it must go
  through `httpx.Client`. The title is the page's `<h1>`; the document title
  carries the site's name. yt-dlp has no extractor for it.
- **streamtape, doodstream and mixdrop** are the most fragile extractors
  here, because each depends on the shape of a script the host can change.
  Each therefore reads the numbers it needs off the page — the substring
  offset, the packer's base and word list — rather than hard-coding what
  they were. `unpackJS` reverses the p,a,c,k,e,d packer and is host-neutral.
  Their `Extract` bodies, and those of xhamster, tnaflix and pornone, are all
  the same shape — one file, re-read at download time because the link is
  signed per visit. Most share `refetchedVideo` (`tube.go`); Doodstream also
  refreshes its redirected page's media referer with the link.
- **suvobox** resolves an album from the listing alone, which is unusually
  generous: each tile carries the file id, the full name *with* extension and
  the size, so a whole album costs one request and shows real names while it
  queues. The bytes come from the media host's `?raw=1`, which takes no token
  and honours ranges; the site's own download button points at a signed
  `?dl=1` link, which is worth knowing only to say why it is not used — it
  would expire while an item waited its turn and buys nothing.
- **filester** keeps nothing in the page: its script POSTs the slug to
  `/v2/api/public/download` and builds the storage link from the answer
  (`server` + `/v2/` + `file` + `?token=`). The token lasts half an hour and
  encodes the requesting address, so it is minted through `Resolve`. The
  `download=true&n=` the page's button adds only sets a disposition name and
  is left off. Only `/d/<slug>` exists anonymously; folders are behind the
  authenticated API.
  The site hops top-level domains (.si, .me, .gg, .sh so far, all one API),
  so `Match` takes `filester.<any TLD>` with the `/d/` shape rather than a
  `hostSet` that trails every move.
- **bandcamp** was a yt-dlp handoff because every endpoint answered with a
  client challenge — but the challenge is aimed at browsers: a request
  claiming to be one (Chrome and Firefox alike) gets a three-kilobyte
  challenge page, while one naming itself (`bandcampAgent`) gets the real
  page. So every request there overrides the client's browser User-Agent.
  The page's `data-tralbum` carries the band, the release and per track its
  number, title and a `mp3-128` link signed for a day, minted through
  `Resolve`. Tracks are filed `<band>/<album>/NN - title.mp3`; a track
  page's album is only in `data-embed`. A band's front page answers 403
  whatever the agent, so a discography is read from `/music` — which also
  carries a `data-tralbum`, empty and of no `item_type`, so the kind is what
  separates it from a band whose `/music` is its one release.
- **imagepond** reads the player element before the page metadata, which is
  the reverse of the usual order and deliberate. For a video, `og:image` is
  the poster frame and `og:video:type` has been seen claiming MP4 for a
  QuickTime file, while the `<video data-src>` is right. The metadata is
  still what identifies an *image*, because the page's own furniture — its
  logo, avatars — is served from the same media host, so scanning the markup
  for a hosted image finds the logo first. `og:title` is form-encoded, so the
  filename needs decoding rather than using as it stands. Some pages have
  since moved to a client-side player and carry no usable link in the markup
  at all; `/i/<slug>/direct` is the route that still redirects to the media
  host, and `imagePondUsable` tries the hosted link first, falling back to
  `imagePondDirect`. Both paths stay because older pages still serve the old
  markup. The fallback needs one guard: `util.NameFromURL` on a `/direct`
  URL yields the literal "direct", which is not a filename.

### Download manager

`download.Manager` owns all jobs and the worker pool.

**Locking rule:** every `Job` and `Item` field except the atomic progress
counters (`downloaded` and `streams`) is guarded by `Manager.mu`. `downloaded`
is atomic so the progress ticker can sample a running transfer without
contending with workers. Workers take `mu`
only to publish state transitions, never while blocked on I/O.

**Item ownership — do not regress this.** `Item.inFlight` marks an item from
dispatch until its worker has fully finished. Status alone cannot serve that
purpose: cancelling flips `Status` to `canceled` while the worker is still
winding down, so gating on status let cancel-then-retry hand the same item —
and the same `.part` file — to two goroutines. A retry requested during that
window sets `retryPending`, and the exiting worker re-queues it.
`enqueueLocked` is the single entry point for queueing; regression tests live
in `internal/download/ownership_test.go`.

**Part-file naming — do not regress this.** The `.part` file is named
`<file>.<hash of the job's source URL>.part`. Two properties depend on that
choice, and a change to either direction breaks one of them:

- It is **derived, not random.** An item id is fresh every process, so
  keying on it meant an interrupted download could never be found again —
  every run restarted from zero and orphaned the previous `.part`.
- It is keyed on the **job's source**, not the item's URL, because several
  hosts hand out signed media links that expire between runs. Hashing those
  would change the name every time and resume nothing.

Collisions are handled by `Manager.claimPart`: the same URL queued twice
gets the deterministic name for whichever transfer claims it first, and the
random item id for the other, so two workers can never share a `.part`.
Tests live in `internal/download/paths_test.go`.

**Pause and the rate ceiling are one mechanism** (`throttle.go`). Every
read draws from a single shared token bucket; pausing is a gate on the same
object, and the dispatcher refuses to start new items while it is closed.
Four consequences worth keeping in mind:

- A paused transfer is idle *on purpose*, so `watchForStall` is given the
  pause predicate and does not count that time against it. Without that,
  pausing would abort exactly the transfers the user asked to keep.
- The stream supervisor asks `throttle.limiting()` before opening another
  connection. A capped transfer and a slow one look identical from the
  outside — bytes are not moving — and splitting a capped one eight ways
  cannot help, because the bucket is shared. `limiting()` measures the
  bucket's own throughput over a rolling window rather than whether anyone
  *blocked*: the bucket hands out whatever it has, so a transfer pinned
  exactly at the ceiling can go a long time without ever waiting.
- A nil `*throttle` is valid and inert (unlimited, never paused). The tests
  build `&Manager{}` by hand, and that has to keep working.
- A job with a speed limit of its own has a second bucket (`Job.limiter`),
  and `throttledReader` draws on it first: waiting there holds back that job
  alone, where waiting with the queue's tokens in hand would hold back every
  other job with it. The supervisors ask its `limiting()` beside the queue's,
  and a job held to its own speed constrains its route measurements as a
  capped queue does. The bucket is made with the job and changed in place,
  so a running transfer is held to a new limit at once, and every item takes
  the pointer when it is made, so readers need no lock to find it.

**The queue outlives the process** (`persist.go`, `state.go`). Written every
`config.StateSaveInterval` (ten minutes — it carries a line per file, and at
every ten seconds a queue of twenty thousand files rewrote five megabytes
several times a minute) and on shutdown, atomically and 0600, as
**zstd-compressed JSON** (klauspost, about 9:1 on a real queue), and skipped
entirely when
nothing but byte counters has moved — the fingerprint covers all persisted
fields, never progress, because the part file on disk is the authority on
how far a transfer got.

Reading goes by content, not by name: a file without zstd's magic number is
the plain JSON every earlier build wrote, and loads as such. The default path
became `queue.json.zst`; when it does not exist, `loadState` reads the
`queue.json` beside it and the first successful save removes that file — a
queue deleted later to start afresh would otherwise come back from it. A path
named explicitly (`HEAPLEACH_STATE`) is read and written as named.
`Config.LegacyStateFile` enables that default-only migration; a custom `.zst`
path must never infer ownership of a neighbouring file. A failed restore
disables persistence for that process, including shutdown saving, to preserve
an unreadable or newer-version queue. Reads check the file and decoded size
against `config.MaxStateBytes`.

Shutdown closes admission before capturing the queue, and serializes that
snapshot with the periodic saver. A late saver cannot overwrite it with
workers' shutdown cancellations. Extractor retries carry a generation number
so a superseded result cannot publish into the new attempt.

`resolveSlots` bounds active source listings across jobs independently of
download concurrency. Waiting resolvers observe cancellation before calling
the registry. Nested registry expansion carries a path-local ancestry list;
cycles and chains beyond `config.MaxExtractionDepth` fail without sharing
mutable traversal state between parallel sources.

Three things here were each got wrong first:

- **No item URL is stored.** Signed links expire in minutes, and a `Resolve`
  host has no URL until one is minted — it is a closure, which no file holds.
  So `Restore` marks an unfinished job `restored` and its source is read
  again on resume; `alreadyOnDisk` skips what is complete and the part files
  carry the rest. That is also why **`RetryItem` re-reads a restored job**
  rather than re-queueing the one item: a restored item has nothing to fetch
  from, and enqueueing it alone failed with "no download URL" — a confusing
  answer to a reasonable click, which left the item failed for a reason that
  said nothing about the file. Both retries funnel through `rereadLocked`.
  Which jobs that applies to is `unfetchable`, not `restored`: `restored`
  means *held*, and a job whose files were all done or cancelled comes back
  unheld while its items are just as URL-less — retrying a cancelled one of
  those hit the same "no download URL" until the two were told apart.
- **Resolution appends to `job.Items`**, so a restored job's items must be
  dropped before it is re-read or every file in it doubles. Both `SetPaused`
  (resuming) and `RetryJob` clear them, and
  `TestResumingARestoredJobReplacesItsItemsRatherThanDoublingThem` holds that
  in place.
- **`Close` persists before `stop()`, not after `wg.Wait()`.** Stopping
  cancels every transfer, and a worker winding down marks its item cancelled
  — which restores as a job with nothing left to do, silently abandoning the
  download. Recorded before the cancellation they are still running, which is
  written down as queued. The cost is that a file completing during the
  wind-down is recorded queued and re-checked next run, which is the
  harmless direction.

Held is the default because resuming is the user's word to give — but a
service replaced by a deploy tool has nobody to give it, and every new image
would stop the queue until noticed. `HEAPLEACH_RESUME` / `-resume` says it at
startup instead (`manager.SetPaused(false)` after `Start`), and
`/api/health` reports the build's `version` so the tool can tell the new
process from the one it replaced.

**The free-space floor** (`config.MinFreeDisk`, 10 GiB) gates `nextLocked`,
which is to say it gates *starting* a transfer. Three choices in that:
running transfers are left alone, because their bytes are on disk either way
and abandoning one near its end frees less than it wastes; the queue waits
rather than failing, so making room is the whole remedy and nothing has to be
re-added; and a destination that could not be measured is never treated as
full, since refusing to download because the check itself failed is a worse
failure than the one it guards. It reads the sampled figure rather than
asking the filesystem, so the dispatcher's hot path costs nothing and never
blocks under `mu`. `DiskMinFree` rides along in the snapshot because a held
queue and a slow one look identical without it.

**Free space** is sampled on its own cadence (`config.DiskSampleInterval`,
seconds rather than the progress tick's milliseconds) and never under `mu`:
`Statfs` is a syscall and the destination may be a network mount, so it obeys
the same rule workers do. `diskSpace` is split per platform in this package;
the unix half reads `Bavail`, not `Bfree`, since the
blocks reserved for root are not room a download can use. A reading that
moved marks the state dirty in its own right — a disk filling from elsewhere
is exactly what the figure is for, and an idle queue would otherwise never
mention it, while a disk that is not moving keeps that queue quiet. Both
`DiskFree` and `DiskTotal` are published because zero free is ambiguous on
its own: a full disk and an unreadable directory report it alike, and only
the total separates them.

**Skipping what is already downloaded** happens twice, on purpose. Once
before connecting, when the extractor gave an exact length; and once after
the response headers arrive, which is the only check available for hosts
that publish no size. `File.SizeApprox` marks a length read off a listing
page — rounded for display, and never sufficient to conclude that a file on
disk is this one. `Item.Skipped` records the outcome and, unlike `Note`,
survives completion, because `runItem` clears notes when an item finishes.
`separateNames` reserves destinations across each resolved listing before
any worker starts. Sanitized names can collide, and equal lengths are common;
without this a second entry was skipped as though the first were its bytes.
All original names are reserved before numbering duplicates, and `fixedName`
prevents a late resolver or disposition header undoing that allocation.

Neither check works for a file whose length nobody states. A DarkGram photo
comes with no size in the listing and no `Content-Length`, and every restart
re-reads each unfinished job. So every restart fetched each such photo again
and `reserve` kept it beside itself as "(2)", then "(3)": 404 identical copies
in one evening. Two things now cover it.
- **The job's own record.** `rereadLocked` records the items the job had
  already saved, by destination (`finishedLocked`, `destinationKey`, the
  key `separateNames` uses). `resolve` stats them outside `mu`
  (`stillOnDisk`). `applyResultLocked` marks an item done, without
  queueing it, when its destination was saved before and that file is still
  there. The recorded path is the evidence, not the length: a playlist is
  counted at its transport stream's length and then rewrapped as a smaller
  MP4. Matching on length passed over every rewrapped video, which a restart
  then saved again as "(2)". The record is added to rather than replaced, so
  a job read again before an earlier reading lands keeps it.
- **A byte-for-byte check at the end** (`identicalOnDisk`, before
  `reserve`). A whole file identical to one already under its name, or a
  numbered name beside it, is that file: the part is dropped and the item
  points at it, marked skipped. That catches a re-added job, and a file
  finished after the shutdown save recorded it. A different file with the
  same name still gets its own "(2)". A playlist's part is a `.ts`, while
  the copy on disk is the `.mp4` it became, so `remuxToMP4` makes the same
  check on its output. Rewrapping is deterministic, and the three copies of
  one video were byte for byte the same. `identicalOnDisk` skips the new
  file itself, which by then already sits under a numbered name.

**External transfers** (`external.go`, `internal/tools`) cover pages where
reaching the media needs more than HTTP. `tools.Find` resolves a helper
binary next to the running executable before falling back to PATH, which is
what makes `make dependencies` work without a system install. It caches
misses as firmly as hits, so `RetryJob` and `RetryItem` call `tools.Recheck`
first: being told a helper is missing is what sends someone off to install
it, and a service that went on insisting it was absent turned a working
install into a puzzle. Hits are kept — a path that resolved once does not
stop existing. Note that a *newly added* job still meets the cached miss;
only retrying looks again. `tools.CommandContext` shares Unix process-group
cancellation and a bounded inherited-pipe wait across metadata, download and
remux helpers. `tools.Probe` additionally bounds metadata time and stdout;
`util.BoundedBuffer` keeps diagnostic readers draining without unbounded
allocation. That bound is why the yt-dlp probes (`youtube.go`) print only the
fields they read with `--print`, one JSON object per line, rather than
dumping with `-J`. A full dump of one ordinary YouTube video came to ten
megabytes, almost all of it machine-translated automatic captions, and the
job failed before downloading anything. The probes also pass `--simulate`
outright: `--print` implies it only while every print is at the default
stage, and the `playlist:` print turns it off. A probe without it downloaded
the 3.4 GB video into its working directory.
Download progress must be read concurrently with `Wait`, so that
its inherited-pipe deadline actually starts. The download
runs through an embedded shell script, overridable by a copy next to the
binary; it reports `PROGRESS`/`FILE` lines that are folded back into normal
item state. yt-dlp's pieces go to `PARTS`, a hidden
`.heapleach/<hash of the page>` beside the destination, which the script
passes on as `--paths temp:`. Each stream and the half-written merge carry a
media extension, and a library watching the download folder took a dubbed
video's twenty streams for twenty videos. The directory is on the same
filesystem, so the finished file arrives by rename. `clearExternalParts`
removes it after a success (the shared parent only once empty) and leaves it
after a failure: `--no-part` resumes from it, which was checked live. Its **format selector keeps every audio language**, not just the best one.
A dubbed release ranks its original track first, so `ba` alone hands back
Spanish and silently drops the English beside it. Each language is therefore
named — `bv*+ba[language=en-US]+ba[language=ar]+…` — which costs one
extraction before the download to learn what they are, falls back to the
plain pair whenever that probe says nothing useful, and skips the naming
entirely for the single-language case that is most videos. Two flags are
load-bearing and neither is obvious: without `--audio-multistreams` yt-dlp
downloads the extra languages and then keeps one, and without
`--embed-metadata` the merge labels every track with the first one's tag, so
four languages arrive all claiming to be English and a player cannot offer
the choice. That second pass rewrites the finished file, so it is asked for
only when there is more than one language to label. `mergeall[vcodec=none]`,
the documented way to take "all audio", is not what is wanted: it takes all
*formats*, which for the video this was written against is 28 — seven
encodings of each of its four languages.

Its progress line is the fiddliest part, and both of its traps produced a
display that looked plausible and was wrong. yt-dlp reports **one file at a
time**, so a merged download reports the video stream and then the audio,
each counting from zero with a total of its own — passed through as they
arrive, a 154MB download finished reading "5 MB / 5 MB". The format id ends
the progress template so the streams can be told apart and added up, with a
collapsed byte counter standing in when an overridden script predates that
field. And a fragmented download has no length to state, so the total comes
back as a **float** average — `129456458.22222222` — which `ParseInt` refuses;
the error was dropped, every honest total with it, and one early estimate
that happened to be whole stuck for the whole download as "158 MB / 712 B".

Three further things to know: `--print` implies `--quiet` in yt-dlp, so
`--progress` is required or no progress is emitted at all; cancelling
must kill the whole process group, since the script spawns yt-dlp which
spawns ffmpeg; and `DENO` is passed by path rather than left to be found,
because yt-dlp looks for a JavaScript runtime on PATH and `./bin` is exactly
where PATH does not reach. Extraction still works without one, but yt-dlp
calls that deprecated and warns that formats may be missing — measured on an
ordinary video it made no difference to the format list, so this is
insurance against the deprecation rather than a fix for anything visible.

**Playlist transfers** (`playlist.go`, plus `hls.go` in the extractor
package) handle files that arrive as an ordered list of parts rather than
one rangeable resource. They bypass the segmented engine entirely: there is
no length to divide and no offsets to seek to, so parts are fetched several
at a time and appended strictly in order, with an out-of-order buffer
bounded by releasing a worker slot only once a part has been *written*.

Two things here are easy to get wrong, and both were:

- A part is read through `m.throttled` straight into a buffer, **not**
  through `httpx.Client.Bytes`. That helper caps what it reads at
  `config.MaxResponseBytes`, which is sized for pages: a media part larger
  than it would be silently truncated and joined anyway. Reading the body
  directly also means a pause parks mid-part and the rate cap holds *within*
  a part rather than only between parts.
- The stall watchdog samples raw fetched bytes, not `Item.downloaded`. The
  item counter only moves when a whole part lands *in order*, so a large part
  arriving slowly looks identical to a dead connection to anything watching
  it. `watchForStall` therefore takes a progress function rather than an
  item. It starts before fetching an encryption key and counts those bytes
  too: the key uses the streaming client, so a silent body otherwise waits
  forever. Through a proxy, that stall follows the same failure penalty,
  cooldown and route retry as a stalled media body.

**AES-128 playlists are decrypted natively** (`hlscrypt.go`). HLS's
`METHOD=AES-128` encrypts each segment on its own (AES-CBC, PKCS#7), so each
segment — already fetched whole into a buffer — is decrypted in place before
it joins the file, and the part file and its checkpoint hold plain bytes,
which keeps resume honest. The key is fetched once per attempt; the IV is
the playlist's own or the segment's media sequence number. A wrong key shows
only in the padding, so a padding error fails the attempt rather than
writing noise. `mediaPlaylistKey` (`hls.go`) accepts one key over every
segment — the same key line repeated before each segment counts as one —
and refuses the rest before a byte is fetched: SAMPLE-AES, a key that
changes or starts partway, and a non-`identity` KEYFORMAT (DRM). Over
fragmented MP4 the initialisation segment is first in the list. Named before
the key it is in the clear (`SegmentKey.Clear`): not decrypted, and not
counted in the sequence an absent IV is numbered from. Named after it, it is
encrypted too, which needs the key's own IV, since it has no sequence number;
without one it is refused. The key reaches the transfer as `File.SegmentKey`, and
a resolver can replace it with the segments (`Target.Segments`,
`Target.SegmentKey`) for a playlist signed for less than the queue may wait.
Every caller of `resolveMediaPlaylist` carries the key, and the audio below,
onto its File. The segments-only `resolvePlaylist` that refused them is
gone, so no path is left that joins an encrypted or demuxed playlist as it
stands: either makes a file that looks finished and is ciphertext or silent.

**Audio kept apart from the video is fetched beside it and joined**
(`mux.go`). A CMAF master, and anything carrying an audio description,
names its audio as `EXT-X-MEDIA` renditions with playlists of their own, and
its variants' segments carry video alone. `resolveMediaPlaylist` follows the
chosen variant's audio group to every rendition's playlist (`hlsMedia.Audio`,
then `File.Audio` and `Target.Audio`), in the group's order with its
default first. A rendition named without a URI is carried in the video's
own segments and is listed with no segments, which is how a main language
muxed into the variant sits beside an alternate kept apart.

The tracks are fetched together: segment *i* of every track before *i+1* of
any, from one pool of connections, each into a part file of its own beside
the video's (`audioPart`) with a checkpoint of its own. Their links were
signed together, so none expires while another downloads ahead of it, and
the item's segment count moves with the time the transfer takes. A
checkpoint is held to the track that wrote it (`playlistState.Track`): a
playlist's tracks commonly have the same number of segments, and a part
resumed from another language's checkpoint would carry both. `onDisk` sums
every track's checkpoint for the retry budget.

The join is ffmpeg, by stream copy into an MP4 (`joinArgs`), and three
things in it each looked right first:

- ffmpeg starts every input at zero on its own unless told otherwise, which
  drops the offset between renditions: half a second in the test fixture,
  lip sync in a real file. `-copyts` keeps their timestamps, which share the
  playlist's one timeline, and `-avoid_negative_ts make_zero` moves the
  output as a whole to start at zero. `TestJoiningKeepsTheOffsetBetweenTracks`
  holds it.
- An MP4 keeps a track's language only as three letters, and ffmpeg drops a
  two-letter `sv` without a word: `mp4Language` maps ISO 639-1 to 639-2/T,
  taking the primary subtag of a longer tag (`sv-x-tydligaretal` is `swe`).
  The label goes in `handler_name`, which is what an MP4 keeps and VLC and
  mpv show; `title` is dropped. And an MP4 enables every audio track
  flagged default, each in an alternate group of its own, for a player to
  take as tracks to play together. So exactly one is: the playlist's
  default.
- The audio is exactly the renditions the playlist names, each from where it
  says. Audio in the video's segments that it does not name is left out:
  Akamai (SRF, ZDF) mixes the default track into the video *and* publishes
  it apart, and keeping both lists it twice. A rendition named as carried in
  the video is mapped from it once ffprobe has counted what is there, and
  one that is not there is left out rather than failing the join.

ffmpeg is a dependency here, not a convenience: without it the video alone
is silent, so `transfer` refuses such an item before fetching a byte
(`errNoFFmpegToJoin`), and a retry looks for it again. The joined file is
written beside the parts (`joinedPart`), checked by `identicalOnDisk`,
reserved and renamed like any other, and only then are the parts and their
checkpoints removed: a join that fails leaves them, and the retry joins
again without fetching anything.

**Byte ranges of one file are parts like any other.** A playlist that slices
one file per rendition (`EXT-X-BYTERANGE`, and a map's `BYTERANGE`), as
ARTE's do, has each part carried as the file's URL with its range in the
fragment (`#range=FIRST-LAST`, `extractor.SegmentRange`), which no request
ever sends. `File.Segments` stays a list of strings for the many extractors
that never meet a range. A range that states no offset starts where the
previous part of the same file ended, and a map's at the start.
`fetchSegmentOnce` asks for exactly those bytes, checks `Content-Range` and
the length, and refuses a host that answers with the whole file
(`errRangeIgnored`, not retried): read whole, a playlist of a thousand parts
would fetch it a thousand times.

**Subtitles come along as text tracks** (`subtitles.go`). A variant naming a
`SUBTITLES` group comes with its WebVTT renditions (`resolveSubtitles`,
`File.Subtitles`), keeping the playlist's default and forced flags. A
subtitle is a bonus to the video, never a condition of it, so each step that
could fail one leaves it out instead: a rendition whose playlist cannot be
read, or is WebVTT wrapped in fragmented MP4 or encrypted, is not listed; a
track that cannot be fetched in `SubtitleFetchTimeout` is logged and left
out; a join that fails with subtitles is made again without them. Nothing is
fetched without ffmpeg, which they could not be joined without.

They are fetched at the start of each attempt, while their links are
freshest, and before the stall watchdog starts, since a host slow to serve
them has not stalled the video. `mergeWebVTT` joins a track's files into
one: a plain header, every style and region block once, and then the cues.
The other blocks go, identifiers too, which would repeat across files. A
segmented track's files each say where their cues sit with an
`X-TIMESTAMP-MAP`, which can differ from file to file, so the cues are moved
onto the first mapped file's timeline, counting on across the 90 kHz
counter's 33-bit wrap. A cue repeated across a boundary, same times and
text, is kept once. The merged file is written whole, beside the part
(`subtitlePart`), and a file an attempt could not replace is removed, so
the join never takes one that may belong to another track.

The join makes each an MP4 text track (`mov_text`). ffmpeg reads no
`X-TIMESTAMP-MAP` of its own accord, so where its cues sit is given with
`-itsoffset`: a mapped file's offset puts them on the media's timeline, and
an unmapped file's cues count from the start of the picture, which ffprobe
reads off the video's part. The two are not the same: ffmpeg's own CMAF
starts its picture 23 ms in, after the audio's priming. A playlist that
names no audio, joined only for its subtitles, carries all its audio in the
video, and all of it comes along. ffmpeg's MP4 muxer enables the first
subtitle track when none is default, which is how every tool's MP4
subtitles behave.

`remux.go` is a convenience on top, never a dependency — where a rendition
carries audio and video together, the extractor picks it. It converts any
finished `.ts`, always by stream copy, and keeps the `.ts` when a lossless
copy is impossible. `mediaprobe.go` chooses what to carry over: the best
video, and the best audio and subtitle track per language. One trap worth
knowing: a copied MP2 stream is reported as `mp3` once inside MP4 because
the MPEG audio layers share an identifier there — the bitstream is
byte-identical, so that is not a re-encode.

**Encrypted payloads** (`crypt.go`) are decrypted on the way in rather than
in a second pass over the finished file, and the reason that is possible is
worth keeping in mind before changing either side. Every write in the engine
is positional — `WriteAt` with an absolute offset, from the sequential path
and from eight connections filling ranges out of order alike — and the mode
is a counter-mode stream cipher, whose keystream at an offset depends on
nothing but the offset. So a decrypting `io.WriterAt` slots under the file
and the rest of the engine does not know it is there. Two consequences:
`segmentedTransfer.file` is an interface rather than an `*os.File`, and the
part file holds **plaintext**, which is what keeps resume working — the
sidecar's byte counts and the file's length go on meaning the same thing.
Decrypting in place would be free but is forbidden: `io.WriterAt` may not
touch the caller's slice, and both callers reuse one read buffer for the
whole transfer. Tests live in `internal/download/crypt_test.go`.

**Multi-connection transfers** live in `segments.go` (the range table and
its resume sidecar), `multistream.go` (the engine and its supervisor),
`speedmeter.go` and `hostlimit.go`. Points worth knowing before changing
them:

- Every hot-path copy and decrypt draws its 256 KiB buffer from one shared
  pool (`buffers.go`, `borrowChunk`). They all want exactly CopyBufferSize,
  so keep new ones on the pool rather than allocating per attempt.

- **Writes are buffered, and that is what separates "received" from "on
  disk"** (`writebuf.go`). A read returns whatever the socket has, and
  writing each one straight through meant a write syscall per read —
  several hundred a second across a busy queue, every one of them an inotify
  event for whatever is watching the download directory. A connection now
  accumulates `config.WriteBufferSize` before touching the file. Measured on
  a 200 MB transfer over loopback, where reads already filled the 256 KiB
  chunk every time, that is 771 write syscalls down to 191; on a real
  connection, where a read is socket-sized, the ratio is far larger.

  A connection starts on `config.WriteBufferStart` (64 KiB), and grows to
  the full size only once it has moved a mebibyte at an average of
  `WriteBufferGrowRate` (64 KiB/s) or more. A free download capped at
  42 kB/s took the better part of half a minute to fill a full buffer.
  That was a mebibyte apiece held for nothing, a quarter of a gibibyte
  across 256 of them. And a connection dropped before its buffer filled had
  written nothing, so its attempt looked unproductive to the retry budget,
  which judges by what is on disk. It is the average over a mebibyte that
  is judged, not how fast one buffer filled. The first version grew a
  connection that filled its starting buffer within a second, and 84 of 138
  transfers grew, 130 of them slower than 64 KiB/s: those downloads arrive
  in bursts, and one read fills 64 KiB at once. A write of a full buffer or
  more still goes straight through. A fast transfer pays sixteen small
  writes before it grows, so the 200 MB loopback transfer above makes about
  215 writes rather than 191.

  The buffer belongs to **one connection**, not to the file: eight
  connections fill eight disjoint ranges at once, so a shared buffer would
  have nothing to coalesce and would need a lock to say so.

  The consequence is the part that must not be regressed. `segment.pos` is
  what has been *received* and decides what to read next; `segment.flushed`
  is what the file actually holds, and it is the only thing `state()` writes
  to the sidecar. A sidecar that counted buffered bytes would resume past a
  gap and finish a file with a hole in it — the sidecar may only ever
  under-report. Reads stay bounded by CopyBufferSize whatever the buffer
  holds, because the promise that keeps a split point ahead of the writer is
  stated in those terms; see `MinSegmentSize`. Tests live in
  `writebuf_test.go`, and the invariant itself in
  `TestSegmentStateRecordsWhatIsOnDiskNotWhatWasReceived`.

  Two consequences worth knowing. An attempt that dies loses whatever its
  connections were holding, which is bytes re-fetched rather than bytes
  lost; and since the retry budget judges an attempt by what it left *on
  disk*, a buffer much larger than what a flaky host manages between drops
  would start making productive attempts look unproductive. The playlist
  path is deliberately left alone: it writes whole parts, which are already
  megabytes apiece, and an external download is yt-dlp's own file to write.

- `ranges.go` validates every partial response before it reaches the file:
  the offset, span, body length and known total must agree. A 416 only proves
  completion when it states the exact length of a sequential part. For a
  partial response with an unknown total, the next range confirms the end.
  Resume also checks that the part still contains the bytes its checkpoint
  records; a missing or truncated part or a corrupt checkpoint starts over.
  These regressions live in `ranges_test.go`.

- Splitting bisects the *remaining* span of the widest segment, with ties
  broken leftmost-first. That is what produces the halves, quarters and
  eighths in their natural order, and it self-balances once some segments
  are ahead.
- `MinSegmentSize` is not just about avoiding tiny requests: it guarantees a
  split point stays ahead of the writer, so a reader can never write past
  the end reassigned beneath it.
- A reader leaves its loop only through `table.finish`, which runs under the
  table lock. Paired with `retire` refusing to extend a finished segment,
  that is what stops a range being left with no reader.
- Decisions use `speedMeter`, a windowed average that reports nothing until
  the window fills. A single interval swings far too much to act on.
- Extra connections are budgeted per host and never retry: a refusal is the
  answer we want, so the range goes back and the host's budget drops.
  Primary connections are never budgeted, so a tight budget cannot deadlock.
- **A host that answers 503 is overloaded, not broken**, and the answer is a
  global admission queue per host (`hostgate.go`), not a longer retry loop.
  The refusal is waited out on the busy schedule up to
  `config.OverloadRetries`, outside the retry budget like the two cases
  below it — a budget spent in a few seconds is no time at all for a backend
  to recover in.

  The note is rendered per snapshot from the host's current answer
  (`Manager.itemNoteLocked`), not stored when the item is turned away. What
  a host is taking changes while an item waits, so a stored sentence goes on
  stating whatever was true when it was written, and a queue behind one host
  showed two different numbers for it at once.

  One wording covers every item that is waiting, refused or merely not yet
  at the front: both are waiting for a turn at a host taking fewer downloads
  than the queue would give it, and the refusal count behind them is
  bookkeeping. Putting it in the row only invited the question of what it
  meant; if the patience runs out the item fails and says so, which is where
  the count is worth something.

  **A refused transfer gives its slot up there and then.** It does not wait
  and try again holding it: an item that has been refused has no open
  connection, only a place in a queue better given to something else. Two
  versions of this waited in place, and both produced the same picture —
  four rows on one struggling host all reading "attempt 5 of 10" while the
  cap said one at a time, because lowering the cap never took back the slots
  already held. The turn is counted on the *item* (`overloadWaits`), since
  the waiting happens in the queue and each turn is a separate transfer.

  Only refusals **in a row** count, the way the retry budget counts only
  unproductive attempts: a turn after which the host got some other
  transfer going (`hostGate.lastServed` against the item's `refusedAt`)
  starts the count over, so an overloaded host still working through its
  queue is never given up on — only one that has stopped serving is. And
  `enqueueLocked` clears both, so a retry starts the patience over;
  carried across, a file that had run out of it failed on the first refusal
  after being retried, without waiting at all. `deferHostQueuedLocked`
  restores them, being the one caller continuing a wait.

  **One at a time, from the first refusal, and the host is left alone
  between turns.** Walking the cap down a step per refusal sounds gentler
  and is not: while it walks, everything admitted before it started is still
  going. And a cap alone slows nothing when the queue behind it is long —
  the next item takes the freed slot the instant it is given back — so the
  gate also holds a host quiet for a growing interval after each refusal,
  cleared the moment a transfer there gets going.

  **The queue is what does the work, and nothing waits in a worker.** An item that cannot
  have a slot is handed straight back to the queue (`hostQueuedError`,
  `deferHostQueuedLocked`), where `hostFullLocked` passes over it until
  there is room. `tryAdmit` never blocks, and that is the point: a worker
  parked on a struggling host is a worker not downloading from anywhere
  else, and three of four workers sitting on one host is what this looked
  like before. Freeing a slot signals the dispatcher, since it is asleep
  until told there is something to look at.

  **The turn comes before the signature.** A link is signed for minutes and
  an item may sit in that queue for hours, so `transfer` takes the slot
  first — keyed on wherever the item last resolved to — and only then mints
  a link, swapping the slot if what it resolved to is somewhere else. Minted
  before the wait it would expire where it stood, and a signature spent on a
  transfer that is then turned away is a request made of a host that has
  just asked for fewer of them. An item that has never run knows no host, so
  it signs once and takes its turn against what that produced; the worker
  pool bounds that, which is a far smaller number than the queue behind
  it.

  The number of slots is learned, which is what separates it from the `Pace`
  an extractor declares: one fewer at each refusal, floored at 1, and one
  back for each transfer that runs to the end. Down on evidence and up on
  evidence, forgotten once it reaches what the queue would run anyway.
  Transfers already running are left alone, for the reason the free-space
  floor leaves them alone.

  **It is keyed on the host that actually answered**, which is where the
  link resolved to and not the site the job was submitted from — and it is
  keyed that way in all three places, the queue, the evidence and the cap,
  or the throttle can never fire. A search on an index site is the case that
  proves the point: every file in that queue lives on somebody else's
  storage, so throttling the index would hold back the queue without
  touching the host that is struggling, under a message naming the wrong
  machine. That is why `hostFullLocked` cannot serve here: it runs at
  dispatch, and nothing has resolved yet.

  **The throttle waits for proof the host serves anything.** A 503 from a
  host that has never got a transfer going is a host that is *down*: cutting
  it back neither helps it nor gets the file. That case still waits, and the
  note says "unavailable" where the other says "overloaded". Only 503 does
  any of this — 500, 502 and 504 are a server that broke or a gateway that
  could not reach what it fronts, which the ordinary budget already repeats
  and which say nothing about how much the host is being asked for.

- **A host that limits how many downloads one caller may have open** is a
  third thing again, and neither a failure nor a rate limit:
  `refusedForConnectionCount` (`hostlimit.go`) recognises it and
  `Manager.transfer` waits it out on the busy schedule, up to
  `config.ConnectionLimitRetries`. What clears it is *our own* other
  transfers finishing, which is why the patience is measured in minutes and
  why it is capped — the identical refusal arrives when the connections
  belong to something else on this address, and then no amount of waiting
  here frees one. The body has to be read, not the status: these hosts
  answer 403, which otherwise means forbidden and must stay fatal, so
  `connectionLimitMarkers` holds each host's own error identifier rather
  than a phrase from its prose. A refusal of a connection a transfer
  *needed* is also the strongest evidence there is about the host, so it
  calls `hostLimiter.saturated` to close the extra-connection budget at
  once instead of letting every sibling learn it by being refused.
- A host that answers with a web page instead of bytes (gofile does this for
  busy storage servers) is caught by `rejectWebPage`. Without it, following
  the redirect writes the page shell to disk and — since Content-Length
  agrees — records it as a complete file. Those attempts sit outside the
  retry budget, but only for an item that can re-resolve: one with a
  resolver re-signs its link or rotates to the next mirror, so trying again
  can genuinely land somewhere else. An item without one asks the same URL
  the same way, and a URL that keeps answering with a page almost always
  *is* a page — something pasted that no extractor recognised — so those
  stop at `config.BusyRetryLimit` and say that instead of retrying forever.

Other behaviours that span files:

- Transfers resume from the `.part` file with a `Range` request. Its name is
  derived from the job's source URL — see the part-file naming section above
  — so an interrupted download is found again on the next run, and
  `claimPart` keeps two transfers of the same URL off one file.
- A stall watchdog aborts an attempt whose byte counter stops moving
  (`config.StallTimeout`); streaming requests retain a header deadline, while
  body reads have no deadline of their own, so
  without it a silent server would pin a worker forever. The aborted item is
  **not retried in place**: `deferStalledLocked` sends it to the back of the
  queue with its part file intact, so a host that has stopped serving does
  not hold a worker slot while the queue waits — bounded by the retry
  budget, past which the next stall is a failure, so the headless run still
  terminates. Its next turn resumes from disk. A stall that came after bytes
  had landed starts that count over, for the reason in the next bullet.
  Relatedly, the stream
  supervisor treats stalled and slow as opposites: a probe window in which
  no bytes arrived opens no extra connection, and discards any pending
  before/after verdict rather than reading the stall as "parallelism made it
  slower" — a verdict that would penalise the host's budget for the rest of
  the process over a condition the host imposed on everyone.
- **The retry budget counts attempts in a row that moved nothing.** An
  attempt that received bytes and then lost its connection — a CDN node
  dropping every few dozen megabytes, a NAT forgetting a long connection —
  is one connection failing on a host that was serving the file a moment
  ago, not the transfer going nowhere. `Manager.transfer` therefore resumes
  it from the part file after a flat `config.ProgressRetryDelay` (30s) and
  starts the attempt count over, so `MaxRetries` bounds consecutive
  unproductive attempts rather than the whole life of a large file on a
  flaky host. The judgement is made against the disk, not the wire:
  `onDisk` reads what the part file held before the attempt — the segment
  sidecar's tally, the playlist checkpoint, or the file's length — and the
  position afterwards has to be further. Bytes arriving is not enough, and
  `TestStalledTransferYieldsTheWorkerSlot` is why: its server ignores
  `Range` and serves the same first 8 KB on every turn, which receives
  plenty and gets nowhere, and judged by bytes alone would be deferred
  forever. Nor can `Item.downloaded` from before the attempt serve as the
  baseline, since `enqueueLocked` zeroes it on every re-queue. This cannot
  cycle forever on a finite file, since every such attempt left more on
  disk than it found. Tests live in
  `internal/download/progress_retry_test.go`.
- A master playlist's `CODECS` describes the whole presentation, not the
  variant's own segments, so a variant that names a separate `AUDIO` group
  advertises audio it does not carry. `hlsVariant.muxed` therefore checks
  the group's renditions for a `URI` of their own rather than trusting
  `CODECS`; without that a demuxed host would yield a silent file that looks
  like a finished download. Such a variant comes with its audio renditions
  instead, which `mux.go` joins to it. At the same height `bestVariant`
  takes H.264 over HEVC and AV1, whatever their bitrates: every player
  decodes it, and HEVC in an MP4 tagged `hev1`, as packagers mostly tag it,
  plays in nothing of Apple's. ARTE's 1080p HEVC outbid its H.264 by a few
  per cent of bandwidth.
- An audio description is named so (" (audio description)") only where its
  name would otherwise read like another rendition's: SRF names it as it
  names the main track, but for a trailing space. ARTE names it
  "Deutsch (Audiodeskription)", and a suffix there said it twice.
- `SafeName`/`SafeRelPath` sanitize remote path components. Native transfers
  and checkpoints use `os.Root` (`files.go`) to confine actual filesystem
  operations too, including symlinks. Finished names are reserved with
  exclusive creation so simultaneous completions cannot overwrite each other.
  `SafeName` first decodes HTML entities (`util.Unescape`, repeated until
  stable): some pages escape a title twice, so one parse left `&#39;` and
  `&amp;` in folder and file names on disk. Decoding comes before the
  separator rules, so an encoded slash is treated like a plain one.
  `Registry.Extract` decodes titles and names too, for the UI's sake.
- **A slash in a title is not a path.** `SafeName` keeps only what follows
  the last separator, which is right for a name a server hands over (RFC
  6266 says to strip a `Content-Disposition` name of its path) and wrong for
  a title: "A Series (1/7)" was saved as "7)", and "AC/DC" as "DC". So
  `Registry.Extract` and `resolveTarget` turn a title's separators into their
  lookalikes first (`util.TitleName`, "⧸", as yt-dlp writes them), for job
  titles and file names alike. `sourceFolder` turns them, and plain slashes,
  into "_" for the folders a listing's sources are filed under, which is
  what those folders have always been named: changing it would file every
  re-read job somewhere new and download it all again.

### Headless mode

URLs on the command line switch `cmd/heapleach` out of server mode entirely: no
listener, no browser, no assets. `internal/cli` drives the same `Manager`,
polls `Manager.Snapshot()` on its own ticker rather than subscribing, and
returns once every job is terminal.

Four things are easy to break:

- **The display owns stdout.** A headless run sends its log to *stderr*.
  Debug logging defaults on and uses plain progress lines. `-debug=false`
  or `HEAPLEACH_DEBUG=0` restores warning-level logs and terminal animation,
  because a log line written into the live region corrupts the frame.
- **One painted line must be one screen row.** The live region is repainted
  by counting rows and moving the cursor up that many times. Every line is
  truncated to the terminal width first — counting printable runes, not
  bytes, so colour escapes do not eat the budget. This approximates cell
  width; wide characters and grapheme clusters still need better handling.
  Remote fields pass through `terminalText` before our own colour escapes
  are added, so they cannot inject controls or new rows. The moment a line wraps,
  the arithmetic is wrong and the display starts eating itself. The width is
  re-read every frame (`Options.TermWidth`), because a terminal that shrank
  mid-run would otherwise wrap every line from then on.
- **A job that resolves to nothing still has to end the run.** It reports
  `queued` forever because it has no items to finish; `finished()` treats a
  non-resolving job with zero items as done. A failed source with no items
  still makes the summary fail, as does any rejected command-line input:
  zero failed files is not proof that every requested source succeeded.
- **Byte counts are SI and must match the web UI digit for digit** — the
  same transfer must not read "1.5 GB" in the terminal and "1.6 GB" in the
  browser. `cli.formatBytes` mirrors `frontend/src/format.ts` deliberately;
  change them together or not at all.

### Server and shutdown

The service's text logger also writes into `internal/logstore` before startup
work begins, preserving job/item fields and the configured log level. The
buffer retains at most 1,000 records or 4 MiB, with 16 KiB per record and UTF-8
boundaries preserved. Headless mode has no history buffer. `/api/logs` copies
entries under the buffer lock and encodes them afterwards; a browser never
holds that lock while reading. Session IDs distinguish restarts, and cursors
before retention or ahead of the current run reset the client's window.
Keep log reads out of API request logging or the viewer generates its own
endless traffic. The Logs panel polls deltas only while open, visible and
unpaused, aborts pending reads on close/pause, and discards expired rows even
when the byte limit shortens the window. Export uses the current filters;
messages render as text, including markup in remote error messages.

The API is `net/http` with Go 1.22 method+pattern routing. `/api/events` is a
server-sent-events stream starting with a **complete state snapshot** and
then carrying changed item rows. Dropped patches are replaced with a complete
snapshot immediately, including when the queue has just become idle.
Everything in that payload is rendered by the UI: it once carried the host
names for a list that only restated what the build supported, and now carries `hostCount` alone,
which is all the progress panel reads.

**A snapshot is complete for what the client is showing, which is not the
same as complete.** A queue of a couple of thousand files was most of a
megabyte a frame, two and a half times a second, to paint a few dozen rows —
2.3 MB/s to a browser that was displaying none of it. Four things cut that,
and they are worth knowing apart because they fail differently:

- **Fields nothing reads are not sent** (`ItemView`). The item URL was the
  single largest thing in a frame — a signed storage link is a couple of
  hundred characters and there is one per item, 39% of the payload — and the
  browser has never looked at it. `Elapsed` is the terminal's, read from a
  Snapshot in process. Together, 43%. The rule this follows is the one
  already stated above: a field the UI stops reading comes out of the
  payload.
- **Only the rows that changed are sent** (`Manager.patchLocked`,
  `frontend/src/snapshotMerge.ts`). Almost nothing in a queue changes from
  one second to the next: a thousand finished files say exactly what they
  said before. So a frame carries every job whole — its counts, its size,
  its rate, all of which are small — and inside it only the items whose wire
  view differs from the one last broadcast, marked `patch`. The browser
  merges them into what it holds, so the search, the progress panel and the
  cards all still see complete lists and none of them knows the wire is
  thrifty.

  Four things keep that self-healing, and each is a way a merge could
  otherwise be wrong, or a way the saving quietly evaporates.

  A job whose list **lost** rows is sent whole, because a merge only adds
  and replaces and has no way to say a row is gone. A list that merely grew
  is a patch carrying the new rows, which is the common case while a large
  album resolves — items arriving a page at a time — and exactly when
  sending the whole list again costs most. A list where *every* row is new
  is sent whole too: that is either first sight or a wholesale replacement,
  and a re-read drops the items and resolves them again, which can land
  between two frames and leave the count unchanged while nothing else is.

  A new subscription is answered with the whole state, handed back by
  `Subscribe` itself so that registering and having something to merge into
  are one act. It records that snapshot as sent, or the next broadcast finds
  every row unlike the nothing it was compared against and sends the lot a
  second time; and it marks every other subscriber stale, since what they
  were last told has just been recorded as delivered.

  A frame dropped on the way to a slow reader is replaced by the current
  whole snapshot in `publishLocked`. Recovery cannot wait for another
  broadcast: the dropped patch may have completed the last active job.

  **Except for what the user just did.** Every user action — add, cancel,
  retry, remove, clear, pause, a setting — calls `Manager.nudge`, which
  sends a frame at once past the ceiling. A row that sits unchanged for most
  of a second after a click reads as the click not having taken. It is only
  for actions: anything that happens by itself goes out on the ordinary
  beat, which is what keeps the stream cheap.

  And the one-a-second ceiling applies whatever else is happening. A job being
  read for the first time marks the state changed on every tick as its items
  arrive; letting that bypass the ceiling put out two and a half frames a
  second on its own. The dirty flag is read rather than taken when a frame
  is skipped, so a frame that is not sent cannot swallow the change that
  would have justified the next one.
- **The stream is compressed** when the client asks (`compressed` in
  `sse.go`). A frame is mostly the same words as the last one, so it packs
  about six to one. Both layers have to be flushed per event, in order, or
  the browser sees nothing until the next frame pushes the last one out.
- **A frame is skipped when nothing moved** (`config.IdleFrameInterval`).
  `sampleLocked` reports whether any byte counter actually advanced, which
  is what separates a queue that is working from one that is merely open —
  a thousand items held behind a throttled host look identical from the
  broadcaster's side. Those refresh on a slower beat rather than not at all,
  since the rates decay towards zero and a viewer should see that.

Measured on a real queue of 2044 files: a frame was 902 kB and is now 1,334
bytes once the first one has gone out, and they go out at most once a second
rather than two and a half times. 2.3 MB/s becomes a couple of kilobytes a second. The
whole state is still sent once per connection, which is what a browser
arriving has to be told.

**Profiling is opt-in and never on the API's port** (`HEAPLEACH_PPROF`,
`-pprof`, `cmd/heapleach/pprof.go`). The handlers get a mux and a listener
of their own, started before anything else so a headless run can be
profiled too; the API listens on every interface, and profiles describe the
process from the inside. Importing `net/http/pprof` also registers on
`http.DefaultServeMux`, which is harmless only because nothing serves it —
keep it that way. The service under contdep sets it to the container's own
`127.0.0.1:6060`, reached with `docker exec heapleach wget -qO-
'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'`.

**Shutdown ordering matters.** `http.Server.Shutdown` waits for in-flight
requests and does *not* cancel their contexts, so an open SSE stream holds it
until the deadline. `main.go` calls `manager.Close()` **before**
`srv.Shutdown()`; the manager closes subscriber channels, which ends the
streams. `Manager.Close` is idempotent so the `defer` stays safe.

### Configuration

`config.FromEnv()` reads the environment with **no filesystem side effects**;
`(*Config).Prepare()` validates, expands `~`, creates the directory and
proves it writable. The split exists so `main.go` can layer CLI flags on top
before anything is created — otherwise loading config would create whichever
directory the environment named, even when an argument overrides it.

Download directory precedence: positional argument → `-dir` → `HEAPLEACH_DIR`.

**The listing caps are settings** (`HEAPLEACH_MAX_SOURCES`,
`HEAPLEACH_MAX_FILES`, and `-max-sources`/`-max-files`), defaulting to
`config.MaxExpandedSources` and `config.MaxListingFiles`. They reach the
extractors through `extractor.Options`, whose unexported `limits` the
registry fills from the config in `Registry.Extract` — so a nested
extraction, an album found by a search, is bounded the same way, and every
extractor that walks a listing reads `opts.maxFiles()` rather than the
constant. Zero means the default, and there is no "unlimited": every file of
a job is also a row in the UI and a line in the saved queue.

**So is the ceiling on parallel transfers** (`HEAPLEACH_MAX_CONCURRENCY`,
`-max-concurrency`, default `config.DefaultMaxConcurrency`, 256; zero means
the default). It bounds the startup concurrency and the live setting, and
reaches the Settings slider as the snapshot's `maxConcurrency`, through
`Manager.maxConcurrency`, which also gives a config-less test manager the
default. It was a fixed 32. Proxy routes changed what one process can
carry, since each address takes its own Keep2Share transfer. A connection
fast enough to need it buffers up to `WriteBufferSize` (1 MiB) before
writing, so 256 fast files split eight ways can hold about 2 GiB of buffers
at worst. Slow ones keep the 64 KiB they start with (see the write buffers).

**Parallel downloads and streams chosen in Settings outlive the process.**
Every release restarts the service, and each restart put them back to the
configured values. They are saved with the queue (`savedSettings`), written
the moment they change rather than at the next save, and taken back by
`Restore` before `Start`. Each is stored with the configured value it
overrode, and is kept only while the configuration still says that. A
configuration changed since is the later word, or a deployment changing it
would see nothing happen. A queue saved before this has no settings, and the
configuration stands. A kept concurrency is held under the current ceiling.
Pause is not kept: resuming stays the user's word, or `HEAPLEACH_RESUME`'s.

**A job can be held beneath the queue's limits** (`joblimits.go`).
`JobLimit` sets how many of the job's files download at once, how many
streams each may use, and how fast the job may go (see the throttle above).
The queue's own settings still apply over it, and its streams replace what
the host asks for itself within them. A host that allows one connection can
be allowed three, and one that splits freely capped at two. Files of 0 holds
the job's new downloads, and those running finish. The dispatcher counts
each job's running transfers (`jobActive`) beside `hostActive`. The note on
a held file (`jobHoldNoteLocked`) is rendered per snapshot rather than
stored, so it follows the limit as it changes and is gone once it is lifted.
`POST /api/jobs/{id}/limits` replaces a job's limits whole. They are saved
with the job: no configuration sets them, so they are kept whatever it says,
under the ceiling in force when they are read back. The job's view carries
them as flat fields (`fileLimit`, `streamLimit`, `speedLimit`), since an
object would be a new one in every frame and defeat the cards' memo. These
limits were first set per transfer group, a service or a site. The job is
what someone adds and watches, and what they meant.

**What names a job's folder is `Result.Title` alone.** An extractor that has
something to admit — a listing cut short by a cap or a rate limit — puts it
in `Result.Note`, which the job's shown title carries (`Result.Label`) and
the folder never does. When the note was part of the title, raising a cap or
waiting out a rate limit changed the folder, and the next run filed
everything somewhere new and downloaded it all again.

`HEAPLEACH_EXTRA_HOSTS` is the one setting that names hosts rather than
tuning behaviour, for the reason above: these platform host lists rot faster
than releases. It reads `family:host,host;family:host`, and the older
`HEAPLEACH_KVS_HOSTS` is still honoured as the `kvs` family.

`main.go` calls `net.Listen` itself rather than `srv.ListenAndServe`, so an
address of `:0` can be resolved to the port the kernel actually assigned and
then logged and opened (`-open`). Picking a free port outside the process
would be a guess that could race another binder.

**Every tunable constant lives in `internal/config/limits.go`**, and HTTP
header names and common values in `internal/httpx/headers.go`
(`httpx.Header`, `httpx.Referer`, `httpx.RefererOrigin`). Add new ones there
rather than scattering literals. Shared dependency-free helpers belong in
`internal/util`.

## Frontend

React 19 + Vite, TypeScript in `strict` mode with `noUnusedLocals` and
`verbatimModuleSyntax`. `tsc -b` runs as part of `npm run build`, so a type
error fails `make build`.

State comes entirely from the SSE snapshot via `useLiveState`, which falls
back to polling `/api/state` when the stream is unavailable. Components
render straight from the payload — the server precomputes job aggregates so
the UI does no derivation. The one vocabulary the UI does own is in
`status.ts`: which statuses count as active, terminal or retryable, and the
sidebar's filters and the search built on them. The sidebar, the cards and
the "Clear finished" count all ask it rather than keeping lists of literals
of their own, and it is where the pure-logic tests for that live. The sidebar's second section, **Files**, is built on the same module:
`phaseOf` sorts every file across all jobs into Downloading, Queued, Failed
(cancelled included — both are what a retry is for) or Done, and choosing
one replaces the job cards with `FileList`, a windowed list of the job
cards' own rows, each naming its job. Its per-row actions carry only the
item id, looked up to a job there, so one stable callback serves every row. Everything the snapshot carries is rendered
somewhere: `note` explains a deliberate wait, and `segmentsDone`/`Total` are
the only progress a playlist has, since it has no byte total until its last
part lands. A field the UI stops reading should come out of the payload
rather than being left to accumulate. `gamification.ts`/`useProgress.ts` add a progress
layer persisted in `localStorage`; it is derived from bytes actually written.

The header's version badge (`VersionBadge`, `version.ts`) shows the
server's build from the snapshot (`Snapshot.Version`) rather than asking
once: contdep redeploys under open tabs, the stream reconnects to the new
server, and the page goes on running the interface it was loaded with. The
first version a snapshot reports is taken as the page's own, and a later,
different one turns the badge into a reload button.

The header's Settings button opens live concurrency, streams, speed and
download proxy controls. Each job card's gauge button opens that job's own
limits (`JobLimits`): number fields that commit on Enter or blur and are held
to the queue's values, a speed menu offering only speeds beneath the
queue's, and the server's value shown otherwise. Tags on the card say which
limits hold while it is closed. `ProxyList` selects Keep2Share, FileBoom or WAF recovery with the
`site` query and polls `/api/proxies` only while mounted and the
tab is visible; the inventory can contain tens of thousands of routes and
must not be added to SSE snapshots. Filtering, sorting and pagination happen
server-side, in `proxytron/lease`'s `Pool.Page`. Since proxytron v0.1.1 it
keeps only the rows up to the end of the page asked for, and works out each
route's ID and shown address once. Hashing, parsing, building and sorting a
row for every route had taken 230 ms and 74 MB a request in a pool of 63,000,
every five seconds while the list was open. Scores use the selector's deterministic mean estimate, not a
fresh random draw when viewed. Source drafts fetch `/api/settings` separately
and survive failed saves; source credentials never belong in the list or
queue snapshots. Proxy settings last for the session; parallel downloads and
streams are kept with the queue (see Configuration).

`styles.css` is a single design system: one accent gradient, one surface
ramp, one shadow scale, dark and light via `prefers-color-scheme`. Prefer
composing from the tokens at the top over adding new colours.

The page scrolls on the root element and `.shell` is centred inside a 1440px
cap, so the scrollbar's width decides where everything sits. `html` therefore
carries `scrollbar-gutter: stable`: without it, selecting a filter that
resolves to nothing shortens the page below one viewport, the scrollbar
disappears and the entire layout slides sideways.
