# Configuration, server and headless mode

`internal/config`, `internal/server`, `internal/logstore`, `internal/cli` and
`cmd/heapleach` in `backend/`.

## Configuration

`config.FromEnv()` reads the environment with **no filesystem side effects**;
`(*Config).Prepare()` validates, expands `~`, creates the directory and proves
it writable. The split exists so `main.go` can layer CLI flags on top before
anything is created: otherwise loading config would create whichever directory
the environment named, even when an argument overrides it.

Download directory precedence: positional argument → `-dir` → `HEAPLEACH_DIR`.

A setting whose empty value means something of its own (`HEAPLEACH_STATE`
empty turns persistence off) is read with `envSet`, which tells set from
unset; the rest treat empty as absent. Numbers, sizes, durations and switches
are one table in `FromEnv`, each row an `envInto(name, &field, parse)`; a
new one is a row, and its error names the variable like the rest. Proxy protocols are checked in one
place, `config.CheckProxyType` and `config.CheckProxyTypes`, for the
environment, flags and `/api/settings` alike, so all three refuse the same
values, and say why in the words `/api/settings` answers with.

**The ceiling on parallel transfers is a setting**
(`HEAPLEACH_MAX_CONCURRENCY`, `-max-concurrency`, default
`config.DefaultMaxConcurrency`, 256; zero means the default). It bounds the
startup concurrency and the live setting, and reaches the Settings slider as
the snapshot's `maxConcurrency`, through `Manager.maxConcurrency`, which also
gives a config-less test manager the default. It is high because proxy routes
let one process carry many transfers, each address taking its own per-address
download. A connection fast enough to need it buffers up to `WriteBufferSize`
(1 MiB) before writing, so 256 fast files split eight ways can hold about 2
GiB of buffers at worst; slow ones keep the 64 KiB they start with
([transfers.md](transfers.md)).

Settings documented with what they govern:

- listing caps (`HEAPLEACH_MAX_SOURCES`, `HEAPLEACH_MAX_FILES`):
  [extractors.md](extractors.md)
- the free-space floor, `HEAPLEACH_RESUME`, `HEAPLEACH_STATE`, and the
  settings and job limits saved with the queue: [download.md](download.md)
- the proxy pool and its protocols: [proxies.md](proxies.md)
- `HEAPLEACH_CAPTCHA_CONCURRENCY` and `HEAPLEACH_OCR_DIR`:
  [hosts/keep2share.md](hosts/keep2share.md)
- `HEAPLEACH_GOFILE_SECRET`: [hosts/file-hosts.md](hosts/file-hosts.md)

`main.go` calls `net.Listen` itself rather than `srv.ListenAndServe`, so an
address of `:0` can be resolved to the port the kernel actually assigned and
then logged and opened (`-open`). Picking a free port outside the process
would be a guess that could race another binder.

## Logs

The service's text logger also writes into `internal/logstore` before startup
work begins, preserving job and item fields and the configured log level. The
buffer keeps at most `config.ServiceLogEntries` records (1,000) or
`ServiceLogBytes` (4 MiB), with `ServiceLogEntryBytes` (16 KiB) per record and
UTF-8 boundaries preserved. Headless mode has no history buffer. `/api/logs`
copies entries under the buffer lock and encodes them afterwards, so a browser
never holds that lock while reading. Session IDs distinguish restarts, and
cursors before retention or ahead of the current run reset the client's
window. Keep log reads out of API request logging, or the viewer generates its
own endless traffic. The Logs panel is in [frontend.md](frontend.md).

## The API and its event stream

The API is `net/http` with Go 1.22 method+pattern routing. `/api/health`
reports the build's `version`, so a deploy tool can tell a new process from
the one it replaced. `/api/events` is a server-sent-events stream starting
with a **complete state snapshot** and then carrying changed item rows. A
dropped patch is replaced with a complete snapshot immediately, including when
the queue has just become idle. Everything in the payload is rendered by the
UI; the host inventory, for one, travels only as `hostCount`, which is all the
progress panel reads.

**A snapshot is complete for what the client is showing, which is not the same
as complete.** Sent whole, a queue of a couple of thousand files is most of a
megabyte a frame, to paint a few dozen rows. Four things keep the stream
small, and they are worth knowing apart because they fail differently:

- **Fields nothing reads are not sent** (`ItemView`). The item URL is left
  out: a signed storage link is a couple of hundred characters, one per item,
  the largest thing a frame would carry, and the browser never looks at it.
  `Elapsed` is the terminal's, read from a `Snapshot` in process. A field the
  UI stops reading comes out of the payload.
- **Only the rows that changed are sent** (`Manager.patchLocked`,
  `frontend/src/snapshotMerge.ts`). Almost nothing in a queue changes from one
  second to the next: a thousand finished files say exactly what they said
  before. So a frame carries every job whole — its counts, its size, its rate,
  all of which are small — and inside it only the items whose wire view
  differs from the one last broadcast, marked `patch`. The browser merges them
  into what it holds, so the search, the progress panel and the cards all
  still see complete lists and none of them knows the wire is thrifty. The
  rules that keep that self-healing are below.
