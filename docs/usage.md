# Using HeapLeach

[Documentation](README.md) · [Project home](../README.md)

[Command line](#on-the-command-line) · [Queue and disk space](#queue-persistence-and-disk-space) · [Network access](#network-access-and-trust) · [Features](#features)

Common options are listed below. Run `heapleach -help` for every flag, or see
the [configuration reference](configuration.md) for environment variables.

```
heapleach                                     serve the UI and open it
heapleach [options] [download-dir]            serve the web UI
heapleach [options] <url>... [download-dir]   download and exit

  -addr string          listen address; :0 takes any free port (default ":8080")
  -concurrency int      parallel transfers, 1 to -max-concurrency (default 4)
  -max-concurrency int  ceiling on parallel transfers, live setting included (default 256)
  -captcha-concurrency int
                        CAPTCHAs read at once, each in a model session of its own (default 4, at most 64)
  -dir string           directory to download into
  -password string      password for protected sources (headless downloads)
  -retries int          retries per request and per transfer (default 3)
  -streams int          connections to split a slow file across, 1-16 (default 8)
  -slow-speed size      rate below which extra connections open, per second (default 2MB)
  -max-speed size       ceiling on the total download rate, per second (0 is unlimited)
  -min-free size        room to leave at the destination before starting another
                        transfer (0 turns the check off) (default 10GiB)
  -stall-timeout dur    abandon and retry a transfer stuck this long (default 1m30s)
  -debug                verbose logging (default true; -debug=false disables)
  -open                 open the UI in a browser once it is listening
  -version              print the version and exit
```

Sizes take a unit — `5MB`, `1.5GB`, `10GiB` — or a plain byte count.

## On the command line

Hand it URLs and it never opens a socket: the files are downloaded to disk,
progress is printed on the terminal, and the process exits.

```bash
heapleach https://example.com/d/abc123 ~/Downloads
heapleach -streams 8 https://example.com/a/one https://example.com/a/two
cat urls.txt | heapleach - ~/Downloads          # "-" reads a list from stdin
```

URLs and the download directory can come in either order — a URL is anything
with an `http(s)` scheme, so the two can never be confused. Lists read from
stdin ignore blank lines and `#` comments.

It exits `0` when every source was accepted, resolved and downloaded, and `1`
when an input was rejected, a source could not be read, or a file failed.
Failures are named, so it drops straight into a script or a cron job. `Ctrl-C` exits
`130` and leaves the partial files in place; running the same command again
continues them rather than starting over.

Everything the server does, this does: the same extractors, the same
multi-connection transfers, the same resume. Debug logging is enabled by
default, with plain progress lines so logs and the display never fight over
the screen. Use `-debug=false` or `HEAPLEACH_DEBUG=0` for quieter output and
animated progress on a terminal. Pipes, log files and CI always use plain
progress lines.

The download directory can be given three ways. The most explicit wins:

```bash
heapleach /mnt/media                    # argument
heapleach -dir /mnt/media               # flag
HEAPLEACH_DIR=/mnt/media heapleach           # environment
heapleach -concurrency 8 -addr :9000 ~/Downloads
```

It is created if missing, `~` is expanded, and the process refuses to start
if the directory cannot be written to — so a permission problem surfaces
once, up front, instead of as a wall of failed transfers.

## Queue persistence and disk space

The queue itself is remembered between runs, in
`~/.local/state/heapleach/queue.json.zst` (`XDG_STATE_HOME` is honoured, and
`HEAPLEACH_STATE` overrides both). Restarting brings back the list, and
anything unfinished comes back **held** rather than running: press resume, or
retry the one job you want, and it picks up from the part files already on
disk. Nothing is fetched until you say so, so a machine that reboots at
3 a.m. does not start saturating the line on its own.

An unfinished job is read from its source again rather than replayed. That is
not caution but necessity: several hosts sign their media links for twenty
minutes or so, and the ones that do carry no link at all until it is minted
per attempt, so a stored URL would be a dead one. Re-reading gives fresh
links, files already complete are skipped, and part-finished ones resume.

The file is written with mode `0600`, because it lists everything you are
downloading and carries the password for any source that needed one. Given
URLs on the command line the whole mechanism is off: that mode downloads and
exits, and has no queue worth outliving it.

If an existing queue cannot be read, the service logs the reason and disables
queue saving for that run, preserving the original file. Downloads can still
run, but their queue changes will not survive a restart. Stop the service,
back up and repair or move the unreadable file, then restart to enable saving
again. Automatic migration from `queue.json` is limited to the default state
location; an explicit `HEAPLEACH_STATE` never adopts a neighbouring file.

A transfer only starts when at least `HEAPLEACH_MIN_FREE` — 10 GiB by
default — is still free there. Below that the queue **waits** rather than
failing: nothing new begins, transfers already running finish normally, and
the moment room is made the queue picks up by itself with nothing to re-add.
The figure says `downloads held` while that is the case, so a queue sitting
still is never a mystery. Set it to `0` to turn the check off.

The UI shows what is left on that filesystem beside the path, and says it in
red once a tenth or less remains: a queue can be larger than the room for it,
and the useful moment to notice is before the last block goes rather than
after. A destination that cannot be measured shows nothing at all rather than
zero, which would read as a disk with no room left.

## Network access and trust

HeapLeach is a single-user service with no login or API authentication.
Anyone who can reach its API can inspect the queue, submit downloads and
change the destination to a directory writable by the service account.
Submitted sources can reach local network addresses as well as public sites.
Use a trusted network or bind to loopback, for example
`heapleach -addr 127.0.0.1:8080`. The bare desktop launch uses loopback, while
the normal `-addr` default and the container port listen on all interfaces.
For remote access, put an authenticated HTTPS reverse proxy in front and
restrict direct access to the backend port. Browser cross-origin protection
and framing restrictions do not authenticate API callers.

Outbound native HTTP requests honour `HTTP_PROXY`, `HTTPS_PROXY` and
`NO_PROXY`. A proxied request uses Go's standard TLS transport rather than
the browser-shaped handshake. Helper programs use their own proxy support.
The destination directory, optional helper executables and script overrides
must be writable only by people you trust to run code as this account.

## Features

- **Parallel downloads** with a live worker count you can change while
  transfers are running.
- **Multi-connection transfers.** A file that is downloading slowly is split
  across more connections, up to a configurable ceiling. Ranges are chosen by
  bisecting whatever is still outstanding — halfway, then the quarters, then
  the eighths — so the extra connections share the remaining work rather than
  duplicating it. Throughput is averaged over a window before any decision is
  made, so one noisy interval never triggers a change.
- **Host-aware.** Hosts cap how many connections they will accept. Extra
  connections are budgeted per host across all downloads, and a host that
  turns one away has its range handed straight back and its budget lowered,
  so a working download is never failed by trying to go faster.
- **Patient with busy hosts.** Gofile answers a request for a file on a busy
  storage server by redirecting to its own web page. That is detected rather
  than saved to disk, and retried with backoff — rotating through the file's
  other storage servers where it has them — because a busy host has not
  failed, it has asked us to come back. Patience is spent only where trying
  again can change the answer: a URL with nothing to re-resolve, which is
  usually a page no extractor recognised, is told so after a few attempts
  instead of retrying forever.
- **Forgives a dropped connection that was getting somewhere.** A transfer
  that loses its connection after moving bytes resumes from its part file
  thirty seconds later, and that attempt is not counted against the
  retries: the budget counts attempts *in a row* that move nothing, and one
  that moved anything starts the count over. A stall after progress is
  treated the same way. So a CDN that drops every few dozen megabytes still
  finishes a large file instead of failing it three drops in — and since
  every such attempt leaves more on disk than it found, a finite file
  cannot cycle forever.
- **Pause and resume** the queue. Native transfers park inside their reads
  rather than being torn down, so a short pause costs nothing and a long one
  falls back on the same resume every other interruption uses. Already-running
  external helpers continue; pausing prevents new ones from starting.
- **A ceiling on native download throughput**, set from the header or with `-max-speed`.
  It is one shared budget across native connections, not a per-file allowance —
  and the code that opens extra connections knows about it, so a transfer
  held at the ceiling is not mistaken for a slow one and split eight ways for
  nothing.
- **Skips what is already there.** A file whose name and length already match
  the destination is not downloaded again — checked against the length the
  server reports, so it works even for hosts that publish no sizes. Sizes
  read off listing pages are rounded, and are never used to make that call.
  Entries in one listing that share a sanitized destination get distinct,
  stable numbered names, including when their lengths match.
- **Notices a stalled transfer — and steps around it.** A connection that
  stops delivering without closing is invisible to a read timeout; if the
  byte counter has not moved for `-stall-timeout`, the attempt is abandoned
  and the item goes to the **back of the queue** with its partial file
  intact, so a host that has stopped serving does not pin a worker while
  everything behind it waits. Its next turn resumes from disk; a host that
  never resumes still fails the item once the retry budget is spent.
  Playlist downloads are watched the same way, against the bytes actually
  arriving rather than against whole parts landing — a large part fetched
  slowly is progress, a silent connection is not. And a stalled transfer is
  never "helped" by splitting it further: slow means more room than one
  connection uses, stalled means the host is serving nothing, and the two
  get opposite answers.
- **Live progress** over server-sent events: per-file bytes, rate, ETA, and
  parts joined for a file that arrives as a playlist and so has no byte total
  until its last part lands. A transfer waiting on purpose says so, rather
  than looking like one that has died.
- **Connection recovery.** If an event stream opens without delivering its
  first snapshot, the UI retries and polls state in the meantime. A partially
  accepted submission keeps rejected URLs in the form for correction.
- **A searchable queue.** Type `/` and filter hundreds of jobs by title,
  source, host or filename; `Esc` clears. Scrolling is not a retrieval
  strategy.
- **The destination is changeable while it runs.** Click the path in the
  header, type another. It is expanded, created and proved writable exactly as
  a directory named on the command line is, and it says why if it cannot be
  used. Transfers already running keep the destination they started with —
  their path was settled when they began, and a part file that moved
  mid-flight could not be resumed — so the change takes hold from the next
  queued file onward.
- **Cancel and retry** a whole job or a single file.
- **Clear finished** removes jobs from the queue only when every file is
  complete and the source has no errors. Failed, canceled and incomplete
  jobs stay available to inspect or retry. Downloaded files stay on disk.
- **Resume** — partial files are kept and continued with a `Range` request;
  a cancelled 100 MB download restarts where it stopped.
- **Password-protected folders** (gofile and filester). Open **Password
  protected?** when adding links and enter the folder password; headless runs
  use `-password`.
- **Safe filenames** — remote names are reduced to one portable path
  component, so nothing can be written outside the download directory.
- A light **progression layer**: ranks, session totals and a few badges.

### Image boards

Boards sharing an API are covered by one adapter each, in the style
gallery-dl uses:

| Family | Boards |
|---|---|
| Danbooru | aibooru, booruvar |
| e621 | e621, e926, e6ai |
| Moebooru | yande.re, konachan, sakugabooru |
| Gelbooru 0.2 | safebooru, tbib, hypnohub, xbooru |
| Philomena | derpibooru, ponybooru, furbooru, twibooru |
| szurubooru | snootbooru, foalcon |
| Gelbooru 0.1 | the booru.org network, one board per subdomain, scraped — it has no JSON API |

Paste a tag search or a post link. A listing with no tags fetches the
board's latest posts; a bare domain is rejected, since that is far more
likely a mis-paste than a request for everything.

Every board above was checked against its live API. Ones that now demand an
API key (gelbooru.com), sit behind a challenge (danbooru.donmai.us) or have
switched their API off (realbooru) are deliberately absent rather than
listed as supported and quietly broken.

## Operating notes

- The queue is written to `HEAPLEACH_STATE` every ten minutes when it has
  changed, and on every clean shutdown, so a restart finds its unfinished
  jobs held and re-reads them on retry; finished files stay put. A crash
  can lose queue changes from the last ten minutes: the part files, not
  the queue file, are what resume a transfer. A `.part` file carries a `.part.state` sidecar recording
  per-connection progress, so an interrupted multi-connection transfer
  resumes rather than starting over. Both can be deleted safely.
- These sites change their plumbing without warning. `make test-live` is the
  fastest way to find out which extractor broke when the ignored live test
  file and its environment-provided sources are present. A fresh clone has
  fixture tests only; a green suite alone does not verify live host support.
- Be a good citizen: the defaults are deliberately modest, and the client
  honours `Retry-After` and backs off on 429s.
