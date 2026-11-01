# Albums, galleries and discographies

Notes on hosts whose pages are collections: indexes of other hosts' albums,
creators' posts, galleries and music. The shared listing machinery — source
expansion, `resolveOutcomes`, caps and folder names — is in
[extractors.md](../extractors.md).

## balbums

balbums indexes somebody else's albums rather than hosting any, so it resolves
each result through the registry with the shared source expansion, the way the
`links:` harvester does. The walk stops **at** the cap rather than collecting
everything and truncating, because a two-word search can state twenty thousand
pages and walking them to throw all but five away is a lot of requests for the
same answer. And `per=100` is the most the site honours: asking for five
hundred is not refused, it is served as twenty under a page count for twenty,
so the walk trusts the stated page count and a page that adds nothing, never
the parameter. Without the shared self-exclusion, the site's own navigation
would turn one search into a walk of the whole catalogue. The pages go through
the shared `walkListing` as a numbered listing: its reader names the next
page by number and stops at the stated last page, however short, or the cap,
and a page that is missing or adds nothing is where the results end, not a
partial job. A later page that fails is a cut, and its note leads the job's. When none of the albums found resolves, the error is
`noneRead`'s, naming the first album's refusal or failure; when some do, the
note counts the caps against what the search found and the albums left out,
per reason (`expansion.note`).

A search is recognised by the index's search form alone: a search of one
page may state no page count, and the walk then ends at the page adding
nothing. The front page is recognised by the form together with its page
count over the whole index, and refused, so that `direct` does not follow
every album it promotes.

## coomerfans

A creator's page on coomerfans lists posts but keeps the media on each post's
own page, and the listing cannot be worked from instead: a post holding a
video shows no thumbnail there at all. Posts are opened page by page through
`listingFiles`, and a profile none of whose posts could be read says why
(`noneRead`); one with some unreadable posts counts them in the note. The creator's pages are walked by `walkListing` following
its next links. The listing states no length, so a page that adds nothing,
one without the listing or a 404 past the end is where it ends, with no
note; a later page that fails any other way is a cut, and noted. Every post costs a
request, so the source limit bounds inspected posts. The file limit counts
the files they produce: a text-only post leaves room for a later post, and
both child requests and pagination stop once enough files have been found.
Either budget cutting the walk is noted.
Posts the site refused are counted in the note (`refusedNote`).

## celeb.st

celeb.st image galleries carry their first page in Inertia's `data-page`
attribute and the rest at `/api/v1/creator/<slug>/media`, with
`mediaType=media` and the selected sort on every request. The `src` field is
the full-size image; thumbnails, avatars and single-image pages' `moreMedia`
recommendations are separate. URLs are base64 plus repeating XOR, as decoded
by the site's player, and the resulting image endpoint is stable rather than
signed. File names use the image id and extension, since the URL's basename is
an opaque token. A pasted later gallery page starts from page one, and the
walk ends on the page count the first page states (`pageOf.pages`). A gallery that comes back
short of the creator's stated image count — a cap, a repeated page, a later
page that failed — says "N of M images" in `Result.Note` beside the walk's own
note for what stopped it, leaving the creator's folder name stable.

## cum.st

Creator pages use `/creators/<service>/<id>` and individual posts append
`/post/<post>`. The public API uses Kemono's route names (`creatorAPI`
builds both), but its schema is different: post listings wrap records with a `total`, and attachments have
content hashes and variants rather than downloadable `path` fields. Keep its
dedicated extractor instead of registering it as a Kemono alias.

The page advertises interchangeable media servers in `EDGE_BASE_URLS`. Read
that list rather than pinning its numbered hosts. Recognition and
extraction of the probed page share one decode of it (`cumProbedEdges`, a
`memo` on the page); a page fetched afresh decodes its own. Originals are served at
`/media/<sha256>/<original variant name>`; `Mirrored` rotates the advertised
servers on retries. Thumbnails and video renditions are separate variants,
and only `original.*` is accepted. Use that variant's byte count, not the
attachment's top-level size, and align the filename extension with the bytes
served. A missing or generic `original` filename uses the content hash.

Each attachment of a creator's posts is queued with a transfer-time
resolver, so the walk is bounded by the file cap, counted in the
attachments the selection keeps (`cumKept`), and stops paging at it. Each
post is a source as well: the walk takes no more posts than the source
limit, asks no page for more than it leaves room for, and says "source
limit" when that cut it.

The creator API's compact posts already list filenames and attachment hashes.
Queue those directly, then fetch each full post once when its first transfer
starts, sharing successful metadata between that post's files. Failed fetches
remain retryable. This preserves source/file caps without opening every post
up front; sizes are unknown until the original variant is resolved. Post
folders include their ids so repeated titles and changed caps cannot merge
them. Individual post input reads full metadata immediately.

Listings start at offset zero, request `config.CumPageSize` records, and
advance by the count returned, ending at the stated `total`; a page of no new
post ids ends the walk too. The source cap bounds the posts and the file cap
the attachments queued from them. The `pp*` search, sort, tag, duration and
media filters are carried to the API; photo, video and audio filters also
select the matching attachments within mixed posts. Only the posts tab is
supported. Source/file/page limits, repeated pages, failed later pages and a
listing short of its `total` produce an incomplete-result note. Posts with no
attachments contribute no files.

## erome

