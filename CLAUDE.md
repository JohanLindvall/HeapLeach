# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`HeapLeach` is a bulk downloader: a Go backend with per-host extractors and a
parallel worker pool, plus a React + TypeScript UI that is **compiled into
the Go binary**. The shipped artefact is one static executable.

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
make dependencies   # fetch static yt-dlp and ffmpeg into ./bin (see tools.Find)
make dist           # cross-compile the release archives into ./dist
make tag            # cut a release: make tag V=v1.2.3 — CI builds and publishes
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

Frontend tests are vitest over the pure logic (`format.ts`,
`gamification.ts`) — `make test-frontend` runs them, in Docker when npm is
absent, and CI runs them after the build. The formatBytes cases mirror
`internal/cli/cli_test.go` digit for digit on purpose: the two sides render
the same numbers, and the paired tables are what hold them together.

Node is **not** installed on this machine — the frontend builds via Docker.
`make build` and `make frontend` both handle that automatically.

Toolchain versions live in the Dockerfile (Go 1.27, Node 24 LTS, Alpine 3.24)
and in `backend/go.mod`. The local Go is older than the `go.mod` directive, so
`go` commands auto-download the pinned toolchain on first use.

### Committing

Work goes straight to `main`: commit and push there rather than opening a
branch or a pull request, and without asking first. That is this repository's
settled workflow, not an oversight.

### Releases

`make tag V=v1.2.3` writes an annotated tag and pushes it;
`.github/workflows/release.yml` fires on `v*`, cross-compiles the five
archives and publishes them alongside a `SHA256SUMS`. The target refuses a
dirty tree, which is what makes step 3 below necessary rather than tidy.

Worth doing before every tag, because each step has caught something:

1. `gh run list` — CI green on the **exact commit** being tagged, not merely
   somewhere on the branch. Check who authored anything unfamiliar in
   `git log <last tag>..main` rather than assuming it is yours. Read the
   plain list rather than filtering: `gh run list --commit <sha>` prints
   nothing when it matches nothing, which looks exactly like a commit CI
   never ran. An empty result is not evidence of a green build.
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

### No live URLs in the repository

Nothing tracked may contain a link to a specific piece of content — no
video pages, albums, profiles, threads or signed media URLs. This applies to
code, tests, README.md, this file, and commit messages alike. Such links go
stale, and a downloader's repository is not the place to catalogue them.

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

`.gitignore` carries `*live_test.go` to keep the live tests out. Before
committing, it is worth checking that nothing new slipped in:

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
  └─ httpx  └─ extractor  └─ download  └─ server, cli
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

Two traps that cost time here:

- `retryAfter` returns `(time.Duration, bool)`. It has to separate "the
  header says 0" from "there is no header", because a stated delay is
  authoritative while an absent one means back off. Returning a bare
  duration made every fixture sit out the full backoff.
- Tests whose fixtures serve a **permanent** 429 now sit out that patience
  too, and burn the whole budget unless the fixture states `Retry-After: 0`.
  A suite that suddenly takes a minute instead of three seconds is this, not
  a hang.

### Extractors

`extractor.Extractor` is `Name/Match/Extract`. `Registry.Find` walks the
registered hosts and falls back to `Direct`, which matches everything — so it
**never returns nil**. Register new hosts in `NewRegistry`.

The supported-site table in README.md is **generated** from the registry by
`make hosts`, between two markers, and `make hosts-check` fails CI when the
file and the code disagree — so edit the extractor, never that table. Adding
a host is then one line in `NewRegistry` plus `make hosts` in the same
commit. Note the optional half of the contract: `SiteLister` may be skipped
only when an extractor's `Name()` *is* its domain, since the catalogue then
uses the name as the site. An extractor named for the site without its TLD —
"eporner" for `eporner.com` — has to implement `Sites()`, or the inventory
quietly advertises the label as the host. `hosts-check` compares the
regenerated file against git, so it also fails on a correct regeneration that
has not been committed yet.

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

`File.Cipher` is the other addition to the contract: a host that serves
ciphertext sets it, and the downloader decrypts on the way to disk. Only
mega does today, and see the download-manager section for why the mode
matters.

