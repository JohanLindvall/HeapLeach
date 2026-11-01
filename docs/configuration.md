# Configuration reference

[Documentation](README.md) · [Project home](../README.md)

Every setting has an environment variable; the common ones also have a flag,
and a flag beats the environment. Sizes and rates take a unit — `5MB`,
`1.5GB`, `10GiB` — or a plain byte count.

| Variable | Default | Meaning |
|---|---|---|
| `HEAPLEACH_ADDR` | `:8080` | Listen address. Flag: `-addr`. |
| `HEAPLEACH_DIR` | your Downloads folder | Where files are written. Defaults to the platform's own download folder — `~/Downloads` on macOS and Windows, and on Linux whatever the desktop's XDG user-dirs file says, which is where a relocated or localised folder is recorded. The container image uses `/downloads` instead. Flag: `-dir`, or the positional argument. |
| `HEAPLEACH_CONCURRENCY` | `4` | Parallel transfers, from 1 to `HEAPLEACH_MAX_CONCURRENCY`. Flag: `-concurrency`. Also settable live in Settings, which is kept across restarts until this value is changed. |
| `HEAPLEACH_MAX_CONCURRENCY` | `256` | Ceiling on parallel transfers, for the startup setting and the Settings slider alike. Flag: `-max-concurrency`. |
| `HEAPLEACH_CAPTCHA_CONCURRENCY` | `4` | Keep2Share CAPTCHAs (FileBoom included) read at once, from 1 to 64. Each loads its own copy of the 54 MB model and runs two threads. Flag: `-captcha-concurrency`. |
| `HEAPLEACH_PROXIES` | on | Enable proxy routes for Keep2Share, FileBoom, EMLOAD, MEGA once this address's quota is used up, and WAF challenge recovery on any host. Set to `0` or use `-proxies=false` to disable. Also settable live in Settings. |
| `HEAPLEACH_PROXY_DB` | `proxies.db` beside `HEAPLEACH_STATE`, else `~/.local/state/heapleach/proxies.db` | Persistent bbolt inventory and health; honours `XDG_STATE_HOME` on Linux. Independent of queue persistence, including in CLI mode: with no queue kept, the default directory is used. Flag: `-proxy-db`. |
| `HEAPLEACH_PROXY_ENDPOINTS` | `direct` | Comma- or whitespace-separated HTTP, HTTPS or SOCKS5 proxy URLs; `direct` means the normal outbound connection, including environment proxy settings. Empty excludes that connection. Also editable live in Settings. |
| `HEAPLEACH_PROXY_FEEDS` | Proxifly's global text feed | Comma- or whitespace-separated feed URLs. Empty disables discovery. Also editable live in Settings. |
| `HEAPLEACH_PROXY_TYPE` | `any` | The protocol every proxy service draws its routes from: `any`, `socks` or `http` (HTTP and HTTPS proxies). `socks` or `http` leaves the normal connection out. Flag: `-proxy-type`. Also settable live in Settings, which is kept across restarts until this value is changed. |
| `HEAPLEACH_PROXY_TYPES` | none | A protocol per service, overriding `HEAPLEACH_PROXY_TYPE` for it: `keep2share=socks,fileboom=socks,emload=http,mega=any,cloudflare=any`, where `cloudflare` is WAF recovery on any host. `keep2share` and `fileboom` are separate groups: each service counts free downloads per address on its own. Flag: `-proxy-types`. Also settable live in Settings, kept as above. |
| `HEAPLEACH_PROXY_RETRIES` | `20` | Route changes after failures that make no disk progress. A proxy that cannot be reached, drops the connection or has its address refused does not count. A host's free-download cooldown does not spend this budget. |
| `HEAPLEACH_MAX_RETRIES` | `3` | Retries per request, per deferred download-link resolution and per native transfer. Temporary failures obtaining or refreshing a link retry with backoff. Transfer retries count attempts in a row that moved nothing: an attempt that downloaded anything before failing resumes after 30s and starts the count over. Flag: `-retries`. Busy responses and rate limits have separate bounded patience; a resolvable busy storage link can be refreshed repeatedly. |
| `HEAPLEACH_STREAMS` | `8` | Connections one slow file may be split across (1–16). Flag: `-streams`. Also settable live in the UI, which is kept across restarts until this value is changed. |
| `HEAPLEACH_SLOW_SPEED` | `2MB` | Rate per second below which extra connections are opened. Flag: `-slow-speed`. |
| `HEAPLEACH_MAX_SPEED` | `0` | Shared ceiling on the native download rate per second; `0` is unlimited. Does not throttle external helpers. Flag: `-max-speed`. Also settable live in the UI. |
| `HEAPLEACH_STALL_TIMEOUT` | `90s` | How long a transfer may make no progress before the attempt is retried. Flag: `-stall-timeout`. |
| `HEAPLEACH_MIN_FREE` | `10GiB` | Room that must be left at the destination before another transfer starts. Below it the queue waits rather than filling the disk; `0` turns the check off. Flag: `-min-free`. |
| `HEAPLEACH_STATE` | `~/.local/state/heapleach/queue.json.zst` (`$XDG_STATE_HOME` when set) | Where the queue is written so a restart can pick it up, as zstd-compressed JSON (`zstdcat` reads it). A plain-JSON file from an earlier version is still read, and a `queue.json` beside the default path is picked up once and retired. Unfinished jobs come back held, and are re-read when retried or when the queue is resumed; nothing is fetched until then. Empty disables it. A run given URLs on the command line never writes one. |
| `HEAPLEACH_USER_AGENT` | a current desktop Chrome UA | Sent on every request. Gofile mixes it into its signature, so it must match what signs. |
| `HEAPLEACH_LANGUAGE` | `en-US` | `Accept-Language`, and part of the gofile signature. |
| `HEAPLEACH_GOFILE_SECRET` | read from gofile | The secret gofile signs requests with. It is normally recovered from gofile's own script and cached for as long as that script says it is good for, so this is only needed if that ever stops working — setting it overrides the lookup entirely. |
| `HEAPLEACH_IA_FORMATS` | unset | Archive.org format labels to keep, overriding the per-mediatype rendition policy — the escape for when an item's interesting rendition is one the policy passes over. |
| `HEAPLEACH_UTLS` | unset | `HEAPLEACH_UTLS=0` turns the browser-shaped TLS handshake off and uses Go's standard one. A few hosts (wiki.gg) challenge the default fingerprint; a few others require it. |
| `HEAPLEACH_DEBUG` | `1` | Debug logging. Set `0`, `false`, `no` or `off` to disable it. Flag: `-debug=false`. |
| `HEAPLEACH_MAX_SOURCES` | `500` | How many of a page's links are followed, each one a full extraction: the albums of an index search, the links of a harvested thread, the entries of a listing that each cost a request to open (a profile's albums, a category's galleries, an account's posts), and the subcategories and categories a wiki walk visits (at most 200 there). CoomerFans posts and Erome albums use this as an inspection budget; empty or unavailable children use no file slots. Channels inspect at most 10000 videos through BitChute's API or 1000 through a federated channel, account or playlist, stopping sooner once successful files fill `HEAPLEACH_MAX_FILES`. A profile's deferred video tab uses the file limit or its 2000-video host bound. Folder shares have a fixed 5000-subfolder bound — except a Filester folder share, whose folders this limit bounds, and a Cum creator, whose posts it bounds beside the file limit. An imgur account or tag listing reads up to 5000 posts; a value set below the default lowers that, and a higher one leaves it be. Flag: `-max-sources`. |
| `HEAPLEACH_MAX_FILES` | `20000` | How many files one submitted URL may resolve to. A listing past it stops at a whole album and says so beside the job's name — never in its folder, so raising the cap and adding the URL again files the rest beside what is already downloaded. Flag: `-max-files`. |
| `HEAPLEACH_PPROF` | unset | Serve Go's runtime profiles at this address, on a listener of their own — `go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30` then shows where the CPU goes. Flag: `-pprof`. Off unless set. Name a loopback address: the profiles describe the process from the inside, and a non-loopback one is logged as a warning. In a container, the container's own 127.0.0.1 is reached with `docker exec`. |
| `HEAPLEACH_RESUME` | unset | Resume the jobs a previous run left unfinished at startup, instead of holding them for Resume or a retry. Flag: `-resume`. For a service that is redeployed without anyone watching — held would mean stopped until noticed. |
| `HEAPLEACH_OPEN` | unset | Open a browser once listening. Flag: `-open`. A bare run does this anyway, so this is mostly how to say **no**: `HEAPLEACH_OPEN=0` (also `false`, `no`, `off`) suppresses it, for a machine with no desktop or a session over SSH. |

`HEAPLEACH_EXTRA_HOSTS` names extra installs of a platform, as
`family:host,host;family:host` — the families are `kvs`, `peertube`,
`chevereto`, `foolfuuka`, `fediverse`, `mediawiki`, `booru`, `kemono` and
`bandzoogle`.
`HEAPLEACH_KVS_HOSTS` is a list of `kvs` hosts. Pages are recognised by what
they serve, so an install nobody listed is usually found anyway; a listed
install is also taken by its family when its page is not recognised — behind
a challenge or an age gate, or with its theme stripping the markup
recognition looks for — and its jobs are labelled by the install's name.
For any other unrecognised site, prefix the URL with `links:` to queue the
supported links on the page, or [report the site](../SUPPORT.md) with the
page it served.
