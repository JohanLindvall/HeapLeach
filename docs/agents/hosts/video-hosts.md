# Video hosts and players

Notes on hosts that serve single videos or embeddable players, and on the
Kernel Video Sharing platform. The shared machinery they use is in
[extractors.md](../extractors.md).

## Player pages: one engine, one row per site

pornhub, xhamster, tnaflix, drtuber, pornone, eporner, vidmoly, ok.ru,
streamtape, doodstream, mixdrop, lulustream, vids.st, sendvid, knuthing and
noodlemagazine are rows of one table in `playersites.go`, run by the engine in `players.go`, each registered as its
own `Player` under its own name, in the registry position its hand-written
extractor had (form order decides `Known`, so the rows are registered one by
one rather than as a block). A row states its forms and recogniser, the
canonical page a pasted link is read as (`page`), the media referer, and
`read`, which fetches the page and returns its title and rendition
candidates. The engine takes the best (`bestCandidate`), follows a playlist
to its segments (named by the segments' container) or hands over a plain
file, and, for a row marked `signed`, reads the page again when the
transfer starts. A 404 on the page is reported as the video being gone, for
every row, keeping the status underneath. New player sites belong in this
table.

What else a row can ask for:

- **A referer of the read's own.** `playerVideo.referer` replaces the row's
  for that read: the address of a player only the read found (knuthing's
  embed), or the page as finally served (noodlemagazine, doodstream). A row
  with neither sends no Referer (eporner).
- **`peek`.** Extract learns only the title, and that the video plays; the
  whole read waits for the transfer and runs again on recovery (`Resolve`
  and `Refresh`). For a site whose player spends its signature by being
  read at all (knuthing), or whose links are signed per visit
  (noodlemagazine). `ext` names such a queued video before it is read.
- **`listing`.** A listing of the site's videos, by `shape`: either a paged
  listing `walkListing` reads (`walk`: the first page and the page reader;
  the engine adds the caps and their notes), or one that pages its own way
  (`links`). Each entry is queued unread under its
  card's title and read like a peeked video.
- **`finished`.** A media playlist without `#EXT-X-ENDLIST` is refused as
  still being written, since it would finish as a fragment named as the
  whole. Only sites that publish videos while they are encoded set it
  (knuthing, noodlemagazine): elsewhere a playlist may be a live stream
  (ok.ru) or simply leave its end unmarked, and is taken as it is.
- **`proxyFallback`.** Opts the row's sources and files into proxy recovery
  (doodstream).

The script-reading rows (streamtape, mixdrop, lulustream) are the most fragile
extractors here, because each depends on the shape of a script the host can
change. Each therefore reads the numbers it needs off the page — the
substring offset, the packer's base and word list — rather than hard-coding
them. `unpackJS` reverses the p,a,c,k,e,d packer and is host-neutral.
`jwSource` reads a JW Player setup's `sources:` list (vidmoly, lulustream):
only that list is media, since a `file:` key above it can name the seek-bar
sprite, and a DASH entry is passed over for the HLS beside it.

## doodstream

doodstream is a row whose `read` is the whole token handshake, with its
retries, `RELOAD` handling and stage notes (`doodstream.go`); the row adds
only `proxyFallback`. `NewDoodStream` builds it for callers outside the
registry.

doodstream includes the `d000d.com` and `playmogo.com` aliases. An ordinary
page embedding one of its players is reached by the generic link scanner, and
the player goes through the native token resolver and the range engine, so
linked players work without an extractor for every blog that embeds them.

The final page URL after redirects supplies both the token endpoint's origin
and the media referer, refreshed together on each attempt. The token endpoint
can also answer `RELOAD`, which the player handles with `location.reload()`:
repeat the whole page/token exchange on the same route, keeping its cookies
and re-reading the token path from the final page. The shared extraction retry
budget bounds this, with cancelable backoff, and a persistent `RELOAD` becomes
a transient failure.

### Gates and refusals

- **Cloudflare challenges** use the shared WAF recovery
  ([proxies.md](../proxies.md)); doodstream has no proxy group of its own.
  Cloudflare's interstitial is also recognised by its `_cf_chl_opt` script
  loading `orchestrate/chl_page` when the usual header is absent. Check this
  before accepting a token path: a challenge can echo that path in its
  options.
- **The player's own Turnstile gate** returns HTTP 200 without `cf-mitigated`:
  a `captcha-player` containing `turnstile-container`, without a media token,
  identifies it. The extractor hands that check to `httpx.GetPage`, which
  applies it before challenge capture and route scoring, so a challenged proxy
  is retried and is not credited with a successful transfer. A background
  Cloudflare script alone is not a gate.
- **A refused address** is answered with a 200, no gate and one bare sentence
  saying the resource "has been removed or is temporarily unavailable", for a
  video every other address is shown and for one that does not exist alike. A
  video that really is gone is shown the host's own HTML "Video not found"
  page. So `doodRefused` reads the missing markup, not the sentence, and
  returns a `RefusedError`: the route is held back for the site and the file
  tries another, outside the retry budget. Taken at its word, the sentence
  would fail the file on the first address that met it.
- **A removed video** is recognised by the not-found page's title (`doodGone`)
  and reported as the host having removed it, rather than as a page without a
  media token.

doodstream opts its sources and files into `ProxyFallback`, so temporary
network and HTTP failures and persistent reloads try the shared recovery pool
after the normal connection fails ([proxies.md](../proxies.md)).

### Diagnostics

Debug logs record the response status and type, the final player URL, the
route, the attempt and the gate markers, keeping raw HTML, cookies and
successful media tokens out of the logs. Both source discovery and deferred
refreshes inherit the job's logger. `ResolveNote` reports the player and token
stages; during source discovery, recovery adds the connection attempt, and
workers use the item's own counter.

## lulustream

lulustream (`lulustream.com`, `luluvdo.com`, `luluvido.com`) packs its JW
Player setup with p,a,c,k,e,d, and `jwSource` reads the HLS master out of it
once `unpackJS` has unpacked it:
signed for eight hours, one muxed rendition, AES-128 segments under one key.
The page is read again at transfer time, segments and key with it, through
`Target.Segments`. Its key server refuses Go's default HTTP client outright
with a 403 while serving the downloader's own client, so anything checking it
must go through `httpx.Client`. The title is the page's `<h1>`; the document
title carries the site's name. yt-dlp has no extractor for it.

## vids.st

vids.st states everything in one `playerConfig` object on `/v/<id>`: the
file's address, usually the uploaded MP4 or MKV itself, tokenless and
rangeable, and its original name. An `.m3u8` address is followed natively, as
the player does. A removed video answers 404 with "Video not found", which is
reported as such. The site's certificate chain is short; see
[http.md](../http.md).

## sendvid

sendvid reads the player's one `<source>` off the watch page (`/<id>`; the
embed, `/embed/<id>`, carries the same player). It is signed for four hours
(`validfrom`, `validto`) with a `rate=250k` that the media host holds each
connection to, about 270 kB/s. The signature covers all three: with the rate
changed or left out, the link is a 401. The rate is per connection, though:
each of several connections at once gets all of it, so the extra connections
the engine opens for a slow file multiply it. The link is minted again when
the transfer starts (a `signed` row). `og:video` names `/<id>.mp4`, which
only redirects back to the page, and the embed's `<title>` is the site's name
alone, so the title is `og:title`. A video that is gone is a 404.

