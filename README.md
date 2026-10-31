# HeapLeach

**Paste a heap of links. Download them together.**

A bulk download manager for **180+ sites**, with a web UI and a command-line
mode. Expand albums, folders and playlists into a queue, download files in
parallel, and resume interrupted transfers. Runs on **Linux, macOS, Windows
and Docker**; the web UI is built into the executable.

[![CI](https://github.com/JohanLindvall/HeapLeach/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/JohanLindvall/HeapLeach/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/JohanLindvall/HeapLeach)](https://github.com/JohanLindvall/HeapLeach/releases/latest)
[![License: MIT](https://img.shields.io/github/license/JohanLindvall/HeapLeach)](LICENSE)

[**Download**](https://github.com/JohanLindvall/HeapLeach/releases/latest) ·
[Quick start](#quick-start) · [Documentation](docs/README.md) ·
[Supported sites](#supported-links) · [Contribute](CONTRIBUTING.md) ·
[Get help](SUPPORT.md)

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: light)" srcset="docs/screenshot-light.png">
    <img src="docs/screenshot-dark.png" alt="HeapLeach download queue with per-file progress, transfer speeds, pause controls and search." width="900">
  </picture>
</p>

## Why HeapLeach?

- **Batch downloads:** paste one link per line; supported albums, folders and
  playlists expand automatically.
- **Resumable transfers:** keep partial files, retry failures, and continue
  after an interruption.
- **Control your bandwidth:** adjust parallel downloads, connections per file
  and the native download speed limit while the queue runs.
- **A queue you can navigate:** search, filter, pause, retry individual files,
  and inspect logs from the browser. Light, dark and mobile layouts included.
- **Desktop or automation:** open the local web UI, pass URLs to the CLI,
  or run the Docker image on your server.
- **One application:** Go backend and embedded React UI. Releases include
  local CAPTCHA model data; Docker also includes the media helpers.

## Quick start

### Download and run

1. Open the [latest release](https://github.com/JohanLindvall/HeapLeach/releases/latest)
   and choose the archive for your system:

   | System | Archive ending |
   | --- | --- |
   | Linux, Intel / AMD 64-bit | `linux_amd64.tar.gz` |
   | Linux, ARM64 | `linux_arm64.tar.gz` |
   | macOS, Apple silicon | `darwin_arm64.tar.gz` |
   | macOS, Intel | `darwin_amd64.tar.gz` |
   | Windows, Intel / AMD 64-bit | `windows_amd64.zip` |

2. Extract the **whole archive** into a folder. Keep `ocr/` beside the
   executable. No Go or Node.js installation is needed.
3. Run `heapleach` (`heapleach.exe` on Windows). It opens your browser and
   saves to your Downloads folder.
4. Paste your links, one per line, and click **Add to queue**.

<details>
<summary>Terminal commands for Linux, macOS and Windows</summary>

Run these from a folder containing only the archive version you want to use.

```bash
# Linux x86-64; use linux_arm64 for an ARM64 machine.
tar -xzf heapleach_*_linux_amd64.tar.gz
./heapleach
```

```bash
# macOS Apple silicon; use darwin_amd64 for an Intel Mac.
tar -xzf heapleach_*_darwin_arm64.tar.gz
./heapleach
```

```powershell
# Windows PowerShell
Expand-Archive -Path .\heapleach_*_windows_amd64.zip -DestinationPath .\heapleach
.\heapleach\heapleach.exe
```

</details>

An argument-free launch uses a free local port. After you close the browser,
it exits once downloads are idle and no tab has connected for a minute.
Unfinished jobs return **held** after a restart; click **Resume** to continue.

See [installation and updates](docs/installation.md) for checksums, Docker
persistence, source builds and platform notes.

### Docker

On Linux or macOS with Docker, run:

```bash
mkdir -p "$HOME/Downloads"
docker run --rm -it --name heapleach \
  -p 127.0.0.1:8080:8080 \
  --user "$(id -u):$(id -g)" \
  -e HEAPLEACH_STATE=/downloads/.heapleach-state/queue.json.zst \
  -v "$HOME/Downloads:/downloads" \
  ghcr.io/johanlindvall/heapleach:latest
```

Open **[localhost:8080](http://localhost:8080)**. Downloads and queue state stay
in your Downloads folder when the container stops. The image supports amd64
and arm64 and includes yt-dlp, ffmpeg, ffprobe, deno and local OCR. `latest`
follows successful main builds; use a [versioned image](docs/installation.md#docker-and-persistent-state)
to pin a manual release.

The web UI has no login. The command above publishes it only on your computer;
see [network access](#network-access-and-trust) for remote deployments.

## Usage

<a id="on-the-command-line"></a>

```bash
# Open the UI on a free local port and choose the destination.
./heapleach -addr 127.0.0.1:0 -open ~/Downloads

# Download a list of URLs, then exit. Blank lines and # comments are ignored.
./heapleach - ~/Downloads < urls.txt

# Limit native download speed and parallel transfers.
./heapleach -addr 127.0.0.1:8080 -concurrency 4 -max-speed 10MB ~/Downloads

# Show every command-line option.
./heapleach -help
```

`urls.txt` contains your own URLs, one per line. On PowerShell, use
`Get-Content .\urls.txt | .\heapleach.exe -` to feed a list to the CLI.
To pass URLs directly, quote each one, especially links containing `&` or `#`.

The CLI exits `0` on success, `1` if any input or download fails, and `130`
after Ctrl-C. Partial files remain available for resume. Native transfers
honor the shared speed limit; external helpers use their own controls.

[Full usage guide](docs/usage.md) · [Configuration](#configuration) ·
[Troubleshooting](SUPPORT.md)

## Supported links

File hosts, image galleries, video sites, broadcasters, image boards and
compatible platforms are supported, including:

| Content | Examples |
| --- | --- |
| Files and folders | gofile, bunkr, MEGA, pixeldrain, MediaFire, Dropbox |
| Video and playlists | YouTube through yt-dlp, Vimeo, PeerTube |
| Broadcasts | SVT Play and other supported broadcasters |
| Galleries and collections | Chevereto, image boards, Internet Archive |
| Direct and embedded media | File URLs, HLS/DASH playlists, open directories, supported links embedded in webpages |

<a id="image-boards"></a>

Some workflows need [optional helpers](#optional-helpers). Support depends on
what a host makes available; see [accepted URL formats and limitations](docs/supported-links.md).
Sites change, so fixture tests alone cannot guarantee current live support.

### Every supported site

<details>
<summary>Expand the complete site inventory, generated from the extractor registry</summary>

<!-- BEGIN HOSTS -->
<!-- Generated by `make hosts`. Do not edit by hand. -->

97 extractors, each chosen by what a page serves rather than by its domain, so a site that moves, or an install nobody listed, is still recognised.

| Extractor | Recognises |
|---|---|
| `4chan` | 4chan threads (`/<board>/thread/<number>`) whose page renders the thread's own `t<number>` container on the board and names the site |
| `abclisten` | episodes and programmes (`/listen/programs/…`) of ABC listen, by the page props its `__NEXT_DATA__` island carries |
| `alohatube` | Aloha Tube view pages (`/ktm/view.cgi?gid=<id>`) that frame another site's player (an off-site `<iframe allowfullscreen>`), resolved where the video lives |
| `archive.org` | items (`/details/<identifier>`, `/download/<identifier>[/<file>]` and the other item routes) whose page is the archive's item page or file listing, or whose metadata API answers for the identifier |
| `ard` | videos (`/video/…/<id>`) and programmes (`/sendung/`, `/serie/`, `/film/`) of the ARD Mediathek, whose page loads from its page gateway |
| `arte` | programmes and collections (`/<lang>/videos/<id>/`, `/embeds/<lang>/<id>`) of ARTE, whose page calls its player API (`/api/player/v2/`) |
| `autoindex` | generated directory listings at any path: a page titled as an index of its own directory (`Index of /…`, `Directory listing for /…`) that links at least one entry inside it |
| `balbums` | searches of a Bunkr album index (`/?search=<query>`) whose page carries the index's search form and its "Page n of m" result count |
| `bandcamp` | releases (`/album/<slug>`, `/track/<slug>`), a band's `/music` and its front page, where the page is built from Bandcamp's client bundle and carries `data-tralbum` release data or the music grid |
| `bandzoogle` | pages of Bandzoogle band sites, at any path, whose player lists tracks as `data-zoogle-track` anchors |
| `bbcsounds` | episodes, brands and series (`/sounds/play/<pid>`, `/sounds/brand/<pid>`, `/sounds/series/<pid>`) served by the BBC Sounds web app |
| `bilibili` | videos (`/video/BV<id>`) and bangumi episodes whose page carries the bilibili player state (`window.__INITIAL_STATE__` with its play information), short links included |
| `bitchute` | videos and channels (`/video/<id>/`, `/embed/<id>/`, `/channel/<id>/`) served by the BitChute web app, a video naming its embed player |
| `booru` | image-board tag searches and posts on Danbooru, e621, Moebooru, Gelbooru, Philomena, Twibooru and szurubooru boards (`/posts?tags=`, `/posts/<id>`, `/post/show/<id>`, `/index.php?page=post`, `/images/<id>`, `/post/<id>`) whose page carries the software's markup and whose API answers for the same post or listing; Gelbooru 0.1 boards by their listing markup alone |
| `bunkr` | file pages (`/f/<slug>`) carrying a numeric `data-file-id`, and albums (`/a/<slug>`) listing `theItem` cards that link to file pages |
| `celeb` | creator galleries and images (`/creator/<name>/images[/<id>]`, `/creator/<name>`) whose page is the site's Inertia app carrying the gallery's `Creator/Media` props in `data-page` |
| `chevereto` | Chevereto items, albums and user listings: pages below the site root carrying the platform's `CHV.obj` script namespace, or its generator or credit beside the item viewer, the `data-object` listing metadata or the password gate |
| `civitai` | images, posts, models and user galleries (`/images/<id>`, `/posts/<id>`, `/models/<id>`, `/user/<name>`) whose site's public API answers for that item on the page's own origin |
| `coomerfans` | creator pages (`/u/<service>/<id>/<name>`) listing that creator's `/p/` posts, and posts (`/p/<post>/<id>/<service>`) carrying the `post-body` the media is read from |
| `cum` | creators and posts (`/creators/<service>/<id>[/post/<post>]`) whose page advertises its media servers as `EDGE_BASE_URLS` in the app's configuration |
| `cyberdrop` | albums (`/a/<id>`) listing `#file` entries with a `file-size`, and file pages (`/f/<slug>`, `/e/<slug>`) whose script asks the `/api/file/info/` metadata endpoint |
| `dailymotion` | videos and playlists (`/video/<xid>`, `/playlist/<xid>`) rendered by the Dailymotion server-side app (its `neon` bundle), short links included |
| `direct` | anything else: a page's supported links or embedded player, or the URL as a plain file |
| `doodstream` | players (`/e/<code>`) naming the DoodStream `/pass_md5/` token endpoint, landing pages (`/d/<code>`) naming DoodStream in their title or site metadata and framing its player, or either form showing its player gate or exact bare address refusal |
| `drive` | file links (`/file/d/<id>`, `/open?id=`, `/uc?id=`) whose page is the Google Drive file viewer (its `apps-fileview` bundle), or the download confirmation form |
| `dropbox` | share links (`/s/…`, `/sh/…`, `/scl/fi/…`, `/scl/fo/…`) whose page is the Dropbox viewer, loading its `/static/atlas/` bundle |
| `drtuber` | watch pages and players (`/video/<id>/<slug>`, `/embed/<id>`) whose player is configured from the DrTuber `player_config_json` endpoint |
| `drtv` | programmes, seasons and series (`/drtv/…/<slug>_<id>`) of DR TV, served by its app with the state it boots from |
| `emload` | file shares (`/v2/file/<id>/<name>`) whose page carries the `dl-file` download panel, or its removed-file or password notice |
| `eporner` | videos (`/video-<id>/<slug>/`, `/hd-porn/<id>/`) whose page sets the EPORNER player's video id and hash, and profile video tabs rendered with its profile script |
| `erome` | albums (`/a/<id>`) and profiles (`/<user>`) whose page carries the site's album markup (the album title heading and media group, or a profile's album links) |
| `fapello` | model pages and their posts (`/<model>/`, `/<model>/<n>/`) whose page links the model's own media under `/content/…/<model>/` |
| `fediverse` | fediverse accounts, communities and posts (`/@name`, `/users/<name>`, `/u/<name>`, `/c/<name>`, `/notes/<id>`, `/notice/<id>`, `/post/<n>`, `/p/<user>/<id>`) on any server whose nodeinfo names its software |
| `feed` | RSS and Atom feeds, at any URL: a response served as a feed type, or whose document element is `<rss>` or `<feed>` |
| `filejump` | public shares (`/s/<id>`) whose page offers the share's own `/s/<id>/download` button |
| `filester` | file shares (`/d/<slug>`) loading `file_dl.js` beside a `#downloadButton`, and folders (`/f/<id>`) listing file cards or asking for their password |
| `foolfuuka` | FoolFuuka archive threads, posts, searches and board pages (`/<board>/thread/<n>/`, `/<board>/post/<n>/`, `/<board>/search/…`, `/<board>/`) whose page names the software as its generator and loads its `/foolfuuka/` assets, or whose origin's `/_/api/chan/` answers the route with archived posts |
| `francetv` | video pages (`/…/<id>-<slug>.html`) and programme listings of France TV whose flight data names `video_factory_id`s |
| `gofile` | share links (`/d/<code>`) whose page is the Gofile app shell, loading its signing script |
| `hls` | bare HLS manifests: a response served as a playlist type or opening `#EXTM3U`, at any URL, or a `.m3u8` link; `.mpd` links are taken in order to refuse them with the reason |
| `imagepond` | viewer pages (`/i/<code>`) naming their stored file (`/media/…`, `/i/<code>/direct`) in the player or og:video/og:image, and albums (`/a/<code>`) of cards linking to viewer pages, on pages named ImagePond |
| `imgur` | posts and albums (`/a/<id>`, `/gallery/<slug>-<id>`, `/<id>`) whose page carries `window.postDataJSON` or its own media as og:image, and account, tag and subreddit listings (`/user/<name>`, `/t/<tag>`, `/r/<name>`) whose listing JSON answers |
| `keep2share` | file links (`/file/<id>`) whose page is the Keep2Share or FileBoom web app shell (its `entry-index` bundle and `chunk-client` preload on `#root`), named Keep2Share or FileBoom; the name chooses the service's own API |
| `kemono` | Kemono and Coomer creators and posts (`/<service>/user/<id>`, `…/post/<id>`) served by the sites' app shell, or whose origin's API returns the creator's matching profile |
| `knuthing` | watch pages (`/watch/<video>.html`) whose player frame or player block the KNUTHING theme renders, and its video listings (`/videos/<section>`) by their video list |
| `kvs` | Kernel Video Sharing pages — videos, member profiles, searches and listings — carrying the licensed player's configuration or script, or its `list_videos` blocks (asynchronous controls, or a container and its `_items` list) |
| `links` | a `links:<url>` prefix — every supported link on the page |
| `lulustream` | videos (`/<id>`, `/e/<id>`, `/d/<id>`) whose page names LuluStream and carries its player's HLS source |
| `mediafire` | file pages (`/file/<key>/…`) carrying the `#downloadButton` link, and folder shares (`/folder/<key>`) whose page is the MediaFire folder viewer |
| `mediawiki` | MediaWiki categories, articles, file pages and upload lists: pages naming a title (`/wiki/<title>`, `index.php?title=`) whose generator is MediaWiki and which carry its ResourceLoader configuration, or whose origin's `api.php` identifies a namespace route as MediaWiki |
| `mega` | file and folder links (`/file/<handle>#<key>`, `/folder/<handle>#<key>`, `/embed/…`, `#!…!…`) whose page is the MEGA web client's boot shell (`secureboot.js`, `#fmholder`) |
| `mixcloud` | shows (`/<user>/<show>/`) whose page, named Mixcloud, carries the app's `cloudcastQuery` for the show |
| `mixdrop` | players (`/e/<code>`) configuring the MixDrop player object (`MDCore`), and landing pages (`/f/<code>`) named MixDrop that frame or configure one |
| `niconico` | watch pages (`/watch/<id>`) describing their video in a `server-response` meta element, and mylists, series and users' uploads rendered by the niconico web client's bundle; live watch pages describing their program in the live client's embedded data are handed to yt-dlp |
| `noodlemagazine` | watch pages (`/watch/<id>`) named Noodlemagazine carrying the player's `window.playlist`, and search listings (`/video/<query>`) of its video cards |
| `npo` | episodes and series (`/start/afspelen/<slug>`, `/start/serie/<slug>`) of NPO Start, served by its app |
| `npr` | NPR pages carrying audio players (`data-audio`): stories, programmes, podcasts and sections |
| `nrk` | programmes and series (`/program/<id>`, `/serie/<slug>`, `/se?v=`) of NRK TV, whose page data names programmes by `prfId` |
| `odysee` | claims (`/@<channel>/<claim>`) whose page, named Odysee, offers the LBRY embed player (`/$/embed/`), and channels (`/@<channel>`) served by its app bundle |
| `ok.ru` | videos (`/video/<id>`, `/videoembed/<id>`, `/live/<id>`) whose page mounts the OK video player (`OKVideo`) with its `data-options` configuration |
| `orf` | episodes and series (`/video/<id>`, `/sendereihe/<id>`, `/profile/…`) of ORF ON, whose page carries the runtime configuration (`__NUXT__`) the API credentials are read from |
| `pbs` | videos (`/video/<slug>/`), shows (`/show/<slug>/`) and player links of PBS, by the player a video page embeds or the videos a show page lists |
| `peertube` | PeerTube videos, playlists, channels and accounts (`/w/<id>`, `/videos/watch/<id>`, `/w/p/<id>`, `/c/<handle>`, `/a/<handle>`) whose page is the PeerTube web client with its inline server configuration, or whose origin's `/api/v1/config` reports a server version |
| `pixeldrain` | pixeldrain files, lists and shared directories (`/u/<id>`, `/l/<id>`, `/d/<id>`) served by the software's viewer, which is handed its API root inline, or whose origin's API describes exactly that file, list or directory |
| `pixhost` | galleries (`/gallery/<code>`) of `/thumbs/<group>/<file>` thumbnails, and viewer pages (`/show/<group>/<file>`) showing an `img#image` from `/images/` |
| `pornhub` | video pages (`/view_video.php?viewkey=<key>`) named Pornhub whose player configuration (`flashvars_<n>` with its `mediaDefinitions`) is in the page |
| `pornone` | video pages (`/<category>/<slug>/<id>/`) named PornOne whose player lists renditions as `<source>` elements named by their frame size |
| `pornpics` | galleries (`/galleries/<slug>`) and category, tag, pornstar and channel listings whose page sets the site's `PP_LANG` global and marks its links `rel-link` |
| `radio-canada` | pages of ICI Radio-Canada and OHdio whose data layer (`IdsCollection`) names their media, and media console links |
| `raiplay` | videos (`/video/….html`) and programmes (`/programmi/<name>`) of RaiPlay, whose page mounts its `rai-player` |
| `rajce` | albums (`/<user>/album/<slug>`) whose page assigns the album's settings, media list included, in a script |
| `redgifs` | clips (`/watch/<id>`, `/ifr/<id>`) and users (`/users/<name>`) served by the RedGIFs app, a clip's page naming its embed player |
| `rtbf` | media, programmes and live pages of RTBF Auvio (`/media/<slug>-<id>`, `/emission/<slug>-<id>`) whose page addresses the Auvio API, and RTBF pages embedding Auvio players |
| `rtpplay` | episodes and seasons (`/play/p<id>[/e<id>]`) of RTP Play, whose page carries its player or its episode list |
| `rtve` | videos and programmes (`/play/videos/…`, `/v/<id>`) of RTVE Play, whose page names its asset or programme id |
| `rumble` | videos (`/v<id>-<slug>.html`, `/embed/v<id>`) that the origin's Rumble oEmbed endpoint describes |
| `ruv` | programmes and episodes (`/<station>/spila/<slug>/<id>`) of RÚV's player, whose page configures its GraphQL endpoint (`window._GQL_`) |
| `sendvid` | videos (`/<id>`, `/embed/<id>`) whose page, named Sendvid, carries the player's `video_source` |
| `soundcloud` | tracks and sets (`/<artist>/<track>`, `/<artist>/sets/<set>`) whose page is the SoundCloud web client hydrated with the sound (`__sc_hydration`) and offering its widget player |
| `srf` | programmes and shows of Play SRF (a `urn=` or a show's `id=` in the query) whose page names its `urn:srf:` media |
| `streamable` | videos (`/<id>`, `/e/<id>`) whose page, named Streamable, offers the site's player (`/t/<id>`) for embedding |
| `streamtape` | videos (`/v/<id>`, `/e/<id>`) whose page assembles the Streamtape `robotlink` media link |
| `suvobox` | albums (`/a/<id>`) listing `thumb-item` tiles, and file pages (`/f/<id>`) whose download button is the file's own `/f/<id>?dl=1` link |
| `svtplay` | videos (`/video/<id>`) and programme pages of SVT Play whose page data names the player API's `videoSvtId`s |
| `tnaflix` | video pages (`/<category>/<slug>/video<id>`) named TNAFlix whose player lists its renditions as `<source>` elements |
| `turbo` | video pages (`/embed/<id>`, `/d/<id>`, `/v/<id>`) whose player or download page calls the `/api/sign?v=` signing endpoint, or that frame that player |
| `vidmoly` | watch pages (`/w/<code>`, `/v/<code>`) of the VidMoly app naming the file's player, and players (`/embed-<code>.html`) whose JWPlayer source list names VidMoly; aliases are validated against the canonical player |
| `vids.st` | videos (`/v/<id>`, `/e/<id>`) whose page carries the player's `playerConfig` with its `videoUrl` |
| `vimeo` | player pages (`/video/<id>`) carrying `window.playerConfig`, and watch pages (`/<id>`, `/<id>/<hash>`, channel, group and album videos) of a site named Vimeo that offer that player for embedding |
| `voyeurking` | video pages (`/video/<slug>`) and category and collection listings (`/categories/<name>`, `/collection/<slug>`) whose React Router data names the `routes/video.$slug`, `routes/categories` or `routes/collection` route |
| `vrtmax` | programmes and episodes (`/vrtmax/a-z/…`) of VRT MAX, served by its app |
| `wetransfer` | download pages (`/downloads/<id>/<hash>`) and the short links (`/t-<code>`) that redirect to them, served by the WeTransfer app with the transfer's `securityHash` in its page data |
| `xhamster` | video pages (`/videos/<slug>-<id>`) named xHamster that carry the site's page state (`window.initials`) and player settings |
| `yandex` | video search previews (`/video/preview/<id>`) built by the Yandex search front end, naming the source video in its viewer |
| `yle` | programmes and series (`…/1-<id>`) of Yle Areena, whose page carries the Areena web app's configuration |
| `youtube` | videos (`/watch?v=<id>`, `/shorts/<id>`, `/embed/<id>`), playlists and channels whose page carries the YouTube web client's configuration (`ytcfg`) beside its player or page data, or the consent form that stands in for such a page |
| `zdf` | videos and show pages (`/…/<slug>-<number>`) of the ZDF Mediathek, whose page data names its PTMD player templates |
<!-- END HOSTS -->

</details>

## Features

See the [feature guide](docs/usage.md#features) for transfer behavior, queue
controls, naming, image-board support and recovery details.

## Screenshots

<details>
<summary>Light theme, mobile layout and terminal mode</summary>

<p align="center">
  <img src="docs/screenshot-light.png" alt="HeapLeach queue in light mode." width="560">
  <img src="docs/screenshot-mobile.png" alt="HeapLeach at phone width with a single-column queue." width="200">
</p>

<p align="center">
  <img src="docs/screenshot-cli.png" alt="Command-line downloads with progress bars, speeds and estimated completion times." width="860">
</p>

Use `-debug=false` for animated terminal progress. Debug logs are enabled by
default; pipes and log files always receive plain progress lines.

</details>

## Documentation

Start with the [documentation index](docs/README.md). If you are reading this
inside a release archive, the full [online documentation](https://github.com/JohanLindvall/HeapLeach/tree/main/docs)
is available on GitHub.

### Configuration

[Flags and environment variables](docs/configuration.md) cover download
locations, concurrency, bandwidth, queue state and proxy settings. Flags take
precedence over environment variables.

### Optional helpers

[Media helpers, local OCR and proxies](docs/helpers.md) explains which
workflows need extra tools, how to install them, and their platform limits.
The Docker image includes them.

### HTTP API

Use the [HTTP API reference](docs/api.md) to submit downloads, change settings
or consume the event stream.

### Network access and trust

HeapLeach is a single-user service without authentication. Anyone who can reach
its API can inspect the queue, submit downloads and change the destination.
Use a loopback address locally; remote access needs an authenticated HTTPS
reverse proxy and restricted access to the backend port.
[Read the network guide](docs/usage.md#network-access-and-trust).

### Architecture

<a id="what-happens-to-a-link"></a>
<a id="what-may-depend-on-what"></a>

The [architecture guide](docs/architecture.md) follows a URL from extraction
to disk and explains the package dependency rules.

## Development

To try a source build on Linux, install Git, GNU Make and Docker, then:

```bash
git clone https://github.com/JohanLindvall/HeapLeach.git
cd HeapLeach
make build
./bin/heapleach
```

Docker produces a Linux executable. On macOS or Windows, use the
[prebuilt release](https://github.com/JohanLindvall/HeapLeach/releases/latest),
run the Docker image, or follow the [native development setup](CONTRIBUTING.md#development-setup).

For hot reload, install the Go toolchain and Node.js 24, run `npm ci` in
`frontend/`, then `make dev` from the root. See
[CONTRIBUTING.md](CONTRIBUTING.md) for the complete setup and checks, and
[the development reference](docs/development.md) for build targets and releases.

<a id="layout"></a>
<a id="adding-a-host"></a>
<a id="releases"></a>

Jump to [source layout](docs/development.md#layout),
[adding a host](docs/development.md#adding-a-host) or
[release packaging](docs/development.md#releases).

## Contributing

Bug reports, documentation improvements, UI fixes and new extractors are
welcome. You do not need to know the whole codebase to help.

- [Read the contribution guide](CONTRIBUTING.md) for setup and suggested starting points.
- [Report a bug or request a feature](https://github.com/JohanLindvall/HeapLeach/issues/new/choose).
- [Get help](SUPPORT.md) or [report a vulnerability privately](SECURITY.md).
- Follow the [code of conduct](CODE_OF_CONDUCT.md).

If HeapLeach is useful to you, **star the repository** to help others find it.
Sharing a clear bug report or a small improvement helps the project too.

<details>
<summary>Star history</summary>

<a id="star-history"></a>

<a href="https://www.star-history.com/#johanlindvall/heapleach&Date">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=johanlindvall/heapleach&type=Date&theme=dark">
    <img alt="HeapLeach's GitHub stars over time" src="https://api.star-history.com/svg?repos=johanlindvall/heapleach&type=Date">
  </picture>
</a>

</details>

## About the name

<details>
<summary>Why “HeapLeach”?</summary>

Heap leaching is a mining method: crushed ore is piled
into a heap and irrigated from above, and the solution percolates down
through it, dissolving out the metal as it goes and draining to a pad at
the bottom. This does the same to a heap of links. The extractors percolate
through whatever the pages are made of — players, listings, signed
redirects, encrypted payloads — and what is worth keeping drains out into a
folder.

</details>

## Notes

The [operating notes](docs/usage.md#operating-notes) explain queue checkpoints,
partial files and live-site tests. Use modest concurrency and respect host
limits.

## License

HeapLeach is [MIT licensed](LICENSE). Bundled OCR assets and external media
helpers retain their own licenses; see the notices in `ocr/` and the
[OCR packaging guide](helpers/captcha/README.md).