Host-specific notes:

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
- **vimeo** goes through the embed player, and that single choice is what
  makes it work: `vimeo.com/<id>` answers a non-browser client with a bot
  check, the player's JSON config endpoint answers 403, and yt-dlp's own
  route through the watch page demands an account — while
  `player.vimeo.com/video/<id>` is served to anyone, no session or referer
  needed. The stream is demuxed and carries no progressive MP4, so there is
  nothing to fetch directly that would be playable; the player URL is handed
  to yt-dlp, which has ffmpeg to mux with. An unlisted link's `<hash>`
  becomes `?h=`.
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
- **coomerfans** is the only extractor that fetches concurrently. A creator's
  page lists posts but keeps the media on each post's own page, and the
  listing cannot be worked from instead: a post holding a video shows no
  thumbnail there at all. So every post is opened, bounded by
  `config.PageFetchConcurrency`, with results collected by index so the job
  lists files in the creator's own order however the requests interleave. A
  post that will not load is skipped rather than failing a job of hundreds.

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
- **streamtape, doodstream and mixdrop** are the most fragile extractors
  here, because each depends on the shape of a script the host can change.
  Each therefore reads the numbers it needs off the page — the substring
  offset, the packer's base and word list — rather than hard-coding what
  they were. `unpackJS` reverses the p,a,c,k,e,d packer and is host-neutral.
  Their `Extract` bodies, and those of xhamster, tnaflix and pornone, are all
  the same shape — one file, re-read at download time because the link is
  signed per visit — so they share `refetchedVideo` (`tube.go`) and supply
  only the per-host fetch.
- **suvobox** resolves an album from the listing alone, which is unusually
  generous: each tile carries the file id, the full name *with* extension and
  the size, so a whole album costs one request and shows real names while it
  queues. The bytes come from the media host's `?raw=1`, which takes no token
  and honours ranges; the site's own download button points at a signed
  `?dl=1` link, which is worth knowing only to say why it is not used — it
  would expire while an item waited its turn and buys nothing.
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

**Locking rule:** every `Job` and `Item` field except `Item.downloaded` is
guarded by `Manager.mu`. `downloaded` is atomic so the progress ticker can
sample a running transfer without contending with workers. Workers take `mu`
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
Three consequences worth keeping in mind:

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

**The queue outlives the process** (`persist.go`, `state.go`). Written every
`config.StateSaveInterval`, atomically and 0600, and skipped entirely when
nothing but byte counters has moved — the fingerprint covers identities and
statuses, never progress, because the part file on disk is the authority on
how far a transfer got.

Three things here were each got wrong first:

- **No item URL is stored.** Signed links expire in minutes, and a `Resolve`
  host has no URL until one is minted — it is a closure, which no file holds.
  So `Restore` marks an unfinished job `restored` and its source is read
  again on resume; `alreadyOnDisk` skips what is complete and the part files
  carry the rest.
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
the same rule workers do. `diskSpace` is split per platform beside
`process_unix.go`; the unix half reads `Bavail`, not `Bfree`, since the
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

**External transfers** (`external.go`, `internal/tools`) cover pages where
reaching the media needs more than HTTP. `tools.Find` resolves a helper
binary next to the running executable before falling back to PATH, which is
what makes `make dependencies` work without a system install. It caches
misses as firmly as hits, so `RetryJob` and `RetryItem` call `tools.Recheck`
first: being told a helper is missing is what sends someone off to install
it, and a service that went on insisting it was absent turned a working
install into a puzzle. Hits are kept — a path that resolved once does not
stop existing. Note that a *newly added* job still meets the cached miss;
only retrying looks again. The download
runs through an embedded shell script, overridable by a copy next to the
binary; it reports `PROGRESS`/`FILE` lines that are folded back into normal
item state. Its **format selector keeps every audio language**, not just the best one.
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
  item.
`remux.go` is a convenience on top, never a dependency — the extractor picks
a rendition that already carries audio and video together. It converts any
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