## knuthing

knuthing watch pages frame Bunny Stream. Read only the `bunnyframe` embed and
its `bunny-stream-video` element's `content-src`; thumbnails and sharing
embeds are not media. Free-video confirmation follows the page's own link for
that same video once. Refresh the page, embed, HLS segments and AES-128 key on
each download attempt, keeping the final embed URL as media referer. The row
peeks: a pasted video is queued by the watch page's title alone, and its
player is first opened when the transfer starts. `/videos/<section>` listings start at page one, read only their primary video
cards, and follow their own pager within the selected section and filters.
Cards supply queued titles without opening players. The walk is the shared
`walkListing` (`listing.go`), which reports source and file caps and
incomplete pagination in the job note; recovery reopens the individual video
rather than walking the listing.

## noodlemagazine

`/video/<query>` is a search, and `/watch/<id>` is one video. Searches
start at page zero, preserve filters and path escaping, and follow the
`more` control's `data-page` through `?p=`. GET renders the same results as
the site's scripted POST. Only cards inside `id=list_videos` belong to the
search: once it is exhausted the site serves a "Nothing Found" marker and
unrelated "Watching now" cards in a container with the same class but no id.
Those are never results: that marker ends the walk cleanly. The walk is the
shared `walkListing` (`listing.go`), which reports repeated pages, failed
pagination and source or file caps in the job note.

