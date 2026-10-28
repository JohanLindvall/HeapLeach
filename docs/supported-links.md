# Supported link formats

[Documentation](README.md) · [Project home](../README.md)

For the generated list of registered domains, see [every supported site](../README.md#every-supported-site). This guide explains accepted link formats and host-specific behavior.

| Host | Accepts | How it resolves |
|---|---|---|
| **gofile** | `/d/<code>` | Mints a guest token, then signs each API call with `sha256(ua :: lang :: token :: 4h-slot :: secret)` in an `X-Website-Token` header. The secret is rotated server-side, so it is read out of gofile's own obfuscated script rather than pinned: its string table is RC4 under a shuffled base64 alphabet, and the one rotation that decodes the whole file is what proves the answer. A signature gofile rejects is not repeated — it blocks addresses that sign badly. Downloads carry the token as a cookie. Walks nested folders. |
| **bunkr** | `/f/<slug>`, `/a/<slug>`, any `bunkr*` domain | Page → numeric file id → metadata endpoint → separate signing service for the `token`/`ex` pair the CDN demands. |
| **pixeldrain** | `/l/<id>`, `/u/<id>`, `/f/<id>` | Public JSON API. |
| **keep2share** | `/file/<id>`, with or without a filename, on `k2s.cc` or `keep2share.cc` | Free downloads through the public API. Reads the image CAPTCHA locally, waits for the host's timer, and reuses the resulting link when resuming. OCR model and runtime included in releases and the container image. Premium-only and private files are reported as restricted. |
| **fileboom** | `fboom.me/file/<id>`, with or without a filename | Uses FileBoom's own API with the same free-download protocol as Keep2Share, including local OCR and optional proxy routing. Tickets, scores and cooldowns are tracked separately for each service. |
| **filejump** | `filejump.com/s/<id>`, including `?access=download` | Reads the public share's filename and download button, then uses the native ranged downloader. Keeps the stable download endpoint so each transfer obtains a fresh storage signature. Rounded display sizes are estimates; expired or restricted shares fail before queueing. |
| **emload** | `/v2/file/<id>[/<filename>]`, `/file/<id>[/<filename>]` | Reads the free-download control when the transfer starts, keeping its page, token and file requests on one proxy route. Uses the shared pool with one file and one connection per address; a daily limit cools that route so another can take the file. Premium-only shares, size limits and unavailable files report their restrictions. A small file or a token in the page does not imply free access. |
| **turbo** | `/embed/<id>`, `/d/<id>` | `/api/sign` issues a short-lived signed URL. |
| **mega** | `/file/<id>#<key>`, `/folder/<id>#<key>`, and the older `#!`/`#F!` shapes | Nothing about a mega link is legible to the server: names, sizes and bytes are all encrypted under the key in the fragment. Attributes decrypt with AES-CBC; the payload is AES-CTR, undone as the bytes arrive, so ranges, resume and parallel connections all still apply. A link quoted without its `#` fragment cannot be opened by anyone. Downloads use the normal connection until mega says this address's transfer quota is used up; files then go on through proxy routes, one to an address, each route resting until its own quota is back. With proxies off they wait for the quota instead. |
| **dropbox** | `/s/…`, `/scl/fi/…`, folder shares | Asks for `dl=1`, with the content host kept as a mirror to fail over to. A folder share downloads as the zip dropbox builds for it. |
| **mediafire** | `/file/<key>/<name>`, `/folder/<key>` | The download link is on the file page, signed per visit, so the page is re-read when the item starts. Folders come from mediafire's own listing API, recursively. |
| **google drive** | `/file/d/<id>`, `?id=<id>` | Walks past the virus-scan confirmation page — HTML, with a token minted per visit — so the transfer is handed real bytes. Folder links need an API key and are not supported. |
| **svtplay** | `/video/<id>/…`, `/<programme>` | Open player API. A programme, or its card's link `/video/<id>/<programme>` (an episode's own link adds `/<episode>`), expands to every episode, grouped into season folders where there is more than one season, read from SVT's GraphQL API rather than the page (which mixes in trailers and recommendations). |
| **youtube** | videos and playlists | Handed to yt-dlp, which is the only practical way in: YouTube withholds playback URLs behind BotGuard attestation on top of its signature and throttling parameters. Needs `make dependencies`. |
| **bandcamp** | albums, tracks, a band's discography | Read natively, no yt-dlp. Each track is filed as `<band>/<album>/<NN> - <title>.mp3`; a band's page resolves every release it lists. The MP3 is the 128 kbps stream anyone may play — nothing better is served without a purchase. |
| **odysee, dailymotion, bilibili, rumble, soundcloud, mixcloud** | videos and tracks | Also handed to yt-dlp, each for a reason recorded in the code so nobody re-derives it: Bilibili signs its playback URLs and ships demuxed DASH; Dailymotion fingerprints TLS and no longer offers a progressive rendition; SoundCloud needs a client id scraped from a rotating bundle; Mixcloud obfuscates its stream URLs; Rumble puts an interstitial in front of its media endpoint. Needs `make dependencies`. |
| **vimeo** | `/<id>`, `/<id>/<hash>`, channel and group links, `player.vimeo.com/video/<id>` | Read natively from the **embed player**'s configuration: the watch page answers anything but a browser with a bot check, while the player page an iframe loads is served to anyone with a browser's TLS. The stream is demuxed, video renditions with audio and subtitles in tracks of their own, all fetched and joined into one MP4 (needs ffmpeg); a progressive MP4 is taken when the stream cannot be read. Links are signed for an hour, and read again when the transfer starts. A video DRM-protected in every form it is offered in is refused, since nothing can decrypt it. |
| **niconico** | `/watch/<id>`, `nico.ms/<id>`, mylists, series, a user's uploads | Read natively through the site's own player API, no yt-dlp: the watch page names the qualities offered without an account, and a grant for the best of them mints an HLS stream whose audio is a track of its own and whose parts are AES-128 encrypted fragmented MP4, decrypted and joined into one MP4 (needs ffmpeg). The grant is minted when the transfer starts. Listings are read a page at a time off the API, leaving out what only an account or a payment fetches; any other page on the site is still handed to yt-dlp. |
| **booru** | tag searches and posts on 19 image boards, plus the booru.org network | One adapter per API family — Danbooru, e621, Moebooru, Gelbooru 0.2, Philomena — which is how a handful of engines covers a lot of sites. |
| **4chan** | `/<board>/thread/<id>` | The site's read-only JSON API; every attachment in a thread. |
| **vids.st** | video and embed pages | The page names the uploaded file itself, which is downloaded as it is, or an HLS playlist, followed natively. The site sends an incomplete certificate chain, which is completed from the certificate's own issuer address the way a browser does. |
| **lulustream** | video, embed and download pages | The player's setup is packed, and names an HLS playlist whose segments are AES-128-encrypted; they are decrypted natively as they arrive, and the playlist is read again when the file's turn comes, since its links are signed for eight hours. |
| **streamtape, doodstream, mixdrop** | watch and embed pages | Each assembles its link inside the player: two halves joined at an offset the page states, a token endpoint plus a random tail, or a packed script naming the delivery host. Every one of them reads the numbers it needs off the page rather than hard-coding what they were. |
| **rajce** | `rajce.idnes.cz/<user>/album/<slug>`, legacy album links on `rajce.net` | Reads the album's embedded media list in one request, preserving its title, filenames and order. Downloads full-size photos and MP4 videos, excluding thumbnails, video posters and recommendations. File limits and unavailable items are reported; private or password-protected albums require public access. |
| **pixhost** | `/gallery/<code>`, `/show/<group>/<file>` | A gallery's thumbnails say where the full images are: the two links differ only by host prefix and one path segment. Deriving them resolves a gallery of fifty in one request instead of fifty, and a mapping that ever changed would fail visibly with a 404 rather than quietly fetching thumbnails. |
| **suvobox** | `/a/<id>`, `/f/<id>` | The album listing states every file's id, its full name with extension and its size, so a whole album resolves in one request with real names in the queue from the start. The bytes come from the media host's `?raw=1`, which needs no token — the site's own signed `?dl=1` link would only expire while an item waited its turn. |
| **imagepond** | `/i/<code>`, the title-slug form of the same page, `/a/<code>` albums, and links straight to `media.imagepond.net` | The metadata names the item, but for a video it names the *poster frame*, and it has been seen misreporting a QuickTime file as MP4 — so the player element's own `data-src` is read first and the metadata is the fallback. An album arrives whole in one document, but its cards link only to each item's viewer, so the files behind them are resolved one at a time as they download rather than in a burst of fetches up front. An item the host has aged out answers 200 with an expiry notice rather than a 404, and is reported as expired instead of as a parse failure. A link to the media host is the stored file already, and is taken as given — that host is a subdomain of the site, so this extractor claims it and nothing else would. Profile pages list their items client-side and are still not supported. |
| **yandex** | `/video/preview/<id>`, on any of its country domains | Yandex hosts none of this: a preview is a viewer wrapped around somebody else's video, and the page links the source beside the player. That link is what is followed, and the video is left to yt-dlp, which knows far more hosts than this program does. Needs `make dependencies`. |
| **noodlemagazine** | `/video/<query>` searches, `/watch/<id>` videos | Walks search results from the first page, preserving duration, sort and HD filters and excluding unrelated recommendations. Queues video titles and refreshes signed playback links when each download starts. Selects the highest offered resolution, preferring MP4 over HLS at the same resolution; HLS keys and separate tracks are handled natively. Source and file limits apply, with incomplete listings reported. |
| **ok.ru** | `/video/<id>`, `/videoembed/<id>` | The watch page carries an empty rendition list for a logged-out caller, and the player's metadata endpoint answers the same way; the embed page, which exists to be framed elsewhere, carries the same structure filled in. Links are signed with an expiry and the requesting address, so they resolve at download time. |
| **imgur** | `/a/<id>`, `/gallery/<slug>-<id>`, `/<id>`, `/user/<name>`, `/r/<sub>` | The album page carries the same JSON the API returns, so no key is needed — and none is accepted either; the header imgur's own web app sends is not validated. A deleted image redirects to a real 503-byte PNG with a correct length that answers a ranged request, which no general rule can catch, so the extractor recognises that dead end itself. |
| **cyberdrop** | `/a/<id>`, `/f/<slug>` | bunkr's sibling. The album page states every filename and exact size in one request; the signed link is minted per attempt, which is what lets a file that will not sign fail on its own rather than sinking the album. |
| **archive.org** | `/details/<id>`, `/download/<id>` | One metadata call lists every file with an exact length and checksum. It is fetched **gently** — one connection, one file at a time — because archive.org answers parallelism with a clean 206 served 300× slower, per address, for minutes afterwards. A collection identifier is refused rather than "succeeding" by fetching five site logos. |
| **bitchute** | `/video/<id>`, `/channel/<name>` | Two unsigned JSON posts give a permanent MP4. Eleven seed hosts serve each other's paths byte-identically, so they are kept as mirrors — failover onto a set that cannot be wrong. |
| **civitai** | `/images/<id>`, `/posts/<id>`, `/models/<id>`, `/user/<name>/images` | Documented public JSON, unsigned content-addressed CDN. Collection links are deliberately **not** matched: the API silently ignores that parameter and returns the site's front page instead, which would look like a successful download of the wrong thing. |
| **pornpics** | `/galleries/<slug>-<id>/`, categories, `/pornstars/`, `/channels/` | A gallery's full-size URLs are in the page, so one request resolves it. Listings take one of two pagination routes, and the wrong one returns the first 20 items forever while looking like it works. |
| **bandzoogle** (family) | any install | One extractor for the band-website product, which musicians rent and run on their own domains — so unlike the families above it ships with no host list at all, and every site is found by the sniff. The pages label their own player: each track anchor carries the title, the artist and the path the audio comes from, so one fetch yields the lot. Files are named `NN Title.ext` under a folder per album, taking the position the page prints beside each track and padding it so a directory listing sorts the way the album plays — the folder matters because the numbering restarts with each album. A listing shows twenty and says whether more follow, so the rest are fetched too rather than the album arriving quietly short. The audio link is the site's own `/player/…/tracks/….mp3`, which redirects to storage signed for a couple of hours; the player link is what is kept, since the signature is minted per request and a stored one would go stale in the queue. |
| **chevereto** (family) | imgbb, freeimage, gifyu, and any install | One extractor for the image-host product, recognised by its own `generator` tag, so an unlisted install works without a rebuild. imagepond was one of these once and is not any more — it was rewritten onto software of its own and keeps its own extractor, which is why it is listed separately above. |
| **peertube** (family) | `/w/<id>`, `/videos/watch/<id>`, channels, accounts | ~1,795 federated instances behind one versioned API, confirmed by a version probe rather than guessed from markup. Unsigned, rangeable, exact lengths. A federated video's file may live on a different instance than the one asked, and the API says which. |
| **fediverse** (family) | Mastodon, Pleroma, Pixelfed, Lemmy, Misskey | Found through `/.well-known/nodeinfo`, which is a specification rather than a list, so the table cannot go stale. Three API dialects behind one sniff. |
| **foolfuuka** (family) | `/{board}/thread/<n>` on the 4chan archives | Where threads persist after 4chan drops them, through an API modelled on 4chan's own — and better than it, since each post carries the poster's original filename and an exact size. |
| **mediawiki** (family) | `Category:`, `File:`, articles, on any wiki | Commons, every Wikipedia, Fandom, any open wiki. A documented, versioned, keyless API returning untouched originals — the least likely thing here ever to break. |
| **nrk, rúv, ard, zdf, srf, vrt, arte, orf, yle, dr, rtbf, france.tv, radio-canada, npo, raiplay, rtve, rtp, pbs, npr, abc listen, bbc sounds** | programmes, series and episodes | svtplay's siblings, twenty-one more of them. Each skips what is DRM-protected, by the broadcaster's own flag wherever that flag means it, and reports a geo-block as the broadcaster states it rather than replacing it with a guess. All of them are fetched directly. A rendition carrying audio and video in the *same* segments is joined as it is; one that keeps its audio apart, as CMAF streams and anything with an audio description do, comes with every audio track, fetched beside the video and joined to it by ffmpeg. Subtitles the playlist offers are joined in as text tracks. ARD and RTVE hand back plain rangeable files with exact lengths, so they get the whole engine — splitting, resume and the skip check. |
| **vidmoly, streamable, wetransfer** | see above | Vidmoly is the highest-traffic embed host here; three of its four advertised domains are dead, so everything routes through the one that works. |
| **feeds** | any RSS or Atom feed | One file per `<enclosure>`, oldest first, since an archive wants the beginning. |
| *a bare `.m3u8`* | any adaptive manifest | Joined into a playable file, with every audio track when the playlist keeps its audio apart, and its WebVTT subtitles as text tracks (both need ffmpeg). A `.mpd` is refused with a reason rather than saved: DASH is usually demuxed, so concatenating it would yield a silent video. |
| *a directory listing* | Apache, nginx, lighttpd autoindex | Walked recursively, with sizes and structure taken from the listing. Also covers IPFS gateway directories. |
| *`links:<url>`* | any public page | Reads the page and downloads every link a supported host claims, each into its own folder. Aimed at the forum thread with two hundred links in it. |
| *an ordinary webpage* | any public HTML page without a dedicated extractor | Automatically reads links to every registered host, including albums and files, without a `links:` prefix. Ordinary HTML links are ignored so navigation cannot recursively crawl the web. Links keep their host's resolver and download settings; source and file caps apply. |
| **DarkGram albums** | any HTML page with a DarkGram player | Reads the first embedded album's videos and full-size photos from its `data-album` metadata. HLS playlists are resolved at download time and refreshed on retries; photos use their original links, never thumbnails. Files keep the page title and album order. File caps and unavailable items are reported. |
| **WordPress categories** | any domain; category permalinks, custom category bases and `?cat=<id>` | Recognises classic and block theme markup without a domain list or version check. Follows the selected category's pagination and reads each post's supported file-host links, embedded albums/players and JPEG images, preferring linked originals and full-size image metadata. File links go through the existing host extractors, including deferred K2S/FileBoom resolution and proxy pacing. Each post gets a folder; player thumbnails, navigation and related-post widgets are excluded. Source/file caps and incomplete results are reported. |
| **voyeurking** | `voyeurking.com/categories/<name>`, `/collection/<slug>` and `/video/<slug>` | Walks category and collection pages and reads each video's K2S or FileBoom file from its page data. Files use the corresponding downloader, including its per-address waits and proxy pool, enabled by default. Listing results keep each video in its own folder; source and file caps apply. |
| **balbums** | `balbums.st/?search=<query>` | An index of somebody else's albums rather than a host: a search is walked page by page and every album it lists is handed to the extractor that does host it, each into its own folder. Asking for a hundred results a page is what makes a search of three hundred albums four requests rather than eighteen — ask for more and the site quietly serves twenty, so nothing trusts the parameter. Only searches are taken. The charts are built in the browser and the front page is the same grid over the whole catalogue. |
| **KVS listings** | `/members/<id>/`, `/search/<query>/`, and any category, model, tag, channel or site-wide list on any KVS install | A member's public videos, everything a search turns up, or every video a category, model, tag or channel lists. Sections are recognised by the page rather than by name, since an install may rename them. The platform pages through its own asynchronous block loader, and which parameter pages a block is the block's own business — a wrong one is not refused, it serves page one again — so the walk sends exactly what the pager's own control carries. The last page says it is the last, and the walk stops there without asking. |
| *anything else* | any `http(s)` URL | Treated as a direct file link after checking for a known platform, a manifest, a directory index, supported download links or a player. |

