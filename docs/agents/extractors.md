# Extractors

`backend/internal/extractor`; file names are relative to it unless stated.
Notes on individual hosts are in [`hosts/`](hosts/), indexed
[at the end](#host-notes).

## The contract

`extractor.Extractor` is `Name/Extract`, and every registered extractor is
also a `Recogniser` (`Shape/Recognise`) and a `Describer` (`Recognises`).
Register new extractors in `NewRegistry`; the first to recognise a page
takes it, so order matters where two could.

### Dispatch is by page, never by host

No extractor claims a domain. `Registry.Extract` probes the submitted URL
(`page.go`: one bounded GET, redirects followed, the body read only when it
is text) and offers the `Page` to each `Recognise` in turn; nothing claims it
and `Direct` takes it. A site that moves domain, or an install nobody listed,
reaches the right code because the code recognises what it serves. Host
names may still appear as operational constants (API bases, CDN hosts,
referers, `Pace` groups), never as the reason an extractor was chosen.
`TestRecognitionNeverReadsTheHost` fails the build if a `Shape` or
`Recognise` reads a URL's host, and `expectRecognised`/`expectShape`
(`recognise_test.go`) re-run each fixture with the page served from an
unrelated host.

Recognition is **deep**: `Recognise` checks the URL's form *and* page content
the extractor's own logic depends on (the player configuration, embedded
JSON, script or DOM structure it parses). The site's name (`Page.Named`) is
never enough alone; a pure app shell pairs it with a structural marker such
as the bundle it loads (`Page.Script`). Each test carries a negative fixture
with the right form and name but without the marker. When the page cannot
say (a challenge stood in for it, or the HTML is an empty shell) a bounded
origin request (`platformJSON`) may decide, but only after the form matched,
since every unrecognised page is offered to every recogniser.

`Shape` is the half that needs no fetch: the link's path, query and
fragment. `Registry.Known` uses it for link discovery (the harvester, the
page scanner, the index extractors), and `Guess` for a job's label before it
is read. A link followed for its form is extracted strictly
(`Options.strict`): if its page turns out to be no extractor's, it is dropped
as navigation (`errUnrecognised`) rather than handed to the fallback. Keep
forms specific enough that a site's own navigation does not turn into a
probe per link.

### Fetching a page once