- Transfers resume from `<name>.<itemID>.part` using a `Range` request. The
  part file is keyed by item id so two jobs downloading the same filename
  cannot collide.
- A stall watchdog aborts an attempt whose byte counter stops moving
  (`config.StallTimeout`); body reads have no deadline of their own, so
  without it a silent server would pin a worker forever. The aborted item is
  **not retried in place**: `deferStalledLocked` sends it to the back of the
  queue with its part file intact, so a host that has stopped serving does
  not hold a worker slot while the queue waits — bounded by the retry
  budget, past which the next stall is a failure, so the headless run still
  terminates. Its next turn resumes from disk. Relatedly, the stream
  supervisor treats stalled and slow as opposites: a probe window in which
  no bytes arrived opens no extra connection, and discards any pending
  before/after verdict rather than reading the stall as "parallelism made it
  slower" — a verdict that would penalise the host's budget for the rest of
  the process over a condition the host imposed on everyone.
- A master playlist's `CODECS` describes the whole presentation, not the
  variant's own segments, so a variant that names a separate `AUDIO` group
  advertises audio it does not carry. `hlsVariant.muxed` therefore checks
  the group's renditions for a `URI` of their own rather than trusting
  `CODECS`; without that a demuxed host yields a silent file that looks like
  a finished download. A host with no self-contained variant at all belongs
  on the external downloader.
- `SafeName`/`SafeRelPath` reduce untrusted remote names to one portable path
  component. Nothing may be written outside the download root.

### Headless mode

URLs on the command line switch `cmd/heapleach` out of server mode entirely: no
listener, no browser, no assets. `internal/cli` drives the same `Manager`,
polls `Manager.Snapshot()` on its own ticker rather than subscribing, and
returns once every job is terminal.

Four things are easy to break:

- **The display owns stdout.** A headless run sends its log to *stderr* at
  warn level, because a log line written into the live region corrupts the
  frame. `-debug` turns the animation off entirely for the same reason.
- **One painted line must be one screen row.** The live region is repainted
  by counting rows and moving the cursor up that many times. Every line is
  truncated to the terminal width first — counting printable cells, not
  bytes, so colour escapes do not eat the budget. The moment a line wraps,
  the arithmetic is wrong and the display starts eating itself. The width is
  re-read every frame (`Options.TermWidth`), because a terminal that shrank
  mid-run would otherwise wrap every line from then on.
- **A job that resolves to nothing still has to end the run.** It reports
  `queued` forever because it has no items to finish; `finished()` treats a
  non-resolving job with zero items as done.
- **Byte counts are SI and must match the web UI digit for digit** — the
  same transfer must not read "1.5 GB" in the terminal and "1.6 GB" in the
  browser. `cli.formatBytes` mirrors `frontend/src/format.ts` deliberately;
  change them together or not at all.

### Server and shutdown

The API is `net/http` with Go 1.22 method+pattern routing. `/api/events` is a
server-sent-events stream carrying a **complete state snapshot** each tick —
a client that misses a frame self-heals on the next one. Everything in that
payload is rendered by the UI: it once carried the host names for a list that
only restated what the build supported, and now carries `hostCount` alone,
which is all the progress panel reads.

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
the UI does no derivation. Everything the snapshot carries is rendered
somewhere: `note` explains a deliberate wait, and `segmentsDone`/`Total` are
the only progress a playlist has, since it has no byte total until its last
part lands. A field the UI stops reading should come out of the payload
rather than being left to accumulate. `gamification.ts`/`useProgress.ts` add a progress
layer persisted in `localStorage`; it is derived from bytes actually written.

`styles.css` is a single design system: one accent gradient, one surface
ramp, one shadow scale, dark and light via `prefers-color-scheme`. Prefer
composing from the tokens at the top over adding new colours.

The page scrolls on the root element and `.shell` is centred inside a 1440px
cap, so the scrollbar's width decides where everything sits. `html` therefore
carries `scrollbar-gutter: stable`: without it, selecting a filter that
resolves to nothing shortens the page below one viewport, the scrollbar
disappears and the entire layout slides sideways.
