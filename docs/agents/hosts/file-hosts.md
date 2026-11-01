# File hosts

Notes on hosts that store uploaded files. The shared machinery they use is in
[extractors.md](../extractors.md).

## Share rows

Most of these hosts are one row of a table rather than an extractor of
their own: `Share` (`shares.go`) is the extractor, and a host is a
`shareSite` — its name, its inventory line, its link forms and the function
that reads a recognised link — registered through its `New…` constructor.
bunkr, cyberdrop, dropbox, emload, filejump, filester, imagepond, mediafire,
pixhost, suvobox, turbo and wetransfer are rows. A form is a `shape`, which
is what the link scanner follows, and a `recognise`, which takes a probed
page whose *final* URL has that shape; the first form the final URL has
decides, as the switch each host used to carry did. A form without
`recognise` is a link that only redirects to another form (wetransfer's
short links, mediafire's oldest `/?<key>`), followed for its form and never
recognised in its own right. `extract` reads the link as the host always
has, its own checks and error texts included. A row may cap its requests
(`timeout`, emload) or name an API base a test serves through `Share.api`
(cyberdrop, mediafire, wetransfer). `pageForms` are offered a page after `forms` and
left out of the row's Shape: a form another row's links already have,
recognised as this row's by its page alone, so the link scanner's label
stays the other row's (bunkr's `/i/` viewer beside imagepond's `/i/`).

Recognition still never reads a host: `TestShareFormsNeverReadTheHost`
applies `TestRecognitionNeverReadsTheHost`'s rule inside every row's forms.
What the rows share besides — `Share.root` for a page fetched with the
host's referer, failing as "<host>: fetch <url>: …", `albumTitle` for a job
named after an album page, `linkPathID` for a listing's `/<kind>/<id>`
links and `enabledControl` for a download button a page has not disabled —
is in `shares.go` too. gofile, mega, keep2share and pixeldrain keep types of
their own, since they hold state (tokens, quotas, cooldowns) or recognise by
asking an API, and Google Drive does because it recognises a link by the
submitted URL as well as the final one. Drive also takes a page of its file
forms that carries one of the refusals it words as a page (`driveRefusal`:
the download quota, a file not shared), whatever its status, and reports
it as the answer; the endpoints take an optional `/u/<n>` account prefix;
and a folder link (`/drive/folders/<id>`) named Google Drive is taken by
its form only to be refused, since listing one needs an API key.

### Links on a host's own domains

Most of these hosts are claimed by their domains before any probe
(`claimFirst` in `hostclaims.go`; [extractors.md](../extractors.md)): a link
on gofile, mega, pixeldrain, Keep2Share or FileBoom, emload, turbo, dropbox,
Drive, wetransfer, cyberdrop, filester, imagepond or filejump goes to its
extractor unprobed, with no page attached (`probed` answers nil). Each
extractor therefore reads such a link through its API, or fetches the page
it reads itself, and never depends on the probe: the API's or the page's own
refusal (gofile's "content not found", pixeldrain's `not_found`, filester's
signing message, cyberdrop's "there is no file", wetransfer's "no longer
exists", Keep2Share's "unavailable or has been removed", filejump's
"restricted, expired or password protected") is the job's error, rather than
a probe's status or the generic fallback's "holds nothing it reads".
mediafire reads its page and is claimed as a fallback: a page without the
download button is still mediafire's, which reports "removed, or need a
password". `TestFileHostClaimsReportTheHostsOwnRefusal` holds each to its
words through `Registry.Extract`.

Recognition and its withheld-page questions are for the same forms on
other hosts only. Those questions never go to a fixed service's API: gofile,
wetransfer and Keep2Share take no withheld page at all, since their APIs
would be asked about another host's id (gofile's at the cost of a guest
account, a signed request and, were it refused, the cooldown every real
gofile link then waits out). turbo and pixeldrain ask the page's own origin.

Dropbox, Drive and imagepond also claim file-named links on their domains
(`claimsFiles`): Dropbox's `?dl=1` and `dropboxusercontent.com` links keep
the file's name and, from the site, the rotation to the content host;
Drive's `/uc?id=` and `drive.usercontent.google.com/download?id=` are
named `drive-<id>` and walk the confirmation again on every attempt; a
`media.imagepond.net` file is imagepond's job, asked with the site's
Referer.

## Nested folders