Several of these hand out URLs that expire in minutes, so bunkr, turbo,
mega, mediafire, ok.ru, cyberdrop, streamable, wetransfer and the three
streaming hosts are **resolved at download time**, not when the link is
queued — otherwise a large queue would start failing halfway down.

Several entries above are **platform families**: one extractor covering
sites running the same software. On unregistered hosts, Pixeldrain-compatible
services, booru boards, FoolFuuka archives, Kemono-compatible services and
MediaWiki articles are detected through distinctive API responses or platform
markup. KVS detection also covers member, search and other listing pages
through its player or asynchronous block controls. Existing PeerTube,
Chevereto and Bandzoogle detection continues to work the same way.

A familiar URL path selects a possible API; it does not establish support.
Guessed API requests are bounded and do not retry. Recognised platforms
use their normal extractors, including pagination, deferred resolvers and
transfer pacing. Unidentified pages continue through the generic fallback.
Known-host registrations remain useful for discovering embedded download
links, naming supported sites, and retaining aliases and site-specific settings
such as booru filters. They are shortcuts, not a requirement for detecting a
compatible platform from a pasted URL.

Other entries are not hosts at all but **shapes** — an adaptive
manifest, an open directory, a page carrying supported download links,
and a page carrying a video in its markup.
Those cover the sites nobody will ever get round to naming.