Cards supply queued titles without opening each player. The watch page's
`window.playlist` JSON supplies MP4 and HLS renditions, ranked by resolution
with MP4 preferred on a tie; previews and advertising URLs elsewhere in the
page are not sources. Media URLs are signed, so the row peeks: a pasted
video's page is read for its title and to see that it plays, and every
transfer attempt reopens its own watch page and uses the final page URL as
referer. HLS
segments, keys and separate tracks are refreshed together. The shared
browser-style HTTP client reads these pages; a plain curl request can meet
a Cloudflare challenge instead.

## ok.ru

ok.ru reads the embed page rather than the watch page. Served to a logged-out
caller the watch page carries an empty rendition list, and the player's own
metadata endpoint answers the same way; the embed page, which exists to be
framed by other sites, carries the identical structure filled in. No session,
token or player JavaScript is involved.

Its MP4s are ranked by name (`okruRank`: the site names renditions rather
than measuring them, and an unknown name ranks below every known one), and
the HLS manifest below them all, taken only when no file is offered. The
links are signed for the address that asked, so the row is `signed`. A page
that calls the video OK and lists nothing is asked for again within the
extraction retry budget; the backend serves such pages for minutes at a
time. A video the site calls gone says so in the site's words
(`okruWhyNot`), and an untitled one is named `ok.ru-<id>`.

## pornone

pornone reads its renditions from `<source>` elements and ranks them by the
dimensions in the filename, not by the `res` and `label` attributes: a page's
largest file is routinely marked 720p while its name says 1920x1080.

## eporner

eporner keeps its renditions out of the page. The page carries a video id and
a 32-character hash, and the endpoint refuses that hash as it stands: each
group of eight hexadecimal digits has to be re-expressed in base 36 and the
groups joined, with no padding, so a group of zeroes collapses to one digit.
What comes back is signed with an expiry *and* the requesting address, so it
is minted through `Resolve` at download time; a profile of a hundred videos
would otherwise start failing partway down its own queue. The media requests
carry no Referer. Profile tabs page by path rather than by a pager link, and
end on a 404, so the row's listing reads them with a walk of its own
(`listing.links`) rather than `walkListing`; each video is queued unread and
reopened on its own on recovery.

Two traps, both of which look like working code. The listing lives under
`streamevents`; the `plister` block beside it is the tab's header and holds no
tiles, so anything choosing a container by what precedes a tile rather than
what contains it finds nothing. And a profile also shows a sidebar of the same
tiles plus unrelated ones, so scraping every `/video-` link on the page
collects entries the tab does not own. The tab states its own total, which is
what `epornerProfileNote` compares against, so a short walk says so instead
of passing for a whole one. The pages are numbered, and asking past the last
answers 404, so the profile is a quiet `walkListing` that names each next
page by number: the stated total, not how the walk stopped, decides whether
the listing came back whole.

Titles need `epornerTitle` rather than the shared `trimSiteSuffix`, which cuts
at the *first* `" - "`. Titles here carry that separator themselves in a
numbered series, and cutting there leaves several videos sharing one name — on
disk, one file that each download in turn overwrites. The page's JSON-LD
states the exact name, and its breadcrumb list is in the same form, so the
type is checked rather than taking the first name found.

## Pointer pages: yandex, alohatube