gofile, mediafire, pixeldrain and filester folder shares are trees, and each
walks its tree through `walkTree` (`tree.go`); the host supplies only a
function that reads one folder into its entries, in the host's own order. The
walk is what bounds them all alike: folders deeper than
`config.MaxFolderDepth` are not read, a folder reached twice is read once, no
more folders are read than `config.MaxTreeFolders` and no more files are
taken than the user's file limit. A host with bounds of its own passes them
(`treeBounds`, `walkTreeWithin`): pixeldrain takes no more than
`config.PixeldrainMaxFiles` files, said as the listing limit, and filester
reads no more folders than the user's source limit, as both always did. A subfolder's directory is its label
through `sourceFolder`, so a label with a slash in it is one folder, falling
back to the folder's id where the label makes no name. A subfolder that cannot
be read is left out rather than failing the share, and every cut — a limit or
an unreadable folder — is a "partial — …" note. An error reading the root, or
one the reader wraps in `treeStop` (filester's password gate), fails the job.

A folder that is itself paged — gofile's pages, mediafire's chunks,
filester's folder pages — is read
through `walkPagesFor` (mediafire taking a chunk that adds nothing as the
list's end, `walkAccepts{repeated}`), and why a listing stopped short (the page cap, a later
page that failed, a page that added nothing) is handed to the tree as it is,
`treeWalk.ended`, which words it in the walk's note; no note is read back. The reader stops reading once the folder's files exceed
`treeWalk.room()`: none past it could be taken, and the walk says the file
limit cut the share. The root of a gofile share is read before the walk,
against the file limit itself, and its walk's end, rendered, seeds the
result's note.

mega builds its tree from one listing and autoindex crawls a server's own
pages, so neither goes through it; their bounds and notes are their own,
below.

## bunkr

Link discovery includes rotating aliases whose host label begins with
`bunkr`, through the explicit `bunkr*.*` host pattern. The pattern retains
the one- or two-label suffix bound and accepts subdomains; other services'
exact-label patterns are separate. File-page recognition uses its ordinary
page markers.

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
download host that `bunkrPageInfo` already parses, and on a refused one it is a
button with `disabled` and a title saying why. The wording is passed through
untouched, since which server is down and for how long is bunkr's to say. The
state is per storage server and an album spans several, so it is read per
file, which is also the only place bunkr publishes it. Album listing never
opens a file page: it reads names and sizes off the cards. File pages are
`/f/<slug>` and the older `/v/` and `/i/` viewers (`/i/` a page form only),
recognised by their `data-file-id` or, where a page names the file only
through it, by the id in the download button's `dl.` link
(`parseBunkrDownloadHref`, the fallback `bunkrPageInfo` reads).
An album without the cards is recognised by the site's name and plain
links to file pages, which the listing then reads.

Album entries combine cards with plain file links on every page. Cards are
read first so their names and approximate sizes survive duplicate plain
links; resolved file-page URLs deduplicate entries within a page and across
the walk. A repeated card can coexist with a fresh plain link, so it must
not prevent that scan. Both kinds of entry keep deferred resolvers. The
file cap applies to the combined list, and a repeated page with no new
entries ends the album without a partial warning.

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
A token that goes stale on a later page fails the whole folder read with
the stale-token error rather than keeping the pages before it, so the
root's renewal asks for the folder again with a fresh token.

Share links are `/d/<code>` and the legacy `/?c=<code>`, both Shapes. A
link on gofile's own host is claimed unprobed and read through the content
API whatever its shell looks like, so `HEAPLEACH_GOFILE_SECRET` keeps
working when the site moves or renames its signing script, and the
signature errors keep pointing at it. On any other host, a share link (and
the bare `/<code>` older links use, whose form alone is anybody's) is
recognised only when the app shell answers it; a page withheld from the
probe there is not taken, and the API is never asked about it.

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

A page without the software's viewer is settled by the API only for the
software's own eight-character ids (`pixeldrainGuessShape`): `/d/`, `/f/`
and `/u/` are every other share and player host's forms as well, and their
landing pages must not cost a round of API guesses each. A page withheld
from the probe (challenged, refused, failed) is asked about whatever its
id's length, since a compatible install names lists and directories as it
likes and no served landing page is in question. A file's API
link, `/api/file/<id>`, is a Shape of its own, the form a page that embeds
the file names it by, and resolves to that file.

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

A link whose boot shell was withheld from the probe is recognised by the API
instead (`Mega.answers`): a file whose attributes, asked without minting a
link, decrypt under the link's key, or a folder the API lists.

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

The waits are kept by route on the `Mega` value (`spent`, a `routeWaits`),
with the normal connection under `httpx.DirectRoute`, in the same store
Keep2Share keeps its cooldowns in, so a
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
`/file/<id>[/<filename>]`. A link on emload's own host is claimed unprobed,
so discovery makes no request at all, on the normal connection or any
other: it uses the URL's filename as a hint and defers every request to
`File.Resolve`, after the worker leases an `emload` proxy route. An address
emload refuses or challenges therefore costs nothing at discovery, which
is what the emload routes are for. The `.free-downloader` carries a percent-encoded storage origin
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

A pasted `/s/<id>/download` or `/content` link is probed to the signed
storage it redirects to, off the share's forms, so the row's `left` hook
(`shareSite.left`, for a submitted URL of a form whose final URL has none)
judges it by its share page instead, fetched once and held to the same
button rule; `extract` then reads that page for the name and size.

## filester

filester file shares use `/d/<slug>`. The script POSTs the slug to
`/v2/api/public/download` and builds the storage link from the answer
(`server` + `/v2/` + `file` + `?token=`). The token lasts half an hour and
encodes the requesting address, so it is minted through `Resolve`, by
`signedResult` (`contract.go`), as turbo's is: signed once at extraction so
a dead link fails the job at once and the queue shows the real name, and
again when the transfer starts. The `download=true&n=` the page's button adds only sets a disposition name and is
left off. The site moves between top-level domains, all serving one API; the
page is recognised by its download script and button, so a move needs
nothing here.

A file link may carry segments after the slug (a filename, say); the slug is
what is signed, so the form takes them.

Folder shares use `/f/<id>`. A folder page is recognised by what the walk
reads: its `#filesGrid` or `#subfoldersGrid` (either may be all a folder has,
and an empty folder still renders the grid, which makes its "no files" error
the answer rather than `direct` queueing the page), a card's script link, or
the password form. The folder half of `filester.go` reads only
`filesGrid` cards: their `onclick` (or `href`) names the file page, `data-name`
preserves the original name, and `data-size` is an exact byte count, unlike
the rounded visible size. It
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
waited its turn, and buys nothing. A file page is recognised by that button
naming the page's own path, a trailing slash on either side ignored.

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

An item the host has aged out answers 200 with an "expired" notice and no
media. The item form recognises that notice on a page named ImagePond
(`imagePondExpired`), so a pasted expired item is reported as expired in
the extractor's words.

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
