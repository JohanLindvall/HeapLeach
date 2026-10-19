# File hosts

Notes on hosts that store uploaded files. The shared machinery they use is in
[extractors.md](../extractors.md).

## bunkr

bunkr's links are signed for about twenty minutes, so files resolve through
`File.Resolve`. It **does not mirror**, so `Mirrored` has nothing to offer it.
Its metadata endpoint (`_001_v2`) names exactly one storage server per file in
a plain `mediafiles` string — never a list — and the signed path is scoped to
that one server: the other `noN` servers answer 404 for a file that is not on
them, and numbers past the last server do not resolve at all. Blind `noN`
guessing therefore only manufactures 404s, and a file whose one server is down
fails rather than failing over, which is correct, since nowhere else has it.
The resolver re-signs against that same host on each attempt for a fresh
token, not for a new location. To check whether that has changed, POST
`{"id": <fileID>}` to `<dlHost>/api/_001_v2` and read `mediafiles`: more than
one server there would be the first sign that rotation had become possible.

A server that is *down* rather than missing is the case worth knowing, because
bunkr's three steps disagree about it: the page carries the file id, `_001_v2`
answers with a real storage path, and the signing service mints a token — only
the storage host knows, and it says so with a bare nginx 403 at transfer time,
once per file in the album. The file page says it in words, so
`bunkrUnavailable` reads it there and the item fails at resolve time instead.
What is read is the download control being present and **disabled**, not any
particular sentence: on a servable page that control is the link to the
download host that `pageInfo` already parses, and on a refused one it is a
button with `disabled` and a title saying why. The wording is passed through
untouched, since which server is down and for how long is bunkr's to say. The
state is per storage server and an album spans several, so it is read per
file, which is also the only place bunkr publishes it. Album listing never
opens a file page: it reads names and sizes off the cards.

## gofile

gofile signs every API call with
`sha256(userAgent :: language :: accountToken :: floor(unix/14400) :: secret)`,
sent as `X-Website-Token`. The user agent mixed into that hash **must** be the
one actually sent, so signing reads `httpx.Client.UserAgent()` rather than a
separate constant; WAF recovery keeps the client's User-Agent for the same
reason. gofile rotates the secret server-side, so it is configurable via
`HEAPLEACH_GOFILE_SECRET`.

Folders may be password protected — `Options.Password` exists for this host —
and the gate refuses with `ErrPasswordRequired` rather than resolving a
partial listing.

A folder offered from several storage servers gets a `MirrorHosts` resolver.
One that names a single server gets no resolver at all, so there is no mirror
to rotate to, and a busy server stops at `config.BusyRetryLimit`. A busy
storage server answers with the site's HTML shell and a `Content-Length` that
agrees, which `rejectWebPage` catches ([transfers.md](../transfers.md)).

## pixeldrain

pixeldrain counts connections rather than files. An anonymous caller is
allowed only so many downloads open at once and the API says so outright, 403
with `max_concurrent_downloads` where the bytes would be, so an eight-way
split spends that whole allowance on one file and a queue of four is refused
before it starts. `pixeldrainPace` therefore asks for `Streams: 1`, which
costs nothing here because free downloads are speed-capped per account rather
than per connection, and which makes the number of connections the number of
transfers. `Files` is deliberately left unset: what the host allows is not
published, so nothing guesses at it and the downloader learns the rest by
being refused (connection-count limits in [transfers.md](../transfers.md)).
nova.storage runs the same software and shares this pacing.

## mega

mega encrypts everything client-side, so `mega.go` cannot ask the API for a
filename — it decrypts one. `megacrypt.go` holds the primitives (mega's
base64, the key split, ECB key unwrapping, CBC attributes) and is kept apart
from the API plumbing the way `kvs.go` is kept apart from its hosts. Two
things are worth knowing: the `"MEGA"` prefix on decrypted attributes is what
makes a wrong key recognisable, and it is also how a folder node's key is
found — every candidate is tried and the one whose attributes carry the prefix
is the right one. The per-file MAC is deliberately not verified; see the
comment on `megaFileKey`. The payload is AES-CTR, set as `File.Cipher` and
decrypted on the way to disk.

