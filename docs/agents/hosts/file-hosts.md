# File hosts

Notes on hosts that store uploaded files. The shared machinery they use is in
[extractors.md](../extractors.md).

## Nested folders

gofile, mediafire, pixeldrain and filester folder shares are trees, and each
walks its tree through `walkTree` (`tree.go`); the host supplies only a
function that reads one folder into its entries, in the host's own order. The
walk is what bounds them all alike: folders deeper than
`config.MaxFolderDepth` are not read, a folder reached twice is read once, no
more folders are read than the user's source limit and no more files are
taken than the user's file limit. A subfolder's directory is its label
through `sourceFolder`, so a label with a slash in it is one folder, falling
back to the folder's id where the label makes no name. A subfolder that cannot
be read is left out rather than failing the share, and every cut — a limit or
an unreadable folder — is a "partial — …" note. An error reading the root, or
one the reader wraps in `treeStop` (filester's password gate), fails the job.

A folder that is itself paged — gofile's pages, mediafire's chunks,
filester's folder pages — is read
through `walkPages`, and a listing that stopped short (the page cap, a later
page that failed, a page that added nothing) is put on the walk's note with
`treeWalk.partial`. The reader stops reading once the folder's files exceed
`treeWalk.room()`: none past it could be taken, and the walk says the file
limit cut the share. The root of a gofile share is read before the walk,
against the file limit itself, and its note seeds the result's.

mega builds its tree from one listing and autoindex crawls a server's own
pages, so neither goes through it; their bounds and notes are their own,
below.

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

Timeouts reaching the metadata or signing service are temporary resolution
failures, handled by the downloader's bounded `resolveTarget` retries after
the HTTP request retries are spent. Keep these errors wrapped with `%w`, so
dial, TLS and request timeouts retain their types through the resolver.

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

Folders may be password protected — `Options.Password` supplies the password —
and the gate refuses with `ErrPasswordRequired` rather than resolving a
partial listing.

A folder offered from several storage servers gets a `MirrorHosts` resolver.
One that names a single server gets no resolver at all, so there is no mirror
to rotate to, and a busy server stops at `config.BusyRetryLimit`. A busy
storage server answers with the site's HTML shell and a `Content-Length` that
agrees, which `rejectWebPage` catches ([transfers.md](../transfers.md)).

The root folder is read in `Extract`, where a stale account token is renewed;
subfolders are read by the tree walk ([above](#nested-folders)) with the same
token and password, so a subfolder that refuses either is noted and left out.
A folder's pages end where `hasNextPage` says, or at a page without children.

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

A `/d/<id>` share is a directory of somebody's filesystem. The root's stat
answers whether it is a file or a directory, and a directory is walked one
stat per subdirectory ([above](#nested-folders)), each child addressed by
appending its name to the path just proved to work rather than by the node's
own path, which is relative to a filesystem the share may not open onto.

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

A folder share is listed whole in one request, and `megaDir` files each file
under its decrypted folder names, each one directory through `treeDir`: a
slash in a name is no folder of its own, and a name that makes none (empty,
undecryptable, `.` or `..`) is the folder's handle. A link may select one
file (`#<key>/file/<node>`) or one subfolder (`#<key>/folder/<sub>`); a
subfolder is picked out of the listing, its files filed from it down under
its name, and one the listing does not hold leaves the whole share.

### Transfer quota

mega counts a free transfer quota per address. Once an address has used it up,
the API answers a request for a link (`g` with `g:1`) with `e: -17` and the
seconds left in `tl`, and a storage server answers a transfer with 509
Bandwidth Limit Exceeded and the seconds left in `X-MEGA-Time-Left`. MEGA's
own SDK reads both, and waits an hour where no time is named
(`MegaQuotaWait`). Either is that address's wait (`WaitError`), not the file's
failure: the resolver returns it for the API's answer, and `File.Refused`
(`refused`) for the storage server's.

The files carry `megaPace`: the `mega` proxy group, one file to an address,
`PerRoute` and `DirectFirst`. A file starts on the normal connection, unpaced,
which beats any proxy while its quota lasts, and moves onto mega's routes once
that connection is made to wait ([proxies.md](../proxies.md)). A route whose
own quota runs out is held back from mega until it is back, and the file goes
on through another at no cost to its retries. Without proxies the file waits
in the queue for the quota. Links are asked for over https (`ssl: 2`), so a
proxy carries the bytes in a tunnel it cannot read or alter.

The waits are kept by route on the `Mega` value (`spent`), with the normal
connection under `httpx.DirectRoute` as Keep2Share keeps its cooldowns, so a
route known to be spent is answered from memory and the rest of the queue
learns a wait from the first file that met it. Link requests use a client
capped at `MegaRequestTimeout`, since they go through public proxies, and the
API's answers for congestion and for too many requests from an address (-3,
-4) are a `TransientError`, which through a route is tried again on another.

A single-file link asks for its link at extraction, which is where its name
and size come from. An address whose quota is used up is answered without one,
so the file is then described by the same command without `g`, which mints
nothing; its link is minted when the transfer starts, wherever that is. A
folder listing (`f`) asks for no links at all.

## emload

emload file shares use `/v2/file/<id>[/<filename>]` or the older
`/file/<id>[/<filename>]`. The registry's probe reads the share page once,
on the normal connection, to recognise it (a page view spends none of the
daily allowance); discovery then uses the URL's filename as a hint and
defers every request of its own to `File.Resolve`, after the worker leases
an `emload` proxy route. The `.free-downloader` carries a percent-encoded storage origin
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

filester file shares use `/d/<slug>`. The script POSTs the slug to
`/v2/api/public/download` and builds the storage link from the answer
(`server` + `/v2/` + `file` + `?token=`). The token lasts half an hour and
encodes the requesting address, so it is minted through `Resolve`, by
`signedResult` (`extractor.go`), as turbo's is: signed once at extraction so
a dead link fails the job at once and the queue shows the real name, and
again when the transfer starts. The `download=true&n=` the page's button adds only sets a disposition name and is
left off. The site moves between top-level domains, all serving one API; the
page is recognised by its download script and button, so a move needs
nothing here.

Folder shares use `/f/<id>`. `filesterfolder.go` reads only `filesGrid` cards:
their `onclick` names the file page, `data-name` preserves the original name,
and `data-size` is an exact byte count, unlike the rounded visible size. It
starts at page one, follows the folder's own next-page links, deduplicates
files, and walks `subfoldersGrid` links through the shared tree walk
([above](#nested-folders)). Each folder's pages go through `walkPages`, keyed
by file slug, which stops asking once the folder holds as many files as the
walk has room for and more pages remain. The page budget
(`config.MaxAlbumPages`) spans the whole walk rather than one folder, so each
folder is given what earlier folders left. A first page without a next link is
the whole folder, so one holding only subfolders is no empty page. A first
page that fails or carries no listing fails the share at the root and is a
note deeper down; a password refusal on any page fails it outright.
Incomplete results add a note without changing the folder title. Discovery
never signs individual files.

The password form requires its nonce and a base64-encoded UTF-8 envelope of
`password|Unix milliseconds|nonce`. Unlocking sets `folder_access_token`,
which the signing API also requires. This cookie has one name across folders,
so each folder retains a separate jar through `httpx.WithCookieJar`, including
on routed attempts. Missing or rejected passwords return `ErrPasswordRequired`.
A signing request refused with 403 revisits the folder gate and signs once
more, so a queue can outlive its access cookie. Storage transfers need only
the signed media URL.

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

## mediafire

A folder is listed through the API, files first and then subfolders, each in
chunks (`get_content.php`). `more_chunks` is the API saying whether another
chunk follows, so its absence ends the listing however empty the last chunk
is: a folder of only subfolders answers an empty file chunk. The files'
first chunk failing is the folder failing; once some files are read, the
subfolders failing leaves them standing with "unreadable folders". Past the
walk's room the subfolders are not asked for, since they come after the
files and could not fit either.

## Open directories (autoindex)

autoindex crawls a server's generated listings a level at a time, so a
level's directories are read concurrently and the bounds apply between
levels, where the counts are settled. It is bounded like a folder share: no
more directories than the user's source limit, no more files than their file
limit, and no deeper than `config.MaxDirectoryDepth`, the guard against a
symlink loop whose every traversal has a longer URL. Its notes are the tree
walk's words — "partial — folder limit", "file limit", "unreadable folders" —
for a bound that left a named directory unread, a cut file list, or a
subdirectory that would not load (which is skipped, not fatal).
