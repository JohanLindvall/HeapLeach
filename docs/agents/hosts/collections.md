# Albums, galleries and discographies

Notes on hosts whose pages are collections: indexes of other hosts' albums,
creators' posts, galleries and music. The shared listing machinery — source
expansion, `FanOut`, caps and folder names — is in
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
the shared `walkListing` as a quiet, numbered listing: its reader names the
next page by number and stops at the stated last page, however short, or the
cap, and a page that is missing or adds nothing is where the results end, not
a partial job. When none of the albums found resolves, the error is
`noneRead`'s, naming the first album's refusal or failure.

## coomerfans

A creator's page on coomerfans lists posts but keeps the media on each post's
own page, and the listing cannot be worked from instead: a post holding a
video shows no thumbnail there at all. So every post is opened, through
`resolveFiles`, and a profile none of whose posts could be read says why
(`noneRead`). The creator's pages are walked by `walkListing` as a quiet
listing following its next links: the listing states no length, so a later
page that fails or adds nothing ends it without a note. Every post costs a
request, so the walk is held to `listingLimit` and says so when it cuts.

## celeb.st

celeb.st image galleries carry their first page in Inertia's `data-page`
attribute and the rest at `/api/v1/creator/<slug>/media`, with
`mediaType=media` and the selected sort on every request. The `src` field is
the full-size image; thumbnails, avatars and single-image pages' `moreMedia`
recommendations are separate. URLs are base64 plus repeating XOR, as decoded
by the site's player, and the resulting image endpoint is stable rather than
signed. File names use the image id and extension, since the URL's basename is
an opaque token. A pasted later gallery page starts from page one, and the
walk follows the page count the first page states. A gallery that comes back
short of the creator's stated image count — a cap, a repeated page, a later
page that failed — says "N of M images" in `Result.Note` beside the walk's own
note for what stopped it, leaving the creator's folder name stable.

## cum.st

Creator pages use `/creators/<service>/<id>` and individual posts append
`/post/<post>`. The public API resembles Kemono's route names, but its schema
is different: post listings wrap records with a `total`, and attachments have
content hashes and variants rather than downloadable `path` fields. Keep its
dedicated extractor instead of registering it as a Kemono alias.

The page advertises interchangeable media servers in `EDGE_BASE_URLS`. Read
that list rather than pinning its numbered hosts. Originals are served at
`/media/<sha256>/<original variant name>`; `Mirrored` rotates the advertised
servers on retries. Thumbnails and video renditions are separate variants,
and only `original.*` is accepted. Use that variant's byte count, not the
attachment's top-level size, and align the filename extension with the bytes
served. A missing or generic `original` filename uses the content hash.

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
only to titles that actually repeat, so the ordinary case keeps clean names. A
walk that hits a rate limit it cannot outwait returns what it got, and its
`failed` hook marks the job "partial — rate limited" rather than letting it
pass for complete; the mark is a note, not part of the title, so the folder
stays put. The profile is a quiet, numbered `walkListing`: any other failed
page is the end of the profile. Every album costs a request of its own, so
the albums are held to `listingLimit`. They are read through
`resolveOutcomes`, which keeps a failed album's reason, so a profile none of
whose albums holds anything says why (`noneRead`).

## rajce

Album pages embed a JSON `settings` object whose `legacy_media` list carries
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
its one release. Each release of a discography is a page to read, so a
catalogue is read as far as `config.MaxResolvedReleases` releases, the note
saying what that left out, and the registry holds the tracks to the user's
file cap. The releases are read through `resolveFiles` (`resolve.go`); when
none can be, `noneRead` gives the first reason.

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
of `walkPages`, read until no list says it has more.