## emload

emload file shares use `/v2/file/<id>[/<filename>]` or the older
`/file/<id>[/<filename>]`. Discovery uses the URL's filename as a hint and
defers all requests to `File.Resolve`, after the worker leases an `emload`
proxy route. The `.free-downloader` carries a percent-encoded storage origin
(`data-srv`) and the `data-dl` token for `download.php?accesstoken=...`; read
them afresh on that route, keeping its cookies and the redirected page URL as
referer through the transfer. The page supplies the final filename; the
transfer supplies the exact size, since `.filesize` is rounded.

Require an enabled `.init-free-download` control: premium-only pages also
carry plausible storage tokens, including for files below the free size limit.
`dlsize`, `dlreached` and `serverblocked` are refusal states, not server
names. A daily limit returns `WaitError`, using `EmloadDailyWait` since the
site states no reset time; the pool cools that route for this service and lets
another address take the file. Keep free transfers to one file and one stream
per route.

The successful free-transfer path is covered by synthetic fixtures only, since
the live samples available are restricted, through a working proxy too.

## filejump

filejump public shares use `/s/<id>`, with `?access=download` on some pasted
links. The original filename is in the share's `.title`; `.meta` rounds the
size and includes thousands separators, so strip commas and mark it
`SizeApprox`. Keep the matching `/s/<id>/download` button URL, which redirects
to freshly signed storage at request time; the native downloader follows it
and supports ranges, so no eager signing or external helper is needed. A
player preview alone is not a download permission: require the public download
button and reject unavailable shares.

## filester

filester keeps nothing in the page: its script POSTs the slug to
`/v2/api/public/download` and builds the storage link from the answer
(`server` + `/v2/` + `file` + `?token=`). The token lasts half an hour and
encodes the requesting address, so it is minted through `Resolve`. The
`download=true&n=` the page's button adds only sets a disposition name and is
left off. Only `/d/<slug>` exists anonymously; folders are behind the
authenticated API. The site moves between top-level domains, all serving one
API, so `Match` takes `filester.<any TLD>` with the `/d/` shape rather than a
`hostSet` that would trail every move.

## suvobox

suvobox resolves an album from the listing alone, which is unusually generous:
each tile carries the file id, the full name *with* extension and the size, so
a whole album costs one request and shows real names while it queues. The
bytes come from the media host's `?raw=1`, which takes no token and honours
ranges. The site's own download button points at a signed `?dl=1` link, which
is worth knowing only to say why it is not used: it would expire while an item
waited its turn, and buys nothing.

## pixhost

pixhost resolves a gallery from its thumbnails, since a thumbnail's URL
differs from the full image's only by host prefix and one path segment. That
is one request instead of one per image, and if the mapping ever changes the
failure is a visible 404 rather than a directory full of thumbnails. Names
keep the site's id prefix because two images in one gallery can share an
original filename.

## imagepond

imagepond reads the player element before the page metadata, the reverse of
the usual order, on purpose. For a video, `og:image` is the poster frame and
`og:video:type` can claim MP4 for a QuickTime file, while the
`<video data-src>` is right. The metadata is still what identifies an *image*,
because the page's own furniture — its logo, avatars — is served from the same
media host, so scanning the markup for a hosted image finds the logo first.
`og:title` is form-encoded, so the filename needs decoding.

Some pages use a client-side player and carry no usable link in the markup at
all; for those, `/i/<slug>/direct` redirects to the media host.
`imagePondUsable` tries the hosted link first and falls back to
`imagePondDirect`, and both are needed, since pages of both kinds are served.
The fallback needs one guard: `util.NameFromURL` on a `/direct` URL yields the
literal "direct", which is not a filename.