- **The stream is compressed** when the client asks (`compressed` in
  `sse.go`). A frame is mostly the same words as the last one, so it packs
  about six to one. Both layers have to be flushed per event, in order, or the
  browser sees nothing until the next frame pushes the last one out.
- **A frame is skipped when nothing moved** (`config.IdleFrameInterval`).
  `sampleLocked` reports whether any byte counter actually advanced, which is
  what separates a queue that is working from one that is merely open — a
  thousand items held behind a throttled host look identical from the
  broadcaster's side. Those refresh on a slower beat rather than not at all,
  since the rates decay towards zero and a viewer should see that.

Together these bring a steady queue's stream down to a couple of kilobytes a
second. The whole state is still sent once per connection, which is what a
browser arriving has to be told.

### Keeping patches correct

Each of these is a way a merge could otherwise be wrong, or a way the saving
quietly evaporates:

- A job whose list **lost** rows is sent whole, because a merge only adds and
  replaces and has no way to say a row is gone. A list that merely grew is a
  patch carrying the new rows, which is the common case while a large album
  resolves — items arriving a page at a time — and exactly when sending the
  whole list again costs most. A list where *every* row is new is sent whole
  too: that is either first sight or a wholesale replacement, and a re-read
  drops the items and resolves them again, which can land between two frames
  and leave the count unchanged while nothing else is.
- A new subscription is answered with the whole state, handed back by
  `Subscribe` itself so that registering and having something to merge into
  are one act. It records that snapshot as sent, or the next broadcast would
  find every row unlike the nothing it was compared against and send the lot a
  second time; and it marks every other subscriber stale, since what they were
  last told has just been recorded as delivered.
- A frame dropped on the way to a slow reader is replaced by the current whole
  snapshot in `publishLocked`. Recovery cannot wait for another broadcast: the
  dropped patch may have completed the last active job.

### When frames go out

Frames go out at most once a `config.FrameInterval` (a second), whatever else
is happening. A job being read for the first time marks the state changed on
every tick as its items arrive, and letting that bypass the ceiling would send
a frame on every tick. The dirty flag is read rather than taken when a frame
is skipped, so a frame that is not sent cannot swallow the change that would
have justified the next one.

**Except for what the user just did.** Every user action — add, cancel, retry,
remove, clear, pause, a setting — calls `Manager.nudge`, which sends a frame
at once, past the ceiling. A row that sits unchanged for most of a second
after a click reads as the click not having taken. It is only for actions:
anything that happens by itself goes out on the ordinary beat, which is what
keeps the stream cheap.

## Profiling

**Profiling is opt-in and never on the API's port** (`HEAPLEACH_PPROF`,
`-pprof`, `cmd/heapleach/pprof.go`). The handlers get a mux and a listener of
their own, started before anything else so a headless run can be profiled too;
the API listens on every interface, and profiles describe the process from the
inside. Importing `net/http/pprof` also registers on `http.DefaultServeMux`,
which is harmless only because nothing serves it — keep it that way. The
service under contdep sets it to the container's own `127.0.0.1:6060`, reached
with
`docker exec heapleach wget -qO- 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'`.

## Shutdown

**Shutdown ordering matters.** `http.Server.Shutdown` waits for in-flight
requests and does *not* cancel their contexts, so an open SSE stream holds it
until the deadline. `main.go` calls `manager.Close()` **before**
`srv.Shutdown()`; the manager closes subscriber channels, which ends the
streams. `Manager.Close` is idempotent, so the `defer` stays safe. What
`Close` saves, and why it saves before stopping, is in
[download.md](download.md).

## Headless mode

URLs on the command line switch `cmd/heapleach` out of server mode entirely:
no listener, no browser, no assets. `internal/cli` drives the same `Manager`,
polls `Manager.Snapshot()` on its own ticker rather than subscribing, and
returns once every job is terminal.

Four things are easy to break:

- **The display owns stdout.** A headless run sends its log to *stderr*. Debug
  logging defaults on and uses plain progress lines. `-debug=false` or
  `HEAPLEACH_DEBUG=0` restores warning-level logs and terminal animation,
  because a log line written into the live region corrupts the frame.
- **One painted line must be one screen row.** The live region is repainted by
  counting rows and moving the cursor up that many times. Every line is
  truncated to the terminal width first — counting printable runes, not bytes,
  so colour escapes do not eat the budget. This approximates cell width; wide
  characters and grapheme clusters still need better handling. Remote fields
  pass through `terminalText` before our own colour escapes are added, so they
  cannot inject controls or new rows. The moment a line wraps, the arithmetic
  is wrong and the display starts eating itself. The width is re-read every
  frame (`Options.TermWidth`), because a terminal that shrank mid-run would
  otherwise wrap every line from then on.
- **A job that resolves to nothing still has to end the run.** It reports
  `queued` forever because it has no items to finish; `finished()` treats a
  non-resolving job with zero items as done. A failed source with no items
  still makes the summary fail, as does any rejected command-line input: zero
  failed files is not proof that every requested source succeeded.
- **Byte counts must match the web UI digit for digit**: `cli.formatBytes`
  mirrors `frontend/src/format.ts` (see the conventions in
  [AGENTS.md](../../AGENTS.md)).
