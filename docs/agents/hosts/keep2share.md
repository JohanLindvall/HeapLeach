# Keep2Share, FileBoom and the CAPTCHA reader

`keep2share.go` and `voyeurking.go` in
`internal/extractor`, and `internal/captcha`. Keep2Share and FileBoom are
two per-address services of the same software: each address gets one free
download at a time from each, which is what the proxy pool is for. How routes are leased, scored and cooled is in
[proxies.md](../proxies.md).

## The free-download flow

keep2share uses the public `/api/v2` free-download flow, on both `k2s.cc` and
`keep2share.cc`, and FileBoom's on `fboom.me`. Metadata needs no CAPTCHA. The resolver requests one, reads
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

## FileBoom

FileBoom (`fboom.me`) serves the same app shell and the same `/api/v2` free
flow as Keep2Share, so one extractor, `keep2share`, takes both. Its
`services` are the hosts (`keep2ShareService`: label, `/api/v2` base, the
newer files API and its own domains). The services' domains are exactly
`k2s.cc` and `keep2share.cc` for Keep2Share and `fboom.me` for FileBoom,
the hosts the registry claims for the extractor first (`siteHosts`,
`claimFirst`): a link on one reaches `Extract` unprobed, and the domain
picks the service (`serviceOf`), so a FileBoom link is resolved, paced,
proxied and labelled as FileBoom's whatever its page would have said, and
an id the API refuses is reported in its words ("file is unavailable or has
been removed", the API's own message). A link of the form on any other host
is taken only when its page is the shell: `Recognise` needs the shell and
one of the names, and `Extract` asks the API of the service the page names
(probing the page itself when called without one, and refusing a page that
names neither). A page there withheld from the probe is not taken, and
neither service's API is asked about another host's id.

The two services count free downloads per address apart, so everything
that follows from that count is kept per service. Each keeps its own address
cooldowns, by route in a `routeWaits` as mega keeps its quota waits (one
service's wait between free downloads says nothing of the other's), and its
own `Pace` (`keep2SharePace`, `fileBoomPace`): its own `Pace.Group`
(`keep2share`, `fileboom`), so its own one-file-per-address admission, its
own proxy scope (`CappedScopes`), lease cooldowns, route health and proxy
statistics, and its own protocol setting. A mixed queue therefore runs one
free download of each service per address at once, and an address waiting
out one service's interval still serves the other. The service is the one
the link's domain or the probed page names. Notes and log lines carry its
label, and API errors are prefixed with its name in lower case. The `keep2share` group covers Keep2Share alone, including in
settings saved while the two shared one group: such a choice now applies to
Keep2Share only, and FileBoom follows the general protocol until it is
given its own.

The API namespaces are separate: each service keeps its own file store, and
a FileBoom file ID is not resolved through Keep2Share. Page sources
deduplicate a K2S file link by file ID within each service
(`Keep2Share.fileKey`, used by `fileLinkSources`), across that service's
domains and whatever filename or query the link carries; the same ID on the
other service is another file, and both are queued. A link of the same form
on any other host is keyed by that host and ID, so a third host's
`/file/<id>` listed first cannot shadow the service's link.

A job is labelled by its service (`keep2share` or `fileboom`) rather than
by the one extractor's name: by `Result.Site` once read, and by the link's
domain (`installName`) before it is read and after it failed. Both labels
are among `Registry.Hosts`, which the UI counts and filters jobs by. A link
on another host whose read failed is labelled `keep2share`.

The API names its CAPTCHA image over HTTP. On the service's own domains
(and their subdomains) it is asked for over HTTPS, as every other step is
(`captchaURL`); an image on any other host is asked as named, since whether
that host serves HTTPS is not known.

## voyeurking

voyeurking hosts nothing: it is an index of K2S files on either host. Category
and collection pages expose their video links and `rel="next"` pager in HTML,
and pagination stays within the same listing type and slug. The walk is the
shared `walkListing` (`listing.go`), with its partial-listing notes. It
reads a listing wherever the site redirects it (`anyRedirect`), a renamed
category's old slug say, as the listing always did. A video's file
link lives in the flattened React Router data table. Read only the primary
route's `video.file`: the same table also contains related videos and player
assets. Expansion goes through the registry, so the host's deferred resolver
and per-route pacing reach the downloader intact. A file link not even
shaped like Keep2Share's (`Registry.Known`) is refused before it is
followed, so a foreign link costs no probe on a third party's host and the
refusal is voyeurking's own; one that is shaped like it is still followed
strictly and must be recognised as Keep2Share's. The listing pages use the
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