`pointers.go` is one table for pages whose only job is to point at a video
hosted elsewhere: yandex's video previews (search output wrapped around
somebody else's video, the source linked beside the viewer) and alohatube's
view pages (an index framing another tube's player). Each row reads the
page's off-site links, likeliest first; `Extract` follows each through
`Registry.follow`, strictly, and returns the first an extractor here
resolves. Only when none is recognised — or the only answer was a challenge
nobody claimed — is the first link handed to yt-dlp, named by
`ytdlpTitle`.

The order matters both ways. A link a native extractor reads beats yt-dlp:
the frame alohatube was written for points at a tube whose media endpoint
yt-dlp no longer gets an answer from. And yt-dlp beats the generic fallback:
yandex's own yt-dlp extractor does not work, but yt-dlp's extractor for the
site a preview points at does, and strict following keeps an advert or an
unknown player from being saved as a page.

Each row is constructed by name (`NewPointer`) where its form sits in the
registry, since a form shared with another host goes to whichever comes
first: a preview's `/video/preview/<id>` is also a `/video/...` form that a
later host claims. `wirePointers` hands every row the registry once it
exists. "Off-site" is per row: yandex excludes its country domains and
`yastatic`, alohatube its own domain, since each page links its own furniture
the same way it links the video.

## Handoffs to yt-dlp

`handoffs.go` is the table of hosts passed whole to yt-dlp (youtube, odysee,
dailymotion, bilibili, soundcloud, mixcloud, rumble); its type comment says
why each is. A row normally becomes one `External` item named by
`ytdlpTitle` (rumble: by its oEmbed endpoint). A row with `expand` asks
yt-dlp to list the link flat instead (`Handoff.list`), so a playlist,
channel or set becomes one item per entry, bounded at the source by
`--playlist-end` one past the file cap, which is how truncation is noticed.
`expand` is on for youtube, odysee (channels), dailymotion (playlists) and
soundcloud (sets): each has a collection form and yt-dlp lists it flat in one
call. Bilibili stays off: a multi-part video lists as a playlist, which
would split one video into its parts. Entries listed without a title are
named by their page's slug (`entrySlug`) before their id; a page named by
its query has no slug, so a YouTube entry falls back to its id and, where it
names no page at all, is reached through the row's `watch` prefix.

## Kernel Video Sharing

KVS is a product, not a site. `kvs.go` holds the reusable parts (flashvars
parsing, license-code permutation) and `kvssites.go` the one `kvs` extractor
for every install, recognised by the platform's fingerprint below the site
root: flashvars carrying a `license_code`, the `kt_player` script, or the
asynchronous `list_videos` controls. There is no install list to keep.

Installs vary more than one product suggests, and three differences each
produce output that looks right and is not:

- The flashvars object is **scanned** rather than matched to a closing
  pattern: some installs put the brace on its own line, others pack the object
  onto one line and close it with `};` behind the last value, and a brace
  inside a quoted value must not end it early.
- A member listing's section is **kept, not rewritten**. An unknown member
  section is not a 404 here: it answers with a generic block of the site's
  newest videos under the member's own URL, which reads as a listing and
  downloads a stranger's work.
- Where an install pages the listing in script, leaving "next" pointing at
  `#`, `kvsAsyncPage` asks for the block directly. The `from` parameter the
  platform also accepts on a member listing is *ignored*, handing back page
  one forever, so the walk deduplicates and stops on a page that adds nothing
  either way.

### Listings and searches

A search, `/search/<query>/`, is the same walk (`kvslisting.go`) from a
different first page, and so is every other listing an install lays out: a
category, a model, a tag, a channel, the site's own latest list. Those are
recognised by shape, not by path, since an install may rename its sections
(one keeps its models under `/onlyfans-models/`). On a registered host, a URL
that is neither a profile nor a search is fetched once (`kvsBrowse`): a page
with a player is a video, a page with no player but a videos block is walked
as the listing it looks like, from the document already in hand, and the front
page is refused outright. A trailing page number means the whole listing, the
way a member's section means the member; a section whose own name is a number
is caught by that, so a canonical listing that lists nothing is retried as
pasted.

The pager is read, never guessed at. Which parameter pages a block is the
block's own business — `from` on a category, `from_videos` on a member's
videos, `from_videos` and `from_albums` together on a search — and the
platform does not refuse a wrong one: it ignores it and serves page one again,
which reads as the end of a one-page listing. So `kvsAsyncNext` builds the
block request from the "next" control's own `data-block-id` and
`data-parameters`, exactly as the site's script does, and a `from_videos`
guess is the last resort for a control that carries none.

Installs page a search in script, and the last page announces itself in the
pager: the "next" item is rendered with a span where the anchor was, which
`kvsListingEnds` reads so the walk stops before asking for the page after it,
which would be a 404. A search page also lists the albums matching the query,
in a block with a pager of its own, so `kvsListingBlock` and `kvsPager` choose
by name rather than by position. `kvsNextPage` treats an anchor at `#` as no
page at all, since following it resolves straight back to page one. The query
keeps the escaping it arrived with, because the site writes a plus as `%2B`
and re-encoding a decoded segment would hand its routing a literal plus.

`kvsListingPages` hands its pages to the shared `walkListing` as a quiet
listing, with the first document from the probe or the browse fetch. Its
reader stops at the stated total ahead of the cap, so a page reaching the
total is kept whole, and a later page that fails, repeats or adds nothing is
the end of the listing rather than a partial one.

### Signed rather than scrambled links

Not every install scrambles. moannest signs its links instead — a plain
`get_file` path with a `?v-acctoken=` that the path is refused without — and
`kvsRealURL` passes anything without the `function/0/` prefix through
untouched, so the link is kept whole, query and all. The token is stable
across renders and carries no expiry; the storage host it redirects to mints
its own two-hour token per request, which the downloader picks up by following
the redirect. So the URL is stored directly like any other install's rather
than resolved at download time.
