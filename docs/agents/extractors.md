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
is text; a body cut short or past `config.MaxResponseBytes` sets `Page.Err`,
so recognisers may sniff the prefix but the page cache and `Document` never
offer it) and offers the `Page` to each `Recognise` in turn; nothing claims it
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
with the right form and name but without the marker or a matching API.
When the page lacks recognised markup, a bounded origin request
(`platformJSON`) may decide, but only after the form matched, since every
unrecognised page is offered to every recogniser. A populated custom
frontend is as eligible as a challenge or empty shell: compatible APIs do
not require the original frontend's branding or markup.

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
Both scanners resolve relative links against the final page URL and exclude
links to that page or its submitted alias, including fragment anchors,
before counting source limits. Self-links are navigation even when their
paths match an extractor's form; they must not trigger recursion failures
or displace downloadable sources. Other links retain their fragments.
Supported links that all fail are reported as an error rather than saved as
an HTML shell, and partial results report counts.

### Platforms behind a closed page

Most platforms are recognised in the probed page itself. When its markup is
unfamiliar — including custom frontends and pages behind a challenge — a
recogniser whose URL form matched may ask the page's own origin a bounded,
single-attempt question instead (`pageJSON`, memoised
per page, so every recogniser asking the same endpoint shares one answer):
pixeldrain file metadata, booru post records, FoolFuuka archive records,
Kemono creator profiles, MediaWiki siteinfo, PeerTube's config. An extractor
then reads the memoised answers back through `probed` and `askedJSON`
instead of asking again. Positive identification commits to an extractor, whose errors must be
returned rather than falling back to saving the page shell.
The API response must identify the software and, where it returns a record,
the requested target. Generic JSON or a matching site name is insufficient;
the HTML's status and branding do not veto a valid API response.

Compatible creator archives may return attachment metadata with
`has_full: false` before storing the originals. Such posts contribute no
files or file-limit weight; mixed listings report them as left out, and a
post or creator with only previews reports that originals are not archived.
An absent availability flag keeps the ordinary API's attachment behavior.
Previews must not become original downloads or fabricated `/data` links.

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
  An extractor expanding its own albums or posts rather than registry links
  uses the same fan-out through `expandSourcesWith`, so its cap, folders and
  refusals behave alike.
- **The folders.** Each source gets a folder named after its title
  (`sourceFolder`). Titles are not unique, so `expandSources` counts the names
  first and gives a repeated one the source's own id (`sourceTag`), which
  keeps two different files of one name from sitting side by side as "(2)" and
  "(3)". A title that stands alone keeps its plain name, which is what
  anything already downloaded under it is filed as. A host whose repeated
  titles were filed under a form of its own before this existed keeps that
  form through `expandSourcesNamed`'s `folders` hook, since renaming them
  would download them all again.
- **The note.** "N of M" admits a truncated answer (`partialNote`, on
  `cappedNote`); a listing expanded into its sources says what its walk,
  the file cap and the sources that did not resolve left out
  (`expansionNote`).

### Walking pages

- Every paged listing that is not a folder tree, broadcaster series included
  (`walkEpisodes`), walks through `walkPages` (`pages.go`). The caller's `fetch` reads page *n*
  and keeps the cursor, offset or page number in its own closure; the engine
  owns the rest, once: a first page that fails is the caller's error as it
  stands and a later one a partial listing (or quiet, or the `failed` hook's
  note), entries deduplicated by `key`, a page that adds nothing ends the
  walk, the page cap (`config.MaxAlbumPages` unless `pages` says otherwise),
  the limit and its note, and cancellation, which is never a partial listing.
  `walkListing` (`listing.go`) is its adapter for HTML pagers.
- Each page says how the listing ends, from what the API states: `total` (the
  count it holds), `pages` (the page count it runs to), `end` (an end flag, a
  missing cursor, or an empty page asked for past the end), or no `next`. Never end on a page shorter than the
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
  and file caps); otherwise the file cap. Where an entry may hold several
  files or none — a post, a status, a search hit — `weigh` counts the file
  cap in what the entries hold rather than in entries, so a page of text
  posts never uses it up; the page that reaches the cap is kept whole, noted
  when it passes the cap or the listing goes on, and the registry cuts the
  files to the cap.
