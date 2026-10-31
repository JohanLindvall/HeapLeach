# Media helpers, CAPTCHA recognition and proxies

[Documentation](README.md) · [Project home](../README.md)

[Media helpers](#media-helpers) · [CAPTCHA recognition](#local-captcha-recognition) · [Proxy settings](#configuring-proxies) · [External downloader limits](#external-downloader-behavior)

## Media helpers

YouTube and hosts marked "Needs `make dependencies`" in the
[supported-link reference](supported-links.md) are fetched by `yt-dlp`,
which is asked for **every audio language a video
carries** rather than only the one it ranks first: a dubbed release puts its
original track at the top, so the English beside it would otherwise be
dropped. The languages arrive as separate tracks in one file, labelled, for
the player to choose between. `ffmpeg` rewraps any finished `.ts` as `.mp4`, joins
the audio tracks a playlist keeps apart from its video, and lets yt-dlp merge
separate video and audio tracks. `deno` runs the player
JavaScript YouTube signs its media URLs with: yt-dlp still extracts without
a runtime, but warns that doing so is deprecated and that formats may be
missing. Everything else needs none of them.

On Linux, from a source checkout:

```bash
make dependencies    # yt-dlp + ffmpeg + deno + local CAPTCHA reader into ./bin
```

On macOS, install the media helpers through your package manager and put them on `PATH`; the `make dependencies` media downloads target Linux. For Windows, use the Docker image for workflows that need the POSIX helper script. Native file downloads do not need that script.

The service looks for these **next to the heapleach binary first**, then on
PATH — so the copies in `./bin` are picked up without touching the system.
That is also why deno is passed to the helper by path: yt-dlp finds one on
PATH by itself, and `./bin` is the place it would not look.

## Local CAPTCHA recognition

Keep2Share and FileBoom free downloads read CAPTCHAs inside the Go process,
using the [ddddocr](https://github.com/sml2h3/ddddocr) beta recognition model
and ONNX Runtime's native CPU library. Recognition happens locally, without
Python, a helper process, an account, API key or paid solver. Release archives
bundle the model, runtime and license notices in `ocr/` beside the executable.
The Docker image includes these files and their system libraries; it makes no
downloads to set up OCR when it starts.

For a source build, `make ocr-assets` installs the same pinned, checksum-verified
files into `bin/ocr`; `make dependencies` includes this step. `make captcha-helper`
is retained as an alias. `HEAPLEACH_OCR_DIR` can name another asset directory
and is set by the development commands. See
[helpers/captcha/README.md](../helpers/captcha/README.md) for packaging details.
By default, four sessions read images at once (`-captcha-concurrency`), with two inference threads each and
spinning disabled. Models are reused and released after five idle minutes.
Cancellation terminates native inference; waiting for a session is cancelable.

## Free downloads and proxy routes

The free flow tries at most three CAPTCHA images, and at most three readings
of each, ranked by the reader's confidence. It shows the host's waiting
time, and allows one file and one connection at a time per service, shared
across its aliases. Unreadable challenges fail with a retryable error; OCR is
not always correct. A download link is bound to the address that first uses
it, so when a route's address changes partway through a file, the link is
replaced and the file resumes from what it has: through another proxy, or
from the direct address with a new link. A file that has started downloading
keeps its place in the queue when a route fails it, so it gets the next
working route before files that have not started. With proxies enabled (the default), the one-file limit
applies to each proxy address,
allowing several free downloads at once without a subscription, up to
`-concurrency`. Each address may carry one Keep2Share transfer at a time,
FileBoom's included, since the two hosts are one service. The hosts' speed
limits and cooldowns still apply per address and host. A proxy port that has never
been tried counts as free whatever the other ports on its address are
doing, since gateway ports often leave through addresses of their own;
once tried, it shares its address's limits. Two downloads never leave through
the same public address: each route is asked which address it leaves by
before its first CAPTCHA, through an IP-echo service, and a route whose exit
another download holds, or the host is holding back, is passed over. The same
question shows whether the proxy works at all: one that answers with anything
but an address is set aside before it costs a CAPTCHA. If the echo services
also fail over the normal connection, downloads proceed with an unknown exit
until those services recover. The shared Proxytron prober accepts plain-text
and JSON IP answers while retaining the route's client and browser identity.
Canceling a probe does not penalize the route. Each route presents a
browser of its own, drawn from over thirty real desktop User-Agents, and an
address the host refuses outright is left alone for an hour. When routes would otherwise
go unused, a slow file also reads through extra routes, up to the streams
setting, each with a free download and a speed cap of its own. That happens
only while no other file of the service is waiting to start, so it speeds up
the end of a queue without slowing the rest. A file held
back by the wait between free downloads goes back to the queue as waiting,
with the time left, and its download slot goes to other files meanwhile.
Waiting is cancelable and bounded to two hours per attempt; retrying within
the same process preserves an accepted ticket and reuses unexpired download
links.

## Runtime settings

Open **Settings** in the UI to change files downloaded at once, streams per
file, and the speed limit. A job's gauge button sets **limits for that job**
beneath those: how many of its files download at once, how many streams each
may use, and how fast it may go. Files set to 0 holds the job's new downloads
while those running finish. These limits are kept across restarts. The same panel enables or disables **download proxies**
and edits proxy endpoints and discovery feeds without restarting HeapLeach.
These settings apply to the current session; flags and environment variables
set the startup defaults. Active transfers keep their assigned route. When
switching routing modes, a service's existing transfers finish before new
ones use the new mode, preserving the per-address limit and download tickets.

The proxy list shows the selection score, measured average and current
throughput, recent success rate, request count, active transfers, cooldowns,
and last successful request. Choose Keep2Share (FileBoom included), EMLOAD, MEGA or **WAF recovery**
to view the corresponding scores. The WAF view also shows challenge success
separately from ordinary request success. Search, status filters and sorting work across
the entire inventory, with 50 rows per page. Scores are estimated useful
bytes per second using the selector's mean success probability; actual
selection also explores untried routes. The list refreshes every five seconds
while open. Discovery status is shown with each feed.

## Configuring proxies

Free proxy routes are enabled at startup. Disable them with `-proxies=false`,
`HEAPLEACH_PROXIES=0`, or the Settings switch. The default pool combines the
normal outbound connection with
[Proxifly's free proxy feed](https://github.com/proxifly/free-proxy-list).
HTTP, HTTPS and SOCKS5 endpoints are supported; SOCKS5 resolves destination
names through the proxy. Plain `host:port` entries mean HTTP. Set
`HEAPLEACH_PROXY_ENDPOINTS` to a comma-separated list of explicit endpoints,
including `direct` if the normal connection should participate, and
`HEAPLEACH_PROXY_FEEDS` to plain-text or Proxifly JSON feed URLs. An explicitly
empty feed setting disables discovery; an empty endpoint setting excludes
the normal connection. Explicit proxies take precedence over `NO_PROXY`.

The routes can be held to one protocol: **SOCKS only** or **HTTP only**
(HTTP and HTTPS proxies), for every service at once or for Keep2Share,
EMLOAD, MEGA or WAF recovery alone, under **Proxy protocol** in Settings or with
`HEAPLEACH_PROXY_TYPE` and `HEAPLEACH_PROXY_TYPES`. Either one leaves the
normal connection out, and a service with no route of its protocol waits
for one rather than using another. Each service's list then shows only the
routes it may use. Choices made in Settings are kept across restarts, until
the configured value they replaced is changed.

Each transfer attempt keeps its route through the CAPTCHA, ticket, redirects
and file requests. Keep2Share and FileBoom API calls and CAPTCHA image
requests, EMLOAD share-page requests and MEGA link requests have a 20-second timeout. A network timeout penalises and cools the
route before another is tried. Status notes show the connection attempt
number on retries, separately from the CAPTCHA image count.
A refused or broken route returns the file to the queue to try another;
other hosts continue downloading while routes are busy or cooling down.
Keep2Share and FileBoom require the bundled OCR data. A file the site keeps
for premium users fails at once, saying so, before any CAPTCHA is spent or
any route is set aside for it.

MEGA counts a transfer quota per address. Its files use the normal
connection, with the usual files at once and streams, until mega says that
quota is used up: it names the time left when a link is asked for, or
refuses a transfer with 509. The files then go on through MEGA's proxy
routes, one file to an address. A route whose own quota runs out rests until
it is back, and its file moves on to another at no cost to its retries. With
proxies off, the files wait in the queue for the quota instead.

Every host using HeapLeach's HTTP client can recover through proxies after
a Cloudflare WAF challenge, including unregistered pages, direct files and
Doodstream/Playmogo. Ordinary requests use the normal connection first.
Challenges during extraction retry the source operation; challenges during
downloads return the file to the queue. A route change re-reads the source
to obtain fresh signed links and keeps the resolver, cookies and media on
the chosen route. Attempts use the configured proxy retry budget, and
waiting for an extraction route is bounded by the request timeout.

Doodstream also switches to this proxy pool after temporary network/HTTP
failures or repeated `RELOAD` replies from its token endpoint. It tries the
normal connection first. Each retry refreshes the player and token together
on one reserved route, using the same random draw for unmeasured candidates
as Keep2Share. Source discovery and file downloads both skip dead or dropped
proxies without spending the file's retry budget, including timeouts waiting
for a proxy connection, and proxies that answer but cannot carry the request:
ones that want credentials, refuse to open a tunnel, speak plain HTTP where
TLS was expected, or present a certificate that does not verify. Response-header timeouts also penalise the proxy and
retry another route, within the configured retry budget. User cancellation
does not penalise a route. An address the host refuses the player to (a bare
"removed or temporarily unavailable" sentence where the player should be,
which other addresses are not shown) is skipped the same way, and the file
tries another. This also applies when a file discovered in a folder is
refreshed through a new proxy. Missing videos and parser errors remain final.
Progress messages show the player/token stage and connection attempt in the
browser and terminal, including while the source is still being resolved.

WAF refusals affect only the WAF success score and cooldown. They do not
reduce ordinary service scores, erase speed measurements or mark a proxy
broken. WAF selection still uses ordinary reliability and measured transfer
speed, excluding user-throttled samples and free-host caps when estimating
unrestricted capacity. Keep2Share, EMLOAD and MEGA retain their per-service
address limits even when WAF recovery is needed.

This tries other addresses; it does not solve interactive challenges.
With proxies disabled, a challenge remains an error. External downloaders
such as yt-dlp manage their own HTTP requests and are outside this recovery
path.

## Route selection and persistence

The pool adapts [amzscrape's proxy logic](https://github.com/JohanLindvall/amzscrape)
and stores inventory, source memberships, request outcomes, measured **bytes
per second**, and cooldowns in a private bbolt database. Faster, reliable
routes score higher; bodies under 64 KiB do not train bandwidth, so a quick
CAPTCHA response cannot stand in for a fast file transfer. Recent measurements
carry more weight. Refusals cool only that service; connection
failures cool the endpoint across services. HTTPS proxies may use self-signed
certificates on the connection to the proxy. Destination certificates are
still verified inside the tunnel. An explicit proxy never silently falls
back to a direct connection.

The scheduler maximises **expected useful bytes per second** within the live
file-concurrency setting and each service's one-file-per-address rule:

1. For each free worker, score available routes for the next file's remaining
   bytes `B`: `p × B / (setup + B / speed + (1 − p) × recovery)`.
   `p` is sampled from recent success/failure evidence, `speed` is measured
   throughput, and `setup` includes CAPTCHA and ticket waits. Unknown setup
   starts at 30 seconds; unknown speed uses the pool's learned average.
   A healthy preferred route can reuse the file's existing ticket. The UI
   shows the deterministic mean score for a 32 MiB transfer; an actual
   dispatch uses that file's remaining size.
2. Update speed from useful-byte windows every five seconds once at least
   64 KiB has arrived. Resumed bytes are excluded. Pause and speed-limit
   periods do not train throughput or setup estimates. These live samples
   update both route selection and the proxy list before the file finishes,
   and the background task saves them to BoltDB.
3. Give unmeasured routes one shared exploration opportunity, choosing its
   candidate uniformly at random from the available unmeasured endpoints.
   Busy and cooling addresses are excluded before the draw; selection and
   reservation are atomic. While measured routes are free, allow at most
   one active unmeasured transfer and space exploration starts by 30 seconds.
   If no measured route is available, use idle workers to discover capacity
   instead of leaving them unused.
4. Reassess slow transfers after 20 seconds of samples. A proven alternative
   must predict at least 25% higher useful throughput, save at least 15 seconds
   after setup and recovery costs, and win two checks ten seconds apart.
   Switch only after the server has confirmed range support and at least
   4 MiB remains. Reserve the new address first, stop and flush the old
   attempt, then obtain a ticket on the new route and resume the same file.
   An unknown route never displaces a working transfer just for exploration.

This is an adaptive estimate: public proxy capacity changes and cannot be
known in advance. Setup costs, confirmation windows and ticket affinity keep
small, noisy speed changes from spending time on repeated route switches.

Saved inventory is available immediately after restart. Feed requests,
including GitHub, are normally 24 hours apart. An earlier top-up requires
runnable Keep2Share, EMLOAD, MEGA or WAF recovery work and too few distinct usable addresses for its current concurrency, with
at least five minutes between attempts. New active transfers count while
their first speed sample is pending; measured and standby routes must have
succeeded and score at least as well as an untried route's learned prior.
Throughput, reliability and cooldowns therefore matter,
not the raw list size. An idle or paused queue cannot trigger an early reload.
The refresh schedule survives restarts. A failed feed
keeps its last good list and refreshes preserve health. Entries absent from
feeds are retired after 30 days without success; current feed entries and
explicit endpoints are retained. Public proxy availability and bandwidth vary,
so extra parallel downloads depend on finding usable routes. The database
allows one HeapLeach process at a time; use a separate `-proxy-db` for a second
instance, and mount its directory on a persistent volume in containers.

## External downloader behavior

The YouTube download itself runs through `yt-download.sh` rather than inline
Go, so the recipe is in one readable place. A copy of that script placed
beside the binary overrides the built-in one, so it can be adjusted without
rebuilding. yt-dlp's pieces (each stream, the merge in progress) are kept in a
hidden `.heapleach` directory beside the destination, so a media library
watching that folder sees only the finished file. The directory is removed
once the download completes, and kept after a failure so the next attempt can
resume.

Metadata probes have a two-minute deadline and an 8 MiB output limit; helper
diagnostics are bounded too. YouTube playlist enumeration uses
`HEAPLEACH_MAX_FILES` at the helper, and a truncated result is labelled partial.
On Unix, cancellation kills the helper's process group, including children.
The bundled download script needs a POSIX shell; it is not a native Windows
helper launcher. External transfers use yt-dlp's own retries and transfer
controls, so the native speed ceiling, mid-transfer pause and stall watchdog
do not apply to them.
