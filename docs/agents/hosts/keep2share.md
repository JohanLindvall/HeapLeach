# Keep2Share (with FileBoom) and the CAPTCHA reader

`keep2share.go` and `voyeurking.go` in
`internal/extractor`, and `internal/captcha`. Keep2Share, on its own hosts
and on FileBoom's, is a per-address service: each address gets one free
download at a time, which is what the proxy pool is for. How routes are leased, scored and cooled is in
[proxies.md](../proxies.md).

## The free-download flow

keep2share uses the public `/api/v2` free-download flow, on both `k2s.cc` and
`keep2share.cc`. Metadata needs no CAPTCHA. The resolver requests one, reads
it with the in-process CAPTCHA model, and waits for the API's timer before
redeeming the accepted key for a ticket and then a storage link.

The two waits the host asks for are handled differently:

- **The ticket timer** (`time_wait`) is usually half a minute and is sat out
  in place, holding the key. A timer longer than `Keep2ShareInPlaceWait` goes
  back to the queue as a `WaitError` instead, keeping its key for the same
  route: the same long wait that applies between downloads can also arrive on
  a ticket.
- **The wait between free downloads**, given when there is no key, is the best
  part of an hour and covers every file from the address. It goes back to the
  queue as a `WaitError`, and is kept on the `Keep2Share` value so that other
  files return the same wait without solving a CAPTCHA to be told it.

The key survives cancellation within the process, and an unexpired storage URL
is reused on retries: a new free transfer spends the host's hourly allowance
even when only a range was fetched. `Pace.Group` keeps the one-file limit
shared across aliases and storage hosts, and `WithResolveNote` carries the
CAPTCHA and wait status to the item's note.

## What the API's refusals mean

Most refusals are about the address, not the file, and telling them apart
decides whether a file fails or tries another route.

- **"Download is not available" (errorCode 42) with no reason given** is about
  the address. Asked for one file through many public proxies, a share of them
  get exactly that answer while the rest get tickets. The resolver returns it
  as a `RefusedError`. The API's answer is an HTTP 406 the route layer counts
  as a success, so nothing else would penalise the route. Through a proxy
  route, `transferRouted` then holds the route back from the service for
  `ProxyAddressRefusedWait`, its exit with it, and retries the file through
  another route. On the direct route it is a failure saying so.
- **A file the site keeps for premium users** gets the same bare refusal, from
  every address. `/api/v2`'s `getFileStatus` calls such files free; the newer
  API the site's own pages use (`api.k2s.cc/v1/files/<id>`,
  `isAvailableForFree`) says they are not. So `checkFree` asks the newer API,
  as the file's `File.Check` before any route is used and again on a bare
  refusal. A file the site keeps for premium users then fails at once with no
  route penalty, and a site that cannot answer leaves the flow as it is. Read
  as refusals of the addresses, such files would spend a CAPTCHA on every
  route they met and set each aside, until nearly every route in the pool was
  cooling. The question goes over this machine's own connection
  (`httpx.WithoutRoute`), since the answer is the same whoever asks: through a
  route it would cost a dead proxy's timeout, and maybe a CAPTCHA, before the
  answer came. errorCode 43 is the same answer in the API's own words.
- **errorCode 33** asks for a reCAPTCHA, and is the address's.
- **"Traffic limit exceed" and "Download count files exceed"** (detail 2 and
  1, under errorCode 21 or 42) are the address's free-download limits, not the
  file's. `limitsAddress` turns them into the route's wait
  (`Keep2ShareAddressLimitWait`, six hours, since the host says nothing of
  when the limit lifts). Through a proxy the route is set aside for that long
  and the file goes on through another; on the direct route it waits in the
  queue.

Asking for a link without a CAPTCHA would tell the address refusals from the
file ones more cheaply, but for a free file it is answered
"Invalid captcha code", and the site keeps a brute-force ban.

The resolver logs the steps of the flow: a CAPTCHA served, a wrong reading or
a refusal at debug level, and a ticket or link granted or a file kept for
premium users at info.

## Links bound to an address

The storage link is bound to the address that obtained it
(`ip_access_policy=first`), and one route can present more than one address:
both hosts are dual-stack, so a new connection can leave by the other address
family, and a public proxy can leave by another of its provider's exits. The
host then answers `409 Conflict`, "assigned with another IP address", partway
into a transfer. So the resolver hands each link out with `Target.Discard`,
which drops the link and its ticket as a change of route does, and the
downloader calls it on a 409 for such a link. Through a proxy that is an
address refusal (`RefusedError`): the route is held back for the service and
the file goes to another, free, as with the API's own address refusals. On the
direct route the next attempt mints a link from the address in use now, within
the ordinary retry budget. Either way the part file keeps what was fetched. A
late discard of a link already replaced keeps the replacement.

A free link allows one connection, from the address that obtained it, and
carries its own speed cap. The resolver keeps a ticket and link per route
(`keep2ShareDownload.routes`), so one route's ticket timer holds up no other,
and a file can read through several routes at once, each with a free download
of its own; the API issues each route its own link, and each link serves
ranges anywhere in the file. How extra streams are set up is in
[proxies.md](../proxies.md).