- Entries that need their own page are then read concurrently through
  `resolveFiles`/`resolveOutcomes` (`resolve.go`, on `FanOut` in
  `fanout.go`), bounded by `config.PageFetchConcurrency`, with results
  collected by index so the job lists files in the site's own order however
  the requests interleave. One that will not load is skipped rather than
  failing a job of hundreds; when none loads, `noneRead` says why, and when
  some do, the note says how many the host refused and why (`refusedNote`).
- `resolveOutcomes` is the one ordered, bounded map: serial (`inTurn`) and
  concurrent modes both start nothing once the context ends, let started
  work settle, and keep each outcome's index and error. `readSeasons`,
  `resolveEach` and source expansion go through it. Every finisher —
  `readListing`, `noneResolved`, `expandSeries`, the walkers' cap exits and
  the album callers that finish by hand — checks the caller's cancellation
  **first**: a listing cut short and returned as a success would be saved
  as if it were the whole listing. `FanOut` remains for callers that only
  fill values and need no outcomes (`Autoindex.readAll`).

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
- **A note is built from the helpers in `notes.go`, so every extractor's
  reads alike.** `joinNotes` is the one joiner: "; " between parts, empty
  parts dropped, a part the note already holds not repeated (a joined note's
  parts count one by one), and "partial — " said once, at the note's start —
  "partial — invalid next page; file limit". The reasons a walk stops on
  most are constants — `fileLimitNote`, `sourceLimitNote`, `invalidNextNote`,
  `couldNotFetchNote`, and bare `fileLimit` and `sourceLimit` for a tree
  walk's `note` — so they cannot drift apart. The parts: `partial` marks one
  as a cut, `cappedNote` says "N of M <unit>" (bare "N of M" with no unit),
  `resolvedNote` "N of M <noun> resolved", `countNote` "N <words>" in the
  caller's words, `leftOutNote` "N left out…" on it, `refusedNote` counts
  refusals per reason ("3 left out: gone; 1 left out: live", on `tally`),
  and `brokenNote` a walk that broke off. An "N of M" is never prefixed with
  "partial — ": the count already says the answer is short, and a cut it
  stands beside carries its own "partial — " reason. A hand-built count, or
  a join of its own, is what these replace; a new note uses them rather than
  a `Sprintf` that only nearly matches.
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

- `about` (`extractor.go`): an extractor's name, inventory text and shape
  as one embedded row, so its constructor reads as data rather than three
  hand-written methods. Every extractor embeds it, and so does every engine
  row (`playerSite`, `shareSite`, `pointerSite`, `handoffSite`), whose
  literals keep writing `name:`, `shape:` and `recognises:` as promoted
  fields; each engine's constructor copies the row's `about` into the
  extractor, so build engines and extractors with their constructors, never
  as bare literals, or they have no name and no shape. A shape handed to it
  is a plain function named `…Shape`, nil for an extractor with no form
  worth a probe; `TestRecognitionNeverReadsTheHost` checks those too.
  `eitherShape` combines shapes.