erome paginates a profile, and two details decide whether it comes back
complete. The tab lives in the query string (`?t=posts`, `?t=reposts`) and
selects a different listing, so `eromeProfilePage` carries it onto every page;
dropping it walks the default tab instead and quietly returns the wrong set at
the right-looking count. And albums are foldered by title, which is not
unique: two albums sharing a title would collide into one folder, which looks
like lost files that did in fact download. `eromeFolders` appends the album id
only to titles that actually repeat, so the ordinary case keeps clean names.
Its form, "title id" compared case for case, predates the shared
`sourceFolders` ("title [id]", case-blind) and is kept through
`sourceExpansion.result(eromeFolders)`, since changing it would rename every repeated album
already downloaded. The title itself goes through `sourceFolder` like every
other folder, so a title carrying a slash or backslash (or a look-alike)
is filed with `_` in its place, which a run before that rule did not do. The
profile is a numbered `walkListing` that accepts as its end what the host
answers past the last page: a 404 or 410, a page adding nothing, or one
without the listing. A walk cut any other way returns what it got and says
so — "partial — rate limited" for a rate limit it cannot outwait, "could not
fetch next page" for a timeout or server error, "page limit" at the page cap
— rather than letting part of a profile pass for complete; the mark is a
note, not part of the title, so the folder stays put. Every album costs a request of its own, so
the source limit bounds inspected albums. Each page is expanded before the
next is fetched, and failed or empty albums spend no file slots. Albums
use `sourceExpansion`: taken whole up to the separate file cap, which is
noted where it cuts, with a failed album's reason kept, so a profile none of
whose albums holds anything says why (`noneRead`).

A deleted album answers 410 with the site's own page and no album markup;
the recogniser takes an album link answered that way by a page named for
the site, so a pasted dead album says it was deleted rather than getting
the fallback's refusal.

## rajce

Rajce is a `shareSite` row (`rajceSite`): one album form, recognised by the
settings script and its `legacy_media` list, read by `rajceExtract`. Album pages embed a JSON `settings` object whose `legacy_media` list carries
the album in order. Decode that object from its script, without collapsing
whitespace inside filenames or consuming the JavaScript after it. Read only
that list: `miniplayer_data` and promoted content elsewhere in the settings
belong to other albums. `image_url` is the full-size photo, with `version`
carried as `ver` after an edit; `thumb_url` is never a download source.

Videos use the separate `video_url`, an extensionless storage endpoint serving
rangeable MP4. Their `filename` can name a JPEG poster or just the media id, so
replace its extension with `.mp4`. Never substitute `image_url` when a video
is unavailable. Storage links are stable and need no deferred resolver. Keep
the final album URL as referer after redirects, including legacy `rajce.net`
links. Deduplicate by media id and URL; file caps, unavailable items and a
shortfall against `media_count` go in the note, leaving the album title stable.
An empty, missing or restricted album is an error, not a downloaded HTML page.

A storage link pasted on its own (`/storage/video/<id>` on an `img<N>.`
server) is no form of the row's: a host-free Shape would take every
`/video/<n>` link on the web. On the site's own hosts the host claim hands
it to `rajceExtract` once nothing recognised it (`rajceStorage`), and it is
the file itself. Derive its name with `util.NameFromURL`, then replace the
extension with `.mp4` when its path contains `/video/`, preserving the
storage video's stable filename. The download URL stays intact, including
its query, and the site remains its referer. Other storage paths retain
their URL-derived names. A storage link that names its file type is a file
the fallback downloads without a probe.

## bandcamp

Every bandcamp endpoint answers a request claiming to be a browser (Chrome and
Firefox alike) with a three-kilobyte client challenge page, while a request
naming itself (`bandcampAgent`) gets the real page. So every request there
overrides the client's browser User-Agent. The page's `data-tralbum` carries
the band, the release and per track its number, title and a `mp3-128` link
signed for a day, minted through `Resolve`. Tracks are filed
`<band>/<album>/NN - title.mp3`, the band and album each one folder through
`sourceFolder`; a track page's album is only in `data-embed`.
A band's front page answers 403 whatever the agent, so a discography is read
from `/music` — which also carries a `data-tralbum`, empty and of no
`item_type`, so the kind is what separates it from a band whose `/music` is
its one release. The same goes for recognition: a site root behind the
challenge, or withheld from the probe some other way, is judged by its
`/music` listing asked for with `bandcampAgent` (memoised on the page as
`bandcampMusicKey`, which the discography reads rather than fetching it
again), never by asking the root once more. Each release of a discography is a page to read, so a
catalogue is read as far as `config.MaxResolvedReleases` releases, the note
saying what that left out, and the registry holds the tracks to the user's
file cap. The releases are read through `resolveListing` (`resolve.go`);
when none can be, `noneRead` gives the first reason, and releases refused or
unreadable beside others that resolved are counted in the note
(`outcomeSummary.note`).

## bandzoogle

bandzoogle is website software musicians rent, so every install answers on
its owner's own domain. It is recognised by its page alone, with no URL form
to follow links by. The marker is `data-zoogle-track` on the track anchors,
which the player script binds to rather than a theme supplying, and the same
anchor carries the title, the artist and the audio path, so nothing is read
from the surrounding markup, which differs from one band's site to the next.
The audio path redirects to storage signed for a couple of hours, minted per
request, so the player link is what goes on the `File`; the signed one would
go stale in a long queue. A track list shows twenty and says whether it has
more (`data-load-more`, `data-offset`); each continuation it names is a page
of `walkPages`, read until no list says it has more. The first page reuses the
tree recognition parsed only when the probe sent the reader's Referer;
otherwise it is fetched with that Referer. Each continuation is fetched and
parsed on its own.
`TestBandzoogleFollowsEveryListThatHasMore` pins the request sequence.