Probes go through `pageCache`: per route (a page fetched through one egress
address says nothing of another's), shared by concurrent callers, kept for
`config.PageCacheTTL`, and never keeping a failure or a challenge. An
extractor that reads the submitted page whole uses `fetchDocument`, which
hands it the probed document instead of fetching again; any extra request a
recogniser makes goes through `memo(p, key, …)` (`pageJSON` for an API
question), keyed by what it asks, so several recognisers asking one
origin's API share one answer. An extractor that reads an endpoint a recogniser may already have asked tries
`askedJSON` first: it hands back that answer, and asks nothing when no
recogniser did, so the extractor's own request, with its retries, is never
preceded by a bounded probe of the same thing. A link whose
path names a non-page file and matches no `Shape` skips the probe
(`probeWorthy`): probing would download the start of the file the fallback
is about to fetch.

### The supported-site inventory

The inventory in README.md is **generated** from the registry by `make
hosts`, between two markers, and `make hosts-check` fails CI when the file
and the code disagree — so edit the extractor's `Recognises`, never that
table. Adding an extractor is one line in `NewRegistry` plus `make hosts` in
the same commit. The inventory is collapsed on the front page; its markers
stay in README.md. The explanatory URL-format table lives in
`docs/supported-links.md` and is maintained separately because it documents
behavior, not registry membership. `hosts-check` regenerates into a copy and
compares it with README.md as it stands, so it judges the working tree
whatever else is in flight there; in CI the tree is the commit, which is
what makes a regeneration that was never committed fail the build.

### Platform families

Some extractors cover software that many operators run rather than one site
(KVS, PeerTube, Chevereto, FoolFuuka, the fediverse, MediaWiki, Bandzoogle,
the booru engines). Each is one recogniser keyed on the software's own
fingerprint, so there is no install list to keep and no setting to extend
one.

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

`Direct` takes what no extractor recognised. It reads the probed document
(`Direct.document`) rather than fetching it again, recognises WordPress
categories and DarkGram albums in it, and otherwise scans it for links of a
supported form before using a generic embedded player. This is how a page
embedding a supported player works without an extractor of its own.
`filelinks.go` shares the `links:` harvester's bounded expansion; its links
are extracted strictly, so a link that only had a supported form is dropped
once its page proves to be nobody's, and a page whose links were all of that
kind falls through to its own media. Keep2Share links are deduplicated by
file ID alone, across both of its hosts, which share one store; other links
keep their full URLs, including Mega keys.
Supported links that all fail are reported as an error rather than saved as
an HTML shell, and partial results report counts.

### Platforms behind a closed page

Most platforms are recognised in the probed page itself. When the page cannot
say — challenged, not OK, empty, or a proof-of-work interstitial served with
200 (`Page.Silent`) — a recogniser whose URL form matched may ask the page's
own origin a bounded, single-attempt question instead (`pageJSON`, memoised
per page, so every recogniser asking the same endpoint shares one answer):
pixeldrain file metadata, booru post records, FoolFuuka archive records,
Kemono creator profiles, MediaWiki siteinfo, PeerTube's config. An extractor
then reads the memoised answers back through `probed` and `askedJSON`
instead of asking again. Positive identification commits to an extractor, whose errors must be
returned rather than falling back to saving the page shell.

Keep forms that are this broad out of `Shape` where they would make ordinary
navigation a probe per link; `TestRegistryKnowsLinksByTheirForm` pins which
forms the link scanner follows.

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

- Every paged listing that is not a broadcaster's series or a folder tree
  walks through `walkPages` (`pages.go`). The caller's `fetch` reads page *n*
  and keeps the cursor, offset or page number in its own closure; the engine
  owns the rest, once: a first page that fails is the caller's error as it
  stands and a later one a partial listing (or quiet, or the `failed` hook's
  note), entries deduplicated by `key`, a page that adds nothing ends the
  walk, the page cap (`config.MaxAlbumPages` unless `pages` says otherwise),
  the limit and its note, and cancellation, which is never a partial listing.
  `walkListing` (`listing.go`) is its adapter for HTML pagers.
- Each page says how the listing ends, from what the API states: `total` (the
  count it holds), `end` (an end flag, a missing cursor, or an empty page
  asked for past the end), or no `next`. Never end on a page shorter than the
  page size asked for: hosts commonly serve fewer than asked without saying
  so. Asking one page past the end is cheap and right where nothing else
  says. A host that ignores its paging parameter serves page one again, which
  adds nothing and ends the walk.
- A listing whose end is found by asking past it — a 404, an empty page, the
  first page again — is `quiet`: the walk adds no note of its own there, and
  `failed` maps the failures that are a cut (a rate limit). `stop` marks a
  page that is not the listing's (missing, repeated, redirected) and `note` a
  reason the walk ends early, such as a next page that cannot be followed.
- Stop **at** the cap rather than collecting everything and truncating: a
  broad search can state tens of thousands of pages. Where every entry costs a
  request at listing time, the cap is `listingLimit` (the lower of the source
  and file caps); otherwise the file cap.
- Entries that need their own page are then read concurrently with `FanOut`
  (`fanout.go`), bounded by `config.PageFetchConcurrency`, with results
  collected by index so the job lists files in the site's own order however
  the requests interleave. One that will not load is skipped rather than
  failing a job of hundreds.

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

- The `Player` engine (`players.go`, its rows in `playersites.go`): a video site is a `playerSite` row
  (its forms, recognition, canonical page and a reader returning the
  candidate renditions, with a per-read referer when the player needs its
  own), registered with `NewPlayer` at the position where its form order
  matters. The engine picks the rendition, follows a playlist and re-reads a
  signed page at transfer time. A row may `peek` (read only the title now
  and everything at transfer time), take a `listing` of its videos (through
  `walkListing`, or a walk of its own), refuse playlists still being written
  (`finished`), and opt into proxy recovery (`proxyFallback`). Add a new
  video site as a row, not an extractor.
- `refreshing` and `signedResult` (`extractor.go`): a file whose link is
  signed per visit, resolved now and signed again when its transfer starts.
- `unpackJS` and `jwSource` (`jsdecode.go`): the p,a,c,k,e,d packer reversed,
  and a JW Player setup's `sources:[{file:…}]` list read; both host-neutral.
- `broadcast.go`: what the public broadcasters share — `broadcastRefusal`
  for DRM and geo refusals in the broadcaster's own words, `expandSeries`
  for a series resolved while it is listed (capped by a request budget, noted
  where cut, filed by season, one error when nothing plays), beneath it
  `resolveListed`, `resolveInTurn` and `resolveEach` for a listing's
  episodes resolved concurrently or one at a time, in order, `lazyEpisode`
  and `refreshing` for files re-read at transfer time, and the listing
  notes. A new broadcaster uses these rather than types of its own.
- `resolveFiles` and `noneRead` (`resolve.go`): a listing whose items each
  open into several files (a band's releases), resolved concurrently with
  the files kept in order and the refusals and first failure beside them,
  and the one error, `<site>: none of the <n> <noun> on <where> could be
  read: <reason>`, for when none could. `resolveEach` and `expandSeries`
  share its outcome handling.
- `linkExt` (`html.go`): the extension a link's file name carries,
  lowercased, read off the path's last element (so a trailing slash still
  names it), with a fallback where it names none.
  `linkExtAsIs` (`links.go`) keeps the link's own spelling, for a host whose
  downloads are already filed under it.
- `walkTree` (`tree.go`): a share's nested folders, depth first in the
  host's order, with the depth, folder and file caps, one folder per child
  through `sourceFolder`, and an unreadable subfolder reported as partial.
  A host supplies only the reader for one folder.
- `resolveMediaPlaylist` (`hls.go`): an HLS master with its key, separate
  audio and subtitles ([media.md](media.md)).
- `walkPages` (`pages.go`), above, for any paged listing, and its HTML
  adapter `walkListing` with `webLink` (`listing.go`): redirect, missing-page
  and repeated-page checks on top of the engine. A new paged listing supplies
  only the reader for one page, which also names the next page for a listing
  paged by number, and sets `badNext` when the pager offers a next page that
  cannot be followed ("partial — invalid next page"). Options cover the rest:
  a first page the caller already read (`root`), a fetch of its own (`fetch`,
  `fetchAsAsked`), page identity (`key`), redirects judged by the reader
  (`anyRedirect`), and the engine's `quiet` and `failed`. `listingNotes`
  (`sources.go`) builds the job note of a listing expanded into its
  sources.
- `hlsMedia.file`/`hlsMedia.target` (`hls.go`) and `Target.file`: a resolved
  playlist or target as a File, every field carried over.
- `rankIn` (`renditions.go`, with the rendition model `bestCandidate` ranks): a name's place in a quality ladder, ignoring case,
  with the caller choosing the value for a name not on it.
- `knownSize` and `parsedSize` (`extractor.go`): a stated byte count as
  `File.Size` wants it, -1 for anything that is not a length.
- `FanOut`, `expandSources`, `Mirrored`/`MirrorHosts`, above.
- `fixturePage`, `htmlPage`, `expectRecognised` and `expectShape`
  (`recognise_test.go`): recognition fixtures, checked from an unrelated host
  as well.

## Live tests

Extractor tests that hit the real sites sit behind a build tag, because these
hosts change their plumbing without warning. They live in the private
[`hl-priv`](https://github.com/JohanLindvall/hl-priv) repository, at the paths
they run from, together with the URL corpora used to check routing and
extraction against the real sites; `internal/extractor/live_test.go` holds the
general one. Its `link.sh` symlinks them into this checkout, where
`*live_test.go` is gitignored. A new live test is written in `hl-priv` and
linked, never left as a loose untracked file here.

```bash
git clone git@github.com:JohanLindvall/hl-priv.git ../hl-priv && ../hl-priv/link.sh .
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
| [hosts/keep2share.md](hosts/keep2share.md) | keep2share (both hosts), voyeurking, and the CAPTCHA reader (`internal/captcha`) |
| [hosts/video-hosts.md](hosts/video-hosts.md) | the player rows (pornhub, xhamster, tnaflix, drtuber, pornone, vidmoly, streamtape, mixdrop, lulustream, vids.st, sendvid, doodstream, knuthing, noodlemagazine, ok.ru, eporner), Kernel Video Sharing; pointer pages (yandex, alohatube); the yt-dlp handoffs (youtube, odysee, dailymotion, bilibili, soundcloud, mixcloud, rumble) |
| [hosts/broadcasters.md](hosts/broadcasters.md) | vimeo, arte, orf, svtplay, yle, drtv, rtbf, francetv, radio-canada, niconico |
| [hosts/collections.md](hosts/collections.md) | balbums, coomerfans, celeb.st, cum.st, erome, rajce, bandcamp, bandzoogle |

A host not listed has no notes beyond its code.