## Timeouts and attempt counting

API calls and CAPTCHA images use a client capped at `Keep2ShareRequestTimeout`
(20 seconds), keeping a stricter configured timeout, so a silent proxy holds a
worker for twenty seconds rather than the general `config.DefaultTimeout` of a
minute before another route can try. Set this on the HTTP client, not on a
child request context: route accounting excludes caller cancellation, so a
child deadline would lose the proxy penalty. Media transfers use their
streaming client and stall watchdog, and OCR and the host's ticket timer keep
their own budgets.

The item counts route attempts independently of its retry budget, including
dead proxies that cost no retry, and appends the connection attempt to active
notes so successive `requesting CAPTCHA (1/3)` attempts visibly differ.

## Two hosts, one extractor

FileBoom (`fboom.me`) serves the same app shell and the same `/api/v2` free
flow as Keep2Share, so one extractor, `keep2share`, takes both. Its
`services` are the hosts (`keep2ShareService`: label, `/api/v2` base and the
newer files API), and the name a file link's page gives itself picks one:
`Recognise` needs the shell and one of the names, and `Extract` asks the API
of the host its probed page names (probing the page itself when called
without one, and refusing a page that names neither). A link's form cannot
tell the hosts apart; only the page can.

Each host keeps its own address cooldowns, by route in a `routeWaits` as
mega keeps its quota waits (one host's wait between free downloads says
nothing of the other's), notes and log lines carry its label,
and API errors are prefixed with its name in lower case. Pacing is one
`Pace.Group`, `keep2share`: one free download per address across both hosts,
one proxy group, one set of proxy statistics. A setting that still names the
former `fileboom` group is read as `keep2share` (see
[proxies.md](../proxies.md)).

Page sources deduplicate a K2S file link by file ID across both hosts and
their aliases (`fileLinkSources`).

## voyeurking

voyeurking hosts nothing: it is an index of K2S files on either host. Category
and collection pages expose their video links and `rel="next"` pager in HTML,
and pagination stays within the same listing type and slug. The walk is the
shared `walkListing` (`listing.go`), with its partial-listing notes. A video's file
link lives in the flattened React Router data table. Read only the primary
route's `video.file`: the same table also contains related videos and player
assets. Expansion goes through the registry, so the host's deferred resolver
and per-route pacing reach the downloader intact. The listing pages use the
ordinary HTTP client; only the K2S transfers use the proxy pool.

## The CAPTCHA reader

The model and native runtime are installed by `make ocr-assets` (also part of
`make dependencies`) and bundled in release archives and the Docker image.
`helpers/captcha/install.sh` pins and verifies every download and includes
upstream license notices; `helpers/captcha/README.md` describes the assets.
`HEAPLEACH_OCR_DIR` overrides the default `ocr/` directory beside the
executable. Official builds enable cgo; deliberately cgo-free source builds
still run every other host and explain that OCR is disabled.

The model is ddddocr's beta model, used as published. Go matches Pillow's
image conversion, contrast stretching and Lanczos resize, then runs the model
through cgo. CTC decoding pools letter cases, enforces six characters and
ranks readings jointly across the cropped and whole-image views. Synthetic
fixtures pin both the preprocessing tensors and all five ranked readings to
the Python reference implementation's output. `make test-captcha` tests real
native inference and cancellation.

The reader returns ranked readings, not one answer. The challenge is six
letters and digits compared without regard to case, and a wrong answer leaves
it open, so `resolve` tries up to `Keep2ShareCaptchaGuesses` readings on one
image before asking for another, and drops the rest the moment anything but a
wrong answer comes back, since an accepted or expired challenge must not be
answered again. Decoding the model's probabilities under those rules is what
makes the reader usable: the model's own best string drops pale and hairline
characters and comes back short.

Two traps when checking answers against the live API: the file is checked
before the CAPTCHA, so a made-up id is no oracle; and once a free download is
pending for a file, `getUrl` hands out the link whatever the answer, so every
guess against that file "succeeds".

### Sessions

With proxy routes, many free downloads reach a CAPTCHA at once. The
`internal/captcha` pool bounds native sessions to the CAPTCHA concurrency
(`HEAPLEACH_CAPTCHA_CONCURRENCY`, `-captcha-concurrency`, default four, at
most 64; `captcha.SetConcurrency`, called by `main` before anything is read),
each with two ONNX Runtime threads, spinning disabled, and its own copy of the
54 MB model. The budget starts after acquiring a slot. Caller cancellation
stays cancellation; exhausting the OCR budget is `errKeep2ShareOCR`, so
another image can be tried. Waiting, model loading and reading have separate
progress notes. Network requests keep their own deadlines and proxy scoring.

Sessions are reused and released after `Keep2ShareOCRIdle`. Native inference
is terminated with per-run options on cancellation. Session construction
cannot be interrupted, so a cancelled caller returns while the worker keeps
its slot until construction finishes, then frees the unused session. This
keeps repeated cancellations from creating unbounded native work.