Automatic webpage scanning reads anchors, embedded frames and links printed
as text, then expands only links claimed by registered extractors. A recognised
WordPress category also follows its own post permalinks and pagination;
ordinary navigation links are not followed. Exact duplicates are
removed; K2S aliases and repeated file IDs are also merged within each service.
Recognised platforms keep their own extraction behavior. Use `links:<url>`
to explicitly scan a page that would otherwise go to a dedicated extractor.

WordPress category extraction uses the HTML the site serves. It supports
classic post markup, block query loops, older/newer-post navigation, and
path or query pagination. A pasted later page starts from the category's
first page. Posts without supported file-host links use the shared album and
media-player parsers on their content, with DarkGram playlists still resolved
at download time. Separate JPEG photos are kept and duplicates removed.
Login-only content and listings rendered entirely by scripts
are not exposed by this parser. Domain redirects are followed, and relative
links use the final page's address.

Debug logs trace WordPress category pages, post counts, pagination and each
post's linked sources, including player/proxy progress and failures.
Files reach the queue after source resolution finishes;
these logs show which post and source are still being resolved during that wait.
Player diagnostics also record response status, content type, redirects,
challenge markers and connection route for discovery and token refreshes.
Debug logging defaults on; use `-debug=false` or `HEAPLEACH_DEBUG=0` to disable it.

