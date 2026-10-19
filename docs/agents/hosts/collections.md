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
would turn one search into a walk of the whole catalogue.

## coomerfans

A creator's page on coomerfans lists posts but keeps the media on each post's
own page, and the listing cannot be worked from instead: a post holding a
video shows no thumbnail there at all. So every post is opened, through
`FanOut`.

## celeb.st

celeb.st image galleries carry their first page in Inertia's `data-page`
attribute and the rest at `/api/v1/creator/<slug>/media`, with
`mediaType=media` and the selected sort on every request. The `src` field is
the full-size image; thumbnails, avatars and single-image pages' `moreMedia`
recommendations are separate. URLs are base64 plus repeating XOR, as decoded
by the site's player, and the resulting image endpoint is stable rather than
signed. File names use the image id and extension, since the URL's basename is
an opaque token. A pasted later gallery page starts from page one; file and
page caps and repeated pages are reported in `Result.Note`, leaving the
creator's folder name stable.

## erome

erome paginates a profile, and two details decide whether it comes back
complete. The tab lives in the query string (`?t=posts`, `?t=reposts`) and
selects a different listing, so `eromeProfilePage` carries it onto every page;
dropping it walks the default tab instead and quietly returns the wrong set at
the right-looking count. And albums are foldered by title, which is not
unique: two albums sharing a title would collide into one folder, which looks
like lost files that did in fact download. `eromeFolders` appends the album id
only to titles that actually repeat, so the ordinary case keeps clean names. A
walk that hits a rate limit it cannot outwait returns what it got, and
`eromeProfileNote` marks the job "partial — rate limited" rather than letting
it pass for complete; the mark is a note, not part of the title, so the folder
stays put.

## bandcamp

Every bandcamp endpoint answers a request claiming to be a browser (Chrome and
Firefox alike) with a three-kilobyte client challenge page, while a request
naming itself (`bandcampAgent`) gets the real page. So every request there
overrides the client's browser User-Agent. The page's `data-tralbum` carries
the band, the release and per track its number, title and a `mp3-128` link
signed for a day, minted through `Resolve`. Tracks are filed
`<band>/<album>/NN - title.mp3`; a track page's album is only in `data-embed`.
A band's front page answers 403 whatever the agent, so a discography is read
from `/music` — which also carries a `data-tralbum`, empty and of no
`item_type`, so the kind is what separates it from a band whose `/music` is
its one release.

## bandzoogle

bandzoogle is a platform family with an empty host list: it is website
software musicians rent, so every install answers on its owner's own domain
and there is nothing to compile in. It is reached entirely through the
direct-fallback sniff, with `HEAPLEACH_EXTRA_HOSTS`'s `bandzoogle` family for
naming one outright. The marker is `data-zoogle-track` on the track anchors,
which the player script binds to rather than a theme supplying, and the same
anchor carries the title, the artist and the audio path, so nothing is read
from the surrounding markup, which differs from one band's site to the next.
The audio path redirects to storage signed for a couple of hours, minted per
request, so the player link is what goes on the `File`; the signed one would
go stale in a long queue.
