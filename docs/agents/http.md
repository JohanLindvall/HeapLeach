# HTTP client

`backend/internal/httpx`; file names are relative to it. Header names and
common values live in `headers.go` (`httpx.Header`, `httpx.Referer`,
`httpx.RefererOrigin`, and `httpx.SiteOrigin(root)` for the common pair
sent from a site's front page). `Header.Apply` sets a header map on a request, and
`httpx.Navigate` is what a browser sends with a page it navigates to (HTML
asked for, and the `Sec-Fetch-Dest`/`Sec-Fetch-Mode` pair some hosts check);
it is shared, so apply or `Merge` it, never write to it. The response header
names the server sets (`HeaderCacheControl`, `HeaderVary` and the like) live
there too.

## Retries and rate limits

`httpx.Client.Do` retries with backoff (`httpx.RetryDelay`, which the
extractors' `withExtractRetries` shares, so a re-asked answer waits as a
failed request would); `DoOnce` does not, and `StatusOnce`
is `DoOnce` reduced to the status, for a probe of whether a host serves an
address at all. One status is
handled apart from the rest: **429 is waited out, not counted as a failure.**
A rate limit is the server saying *later*, not *no*, so `Do` honours
`Retry-After` when the response states one and otherwise backs off from
`config.RateLimitRetryBase` towards `RateLimitRetryMax`, then decrements the
attempt counter so waiting never spends the retry budget. It gives up after
`config.RateLimitRetries` waits. This matters most to paginated listings,
where reading a 429 as the end of the results silently truncates a job instead
of failing it.

Two traps:

- `retryAfter` returns `(time.Duration, bool)`. It has to separate
  "the header says 0" from "there is no header", because a stated delay is
  authoritative while an absent one means back off. With a bare duration,
  every fixture would sit out the full backoff.
- A test fixture that serves a **permanent** 429 sits out that patience too,
  and burns the whole budget unless it states `Retry-After: 0`. A suite that
  suddenly takes a minute instead of three seconds is this, not a hang.

## Reading a response

`Client.Fetch` is the GET that reads a whole body: the client's defaults with
the caller's headers over them, `Do`'s retries, and a non-2xx answer returned
as a `StatusError`. A body over `config.MaxResponseBytes` is an error, never
cut short, because a truncated script or playlist parses as something it is
not. It hands back the response with its body read and closed, for its
headers and for `resp.Request.URL`, the address after redirects that relative
references resolve against. `GetPage` is `Fetch` with `Navigate` under the
caller's headers; a playlist or script is fetched with `Fetch` itself, since
it is not a page being navigated to.

Build a refusal with `NewStatusError(resp)`, not a `StatusError` literal. It
keeps the code, status, headers and the first `config.ErrorBodySample` bytes
of the body, which is where hosts say why: the downloader reads a connection
limit out of it, and mega's quota out of the headers. It names the address the
caller asked for, without a password, not one a redirect led to, which is
often a signed link. `Do` builds one from the last retryable answer, so an
exhausted budget still quotes the host's reason. A body already read whole
(`Bytes`, `GetPage`) is kept whole, because some extractors decode a JSON
refusal.

## Response logs

There is one logger key. `httpx.WithLogger` sets it and
`extractor.WithLogger` is the same call, so a job's extraction log gets a
debug line for each response that went wrong (an error status or a
challenge): host, status, type, length, server and Cloudflare markers, with
no body, cookies or path. Responses that went right are not recorded: a
listing reads hundreds of pages, and the Logs panel holds a thousand lines
across every job. `httpx.WithResponseLog` records every response, for a stage
being diagnosed request by request (the DoodStream player handshake). A
client from `Streaming()` (the downloader's) logs none of these, because a
segmented video means thousands of responses and a transfer reports its own
failures. Keep any new per-request record at debug level.

## Short certificate chains

**A short certificate chain is completed, not refused** (`aia.go`). Some
servers send only their own certificate and leave out the intermediate that
links it to a trusted root — vids.st does — which browsers complete by
fetching the issuer from the certificate's own AIA address, and Go and curl do
not. Both TLS paths (the standard transport and the uTLS dial) therefore set
`InsecureSkipVerify` and verify in `VerifyConnection` through `chains.verify`:
crypto/x509's own check against the system roots, hostname and validity, run
on every connection, and only on an unknown-authority failure the issuer
fetched (http/https only, three hops, 64 KiB, cached per address) and the
check run again. A fetched intermediate completes a chain only if it really
signed the certificate and leads to a trusted root.
`TestBothTLSPathsVerifyOnTheirOwn` holds each path to refusing a chain that
leads nowhere — keep it, since a regression there would accept anything.

## Proxy errors

These shape how routes are scored; the routes themselves are in
[proxies.md](proxies.md).

- Proxy connection and response-header timeouts both penalise the route and
  cool its endpoint across services. The dial is bounded by
  `config.ProxyDialTimeout` (5 seconds, set in `ThroughProxy`): most routes in
  a public feed are dead and most dead ones never answer, so the dial timeout
  is what each costs a file.
- An unanswered explicit proxy connection returns `httpx.ProxyConnectError`,
  so it does not spend the file's retry budget. Go's client timeout would
  otherwise hide the underlying dial or CONNECT error behind "request canceled
  while waiting for connection".
- A CONNECT answered with anything but 200 returns `httpx.ProxyRefusedError`
  with the status, from `OnProxyConnectResponse`, which net/http calls before
  its own check. Left to net/http it is the status text alone ("Bad Request"),
  which says neither that a proxy answered nor what it said.
- A 502 or 504 refusal is scored as the route refusing that service, not as a
  dead proxy (`reachesNoHost`): the proxy works and cannot reach that host,
  and scoring it dead would shelve it for every other service too. Every other
  refusal is the proxy's own and sets it aside for every service: a Tor port's
  "501 Tor is not an HTTP Proxy", a server that knows no CONNECT
  ("503 unknown method"), a plain 500. Do not treat other 5xx as the host's: the exit probe scores nothing but a success or a dead connection, so routes refusing that way would never be set aside, and would come back after each short rest to take extra streams' turns.
- Request traces reset on each redirect. A CONNECT reply or a completed SOCKS
  negotiation ends proxy setup, so later TLS and response timeouts keep their
  bounded retry budget.
- Caller cancellation and caller deadlines are never proxy penalties.

## Challenge pages

Cloudflare's `cf-mitigated: challenge` header becomes `httpx.ChallengeError`,
including on HTTP 200 challenge pages. `CaptureChallenges` keeps platform
probes from swallowing it and stops further requests in that extraction
attempt. `GetPage` takes an optional host-specific check that recognises
challenge HTML before retries, challenge capture and route scoring, just as
the header is; Doodstream supplies one for its Turnstile gate and Cloudflare's
interstitial ([hosts/video-hosts.md](hosts/video-hosts.md)). What recovery
does with a challenge is in [proxies.md](proxies.md).

## Routes on a request

`WithCookieJar` scopes cookies to one source without changing its transport,
timeouts or route observations. It overrides the jar after route selection,
so redirects and retries use that source's session without modifying the
shared client or the leased route. Protected folders use it when a host gives
every folder the same cookie name; retaining only the shared jar would let
one job's unlock replace another job's access.

A routed attempt carries its route in the request context (`route.go`):

- `httpx.WithRoute` pins every request, redirect and retry in the context to
  the route's client, and its ID lets IP-bound resolvers keep tickets with
  their route.
- `httpx.WithRouteAgent` does the same for a route that presents a User-Agent
  of its own on every request, whichever client builds it.
- `httpx.WithoutRoute` takes the route off, so a question whose answer does
  not depend on the address asking goes over this machine's own connection
  rather than a proxy that may be failing.
- `httpx.DirectRoute` is the direct connection's route ID, which keeps an
  IP-bound ticket's identity across live proxy toggles.