Open **Logs** beside Settings to view the service's recent messages without
opening a terminal. The panel updates while visible, with search, severity
filters, pause/resume and a download of the filtered messages. Scrolling up
stops following new messages; **Jump to latest** resumes following. The service
retains up to 1,000 messages or 4 MiB for the current run, truncating unusually
large entries. Older messages roll off and a restart clears this history.
Console logging continues as usual.

## Mirrors and adaptive streams

Where a host offers the same file from more than one place — gofile's
storage servers, dropbox's two front ends — the alternatives are kept as
mirrors and a failed attempt lands on a different one.

SVT Play is an adaptive stream, so it has no single file to fetch. Where a
rendition carries **audio and video together**, the extractor picks it, and
the segments join into a playable file with no muxing at all. Newer
programmes come as CMAF, every rendition keeping its audio apart. Then the
video and **every audio track** are fetched side by side, each into a part
file of its own, and joined into one MP4 by ffmpeg: a stream copy, each
track labelled with its language and name, the playlist's default first.
The same goes for any broadcaster or bare `.m3u8` packaged that way. It
needs ffmpeg, and without it such a download fails before it fetches
anything rather than finishing without sound. Subtitles come along as text
tracks in the same MP4, each under its language and name, their cues where
the playlist puts them; a subtitle track that cannot be fetched is left out
rather than holding the video up.

Any finished `.ts` is rewrapped to `.mp4` when `ffmpeg` is available. The
rewrap is always a stream copy, so nothing is ever re-encoded: if the
streams cannot enter MP4 untouched, the `.ts` is kept instead. What comes
over is the best video plus the best audio and subtitle track **of each
language**, so a recording that shipped in several languages stays usable in
all of them without also carrying the duplicate encodes within each. Without
ffmpeg the `.ts` is kept and plays fine.
