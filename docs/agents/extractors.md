# Extractors

`backend/internal/extractor`; file names are relative to it unless stated.
Notes on individual hosts are in [`hosts/`](hosts/), indexed
[at the end](#host-notes).

## The contract

`extractor.Extractor` is `Name/Match/Extract`. `Registry.Find` walks the
registered hosts and falls back to `Direct`, which matches everything — so it
**never returns nil**. Register new hosts in `NewRegistry`.

### The supported-site inventory

The supported-site table in README.md is **generated** from the registry by
`make hosts`, between two markers, and `make hosts-check` fails CI when the
file and the code disagree — so edit the extractor, never that table. Adding a
host is one line in `NewRegistry` plus `make hosts` in the same commit.
The inventory is collapsed on the front page; its markers stay in README.md.
The explanatory URL-format table lives in `docs/supported-links.md` and is
maintained separately because it documents behavior, not registry membership.
`hosts-check` regenerates into a copy and compares it with README.md as it
stands, so it judges the working tree whatever else is in flight there; in CI
the tree is the commit, which is what makes a regeneration that was never
committed fail the build.

`SiteLister` is the optional half of the contract. An extractor that claims a
plain list of domains embeds a `hostSet` (`hosts.go`): one declaration that
answers both `Match` and `Sites`, so the inventory cannot disagree with the
matcher, and `catalogue_test.go` checks that every host so declared routes
back to the extractor declaring it. Only an extractor whose `Match` is more
than a host list — a section of a domain, a wildcard TLD, a document shape —
writes `Sites()` by hand, in `catalogue.go`; one that skips it altogether must
be named for its domain, since the catalogue then uses the name as the site.

### Platform families

Some extractors cover software that many operators run rather than one site.
Their families are named in `config/config.go` (`FamilyKVS`, `FamilyPeerTube`,
`FamilyChevereto`, `FamilyFoolFuuka`, `FamilyFediverse`, `FamilyMediaWiki`,
`FamilyBandzoogle`). A compiled-in install list can only trail such a
platform, so `HEAPLEACH_EXTRA_HOSTS` adds installs at runtime, and it is the
one setting that names hosts rather than tuning behaviour. It reads
`family:host,host;family:host`; the legacy `HEAPLEACH_KVS_HOSTS` is honoured
as the `kvs` family. The generic fallback also recognises several platforms at
a URL nobody registered ([below](#platform-detection)).

### How a host may be used: `Pace`

An extractor declares the host's limits with `Pace` (documented field by field
in `extractor.go`): `Streams` caps connections per file, `Files` caps
transfers to the host at once, `Group` shares that limit across aliases and
storage servers, `PerRoute` applies it per egress address through the proxy
pool ([proxies.md](proxies.md)), `DirectFirst` keeps a per-address file on the
normal connection, unpaced, until the host makes that connection wait, and
`WAF` adds challenge history to route selection. Declare only what the host is
known to enforce; anything else the downloader learns by being refused
([transfers.md](transfers.md)).

## The `File` contract

Most hosts set `File.URL` at extraction time. The rest of `File` exists for
hosts where that is not enough:

- **`Resolve`.** Hosts that mint **short-lived signed URLs** (bunkr and turbo,
  roughly 20 minutes) leave `URL` empty and set `Resolve` instead. The
  downloader calls it immediately before transferring, and again before each
  retry. Resolving eagerly would make a long queue start failing partway down,
  so any link signed for less than the queue may wait belongs here.
- **`Mirrored`** (`mirror.go`) builds on that per-attempt cadence: a file
  offered from several equivalent links gets a resolver that hands out the
  next one each time. Nothing tracks which mirror failed — telling a dead
  mirror from a cancelled job, a paused queue and a stall, without ever
  leaving a file with no source, buys nothing over rotating blindly.
  `MirrorHosts` builds the list when the alternatives differ only by host,
  which is how gofile's storage servers are expressed.
- **`Target.Discard`.** A resolver whose links are bound to the address that
  obtained them sets it. The downloader calls it when the host refuses the
  link for another address (409), so that the next resolve mints a new link
  rather than handing back the cached one. Keep2Share is the case
  ([hosts/keep2share.md](hosts/keep2share.md)).
- **`Check`** asks about the file itself before the downloader spends a route
  on it, over the caller's own connection (`transferRouted`, ahead of the exit
  probe). An error fails the file; a question that cannot be answered returns
  nil and leaves the deciding to `Resolve`. Keep2Share asks there whether the
  site still offers the file free.
- **`Cipher`** is set by a host that serves ciphertext, and the downloader
  decrypts on the way to disk. Only mega does. The mode is AES-CTR and must
  stay a stream cipher; see encrypted payloads in
  [transfers.md](transfers.md).
- **`Refresh`** re-reads the file's source on a new route before resolving,
  for WAF recovery ([proxies.md](proxies.md)).
- **`SizeApprox`** marks a length read off a listing page: rounded for
  display, and never enough to conclude that a file on disk is this one.
- **`Segments`, `SegmentKey`, `Audio` and `Subtitles`** carry a playlist and
  its separate tracks ([media.md](media.md)).
- **`External`** names a page that yt-dlp handles end to end
  ([media.md](media.md)); **`Reject`** sees the response a transfer is about
  to write and may refuse it ([transfers.md](transfers.md));
  **`ProxyFallback`** opts the file into proxy recovery after a temporary
  failure ([proxies.md](proxies.md)); **`Pace`** is above.
- **`Refused`** reads a refused transfer, its status and headers and the route
  it came through, in the terms of the errors below: mega's storage answers an
  address whose quota is used up with 509 and the time left, which is that
  address's `WaitError` ([hosts/file-hosts.md](hosts/file-hosts.md)). The
  downloader asks it before anything general is made of the refusal, and does
  not retry in place what it reads (`refusedByHost`).

### Errors that steer the queue

What a resolver returns, or `Refused` makes of a refusal, tells the manager
what to do with the item ([download.md](download.md)):

- **`WaitError`** means *later*, with the time to come back. The resolver
  returns it and does not sleep it out: sleeping would hold a worker for the
  whole wait while the item reads as downloading. The manager puts the item
  back in the queue until then. Through a proxy route the wait is the route's:
  the route is held back from the service until then and the file goes on
  through another. A `DirectFirst` file meeting one on the normal connection
  moves onto its group's routes.
- **`RefusedError`** means the host refuses this address, not the file.
  Through a proxy route the route is held back for the service and the file
  tries another, outside its retry budget ([proxies.md](proxies.md)).
- **`TransientError`** is a failure the next attempt may well not meet, such
  as a run of unreadable CAPTCHAs; through a proxy route it is retried on
  another route. On the normal connection, deferred resolution retries it
  with backoff up to `MaxRetries`, as it does temporary network failures.

### Progress notes

A resolver that takes a while says what it is doing with `ResolveNote`
(`WithResolveNote` carries it to the item's note): a CAPTCHA being read, a
host's timer, a player's token stage. During source discovery the same notes
reach `JobView.Note`, which carries them to the browser and the terminal,
guarded by the source generation and cleared from the view once resolving
ends.

## The generic fallback

`Direct` checks an ordinary HTML page for every registered host's links before
using a generic embedded player. This is how a page embedding a supported
player works without an extractor of its own. `filelinks.go` shares the
`links:` harvester's bounded expansion, but never follows links assigned to
the generic fallback, so a plain HTML navigation link cannot start another
scan. Named hosts and recognised platforms keep their own extractors.
K2S/FileBoom links are deduplicated by service and file ID across aliases;
other links keep their full URLs, including Mega keys. The fetch checks HTML
headers before reading a bounded body, and supports extensionless pages and
common HTML/script extensions. Supported links that all fail are reported as
an error rather than saved as an HTML shell, and partial results report
counts.

### Platform detection

The fallback also identifies unregistered installs of known platforms.
`platform.go` supplies bounded, single-attempt JSON probes and page-marker
detection. Pixeldrain file/list/filesystem metadata, booru original-media
records, FoolFuuka archive records, Kemono creator profiles and MediaWiki
siteinfo select the existing extractors at the pasted URL's origin. KVS player
signatures and asynchronous listing controls identify its pages, including
members and searches; a generic videos container alone does not. MediaWiki's
generator and EditURI link also locate custom API directories. Recognised page
content is tried before guessed APIs, so an ordinary blog post with an
embedded album does not probe unrelated services. The fediverse's `/u/` and
`/post/` path matches overlap two of these platforms; if nodeinfo does not
identify the site, their APIs get a turn.

Keep these broad URL shapes out of `Registry.Known`: its answer controls which
links the ordinary page scanner follows. Host registrations provide that
discovery boundary, the generated inventory, canonical aliases and per-site
settings (including booru filters), so a host stays registered even where
direct input would autodetect it; dropping the registration silently loses
embedded-link discovery. Positive identification commits to an extractor,
whose errors must be returned rather than falling back to saving the page
shell.

### DarkGram albums

DarkGram albums are recognised in the same fetched HTML, by the player's
`darkgram-album` class and JSON `data-album` attribute. The first player
supplies its own videos (`hls_url`) and full-size photos (`photo_url`);
thumbnails and other players are not album items. Discovery makes no media
requests. Each video resolves its playlist through `File.Resolve`, refreshing
segments and keys per attempt and keeping the final page URL as referer. File
names keep the original item positions when missing entries or file caps
shorten an album.

### WordPress categories

WordPress categories are recognised in the same fetched document, before the
ordinary link scan, rather than claimed by domain. `wordpress.go` reads
classic post permalinks or block-theme query loops, follows the category's own
next control, and opens only the listed posts. The category heading and pager
may sit outside `main`, so the primary content area is kept around it. Post
content supplies supported host links, embedded media and JPEG images;
original-image links and metadata outrank thumbnails. Registered extractors
resolve the host links, keeping their deferred resolvers and pacing. Without
such links, the album and player parsers of the generic fallback read the
post's content and title, keeping sidebar players and head metadata out.
DarkGram videos keep their deferred playlist resolvers; player artwork is
excluded, and independent JPEGs are kept without duplicates. The post reader
is passed to the shared source expansion
([below](#sources-that-lead-to-other-sources)), so folder names, concurrency
and caps are the shared ones, and no host-specific WordPress category scraper
is kept beside it. Tests use invented classic and block-theme pages; saved
browser archives stay untracked and can be parsed offline without contacting
their source sites.

## Listings

### Sources that lead to other sources

An extractor whose results are other sources — an index, a search, the
`links:` harvester, a WordPress category — resolves each through the registry,
so every host's deferred resolver and per-route pacing reach the downloader
intact. `sources.go` holds what they share:

- **The supported-link filter.** A source excludes itself by extractor *name*
  rather than pointer (`supportedSources`), since the recursion is in the kind
  and not the instance; without it a site's own navigation turns one search
  into a walk of the whole catalogue.
- **The fan-out.** `expandSources` resolves sources concurrently and applies
  the file cap **a source at a time**: an album is taken whole or not at all,
  resolving stops once the cap is reached, and only a first album larger than
  the cap on its own is cut. The note counts the sources actually in the job.
- **The folders.** Each source gets a folder named after its title
  (`sourceFolder`). Titles are not unique, so `expandSources` counts the names
  first and gives a repeated one the source's own id (`sourceTag`), which
  keeps two different files of one name from sitting side by side as "(2)" and
  "(3)". A title that stands alone keeps its plain name, which is what
  anything already downloaded under it is filed as.
- **The note.** "N of M" admits a truncated answer.

### Walking pages

- Pages are fetched concurrently with `FanOut` (`fanout.go`), bounded by
  `config.PageFetchConcurrency`, with results collected by index so the job
  lists files in the site's own order however the requests interleave. A page
  that will not load is skipped rather than failing a job of hundreds.
- Stop **at** the cap rather than collecting everything and truncating: a
  broad search can state tens of thousands of pages.
- Follow a listing by the count it states and by what each page adds, never by
  the page size asked for. Hosts commonly serve fewer than asked without
  saying so, or ignore a paging parameter and serve page one again, which
  reads as the end of a one-page listing. Deduplicate, and stop on a page that
  adds nothing.

### Caps

The listing caps are settings (`HEAPLEACH_MAX_SOURCES`, `HEAPLEACH_MAX_FILES`,
and `-max-sources`/`-max-files`), defaulting to `config.MaxExpandedSources`
and `config.MaxListingFiles`. They reach the extractors through
`extractor.Options`, whose unexported `limits` the registry fills from the
config in `Registry.Extract`. So a nested extraction, an album found by a
search, is bounded the same way, and every extractor that walks a listing
reads `opts.maxFiles()` rather than the constant. Zero means the default, and
there is no "unlimited": every file of a job is also a row in the UI and a
line in the saved queue.

### Titles, notes and folders

- **What names a job's folder is `Result.Title` alone.** Anything an extractor
  has to admit — a listing cut short by a cap or a rate limit — goes in
  `Result.Note`, which the job's shown title carries (`Result.Label`) and the
  folder never does. A note in the title would change the folder whenever a
  cap was raised or a rate limit waited out, and the next run would file
  everything somewhere new and download it all again.
- **A slash in a title is not a path.** `SafeName` keeps only what follows the
  last separator, which is right for a name a server hands over (RFC 6266 says
  to strip a `Content-Disposition` name of its path) and wrong for a title:
  "A Series (1/7)" would be saved as "7)". So `Registry.Extract` and the
  downloader's `resolveTarget` turn a title's separators into lookalikes first
  (`util.TitleName`, "⧸", as yt-dlp writes them), for job titles and file
  names alike. `sourceFolder` turns them, and plain slashes, into "_" instead;
  keep that, since changing it would file every re-read job somewhere new and
  download it all again.
- `Registry.Extract` decodes HTML entities in titles and names for the UI's
  sake; `SafeName` decodes them again on the way to disk
  ([download.md](download.md)).

## Helpers worth reusing

- `refetchedVideo` (`tube.go`): a single video whose link is signed per visit,
  re-read when its transfer starts.
- `unpackJS` (`streamhosts.go`): reverses the p,a,c,k,e,d packer,
  host-neutral.
- `resolveMediaPlaylist` (`hls.go`): an HLS master with its key, separate
  audio and subtitles ([media.md](media.md)).
- `FanOut`, `expandSources`, `Mirrored`/`MirrorHosts` and `hostSet`, above.

## Live tests

Extractor tests that hit the real sites sit behind a build tag, because these
hosts change their plumbing without warning. They live in untracked files
(`*live_test.go` is gitignored, and `internal/extractor/live_test.go` holds
the general one), so a fresh clone will not have them: write what you need and
leave it untracked.

```bash
make test-live                                              # all hosts
cd backend && go test -tags live ./internal/extractor/ -run 'TestLiveExtract/gofile' -v
```

Run these after touching anything under `internal/extractor/`. They are the
fastest way to find out which host broke.

Two conventions worth keeping to when writing one:

- A check that only calls `Extract` proves the API answered, not that the file
  is reachable. Fetch a range of the first file as well and look at the
  content type — gofile serves the site's HTML shell when a storage server is
  busy, with a `Content-Length` that agrees, and that records as a complete
  download. A `206` also confirms the host honours ranges, so the segmented
  engine will work on it.
- Take the URL, and any password, from the environment rather than writing it
  into the file. A literal would not be committed either way, but a file free
  of it can be pasted into a report or handed to someone else without leaking
  anything.

## Host notes

| Notes | Hosts |
| --- | --- |
| [hosts/file-hosts.md](hosts/file-hosts.md) | bunkr, gofile, pixeldrain, mega, emload, filejump, filester, suvobox, pixhost, imagepond |
| [hosts/keep2share.md](hosts/keep2share.md) | keep2share, fileboom, voyeurking, and the CAPTCHA reader (`internal/captcha`) |
| [hosts/video-hosts.md](hosts/video-hosts.md) | doodstream, streamtape, mixdrop, lulustream, vids.st, sendvid, knuthing, noodlemagazine, ok.ru, pornone, eporner, yandex, Kernel Video Sharing |
| [hosts/broadcasters.md](hosts/broadcasters.md) | vimeo, arte, orf, svtplay, yle, drtv, rtbf, francetv, radio-canada, niconico |
| [hosts/collections.md](hosts/collections.md) | balbums, coomerfans, celeb.st, cum.st, erome, rajce, bandcamp, bandzoogle |

A host not listed has no notes beyond its code.
