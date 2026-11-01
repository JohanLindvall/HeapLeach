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
address at all. Both run one loop (`send`) with the budgets given: `Do` with
the client's retries and `config.RateLimitRetries`, `DoOnce` with none of
either, so a single attempt reads every answer exactly as `Do`'s last one
would. `GetJSON`, `PostJSON`, `Fetch` and `StatusOnce` build their requests
with `request`, which applies header maps over the defaults in order. One status is
handled apart from the rest: **429 is waited out, not counted as a failure.**
A rate limit is the server saying *later*, not *no*, so `Do` honours
`Retry-After` when the response states one and otherwise backs off from
`config.RateLimitRetryBase` towards `RateLimitRetryMax`, then decrements the
attempt counter so waiting never spends the retry budget. It gives up after
`config.RateLimitRetries` waits. This matters most to paginated listings,
where reading a 429 as the end of the results silently truncates a job instead
of failing it.

**Admission.** A context may carry an `httpx.Admission`
(`WithAdmission`), which `send` admits every attempt through: it waits,
cancellably, for a slot before the attempt, gives it back when the attempt
fails, and otherwise hands the slot to the response body, which gives it
back once read to its end or closed — exactly once. A retryable answer or a
429 is drained, so its slot is back before the backoff or `Retry-After`
wait; a redirect is followed inside one attempt and holds one slot. The
route path (`requestRoute.do`) does not re-enter `send`, so a routed request
is admitted once. A caller that keeps a body open holds its slot, which is
why every `Do` caller closes its body as soon as it has read it. The
extractor registry gives each top-level extraction one
([extractors.md](extractors.md)); without one, requests are not held back.
A caller with a deadline of its own — a bounded identification probe
(`probeContext` in the extractor package) — admits first with `Admit` and
sends in `WithoutAdmission` while holding the slot, so its deadline is
spent on the network rather than in the queue: a probe that timed out
waiting for a slot would be remembered on the shared page as the
endpoint's answer.

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
not. Every buffered read uses `httpx.ReadBody`, which reads one byte past the
limit so a body exactly at it is complete, and returns `ErrTooLarge` or the
read's own error together with the prefix it got. Paths with their own request
policy — the registry's page probe, platform JSON probes, the player's
canonical probe, the challenge sniff — call it too: a prefix may be sniffed,
but a body with any read error never becomes a reusable document. The
registry's probe reads an untyped or plain-text answer only to
`config.ProbeSniffBytes`, a prefix it never offers as the document, and
spends `DoOnce` plus one retry after a transient failure rather than
`Do`'s budget (extractors.md). It hands back the response with its body read and closed, for its
headers and for `resp.Request.URL`, the address after redirects that relative
references resolve against. `GetPage` is `Fetch` with `Navigate` under the
caller's headers; a playlist or script is fetched with `Fetch` itself, since
it is not a page being navigated to.

After validating a partial response's `Content-Range`, the transfer reader
calls `LimitResponseBody` before reading. This bounds the payload and gives
the routed body its verified length, including when chunked transfer encoding
omits `Content-Length`. Reaching that length and closing records success once
without waiting for HTTP EOF; a short EOF is a failed response. Unread or
partly read bodies closed by the caller remain neutral. Preserve this through
the admission wrapper, so accounting cannot leak a request slot.

Build a refusal with `NewStatusError(resp)`, not a `StatusError` literal. It
keeps the code, status, headers and the first `config.ErrorBodySample` bytes
of the body, which is where hosts say why: the downloader reads a connection
limit out of it, and mega's quota out of the headers. It names the address the
caller asked for, without a password, not one a redirect led to, which is
often a signed link. `Do` builds one from the last retryable answer, so an
exhausted budget still quotes the host's reason. A body already read whole
(`Bytes`, `GetPage`) is kept whole, because some extractors decode a JSON
refusal. The message (`Error`) quotes a plain or JSON body, cut to 200
characters, but an HTML error page only by its title: the rest of one is a
WAF's or a CDN's markup and reads as noise in a job's error. A body
labelled HTML that holds no markup at all (no `<`) is plain text under a
server's default type, PHP's among them, and is quoted as it came. `Body`
keeps it all for the extractors that parse it.

`ResponseURL(err)` separately exposes the destination that answered an HTTP
request or failed during transport. A `StatusError` keeps this URL privately,
outside its `URL`, `Error()` text and JSON representation; wrapped transport
errors provide it through `url.Error`. The probe needs the destination to
preserve site claims after a failed redirect. This metadata is for
classification, while diagnostics keep the error's existing display URL.

A proxy route's refusal (403, 407, 429, 5xx: `requestRoute.do`) is the one
exception: its status is reported to the route's observer before the body
is read, and the sample is read for at most `config.RouteRefusalBodyWait`
(100 ms: a reason that is coming arrives within it, and failover is held no
longer). A proxy that sends the refusal's headers and stalls would otherwise
hold the file up to the header timeout before another route is tried (the
stall watchdog starts only once `Do` returns), and score the route by the
stall (`TestRefusalSampleHoldsFailoverBriefly`).

## Response logs

There is one logger key. `httpx.WithLogger` sets it and
`extractor.WithLogger` is the same call, so a job's extraction log gets a
debug line for each response that went wrong (an error status, or a
challenge its headers mark with `cf-mitigated`): host, status, type, length,
server and Cloudflare markers, with no body, cookies or path. A challenge
recognised only from a 200 page's body is not logged here, since the line is
written before the body is read; it surfaces as the request's
`ChallengeError` instead. Responses that went right are not recorded: a
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
  `config.ProxyDialTimeout` (5 seconds, set in `ThroughProxy`) rather than the
  `config.DialTimeout` every other path's `dialer` uses: most routes in
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

`RequestKey` describes the effective cookies for one URL. A response reached
through redirects also depends on each hop's cookies: `SentRequestKey`
reads those from the actual request after net/http added its jar's cookies.
The page cache validates the full chain before reuse, using the effective
source, route or client jar. Both keys contain cookie values and must never
be logged. Cookie-setting redirect chains stay with their original caller
because an earlier hop may change the session before a later one is observed.

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