- The `Share` engine (`shares.go`): a file host whose share or album page
  is read by its own code is a `shareSite` row — its name, inventory line,
  forms (a shape plus what a page of that shape must carry; the first form
  matching the final URL decides) and its `extract`. The row sets no shape
  of its own: `newShare` makes the extractor's the union of its forms'. A form with no
  recogniser is a link that only redirects: followed, never recognised.
  `TestShareFormsNeverReadTheHost` holds the rows to the same no-host rule as
  `Shape`/`Recognise`, and `TestShareRowsAreCompleteAndRegisteredOnce`
  checks that every registered row has its callbacks, a recognising form and
  one stable place in the registry. Hosts with state of their own (tokens,
  quotas, cooldowns) stay extractors; `routeWaits` (`routewaits.go`) keeps
  their per-route waits. Do not convert a stateless extractor whose
  `Recognise` accepts pages its `Shape` does not advertise (Erome's profile
  pages, PornPics' listings without a discoverable form): `Share` builds
  `Shape` from its forms and recognises only a matching form, so such a
  conversion either loses those pages or widens link harvesting to
  navigation. Rajce is the reference conversion: one form, page-only
  recognition, its parser and notes kept as they were.
- The `Player` engine (`players.go`, its rows in `playersites.go`): a video site is a `playerSite` row
  (its forms, recognition, canonical page and a reader returning the
  candidate renditions, with a per-read referer when the player needs its
  own), registered with `NewPlayer` at the position where its form order
  matters. The engine picks the rendition, follows a playlist and re-reads a
  signed page at transfer time. A row may `peek` (read only the title now
  and everything at transfer time), take a `listing` of its videos (through
  `walkListing`, or a walk of its own), refuse playlists still being written
  (`finished`), and opt into proxy recovery (`proxyFallback`, which
  `Registry.Extract` reads off the `*Player`; no other extractor opts a
  source in). Add a new video site as a row, not an extractor.
- `refreshing` and `signedResult` (`extractor.go`): a file whose link is
  signed per visit, resolved now and signed again when its transfer starts.
- `unpackJS` and `jwSource` (`jsdecode.go`): the p,a,c,k,e,d packer reversed,
  and a JW Player setup's `sources:[{file:…}]` list read; both host-neutral.
  `jwSource` takes the **first** non-DASH link of a JavaScript setup and
  reads nothing else about it, while `jwPlayerBest` (`mediapage.go`) ranks a
  JSON feed's typed renditions, video before audio, with exact sizes. They
  stay separate readers: a shared candidate step would either change the
  first-source behaviour the player rows rely on or need the JS reader to
  parse fields it does not have. Revisit only with fixtures that pin the
  intended ordering, the sprite exclusion and each caller's preference.
- `broadcast.go`: what the public broadcasters share — `broadcastRefusal`
  for DRM and geo refusals in the broadcaster's own words, `expandSeries`
  for a series resolved while it is listed (capped by a request budget, noted
  where cut, filed by season, one error when nothing plays) and
  `listSeries`, which ends a listing with it, beneath it
  `resolveEach` for a listing's episodes resolved concurrently or one at a
  time, in order, `walkEpisodes`, which walks a series through `walkPages`
  up to the request budget and says whether it went on, `readSeasons` for
  a programme's season lists read at once or in turn, an unreadable season
  left out and noted, `lazyEpisode` and
  `refreshing` for files re-read at transfer time, the note helpers above,
  `joinTitle` with its `titleEqual`, `titlePrefixed` and `titleContains`
  for naming an episode by its programme, and `writtenTitle` for a
  page's `<title>` with its spacing kept. A new broadcaster uses these
  rather than types of its own.
- `resolveFiles` and `noneRead` (`resolve.go`): a listing whose items each
  open into files (a band's releases, a board's threads, a channel's
  videos, a profile's posts, albums or galleries), resolved concurrently
  with the files kept in order and the refusals and first failure beside
  them, and the one error, `<site>: none of the <n> <noun> on <where> could
  be read: <reason>`, for when none could; when some could, the refusals
  go in the note (`refusedNote`). A listing that needs each item
  beside what it resolved to uses `resolveOutcomes` and `sortOutcomes`
  directly; `resolveEach` and `expandSeries` share the same outcome
  handling. An item whose host states why it holds nothing (a live stream,
  a video still transcoding) returns a `broadcastRefusal`, so that reason
  outranks a failure in the error. Where every item costs a request at
  listing time, the listing is held to `listingLimit` and notes the cut.
  A listing of items to resolve uses these rather than `FanOut` alone, which
  drops every error.
- `linkExt` (`html.go`): the extension a link's file name carries,
  lowercased, read off the path's last element (so a trailing slash still
  names it), with a fallback where it names none.
  `linkExtAsIs` (`links.go`) keeps the link's own spelling, for a host whose
  downloads are already filed under it.
- `walkTree` (`tree.go`): a share's nested folders, depth first in the
  host's order, with the depth, folder and file caps, one folder per child
  through `sourceFolder` (`treeDir`), and an unreadable subfolder reported
  as partial. A host supplies only the reader for one folder; one that pages
  a folder adds its listing's note with `treeWalk.partial` and may stop
  reading past `treeWalk.room()`.
- `resolveMediaPlaylist` (`hls.go`): an HLS master with its key, separate
  audio and subtitles ([media.md](media.md)).
- `walkPages` (`pages.go`), above, for any paged listing, and its HTML
  adapter `walkListing` with `webLink` (`listing.go`): redirect, missing-page
  and repeated-page checks on top of the engine. A new paged listing supplies
  only the reader for one page, which also names the next page for a listing
  paged by number, and sets `badNext` when the pager offers a next page that
  cannot be followed ("partial — invalid next page"). A `listingLink` is
  told from its repeats by its URL unless it names a `key`, and
  `listingURLs` turns the walk's links into the page URLs a caller expands.
  `webLink` is the one check that a scraped href is a web link worth
  following; a host's own rule (same site, a file extension) goes on top of
  it rather than beside a copy of it. Options cover the rest:
  a first page the caller already read (`root`), a fetch of its own (`fetch`,
  `fetchAsAsked`), page identity (`key`), redirects judged by the reader
  (`anyRedirect`), and the engine's `quiet` and `failed`; a `failed` hook
  that singles out one failure returns `pageFailed` for the rest, leaving
  their note to the walk. `expansionNote` (`sources.go`) builds the job note
  of a listing expanded into its sources, and `partialNote` that of a page
  or search whose sources a cap cut.
- `expandSources` keeps, beside the files, the refusals and first failure of
  the sources that did not resolve (`expansion.refusals`, `failed`), so a
  page none of whose sources resolved says why through `noneRead`; a link no
  extractor recognised is counted apart (`unrecognised`), never a failure.
- `hlsMedia.file`/`hlsMedia.target` (`hls.go`), `Target.file` and
  `File.target`: a resolved playlist or target as a File and back, every
  media field carried over — segments, key, audio and subtitles. Build a
  Target from a File only through `File.target`: the downloader replaces an
  item's key and tracks whenever a target has segments, so a hand-built
  conversion that drops a field clears it. Control fields (`Resolve`,
  `Check`, `Cipher`, `Pace`, `Reject`, `Discard`, the directory) are not
  media and stay with their owners.
- `rankIn` (`renditions.go`, with the rendition model `bestCandidate` ranks): a name's place in a quality ladder, ignoring case,
  with the caller choosing the value for a name not on it.
- `renewable` (`renewable.go`): a process-wide credential an extractor holds
  for every job — Gofile's guest account, RedGifs' bearer token, VRT MAX's
  player token. `get` reuses the value until it expires; `renew(stale)`
  replaces it once however many callers were refused with the same value,
  and returns an existing replacement to a refusal that arrives late. Only a
  validated value is cached, a failed renewal is retried by the next caller,
  and waiting is cancelable while the fetch runs detached from any one
  caller's cancellation. Requesting and decoding, what counts as refused,
  device ids and retry policy stay in the extractor; TTLs and margins live
  in `config/limits.go`. RTBF's session (with its device id) and ORF's
  site-read API credentials keep their own code, as do page memoisation,
  platform `hostMemo`, per-post loaders and address-bound tickets such as
  Keep2Share's and `Target.Discard`: their identities and lifetimes differ.
- `rawText`, `scriptTexts`, `linkedData` and `jsonAttr` (`html.go`): script
  payloads and JSON attributes as written. Decode JSON-LD, `__NEXT_DATA__`
  and `data-page` through these, never through `textOf`, which collapses
  whitespace for display text and would change names (and so paths) inside
  the JSON. Schemas, required fields and which object counts stay in each
  reader. A reader that recognition and extraction both need takes the
  parsed root and is shared through `memo` on the same `Page` (the gallery
  path in `celeb.go`); keep a string entry point for synthetic fixtures, and
  treat memoised values as immutable. A canonical URL that differs from the
  probed one reads its own page.
- `knownSize` and `parsedSize` (`extractor.go`): a stated byte count as
  `File.Size` wants it, -1 for anything that is not a length.
- `FanOut`, `expandSources`, `Mirrored`/`MirrorHosts`, above.
- `fixturePage`, `htmlPage`, `expectRecognised` and `expectShape`
  (`recognise_test.go`): recognition fixtures, checked from an unrelated host
  as well.

## Live tests

### Recorded parsing corpus

Keep copies of corpus pages, API responses and metadata-helper outputs in
the private `hl-priv` repository. Routine parsing checks replay those
responses through the actual registry and extractors, with the network
disabled and a missing recording reported as a failure. Reuse recordings
before making live requests; explicitly fill gaps or refresh them when a
site changes. The private audit runner preserves request identity and
redirects and can record responses as it checks them. Its README documents
the commands and refresh modes.

Parsing checks read only pages and metadata: no download manager, file
resolver, media body, segment or key fetch. Keep the saved real pages out of
this public repository; public unit tests use invented fixtures. A parsing
success verifies discovered metadata, not whether a deferred transfer would
succeed. Record unsupported, removed and inaccessible pages as negative
cases rather than silently discarding them from the corpus.

### Live transfer tests

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
