# Extractors

`backend/internal/extractor`; file names are relative to it unless stated.
Notes on individual hosts are in [`hosts/`](hosts/), indexed
[at the end](#host-notes).

## The contract

`extractor.Extractor` is `Name/Extract`, and every registered extractor is
also a `Recogniser` (`Shape/Recognise`) and a `Describer` (`Recognises`).
Register new extractors in `NewRegistry` (`registry.go`); the first to
recognise a page takes it, so order matters where two could. That list is
the only production record of the order: `registryOrder` in `rows_test.go` restates it
independently, so a change in which extractor wins has to be made in both on
purpose, and `TestEveryDeclaredRowIsRegistered` reads the package source for
every Player, Share, Pointer and Handoff row so none is declared and left out.
Extractors that resolve through the registry (links, pointers, indexes, the
fallback) are handed it as they are built. `File`,
`Target`, tracks and `Pace` live in `contract.go`, and the errors that steer
the queue in `errors.go`.

### Dispatch is by page, and by host where the page cannot say

A site's own hosts are claimed by its extractor (`hostclaims.go`), in one of
two ways. An extractor that reads an API or hands the link to yt-dlp takes a
link on its hosts before any probe (`claimFirst`; a broadcaster or a
platform family only for its own forms, `claimFirstForm`), and no page is
attached. A broadcaster's other pages go through recognition and the
generic fallback, so a news article with a clip is still the fallback's;
only a page the fallback finds nothing on (`errPlainPage`) is handed to the
broadcaster to explain (`claimFirstFormThenLast`, `claimLast`): probing a page it never reads only cost a request and failed jobs
whose API still answered. An extractor that reads its page takes its hosts'
links when no recogniser took the page or the probe failed
(`claimFallback`): a removed, private or blocked page carries none of the
markers, and the site's extractor says why, where the generic fallback
would read the page's related videos as the item. The hosts are the
baseline lists (`siteHosts`) and each family's known installs plus
`HEAPLEACH_EXTRA_HOSTS` (`familyInstalls`); a family's job is labelled by
install, and a KVS install names itself in its errors the same way
(`kvsName`). A file on a claimed host (an image subdomain) stays a file unless
its form is the extractor's (`claimsFiles`: Dropbox's and Drive's
direct-link resolvers, and ImagePond's media host, asked with the site's
Referer).
Tests stand a local server in for a site with `aliasClaims`.

So an extractor claimed first never leans on the probe: `probed(ctx, u)`
is nil, and whatever it reads (an API, a player, a document) it asks for
itself, with its own retries, and words its own refusals. One claimed as a
fallback is handed its site's page as the probe found it, a 404 included,
and must refuse it in the site's words without following the page's links;
a player row's 404 is the video gone (`playerGone`), since its read asks
the page again. `hostclaims_sites_test.go` pins both for every site that
can be stood in for, and the claim itself for those whose API host is
fixed.

Everywhere else no extractor claims a domain. `Registry.Extract` probes the submitted URL
(`page.go`: one bounded GET, redirects followed, sent as a navigation with
no Referer and asked once more with `Referer: <origin>/` when answered 401
or 403, that second answer being the page — a hotlink-protected file or
manifest is judged by what it serves its own pages, as the fallback's
download and the manifest reader ask. The probe spends one attempt and one
more after a transient failure (`probeDo`: no answer, 408, 429, 5xx), the
wait before it `RetryDelay(0)`, or `config.PlatformProbeRetryWait` for a
rate limit whatever `Retry-After` asks, never the client's whole budget:
the chosen extractor's requests and the transfer have their own. The body
is read whole, to `config.MaxResponseBytes`, only for a document type
(`probeReadsType`: HTML, JSON, XML, a script, a playlist); an answer with no
Content-Type or another text type (`probeSniffsType`: plain text, CSV) is
read only to `config.ProbeSniffBytes`, since an extensionless link is as
likely a large untyped file as a page. That prefix is kept for sniffing
(`Page.partial`), never offered as the document and never shared with
another route. A document body cut short or past `config.MaxResponseBytes`
sets `Page.Err`, and the page cache and `Document` never offer it. Most recognisers require
`Page.OK()` and so decline such a page; only code that is written to read a
prefix reads past `httpx.IsTooLarge(p.Err)`: the generic fallback, sniffing
an oversized page for its media, and the feed recogniser, since a feed is
decided by its head — one past the bound is read again to
`config.MaxFeedBytes`, its enclosures listed up to the cut. Feed extraction
reuses only a complete successful probe (`Page.Document`). A plain-text or
untyped feed's sniff prefix can identify the feed, but must be fetched
through the feed's bounded reader before decoding enclosures, including
when the prefix has no read error. Otherwise the tolerant decoder can
silently omit later episodes. `TestFeedsReadPastAPlainTextProbe` covers RSS
and Atom with enclosures before and after the prefix. The feed recogniser
also asks a feed-shaped URL, or any URL on a `feeds.`, `feed.`
or `rss.` host (`feedHost`, also a followed `linkForms` entry), once more with the feed's own
`Accept` when the probe got a generic binary type it did not read or an
HTML page, which is how a host that negotiates on `Accept` serves a
subscribe page to a browser) and offers the `Page` to each `Recognise` in
turn; nothing claims it and `Direct` takes it. A link whose path names a file (`probeWorthy` false)
is probed only because some `Shape` claims it, and is asked with a HEAD
first (`headProbe`), as is a file API's extensionless download route
(`fileRoute`, `/api/file/<id>`): media answers there, never fetched, since a download
link may be single-use or quota-counted and a host that punishes parallel
connections counts the probe's; a text answer, or any HEAD that did not
succeed, is fetched with the GET as before. A link naming a manifest
(`.m3u8`, `.mpd`) skips the HEAD: the probe's GET is the manifest, and
`hlsPlaylistResult` accepts the probe's navigation when it read a complete,
valid manifest, so a pasted manifest — often signed for one use — is
asked for once (`TestPastedManifestIsFetchedOnce`). Such a link's recogniser decides
on the headers and redirect, not on its origin's API: archive.org takes a
file route only when it ended on a storage path naming the item
(`archiveStored`), and asks nothing of a host that served the file in place.
Its embed and stream routes, bare player documents, are asked about only
when they name the item's own `/download/` or `/details/` path, and its own
page without an item's markup (a collection, a darkened or missing item) is
taken on any metadata answer, so the extractor says which.
A site that moves domain, or an install nobody listed,
reaches the right code because the code recognises what it serves. Host
names may still appear as operational constants (API bases, CDN hosts,
referers, `Pace` groups), never as the reason an extractor was chosen.
`TestRecognitionNeverReadsTheHost` fails the build if a `Shape` or
`Recognise` reads a URL's host, and `expectRecognised`/`expectShape`
(`recognise_test.go`) re-run each fixture with the page served from an
unrelated host.

Recognition is **deep**: `Recognise` checks the URL's form *and* page content
the extractor's own logic depends on (the player configuration, embedded
JSON, script or DOM structure it parses). The site's name (`Page.Named`) is
never enough alone; a pure app shell pairs it with a structural marker such
as the bundle it loads (`Page.Script`). Each test carries a negative fixture
with the right form and name but without the marker or a matching API.
When the page lacks recognised markup, a bounded origin request
(`platformJSON`) may decide, but only after the form matched, since every
unrecognised page is offered to every recogniser. It is asked once more
after a rate limit, a server error or no answer
(`config.PlatformProbeAttempts`), since a busy moment is not the endpoint
saying no, and its deadline starts once it is admitted (`probeContext`).
What a server says of itself for every page — the fediverse's nodeinfo —
is remembered per server (`hostMemo`) by the recogniser as well as the
extractor, as a cache key, never as the reason a page is taken. A populated custom
frontend is as eligible as a challenge or empty shell: compatible APIs do
not require the original frontend's branding or markup.

`Shape` is the half that needs no fetch: the link's path, query and
fragment. `Registry.Known` uses it for link discovery (the harvester, the
page scanner, the index extractors), together with the discovery forms in
`linkforms.go`. Those name, by the host that hands them out, links that
plainly lead to a supported item but have no form a Shape could name
without probing every bare link on every page: redirect-only short links
(`youtu.be/<id>`, `dai.ly/<x>`, `b23.tv/<c>`, `on.soundcloud.com/<c>`), a
site's canonical single-item `/<id>` (streamable, sendvid, lulustream,
vidmoly, imgur, the imgbb short domain), two-segment yt-dlp items
(soundcloud tracks, mixcloud shows), an install that serves videos at
`/<slug>.html`, and legacy domains that redirect into a catalogue (npostart,
vrtmax.be). They choose which links are followed and how a queued job is
labelled, never which extractor reads a page: the page is still probed and
recognised by what it serves, and `TestDiscoveryFormsNeverDecideRecognition`
fails if a Shape or Recognise reaches them. The same table marks the part
of a platform's Shape that the platform's own pages use for navigation
(`linkForm.local`: every platform's forms — booru, 4chan, FoolFuuka,
kemono, KVS, PeerTube, Chevereto, Bandzoogle, fediverse, MediaWiki).
Automatic scanners and indexes ask `Registry.Follows` with the page the link
was found on, which follows both kinds only to another site: a forum's own `/posts/<n>` and
`/members/<n>/` links are its navigation, and following them cost a probe
and platform API guesses per link; on the named host's own pages a bare
`/<id>` or `/<user>/<name>` is as often its account, login, genre or
settings page as an item (`TestHostLinksOnTheirOwnSiteAreNavigation`). A
two-segment form never takes a last segment with a dot, which names a
file, not an item.

An explicit `links:` harvest also asks `Registry.harvests`, which admits
the table's `harvest` item predicates on known installs. The host must be
in `claimHosts` (built-in sites, family installs, configured extra hosts or
claim aliases), or a discovery alias. A broad platform Shape alone cannot
establish either the install or an item: profiles, search results and
listings stay excluded. Automatic scanning continues to use `Follows`.
The exception changes admission only; the existing strict expansion still
reads each admitted item, preserves its failures and applies source/file
caps. Platforms without stable item routes, such as Bandzoogle, have no
exception. `harvestforms_test.go` covers item and navigation boundaries;
`links_platform_test.go` exercises the real registry and player extraction,
redirects, self-links, duplicates, caps and unavailable items.

An extractor that serves one site is followed only on its own hosts
(`siteHosts` in `linkforms.go`, the baseline's host lists): its Shape
names paths (`/v/<id>`, `/e/<id>`, `/d/<code>`, `/watch/…`,
`/view_video.php`) that every other site uses for its own pages, and
following them wherever they appear made a page's whole navigation a probe
per link (`TestAPagesOwnNavigationCostsNoProbes`,
`TestSingleSiteFormsAreFollowedOnlyOnTheirHosts`). On those hosts a link
found on another site's page is followed whatever its path, unless it
names a file — a band's front page, a profile, a programme, as when
extractors were chosen by host (`TestSiteHostLinksAreFollowedFromOtherSites`);
on the site's own page only its Shape is. A form whose id alone names the
service (`offHostForms`: a gofile content UUID, a mega key in the
fragment, a Keep2Share hex id, an ok.ru embed) is followed on any host, for
mirror and custom domains. `siteHosts` decides only what discovery
follows, never recognition. Tests make a reserved host stand in for a
site's own with `Registry.AliasHosts`, since fixtures may not carry real item links.

Keep fixed mirrors in this inventory alongside the primary domain: PornPics
uses both `pornpics.com` and `pornpics.de`, including their subdomains. A
mirror omitted here can still work when pasted directly while disappearing
from a harvested page. `TestGalleryMirrorDiscoveryHarvestsGalleries` covers
direct, explicit and automatic discovery, including a mixed list; its local
proxy serves invented pages for the actual host labels without contacting them.
`TestGalleryMirrorDiscoveryHostBoundaries` keeps unrelated hosts excluded.

Host patterns distinguish an exact label (`name.*`) from an explicitly
opted-in prefix (`name*.*`), both under a suffix of one or two labels.
Bunkr uses the prefix form for its rotating aliases; other services retain
their exact labels. Prefix matching considers eligible labels beneath
subdomains, so a subdomain sharing the prefix cannot hide the service's host label.

A job not yet read is labelled by
`Registry.Label`: the claimant when exactly one form claims the link, else
the host, since overlapping forms make the first claimant a wrong guess as
often as a right one; a job that fails takes the name of the extractor that
recognised its page (`Registry.Identified`). A job a platform family read
is labelled by its install (`Registry.JobLabel`: KVS by its host's first
label, PeerTube by the label naming the instance), as each was while
installs were listed; the label names no folder. A link followed for its form is extracted strictly
(`Options.strict`): if its page turns out to be no extractor's, it is left
out (`errUnrecognised`) rather than handed to the fallback, and the
collection's note counts it as unrecognised beside how many links resolved
("6 of 10 links resolved; 4 unrecognised"), so a recogniser a site change
broke for some links shows rather than leaving a job that looks complete. Keep
forms specific enough that a site's own navigation does not turn into a
probe per link.

A failed probe is not evidence that a followed link is navigation. When no
extractor claims it, strict extraction preserves transport and body-read
errors, and reports HTTP failures as `httpx.StatusError` with the response
headers and a bounded body sample (`probeFailure`). Source retries and route
recovery need those typed failures, and a listing counts a dead link (`404`,
`410`) among what it could not read, with its status, rather than dropping it
as navigation: "none of the 3 links could be read: 404" is the answer a
thread of expired links needs.

A permanent failure of a submitted URL is kept when an extractor's form
claims it, or when the refusal has a text body (`refusedPage`), after the
probe's Referer retry. An unclaimed refusal served as media or a binary type
is still the fallback's. A page too large to read whole is the submitted
URL's exception: the fallback sniffs its prefix.

For a transient probe failure (`transientProbe`: no answer, a timeout,
408, 429, 5xx), a single-site form claims a submitted URL only where it is
valid (`claimsHere`): on that site's known or aliased hosts, or where the
ID itself names the service. Platform and media forms retain their claims
on any host. Check `hostClaim` at both the submitted and final URLs too,
including a known host's non-item pages. Failed probes retain that
destination through `httpx.ResponseURL`, separately from the URL displayed
in an HTTP error. A path resembling another site's on an unrelated host
can therefore reach `Direct`, which queues the original submitted URL
without fetching it again; the downloader's retries, stall handling and
route recovery can outlast the outage. Only top-level submissions get this
exception: strict and nested sources keep their existing failures, as do
known services, permanent refusals, challenges, refused routes and
cancellation. `transientclaims_test.go` covers these boundaries and
`TestSubmittedUnclaimedLinkOutlastsAFailedProbe` covers unclaimed forms.

The probe is made outside any enclosing extraction's challenge capture
(`httpx.WithoutChallengeCapture`):
a side link's challenge is that link's answer and must not become the
page's it was found on, nor stop every later request of that page's
extraction.

Every read after the first — a refresh at transfer time, a read recovered
through another route — must come back to the extractor the first
successful read chose (`pinned` in `Registry.Extract`), and a read that
lands on the fallback with a failed probe is a failure. Otherwise a route
answering a plain `403`, or a block page served on refresh, would be
accepted as recovery and swap the item for the page under a new name.

### Fetching a page once

Probes go through `pageCache`: per route and request identity, shared by
concurrent callers sending the same cookies, and kept for
`config.PageCacheTTL` only when the answer is a success
(`Page.servesAnyRoute`: a 2xx read whole). A kept success also serves any
route that would make the same request (`httpx.RequestKey`: the URL, the
language and the cookies its jar holds for the URL; not the User-Agent,
which a route may present of its own), and `probed` accepts it there: only
the address and agent differ. This identity keys both cached and in-flight
pages, including reads on the same route: changing or removing a cookie
must not reuse another session's media or a password gate from before an
unlock. The effective jar is the source's when supplied with
`WithCookieJar`, otherwise the route's, otherwise the client's. Cookies
that would not be sent to any request in the chain do not change its identity.

A redirect's identity includes every hop, not just the submitted and final
URLs. `Page.recordRedirects` follows `Request.Response` and keeps each URL
and `httpx.SentRequestKey`, including the cookies net/http actually sent.
The cache checks the current jar against every hop before handing out a
kept page or an in-flight answer; `probed` makes the same check for an
attached page. This includes cookies scoped only to an intermediate path
or another host. Matching sessions still share across routes; a mismatch
requires a new probe. The saved redirect identities count against the
cache's byte bound and are never logged.

A probe snapshots its request identity before and after the fetch. If its
cookies change while it runs, its response belongs only to its original
caller: it is neither cached nor handed to waiting callers, which recompute
their identities before asking again. The original caller can still reuse
that response with the cookie state left by the probe, since fetching a
single-use manifest or password token twice could consume it. It is never
cached under that new cookie state: a gate served before a cookie update
does not become an authenticated page after it. `probed` rejects any later
cookie change even on the original route. `TestRegistryPageCacheDropsUnlockedPasswordGate`
and the cookie-session, response-cookie and in-flight tests cover these
transitions.

A redirect chain containing any `Set-Cookie` response belongs only to its
original caller. It is neither cached nor shared with waiters, even when
the later hops already sent the new cookie: an earlier response may have
established a cookie for a destination whose previous state was never
observed. The originating reader can reuse the response with the cookie
state left after all hops, preserving single-use links. This conservative
rule also excludes idempotent cookie updates from sharing. The cases in
`redirectcookies_test.go` cover source/client/route jars, intermediate and
cross-host redirects, cookie removal, waiting callers and HEAD-only probes.

Nothing else outlives its probe. A failure or a challenge is not even shared
with the probe's waiters, who ask again; any
other answer — a refusal, an error page, a redirect left unfollowed — is
handed to the readers waiting for that probe on its route and then dropped,
since what one address is refused says nothing of another, route recovery
moves a read precisely to ask again, and a refusal is often of the moment
(`TestPageCacheKeepsNoRefusal`). A probe in flight on another route is not
waited for. Questions recognisers put to the network about a page
(`memoAsked`, `pageJSON`) are kept per route even when the page is shared,
for the same reason. The cache's byte bound (`config.PageCacheBytes`, oldest
dropped first) counts each entry's allowance
(`config.PageCacheEntryBytes`, which is what bounds header-only answers),
its body, and what its memoised answers hold (`memoSize`: a string, bytes,
raw JSON, a whole second page), charged as each is kept (`Page.charge`)
and not at all once the entry was dropped (`TestPageCacheCountsWhatItKeeps`);
parsed trees ride along uncounted. Expired entries are swept on a timer as
well as on the next probe. A fresh read (`freshRead`: a retry, every
transfer-time read, and a file's `Refresh`, which re-identifies through the
registry) takes no kept page: it is probed again and its answer replaces
the kept one, since the kept page is where the stale link came from. A
user's Retry is one: the manager re-reads a job it retries through
`FreshRead` (`TestRetryJobReadsTheSourceAgain`), so neither a kept page nor
the answers memoised on it are replayed, while a new job for the same link
still shares the minute-old success. A reader's explicit `Referer` must
match the probe's request (`Page.answers`); a different one fetches the
reader's own representation. An
extractor that reads the submitted page whole uses `fetchDocument`,
`readDocument` or `fetchRoot`, which hand it the probed document instead of
fetching again. All three are adapters over `acquire` (`page.go`), the one
implementation of that decision: the probe when `probedFor` says it answers
this request and `Page.Document` says it is a complete 200 document,
otherwise `GetPage`; a final URL the caller owns either way; and the probe's
tree only when recognition already parsed it (`parseDocument`), so a body
read without a tree never builds the page's index, and a parse error is
returned rather than lost behind a nil tree. `TestAcquisitionReusesOnlyACompleteProbe`
runs every adapter over each probe state. Any extra request a
recogniser makes goes through `memoAsked(ctx, p, key, …)` (`pageJSON` for an
API question, the player engine's canonical probe, Bandcamp's own-agent
read), keyed by what it asks, so several recognisers asking one origin's API
share one answer. An answer computed after its asker's context ended is that
caller's cancellation, not the page's: it is not kept, and a caller waiting
on it asks again with its own context, since the page is shared through the
cache with every job reading the URL (`TestPageAnswerAskedByACancelledCallerIsNotKept`).
A caller waiting for another's answer waits only while its own context
lasts — the question runs under the first asker's admission, and a
cancelled job is not to stay behind another job's queue — and leaves with
the zero value (`TestPageMemoAskedWaiterLeavesWithItsContext`).
A failure or the probe's own timeout is still an answer and is kept for as
long as its page is. Plain
`memo` is for decoding the page's own body, which no cancellation reaches. An extractor that reads an endpoint a recogniser may already have asked tries
`askedJSON` first: it hands back that answer, and asks nothing when no
recogniser did, so the extractor's own request, with its retries, is never
preceded by a bounded probe of the same thing. A link whose
path names a non-page file and matches no `Shape` skips the probe
(`probeWorthy`): probing would download the start of the file the fallback
is about to fetch.

A `Page` is borrowed, never owned: the cache hands one to every concurrent
caller and every recogniser reads it. Its URLs, header, body, parsed tree and
memoised values must not be changed. `probedDocument`, `fetchDocument` and
`fetchRoot` hand back a final URL the caller owns, and the cache probes with
a URL of its own, so editing either is safe; a reader that edits a tree (the
WordPress post reader removes related-post blocks) parses a private copy
rather than taking the shared one from `fetchRoot`. `TestSharedPageIsLeftAsItWas`
runs concurrent extractions over one page under `-race`.

The probe is reused only for the request it answers: `probed` checks its
URL, route and effective cookies across its redirect chain, while
`Page.answers` checks the reader's client identity and headers. A probe
records who asked — its client, route, cookie snapshots, and the Referer
sent at the submitted and final URLs.
`fetchDocument` and `fetchRoot` take it only for a GET
of the same URL through a client of the same identity
(`httpx.Client.SameIdentity`: User-Agent, language and cookie jar, so a
`WithTimeout` copy qualifies and another session does not), with the
matching effective cookies checked by `probed`, and no header but a matching
Referer. Header names are case-insensitive; Referer values must match exactly.
At the submitted URL this compares the initial request; at a redirect's final
URL it compares the request that reached that URL, including a Referer generated
by the redirect. An explicitly empty Referer also must match. A reader with no
Referer header accepts the probe's navigation, including its 401/403 retry.
Referer is a reuse check, not another probe-cache lookup key: a reader's fresh
response to a different Referer is not stored as a separate cached variant.
Another route can reuse only a complete success whose probe did not change
its cookies, as above.
Any other header — an agent, an Accept, a cookie, a credential — asks for
its own response, as does another method or body, which these readers never
send. A host that knows its page does not depend on a header it sends says
so where it reads: Bandcamp takes a probe the site served (not its
challenge) although it asks with an agent of its own. `pageJSON` and
`askedJSON` name a question by its endpoint and every header but the
referer (`jsonKey`), and a client of another identity neither reads nor
fills a page's answers. Fixture pages built without a recorded client
answer any reader. `TestProbeAnswersOnlyItsOwnRequest`,
`TestPageAcquisitionHonorsReferer` and `TestPageJSONKeysTheRequest` pin the
variants. The bare-manifest reader deliberately accepts a complete valid
playlist from the probe, avoiding a second use of a signed link; its variants,
tracks and queued media retain the origin Referer. A player explicitly asking
for another playlist Referer through `fetchPlaylist` still fetches that request.

Whether a read may take the probe is decided in one place, `freshRead`
(`page.go`): a context with the probed page withheld and everything else —
cancellation, route, cookie jar, logger, challenge capture — kept. Every
read that must ask the site again uses it rather than its own rule. A
semantic retry (`withExtractRetries`) hands each attempt the context to read
with, the probe's for the first and `freshRead`'s after, so a page answered
`OK` with nothing playable is read again rather than handed back; an
attempt reads with the context it is given, never one it captured.
`refreshing` and the player engine's deferred resolvers read fresh at
transfer time, since the probed page is where an expired signed link came
from; both prepare that context with `transferRead` (`contract.go`), which
also leaves the discovery job's request admission behind.
`TestTransferReadDropsOnlyThePageAndAdmission` pins what it keeps. Transport failures and 429 waits stay with `httpx.Client.Do`; the
extraction retry covers only a response that arrived and was wrong.
`TestRetryOfAProbedPageReadsItAfresh` runs the empty-then-playable player
through the registry and expects two requests.

### The supported-site inventory

The inventory in README.md is **generated** from the registry by `make
hosts`, between two markers, and `make hosts-check` fails CI when the file
and the code disagree — so edit the extractor's `Recognises`, never that
table. Adding an extractor is one line in `NewRegistry` plus `make hosts` in
the same commit. The inventory is collapsed on the front page; its markers
stay in README.md. The explanatory URL-format table lives in
`docs/supported-links.md` and is maintained separately because it documents
behavior, not registry membership. `hosts-check` regenerates into a copy and
compares it with README.md as it stands, so it judges the working tree
whatever else is in flight there; in CI the tree is the commit, which is
what makes a regeneration that was never committed fail the build.

<a id="platform-detection"></a>

### Platform families

Some extractors cover software that many operators run rather than one site
(KVS, PeerTube, Chevereto, FoolFuuka, the fediverse, MediaWiki, Bandzoogle,
the booru engines). Each is one recogniser keyed on the software's own
fingerprint, so there is no install list to keep and no setting to extend
one.

The booru engines are registered after every single-site extractor,
because their forms (`/posts/<n>`, `/images/<n>`, `?tags=`) are common and
an unmarked page is settled by guessing APIs on its origin: a page a
narrower recogniser takes costs none of those guesses. The boards the host
claim names (`familyInstalls`) are known instead (`booruBoards`): a link
on one is read as the API family it runs, with no page fetched and nothing
guessed, and a known Philomena board's "Everything" filter is its own
number rather than a lookup (`TestKnownBoardsReadAsWhatTheyRun`). A
Philomena answer that is empty under the default filter is asked again
under the board's "Everything" filter, looked up among its system filters,
before the page is refused, since that filter is what hides explicit
posts. A board is read as Gelbooru 0.1 only on that
network's own image layout (`gelbooru01Layout`), never because a 0.2 API
failed to answer once: the markup is shared, and a 0.2 board misread so
downloads samples. A tag search is held to `config.MaxBooruPosts` below the
file cap, noted as the post limit and counted in files (`weigh`), so posts
that withhold their file from an anonymous caller use none of it, and cut
at the bound rather than a page past it. A page past the end that answers
with an empty body (Gelbooru 0.2) is the search's end (`undecodable`), not a
page that could not be fetched. A marked page also answers for a first page
of only such withheld posts, and for a bare `/search` (Philomena's latest
images), which stays out of `Shape` as too common a path.

FoolFuuka's board pages (`/<board>/`, `/<board>/page/<n>/`, trailing slash included) have no Shape,
because the path is too common, but a board page the probe found withheld
(challenged, refused, failed: `foolFuukaWithheld`) is settled by its index
API, since the largest archives challenge every HTML page. An archive is
addressed at the probe's final origin, so an alias that redirects is read
as the archive it lands on. Searches are bounded by
`config.MaxFoolFuukaSearchPosts` the same way booru searches are, and cut
to it exactly.

### How a host may be used: `Pace`

An extractor declares the host's limits with `Pace` (documented field by field
in `contract.go`): `Streams` caps connections per file, `Files` caps
transfers to the host at once, `Group` shares that limit across aliases and
storage servers, `PerRoute` applies it per egress address through the proxy
pool ([proxies.md](proxies.md)), `DirectFirst` keeps a per-address file on the
normal connection, unpaced, until the host makes that connection wait, and
`WAF` adds challenge history to route selection. Declare only what the host is
known to enforce; anything else the downloader learns by being refused
([transfers.md](transfers.md)).

## The `File` contract

Most hosts set `File.URL` at extraction time. The rest of `File` exists for
hosts where that is not enough:

- **`Resolve`.** Hosts that mint **short-lived signed URLs** (bunkr and turbo,
  roughly 20 minutes) leave `URL` empty and set `Resolve` instead. The
  downloader calls it immediately before transferring, and again before each
  retry. Resolving eagerly would make a long queue start failing partway down,
  so any link signed for less than the queue may wait belongs here.
- **`Mirrored`** (`mirror.go`) builds on that per-attempt cadence: a file
  offered from several equivalent links gets a resolver that hands out the
  next one each time. Nothing tracks which mirror failed — telling a dead
  mirror from a cancelled job, a paused queue and a stall, without ever
  leaving a file with no source, buys nothing over rotating blindly.
  `MirrorHosts` builds the list when the alternatives differ only by host,
  which is how gofile's storage servers are expressed.
- **`Target.Discard`.** A resolver whose links are bound to the address that
  obtained them sets it. The downloader calls it when the host refuses the
  link for another address (409), so that the next resolve mints a new link
  rather than handing back the cached one. Keep2Share is the case
  ([hosts/keep2share.md](hosts/keep2share.md)).
- **`Check`** asks about the file itself before the downloader spends a route
  on it, over the caller's own connection (`transferRouted`, ahead of the exit
  probe). An error fails the file; a question that cannot be answered returns
  nil and leaves the deciding to `Resolve`. Keep2Share asks there whether the
  site still offers the file free.
- **`Cipher`** is set by a host that serves ciphertext, and the downloader
  decrypts on the way to disk. Only mega does. The mode is AES-CTR and must
  stay a stream cipher; see encrypted payloads in
  [transfers.md](transfers.md).
- **`Refresh`** re-reads the file's source on a new route before resolving,
  for WAF recovery ([proxies.md](proxies.md)).
- **`SizeApprox`** marks a length read off a listing page: rounded for
  display, and never enough to conclude that a file on disk is this one.
- **`Segments`, `SegmentKey`, `Audio` and `Subtitles`** carry a playlist and
  its separate tracks ([media.md](media.md)).
- **`External`** names a page that yt-dlp handles end to end
  ([media.md](media.md)); **`Reject`** sees the response a transfer is about
  to write and may refuse it ([transfers.md](transfers.md));
  **`ProxyFallback`** opts the file into proxy recovery after a temporary
  failure ([proxies.md](proxies.md)); **`Pace`** is above.
- **`Refused`** reads a refused transfer, its status and headers and the route
  it came through, in the terms of the errors below: mega's storage answers an
  address whose quota is used up with 509 and the time left, which is that
  address's `WaitError` ([hosts/file-hosts.md](hosts/file-hosts.md)). The
  downloader asks it before anything general is made of the refusal, and does
  not retry in place what it reads (`refusedByHost`).

### Errors that steer the queue

What a resolver returns, or `Refused` makes of a refusal, tells the manager
what to do with the item ([download.md](download.md)):

- **`WaitError`** means *later*, with the time to come back. The resolver
  returns it and does not sleep it out: sleeping would hold a worker for the
  whole wait while the item reads as downloading. The manager puts the item
  back in the queue until then. Through a proxy route the wait is the route's:
  the route is held back from the service until then and the file goes on
  through another. A `DirectFirst` file meeting one on the normal connection
  moves onto its group's routes.
- **`RefusedError`** means the host refuses this address, not the file.
  Through a proxy route the route is held back for the service and the file
  tries another, outside its retry budget ([proxies.md](proxies.md)).
- **`TransientError`** is a failure the next attempt may well not meet, such
  as a run of unreadable CAPTCHAs; through a proxy route it is retried on
  another route. On the normal connection, deferred resolution retries it
  with backoff up to `MaxRetries`, as it does temporary network failures.

### Progress notes

A resolver that takes a while says what it is doing with `ResolveNote`
(`WithResolveNote` carries it to the item's note): a CAPTCHA being read, a
host's timer, a player's token stage. During source discovery the same notes
reach `JobView.Note`, which carries them to the browser and the terminal,
guarded by the source generation and cleared from the view once resolving
ends.

## The generic fallback

`Direct` takes what no extractor recognised. It reads the probed document
(`Direct.document`) rather than fetching it again, recognises WordPress
categories and DarkGram albums in it, and otherwise scans it for links of a
supported form before using a generic embedded player. This is how a page
embedding a supported player works without an extractor of its own.
`filelinks.go` shares the `links:` harvester's bounded expansion; its links
are extracted strictly, so a link that only had a supported form is dropped
once its page proves to be nobody's, and a page whose links were all of that
kind, or none of whose links could be read, falls through to its own media:
a side link's failure says nothing of the article's own clip. When it has
none, the error says what the links came to: that N links of a supported
form led to pages no extractor recognised (removed items, a login, a
redesign), not that the page links nothing supported; a `links:` harvest
says the same.
A complete HTTP-200 HTML probe that yields no usable content and no
extraction error gets one additional acquisition with the origin Referer
(`Direct.pageSniff`). The probe records whether it already used that
Referer, so the 401/403 recovery does not cause another fetch. Useful pages
keep their single acquisition; genuinely empty pages also get this one
extra attempt because a soft gate need not identify itself. Eligibility
uses the probe actually consumed by `Direct.document`, since cookie
changes can make a reader fetch for itself instead. The retry uses
`freshRead` and the bounded `mediaPageFetch`, preserving the route, cookies,
admission and cancellation. HTML responses go through the same parser and
link expansion (`Direct.pageContents`). A successful retry declaring a binary
file type queues the original submitted URL with the navigation headers and
origin Referer that worked; it never reads the file body or substitutes a
temporary redirect URL. Empty-content statuses, missing/invalid types, text,
structured documents and manifest types do not establish a file. A failed
retry or one still holding unusable HTML keeps the original page refusal.
HTML-only readers retain `mediaPageFetchURL`, which never supplies a file
as a page. `referer_retry_test.go` and `referer_media_test.go` cover the gate,
request counts, unread bodies, redirects, request headers, sessions, routes,
admission and cancellation.

A URL read as a web page that still holds nothing is refused (`Direct.emptyPage`),
naming the claimant when exactly one form claims it and the host when
several do (an album link on one site has another's share form), rather
than queued as a file the
downloader can only reject after its busy retries. A probe that answered
with a failed status or a non-page type is not fetched again, nor is one nothing
answered (the URL is queued as the file); a page past
`config.MaxResponseBytes` is sniffed by the prefix the probe kept, and
`mediaPageFetchURL`, the sniff's own fetch where there is no probe, reads
such a page's prefix the same way rather than refusing it. Keep2Share links are deduplicated by
file ID within each service, across that service's domains (the two keep
their file stores apart), and by host and ID elsewhere
(`Keep2Share.fileKey`); other links keep their full URLs, including Mega
keys.
Both scanners resolve relative links against the final page URL and exclude
links to that page or its submitted alias, including fragment anchors,
before counting source limits. Self-links are navigation even when their
paths match an extractor's form; they must not trigger recursion failures
or displace downloadable sources. Other links retain their fragments.
Supported links that all fail are reported as an error rather than saved as
an HTML shell, and partial results report counts.

### Platforms behind a closed page

Most platforms are recognised in the probed page itself. When its markup is
unfamiliar — including custom frontends and pages behind a challenge — a
recogniser whose URL form matched may ask the page's own origin a bounded
question instead, asked a second time only after a busy answer
(`pageJSON`, memoised
per page, so every recogniser asking the same endpoint shares one answer):
pixeldrain file metadata, booru post records, FoolFuuka archive records,
Kemono creator profiles, MediaWiki siteinfo, PeerTube's config. A question
about the whole install rather than one page — PeerTube's config, whether
an origin answers szurubooru's post API at all (`Booru.szurubooru`) — is
also remembered per origin (`verdictMemo`: a yes for good, a no for
`config.OriginVerdictTTL`), so a page of another platform's `/c/<name>` or
`/post/<n>` links asks that origin once (`TestBooruGuessIsAskedOncePerOrigin`).
Only a no the endpoint gave is kept — a 2xx that is not the platform's
answer, or a 4xx other than 429 (`pageJSONAnswer` reports which) — never a
timeout, a rate limit, a 5xx, a challenge, a route's refusal or a check
skipped for a client of another identity; and a fresh read (a user's
Retry) takes no kept no (`TestVerdictMemoKeepsOnlyDefiniteAnswers`). The
fediverse and PeerTube take the form from either end of the probe's
redirect chain, the submitted link first, and ask that link's origin: a
server sends a logged-out visitor of an account, post or private video to
its sign-in or remote-interaction page, and the link is still the item the
API reads. An extractor
then reads the memoised answers back through `probed` and `askedJSON`
instead of asking again. Positive identification commits to an extractor, whose errors must be
returned rather than falling back to saving the page shell.
The API response must identify the software and, where it returns a record,
the requested target. Generic JSON or a matching site name is insufficient;
the HTML's status and branding do not veto a valid API response.

MediaWiki also asks its conventional API paths for a successful HTML page
with an explicit title route (`/wiki/<title>`, `index.php?title=`), or a
generator naming the software. FoolFuuka asks the corresponding index API
for a successful HTML board route with its trailing slash, including paged
boards. These checks admit custom and generator-only frontends only after
the API proves the platform; missing endpoints and unrelated JSON do not
establish support. They use the same bounded, memoised `pageJSON` reads.
Ordinary missing pages, non-HTML responses and unmarked bare slugs do not
gain these checks. Neither broad article nor board routes enter `Shape`,
so automatic discovery still excludes their navigation.

An extractor that reads an open API and never its page — 4chan's thread
JSON, the MEGA API, RedGIFs, Turbo's signing endpoint, Streamable's player
document — takes a
link of its own form whose probe was **withheld** (`probeWithheld`,
`withheld.go`: a challenge, a 401 or 403, a 429, a 5xx, a transport
failure; never a 404 or a page too large to read) when that API answers
for the link's id. The question is asked once per page through
`withheldAsked` or `pageJSON`, so the extractor reads the same answer
back, and only for a withheld page, so a served page of the same form
without the markers still costs nothing for these readers. The answer must name the link's
own target: a thread with posts, a file whose MEGA attributes decrypt
under the link's key, a status naming the file. A page the site served
keeps the marker rules above. An extractor whose API is one fixed
service's — Keep2Share and FileBoom, Gofile, WeTransfer — asks no such
question: its own domains are claimed before any probe, and a withheld page
on another host is not worth that service's API being asked about another
host's id. 4chan defers to an
archive whose own origin answers as FoolFuuka, since archives mirror the
board's thread links and numbers. Gelbooru 0.1 has no API to ask, so a
withheld legacy board page stays unrecognised. Because a fixture on an
unresolvable host is a withheld page, the package's `TestMain`
(`main_test.go`, left out of the `live` and `corpus_audit` builds) sends
every request but loopback ones to a closed proxy port, so a unit test can
never ask a production API.

Compatible creator archives may return attachment metadata with
`has_full: false` before storing the originals. Such posts contribute no
files or file-limit weight; mixed listings report them as left out, and a
post or creator with only previews reports that originals are not archived.
An absent availability flag keeps the ordinary API's attachment behavior.
Previews must not become original downloads or fabricated `/data` links.

Keep forms that are this broad out of `Shape` where they would make ordinary
navigation a probe per link; `TestRegistryKnowsLinksByTheirForm` pins which
forms the link scanner follows.

### DarkGram albums

DarkGram albums are recognised in the same fetched HTML, by the player's
`darkgram-album` class and JSON `data-album` attribute. The first player
supplies its own videos (`hls_url`) and full-size photos (`photo_url`);
thumbnails and other players are not album items. Discovery makes no media
requests. Each video resolves its playlist through `File.Resolve`, refreshing
segments and keys per attempt and keeping the final page URL as referer. File
names keep the original item positions when missing entries or file caps
shorten an album.

### WordPress categories

WordPress categories are recognised in the same fetched document, before the
ordinary link scan, rather than claimed by domain. `wordpress.go` reads
classic post permalinks or block-theme query loops, follows the category's own
next control, and opens only the listed posts. The category heading and pager
may sit outside `main`, so the primary content area is kept around it. Post
content supplies supported host links, embedded media and JPEG images;
original-image links and metadata outrank thumbnails. Registered extractors
resolve the host links, keeping their deferred resolvers and pacing. Without
such links, the album and player parsers of the generic fallback read the
post's content and title, keeping sidebar players and head metadata out.
DarkGram videos keep their deferred playlist resolvers; player artwork is
excluded, and independent JPEGs are kept without duplicates. The post reader
is passed to the shared source expansion
([below](#sources-that-lead-to-other-sources)), so folder names, concurrency
and caps are the shared ones, and no host-specific WordPress category scraper
is kept beside it. Tests use invented classic and block-theme pages; saved
browser archives stay untracked and can be parsed offline without contacting
their source sites.

## Listings

### Sources that lead to other sources

An extractor whose results are other sources — an index, a search, the
`links:` harvester, a WordPress category — resolves each through the registry,
so every host's deferred resolver and per-route pacing reach the downloader
intact. `sources.go` holds what they share:

- **The supported-link filter.** A source excludes itself by extractor *name*
  rather than pointer (`supportedSources`), since the recursion is in the kind
  and not the instance; without it a site's own navigation turns one search
  into a walk of the whole catalogue. Only the `links` extractor opts into
  the explicit same-site item policy; indexes retain `Registry.Follows`.
- **The fan-out.** `expandSources` resolves sources concurrently and applies
  the file cap **a source at a time**: an album is taken whole or not at all,
  resolving stops once the cap is reached (filled exactly, no further batch
  is asked for), and only a first album larger than
  the cap on its own is cut. The note counts the sources actually in the job.
  An extractor expanding its own albums or posts rather than registry links
  uses the same fan-out through `expandSourcesWith`, so its cap, folders and
  refusals behave alike.
- **The folders.** Each source gets a folder named after its title
  (`sourceFolder`). Titles are not unique, so `expandSources` counts the names
  first and gives a repeated one the source's own id (`sourceTag`), which
  keeps two different files of one name from sitting side by side as "(2)" and
  "(3)". A title that stands alone keeps its plain name, which is what
  anything already downloaded under it is filed as. A host whose repeated
  titles were filed under a form of its own before this existed keeps that
  form through `expandSourcesNamed`'s `folders` hook, since renaming them
  would download them all again.
- **The note.** A collection expanded from sources finishes through
  `expansion.unresolved` (cancellation, then `noneRead` when nothing
  resolved) and `expansion.note`, the one place its note is put together,
  in order: the walk's own note; the caps, as the file limit or, with
  `sourceWords.found`, as "N of M" against what the page offered
  (`partialNote`, on `cappedNote`, which stays about caps alone); the
  sources left out, per reason through `outcomeSummary.note` or, with
  `counted`, as "N of M <noun> resolved" where no cap already counted them;
  and the sources whose own result carried a note, "N of M <noun>
  incomplete" unless `children` gives the family's words. A source
  resolved but cut by the file cap is not a failure. A link no extractor
  recognised is counted as not resolved and named, "K unrecognised";
  where the sources are the collection's own pages (`own`) an
  unrecognised page is a dead one, counted as not resolved alone. A child's note is counted, not parsed: it says the
  child reported something left out, not what. Use these rather than
  joining a collection's note by hand.

### Walking pages

- Readers that open children while walking use `pageWalk.consume`, also
  exposed by `walkListing`. It receives only new entries admitted by the
  inspection budget, before another listing page is fetched. A separate
  produced-file budget stops the walk; an exact fit at the listing's stated
  end is complete, while uninspected entries or pages retain the file-limit
  note. `listingFiles` resolves small ordered batches for flat channel/post
  results. Erome uses `sourceExpansion` to retain whole-album boundaries and
  title disambiguation across pages. Empty, deleted or unavailable children
  spend the inspection budget but no file slots; cancellation still wins.
- Every paged listing that is not a folder tree, broadcaster series included
  (`walkEpisodes`), walks through `walkPages` (`pages.go`). The caller's `fetch` reads page *n*
  and keeps the cursor, offset or page number in its own closure; the engine
  owns the rest, once: a first page that fails is the caller's error as it
  stands and a later one a partial listing (or an accepted end, or the `failed` hook's
  note), entries deduplicated by `key`, a page that adds nothing ends the
  walk, the page cap (`config.MaxAlbumPages` unless `pages` says otherwise),
  the limit and its note, and cancellation, which is never a partial listing.
  `walkListing` (`listing.go`) is its adapter for HTML pagers.
- Why a walk ended is state, not wording. `walkPagesFor` returns `walked`:
  the entries and a `walkEnd`, which carries the `walkStop` (the listing's
  own end, the limit, the page cap, a repeated or empty page, a stop page, a
  later page that failed) beside the page's and the `failed` hook's notes
  and the walk's `ends` and `limitNote`. `walkEnd.render` alone turns that
  into the note `walkPages` has always returned, string for string, and
  `walkEnd.reason` into the same thing as a bare reason for a caller that
  marks its result partial itself. A caller that needs the reason reads
  `stop` rather than matching a note: `walkEpisodes` reports `more` from
  `walkLimit`, and a folder tree takes the `walkEnd` itself
  (`treeWalk.ended`). `walkPages` remains the adapter for callers that need
  only the items and the displayed note. The later page's error is kept
  whole (`walkEnd.err`), so a caller reads its status or challenge rather
  than the note.
- Each page says how the listing ends, from what the API states: `total` (the
  count it holds), `pages` (the page count it runs to), `end` (an end flag, a
  missing cursor, or an empty page asked for past the end), or no `next`. Do not end on a page shorter than the
  page size asked for unless the API is known to serve every page but its
  last in full at the size asked for, one entry per item listed and none
  filtered out after the fact (BitChute's channel endpoint; the fediverse
  timelines, `fediverse.go`, one entry per status at the dialect's
  `stride`): hosts commonly serve fewer than asked without saying so. Asking
  one page past the end is cheap and right where nothing else says. A host
  that ignores its paging parameter serves page one again, which adds
  nothing and ends the walk.
- A page that adds nothing but is no repeat — cards none of which could be
  used, a pager alone, a cursor page whose items were all filtered out, a
  continuation that only names others — sets `pageOf.skip` (`listingPage.skip`
  through `walkListing`), and the walk goes on to the next page, even from
  the first. At most `config.MaxSkippedListingPages` in a row are passed
  over, so a host serving empty pages with a next link forever costs a few
  requests. A page that only repeats sets no skip and still ends the walk.
  A last page (no next, no note) that adds nothing is the listing's end, not
  a repeat, unless it is the first page served again: a newest-first feed
  walked by offset ends this way when new posts shift the window.
- MediaWiki's `mediaWikiWalk` passes over empty batches with a continuation,
  including a response with no `query` payload. `batchcomplete` ends a batch,
  not necessarily the generator. Each request combines the original query
  with only the latest continuation: a completed property module can drop
  its token while another module continues. The whole encoded request is
  checked for repetition before sending, so a repeated token or a cursor
  cycle cannot keep the walk going. An empty terminal batch ends normally;
  duplicate entries, the file/source caps, `MaxSkippedListingPages` and the
  page cap still bound the walk. `mediawiki_continue_test.go` covers file
  and subcategory queries, compound tokens, limits and later failures.
- A listing whose end is found by asking past it declares which ends are
  its own (`pageWalk.ends`, `walkAccepts`): a page that adds nothing or is
  served again (`repeated`), a later page without the listing (`missing`) or
  moved to another (`redirected`), or particular later-page statuses (a 404
  asked for page n+1), or a later page whose answer does not decode
  (`undecodable`: an API that answers past its end with an empty or broken
  body). Those end the walk with no note. Anything not
  declared is a cut and keeps its note: a timeout, a challenge or a server
  error ("could not fetch next page"), a rate limit the client could not
  outwait ("rate limited", read off the kept error), and the page cap. A
  stop page carries its kind (`pageOf.stopped`: `stopRepeatedPage`,
  `stopMissing`, `stopRedirected`) beside its wording, and the policy reads
  the kind, never the text. `TestWalkEndPolicies` is the matrix. There is
  no blanket switch accepting every end alike: each listing names its own.
  `failed` maps a failure to a note of the caller's own where the shared
  wording does not fit. `note` is a reason the
  walk ends early, such as a next page that cannot be followed.
- Stop **at** the cap rather than collecting everything and truncating: a
  broad search can state tens of thousands of pages. Where every entry is a
  full extraction at listing time — an album, a gallery, a harvested link —
  the cap is `listingLimit` (the lower of the source and file caps), except
  CoomerFans posts, Erome albums and Chevereto's album discovery, which use
  the source cap, and PornPics'
  gallery listings, which are held to the extracted file count alone:
  one block request names hundreds of galleries, and the expansion stops
  opening them once the files fill the cap. Imgur's account and tag
  listings hold all their rows to `config.MaxListingPosts` (or the file cap
  when lower), and their post rows to it too unless the user set a source
  limit below the default, which then bounds them (`imgurPostCap`: a raised
  source limit never lowers the bound), counting each post once however
  many pages repeat it. A Cum creator's posts are each a source as well as
  files: the walk takes no more posts than the source limit and asks no
  page for more than it leaves room for. Where
  every entry is queued as a file of its own — a profile's deferred videos — it is
  `entryLimit`: the file cap, or the host's own lower cap from
  `config/limits.go` (`MaxChannelVideos`, `MaxProfileVideos`,
  `MaxListingVideos`), noted as the listing limit. The source cap is not a
  bound on those. BitChute and PeerTube inspect up to `MaxChannelVideos`
  and `MaxListingVideos` respectively, stopping sooner when the extracted
  files fill the file cap; unavailable videos do not consume that cap.
  Otherwise the file cap. Where an entry may hold several
  files or none — a post, a status, a search hit — `weigh` counts the file
  cap in what the entries hold rather than in entries, so a page of text
  posts never uses it up; the page that reaches the cap is kept whole, noted
  when it passes the cap or the listing goes on, and the registry cuts the
  files to the cap. Fediverse timelines count each attachment URL once across
  the whole walk, matching the final file deduplication, so reposted media
  cannot crowd out later unique files. Post IDs still control repeated-page
  detection.
- RedGifs user feeds turn records with HD or SD media into files before
  applying the file cap and ID deduplication. An unavailable copy must not
  hide a later playable copy with the same ID. Batches without playable
  files, and empty responses whose page count names later pages, use the
  bounded skip path; an empty response without later pages ends the walk. Repeated
  playable entries still stop the walk, and `MaxSkippedListingPages` and
  `RedGifsMaxPages` bound requests independently of the file cap.
- Chevereto listings count direct files against the file cap and distinct
  album links against the source cap. A page's direct files take precedence
  over its album links; album discovery cannot cut off those files. Empty
  or unreadable albums spend no file slots when expanded. PornPics listings
  remain exempt from the source cap: each block's galleries are opened before
  fetching the next block, and only admitted files spend the file budget.
  Both retain the page walk's bounds and repeated-page termination.
- Entries that need their own page are then read concurrently through
  `resolveListing`/`resolveOutcomes` (`resolve.go`), bounded by `config.PageFetchConcurrency`, with results
  collected by index so the job lists files in the site's own order however
  the requests interleave. One that will not load is skipped rather than
  failing a job of hundreds; when none loads, `noneRead` says why, and when
  some do, the note says how many the host refused and why (`refusedNote`).
- `resolveOutcomes` is the one ordered, bounded map: serial (`inTurn`) and
  concurrent modes both start nothing once the context ends, let started
  work settle, and keep each outcome's index and error. `readSeasons`,
  `resolveEach` and source expansion go through it. Every finisher —
  `readListing`, `outcomeSummary.noneResolved`, `expandSeries`, the walkers' cap exits and
  the album callers that finish by hand — checks the caller's cancellation
  **first**: a listing cut short and returned as a success would be saved
  as if it were the whole listing. It is the only scheduler: callers that
  fill values in place and need only whether each item could be read use it
  too (`Autoindex.readAll`, which keeps a read cut short by cancellation
  from counting as an unreadable directory).
- The `PageFetchConcurrency` bound is per map, not per extraction: a
  listing whose items are listings (source expansion through the registry,
  a WordPress category's posts) multiplies it. What bounds the extraction is
  its request admission: the outermost `Registry.Extract` puts one
  `httpx.Admission` of `config.DiscoveryRequests` slots in the context, and
  every nested registry call, probe, memoised question and credential fetch
  made in it is admitted through that one (`httpx.Client.send`). A slot is
  held only while a request is out and its body unread, never by a parent
  waiting on its children, a caller waiting on another's coalesced fetch or
  a retry waiting out its backoff, so nesting deeper than the slots cannot
  deadlock. Each top-level job has its own admission; a process-wide or
  per-origin bound would be another decision. Transfer-time resolvers read
  without it (`refreshing` and the player engine's deferred resolvers drop
  it with `httpx.WithoutAdmission`), as do media transfers, which never
  carry it, and metadata-helper processes are not counted.
  `TestNestedDiscoveryRequestsUnbounded` measures the multiplied baseline at
  the HTTP boundary (36 leaf requests in flight for one nested root, 72 for
  two) and `TestNestedDiscoveryRequestsAreAdmitted` pins the bound (12 and
  24), the same files, order and request count, slow bodies and a budget
  below one map's fan-out; the cost is latency, about three waves where
  there was one for that fixture. `config.SourceBatchRounds` sizes source
  expansion's batches, not the bound.

### Caps

The listing caps are settings (`HEAPLEACH_MAX_SOURCES`, `HEAPLEACH_MAX_FILES`,
and `-max-sources`/`-max-files`), defaulting to `config.MaxExpandedSources`
and `config.MaxListingFiles`. They reach the extractors through
`extractor.Options`, whose unexported `limits` the registry fills from the
config in `Registry.Extract`. So a nested extraction, an album found by a
search, is bounded the same way, and every extractor that walks a listing
reads `opts.maxFiles()` rather than the constant. Zero means the default, and
there is no "unlimited": every file of a job is also a row in the UI and a
line in the saved queue.

A few hosts keep a fixed bound of their own where the shared one would ask
too much of them, each the bound the host always had, and the user's file
cap still cuts first where it is lower:

- a broadcaster series is resolved to `config.MaxResolvedEpisodes`; NPO,
  VRT and the strand broadcasters lower (`MaxResolvedEpisodesNPO`,
  `MaxResolvedEpisodesVRT`, and `MaxResolvedEpisodesStrand`, 500, for NRK,
  SVT, RÚV, RaiPlay, ABC listen and PBS, through `series.budget`); Yle, ARTE
  and RTBF follow the user's file cap where it is higher,
  `Options.episodeBudget`, and where it is lower resolve in rounds up to
  it, `series.files`, so no episode past the cap is read, a refused one
  before it costs no playable one, and the note says "N of M episodes".
  RTBF's article pages resolve their embedded players the same way, in
  rounds up to the file cap. Radio-Canada's programmes cut the listed
  episodes to the file cap themselves and say "N of M episodes";
- a paged listing on a host that rate-limits or answers a long walk slowly
  reads no more pages than its own bound (`pageWalk.pages`,
  `listingWalk.pages`), noted as the page limit: `KemonoMaxPages` (40),
  `RedGifsMaxPages` (25), `EpornerMaxProfilePages` (60, with
  `MaxProfileVideos` on the videos), `EromeMaxProfilePages` (50) and
  `BandzoogleMaxContinuations` (20 past the page itself);
- a paced wiki's category walk to `MaxMediaWikiCategories` under the source
  cap;
- a folder share's tree to `MaxTreeFolders` folders and depths 0 through
  `MaxFolderDepth` (the root is depth 0), within the host's own
  `treeBounds` where it has them (`walkTreeWithin`): a Pixeldrain share to
  `PixeldrainMaxFiles` (5000) files, noted as the listing limit, and a
  Filester folder walk to the user's source limit in folders.

A listing that a failure cut short — a note naming a page, folder or season
that could not be read, or a rate limit (`Result.Incomplete`, `notes.go`) —
is kept rather than failed, and the manager marks the job `incomplete`, so a
retry reads the source again. A note's failure words are the contract here:
a new failure note says "could not fetch", "could not read", "unreadable" or
"rate limited", or adds its words to `failedReasons`.

A host's own request sizes and waits are sorted by purpose. What the
application chooses — a pause between pages (`Gelbooru01Pace`), a cooldown
after a refusal (`GofileRejectionCooldown`), a page size asked for below what
the server allows (`PornPicsPageSize`, `ORFPageSize`, `RedGifsPageSize`) — is
a tunable in `config/limits.go` under the host's name, even where two hosts
share a value. What the server fixes stays beside the code that speaks its
protocol: a page size that is the API's own ceiling (the fediverse strides,
MediaWiki, BitChute, Niconico, Civitai, RTBF, ABC listen, the booru families'
limit) or the unit an offset counts in (`gelbooru01PageSize`), where changing
the number would not be a different setting but a broken walk.

### Titles, notes and folders

- **What names a job's folder is `Result.Title` alone.** Anything an extractor
  has to admit — a listing cut short by a cap or a rate limit — goes in
  `Result.Note`, which the job's shown title carries (`Result.Label`) and the
  folder never does. A note in the title would change the folder whenever a
  cap was raised or a rate limit waited out, and the next run would file
  everything somewhere new and download it all again.
- **A note is built from the helpers in `notes.go`, so every extractor's
  reads alike.** `joinNotes` is the one joiner: "; " between parts, empty
  parts dropped, a part the note already holds not repeated (a joined note's
  parts count one by one), and "partial — " said once, at the note's start —
  "partial — invalid next page; file limit". The reasons a walk stops on
  most are constants — `fileLimitNote`, `sourceLimitNote`, `invalidNextNote`,
  `couldNotFetchNote`, and bare `fileLimit` and `sourceLimit` for a tree
  walk's `note` — so they cannot drift apart. The parts: `partial` marks one
  as a cut, `cappedNote` says "N of M <unit>" (bare "N of M" with no unit),
  `resolvedNote` "N of M <noun> resolved", `countNote` "N <words>" in the
  caller's words, `leftOutNote` "N left out…" on it, `refusedNote` counts
  refusals per reason ("3 left out: gone; 1 left out: live", on `tally`),
  and `brokenNote` a walk that broke off. An "N of M" is never prefixed with
  "partial — ": the count already says the answer is short, and a cut it
  stands beside carries its own "partial — " reason. A hand-built count, or
  a join of its own, is what these replace; a new note uses them rather than
  a `Sprintf` that only nearly matches.
- **A slash in a title is not a path.** `SafeName` keeps only what follows the
  last separator, which is right for a name a server hands over (RFC 6266 says
  to strip a `Content-Disposition` name of its path) and wrong for a title:
  "A Series (1/7)" would be saved as "7)". So `Registry.Extract` and the
  downloader's `resolveTarget` turn a title's separators into lookalikes first
  (`util.TitleName`, "⧸", as yt-dlp writes them), for job titles and file
  names alike. `sourceFolder` turns them, and plain slashes, into "_" instead;
  keep that, since changing it would file every re-read job somewhere new and
  download it all again.
- **Names the consolidation changed are kept as they now are.** Moving to
  recognition by page renamed some files and folders, and a file saved under
  the older name downloads once more under the new one beside it. Each is
  kept on purpose, since changing it back would rename what was saved since:
  - a FoolFuuka board page and a booru tag search are titled by the host's
    label (`hostLabel`, `booruLabel`) rather than a name from an install
    list, which reads differently for some installs (an `archive.`
    subdomain gives "archive");
  - `folderName` goes through `sourceFolder`, so a slash in a fediverse post
    or note title, or in a creator post's folder, is "_", not "⧸", and a
    broadcaster's season or programme label holding a slash is one folder,
    not two nested ones;
  - an HLS file is named after its segments' container
    (`hlsMedia.extension`), not after the variant URL, so `.ts` and `.mp4`
    can swap (see vidmoly in [hosts/video-hosts.md](hosts/video-hosts.md));
  - a vids.st name that carries no extension gets `.mp4`;
  - a media link on rajce's storage hosts, pasted alone, is the generic
    fallback's, named as it names any file, without the `.mp4` the album
    reader gave an extensionless video.
- `Registry.Extract` decodes HTML entities in titles and names for the UI's
  sake; `SafeName` decodes them again on the way to disk
  ([download.md](download.md)).

## Helpers worth reusing

Before writing a new extractor, pick its shape from this table; before
adding another walker, resolver, formatter or cache, look for it in the
list below.

| The site | Write | Where |
| --- | --- | --- |
| A video page whose player names its renditions | a `playerSite` row: `parse` when the page is all it reads, `read` for an API or handshake | `playersites.go` |
| A stateless share or album page read by its own parser | a `shareSite` row, unless its `Recognise` must accept pages its forms do not describe | `shares.go` |
| A link that only hands off to another host's page, or a page that only points at one | a `handoffSite` or `pointerSite` row | `handoffs.go`, `pointers.go` |
| Software many operators run | one platform-family extractor keyed on its fingerprint, registered after the single-site extractors and before the fediverse (`TestRegistryPrefersTheNarrowerFamily`) | [Platform families](#platform-families) |
| A host with tokens, quotas, cooldowns, sessions or signing of its own | a dedicated extractor, composing the helpers below | its own file |

- `about` (`extractor.go`): an extractor's name, inventory text and shape
  as one embedded row, so its constructor reads as data rather than three
  hand-written methods. Every extractor embeds it, and so does every engine
  row (`playerSite`, `shareSite`, `pointerSite`, `handoffSite`), whose
  literals keep writing `name:`, `shape:` and `recognises:` as promoted
  fields; each engine's constructor copies the row's `about` into the
  extractor, so build engines and extractors with their constructors, never
  as bare literals, or they have no name and no shape. A shape handed to it
  is a plain function named `…Shape`, nil for an extractor with no form
  worth a probe; `TestRecognitionNeverReadsTheHost` checks those too.
  `eitherShape` combines shapes.
- The `Share` engine (`shares.go`): a file host whose share or album page
  is read by its own code is a `shareSite` row — its name, inventory line,
  forms (a shape plus what a page of that shape must carry; the first form
  matching the final URL decides) and its `extract`. The row sets no shape
  of its own: `newShare` makes the extractor's the union of its forms'. A form with no
  recogniser is a link that only redirects: followed, never recognised.
  `TestShareFormsNeverReadTheHost` holds the rows to the same no-host rule as
  `Shape`/`Recognise`, and `TestRegisteredRowsAreComplete` checks that
  every registered row has its callbacks and a recognising form. Hosts with state of their own (tokens,
  quotas, cooldowns) stay extractors; `routeWaits` (`routewaits.go`) keeps
  their per-route waits. Do not convert a stateless extractor whose
  `Recognise` accepts pages its `Shape` does not advertise (Erome's profile
  pages, PornPics' listings without a discoverable form): `Share` builds
  `Shape` from its forms and recognises only a matching form, so such a
  conversion either loses those pages or widens link harvesting to
  navigation. Rajce is the reference conversion: one form, page-only
  recognition, its parser and notes kept as they were.
- The `Player` engine (`players.go`, its rows in `playersites.go`): a video site is a `playerSite` row
  (its forms, recognition, canonical page and a reader returning the
  candidate renditions — `parse` over the engine's one page acquisition,
  `readPlayerPage` on `readDocument`, or a `read` of its own for an API or
  handshake — with a per-read referer when the player needs its own), registered with `NewPlayer` at the position where its form order
  matters. The engine picks the rendition, follows a playlist and re-reads a
  signed page at transfer time. A row may `peek` (read only the title now
  and everything at transfer time), take a `listing` of its videos (through
  `walkListing`, or a walk of its own), refuse playlists still being written
  (`finished`), and opt into proxy recovery (`proxyFallback`, which
  `Registry.Extract` reads off the `*Player`; no other extractor opts a
  source in). Add a new video site as a row, not an extractor.
- `refreshing` and `signedResult` (`contract.go`): a file whose link is
  signed per visit, resolved now and signed again when its transfer starts.
- `unpackJS` and `jwSource` (`jsdecode.go`): the p,a,c,k,e,d packer reversed,
  and a JW Player setup's `sources:[{file:…}]` list read; both host-neutral.
  `jwSource` takes the **first** non-DASH link of a JavaScript setup and
  reads nothing else about it, while `jwPlayerBest` (`mediapage.go`) ranks a
  JSON feed's typed renditions, video before audio, with exact sizes. They
  stay separate readers: a shared candidate step would either change the
  first-source behaviour the player rows rely on or need the JS reader to
  parse fields it does not have. Revisit only with fixtures that pin the
  intended ordering, the sprite exclusion and each caller's preference.
- `broadcast.go`: what the public broadcasters share — `itemRefusal`
  (declared beside the listing finishers in `resolve.go`, since other
  listings refuse items too) for DRM and geo refusals in the broadcaster's
  own words, `expandSeries`
  for a series resolved while it is listed (capped by a request budget, noted
  where cut, filed by season, one error when nothing plays) and
  `listSeries`, which ends a listing with it, beneath it
  `resolveEach` for a listing's episodes resolved concurrently or one at a
  time, in order, `walkEpisodes`, which walks a series through `walkPages`
  up to the request budget and says whether it went on, `readSeasons` for
  a programme's season lists read at once or in turn, an unreadable season
  left out and noted, `lazyEpisode` and
  `refreshing` for files re-read at transfer time, the note helpers above,
  `joinTitle` with its `titleEqual`, `titlePrefixed` and `titleContains`
  for naming an episode by its programme, and `writtenTitle` for a
  page's `<title>` with its spacing kept. A new broadcaster uses these
  rather than types of its own.
- `resolveListing` and `noneRead` (`resolve.go`): a listing whose items
  each open into files (a band's releases, a board's threads, a channel's
  videos, a profile's posts, albums or galleries), resolved concurrently
  with the files kept in order and the account of the items beside them,
  and the one error, `<site>: none of the <n> <noun> on <where> could be
  read: <reason>`, for when none could. `resolveListing`, `readListing`,
  `resolveEach` and source expansion keep the whole account of the items, `outcomeSummary`
  (`summarise`): resolved, empty, refused (each reason, in order), failed
  (counted, the first kept as the diagnostic), unrecognised and not
  started, counted per item rather than per file. When some resolved,
  `outcomeSummary.note` says what was left out — the refusals per reason
  (`refusedNote`), then "N left out: could not be read" (`unreadNote`) —
  so one unreadable item beside others that resolved is never silent. An
  item that resolved to nothing is not mentioned, and a link no extractor
  recognised is never a failure; source expansion names it apart
  (`expansion.note`). `expandSeries` always notes
  unreadable episodes and leaves refusals to the broadcaster
  (`noteRefused`). A listing that needs each item beside what it resolved
  to uses `resolveOutcomes` with `summarise` directly. An
  item whose host states why it holds nothing (a live stream, a video
  still transcoding) returns an `itemRefusal` (`resolve.go`), so that
  reason outranks a failure in the error. Where every item costs a request at
  listing time, the listing is held to `listingLimit` and notes the cut.
  A listing of items to resolve uses these rather than reading
  `resolveOutcomes` by hand, so its refusals and first failure are kept.
- `linkExt` (`html.go`): the extension a link's file name carries,
  lowercased, read off the path's last element (so a trailing slash still
  names it), with a fallback where it names none.
  `linkExtAsIs` (`links.go`) keeps the link's own spelling, for a host whose
  downloads are already filed under it.
- `walkTree` (`tree.go`): a share's nested folders, depth first in the
  host's order, with the depth, folder and file caps, one folder per child
  through `sourceFolder` (`treeDir`), and an unreadable subfolder reported
  as partial. A host supplies only the reader for one folder; one that pages
  a folder walks it with `walkPagesFor`, hands why the listing ended to
  `treeWalk.ended` and may stop reading past `treeWalk.room()`.
- `resolveMediaPlaylist` (`hls.go`): an HLS master with its key, separate
  audio and subtitles ([media.md](media.md)).
- `walkPages` (`pages.go`), above, for any paged listing, and its HTML
  adapter `walkListing` with `webLink` (`listing.go`): redirect, missing-page
  and repeated-page checks on top of the engine. A later page's redirect ends
  the walk only when it changes what was asked for (`sameListingPage`): an
  upgrade to https, the host's case, a trailing slash and the order or
  escaping of the query are a server's canonical form of the same page. A new paged listing supplies
  only the reader for one page, which also names the next page for a listing
  paged by number, and sets `badNext` when the pager offers a next page that
  cannot be followed ("partial — invalid next page"). A `listingLink` is
  told from its repeats by its URL unless it names a `key`, and
  `listingURLs` turns the walk's links into the page URLs a caller expands.
  `webLink` is the one check that a scraped href is a web link worth
  following; a host's own rule (same site, a file extension) goes on top of
  it rather than beside a copy of it. Options cover the rest:
  a first page the caller already read (`root`), a fetch of its own (`fetch`,
  `fetchAsAsked`), page identity (`key`), redirects judged by the reader
  (`anyRedirect`), and the engine's `ends` and `failed`; a `failed` hook
  returns its note and whether it decided one, and a hook that singles out
  one failure returns false for the rest, leaving their note to the walk. `expansion.note` (`sources.go`) builds the job note
  of a listing expanded into its sources, `partialNote` its caps.
- `expandSources` keeps, beside the files, the `outcomeSummary` of every
  source it attempted (embedded in `expansion`), so a page none of whose
  sources resolved says why through `noneRead`; a link no extractor
  recognised is counted apart (`unrecognised`), never a failure. A source
  that resolved but was left out by the file cap is resolved, not failed:
  `used` counts the nonempty sources admitted, apart from the batch work done
  past the cap. Empty successes remain in the outcome account without using
  the first nonempty source's exception to be trimmed when it alone exceeds
  the file cap. `sourceExpansion` carries the same admission budget and
  outcomes across listing blocks, stopping when the files fill the budget or
  the next whole source cannot fit. Folder names are assigned after all
  admitted sources are known, so titles repeated across blocks remain distinct.
- `hlsMedia.file`/`hlsMedia.target` (`hls.go`), `Target.file` and
  `File.target`: a resolved playlist or target as a File and back, every
  media field carried over — segments, key, audio and subtitles. Build a
  Target from a File only through `File.target`: the downloader replaces an
  item's key and tracks whenever a target has segments, so a hand-built
  conversion that drops a field clears it. Control fields (`Resolve`,
  `Check`, `Cipher`, `Pace`, `Reject`, `Discard`, the directory) are not
  media and stay with their owners.
- `rankIn` (`renditions.go`, with the rendition model `bestCandidate` ranks): a name's place in a quality ladder, ignoring case,
  with the caller choosing the value for a name not on it.
- `renewable` (`renewable.go`): a process-wide credential an extractor holds
  for every job — Gofile's guest account, RedGifs' bearer token, VRT MAX's
  player token. `get` reuses the value until it expires; `renew(stale)`
  replaces it once however many callers were refused with the same value,
  and returns an existing replacement to a refusal that arrives late. Only a
  validated value is cached, a failed renewal is retried by the next caller,
  and waiting is cancelable while the fetch runs detached from any one
  caller's cancellation and from its admission (the value is every job's,
  and they do not queue behind one job's requests); it keeps the route of
  the caller that started it. Only a success is shared: a failure may be
  that route's (a proxy's error, an address refused), so a caller that
  shared a failed renewal renews once more with its own context, joining
  only a renewal already under way on its own route
  (`TestRenewableSharesOnlyASuccess`).
  Requesting and decoding, what counts as refused,
  device ids and retry policy stay in the extractor; TTLs and margins live
  in `config/limits.go`. RTBF's session (with its device id) and ORF's
  site-read API credentials keep their own code, as do page memoisation,
  platform `hostMemo`, per-post loaders and address-bound tickets such as
  Keep2Share's and `Target.Discard`: their identities and lifetimes differ.
- `rawText`, `scriptTexts`, `linkedData` and `jsonAttr` (`html.go`): script
  payloads and JSON attributes as written. Decode JSON-LD, `__NEXT_DATA__`
  and `data-page` through these, never through `textOf`, which collapses
  whitespace for display text and would change names (and so paths) inside
  the JSON. `linkedData` escapes the raw line breaks, tabs and other
  control characters templates leave inside JSON-LD strings
  (`jsonControlsEscaped`), which a strict decoder would otherwise refuse,
  skipping the block. Schemas, required fields and which object counts stay in each
  reader. A reader that recognition and extraction both need takes the
  parsed root and is shared through `memo` on the same `Page` (the gallery
  path in `celeb.go`); keep a string entry point for synthetic fixtures, and
  treat memoised values as immutable. A canonical URL that differs from the
  probed one reads its own page. A reader that only extraction needs still
  takes the root, through `fetchRoot`, so a tree from a matching probe is not
  parsed again: NPR (`nprRead`), ABC listen (`abcListenRead`, its
  `__NEXT_DATA__` read off the tree with `nextDataIn`), CoomerFans posts
  (`coomerFansRead`), PornPics galleries (`pornpicsGalleryRead`) and
  Chevereto pages (`cheveretoFetch`), ok.ru's player (`okruMetadataRead`)
  Bandzoogle pages (`bandzoogleRead`, whose recogniser finds a track
  element and so has always built the tree), Imgur's bare media page
  (`imgurMediaIn`, recognised by its meta), france.tv's page title
  (`francetvTitleOf`) and Noodlemagazine's watch page (`noodleRead`), both
  recognised by the site's name, do, each string adapter only parsing and delegating, so there is one
  parser per page. `fetchRoot` and `readDocument` take the probe's tree only
  when recognition already built it (`Page.parsedRoot`); otherwise they parse
  the body for the reader alone, which costs less than the index
  `Page.HTML` builds beside the tree, so a reuse is never dearer than a
  reparse. Rajce's recogniser only scans the body, so no tree exists when its
  extractor reads, and it parses its own copy of the probed body. Player rows
  share a decoded payload between recognition and extraction through `memo`
  where both need it (LuluStream's unpacked source, `luluProbedSource`), and
  only for a matching probed page: a fresh or canonical page, or a read with
  a different Referer, decodes its own. `TestProbedReadersReuseTheParsedDocument`
  holds both paths to the same result with no client to fetch through
  (`BenchmarkProbedReaders` measures them). A read that must be fresh stays
  fresh: CoomerFans' video resolver re-reads the post at transfer time for
  a new signed link, and Chevereto's password unlock parses the answer to
  its POST, a different document from the gate. Before moving another
  reader over, check what it reads besides the tree: Niconico keeps
  metadata a 4xx body carries, Bandcamp reads a document fetched with
  another User-Agent, WordPress edits its private tree, and
  `Direct.pageSniff` must keep its content-type gate and fallback order.
- `knownSize` and `parsedSize` (`contract.go`): a stated byte count as
  `File.Size` wants it, -1 for anything that is not a length.
- `resolveOutcomes`, `expandSources`, `Mirrored`/`MirrorHosts`, above.
- `fixturePage`, `htmlPage`, `expectRecognised` and `expectShape`
  (`recognise_test.go`): recognition fixtures, checked from an unrelated host
  as well.

## Live tests

### Recorded parsing corpus

Keep copies of corpus pages, API responses and metadata-helper outputs in
the private `hl-priv` repository. Routine parsing checks replay those
responses through the actual registry and extractors, with the network
disabled and a missing recording reported as a failure. Reuse recordings
before making live requests; explicitly fill gaps or refresh them when a
site changes. The private audit runner preserves request identity and
redirects and can record responses as it checks them. Its README documents
the commands and refresh modes.

Parsing checks read only pages and metadata: no download manager, file
resolver, media body, segment or key fetch. Keep the saved real pages out of
this public repository; public unit tests use invented fixtures. A parsing
success verifies discovered metadata, not whether a deferred transfer would
succeed. Record unsupported, removed and inaccessible pages as negative
cases rather than silently discarding them from the corpus.

### Live transfer tests

Extractor tests that hit the real sites sit behind a build tag, because these
hosts change their plumbing without warning. They live in the private
[`hl-priv`](https://github.com/JohanLindvall/hl-priv) repository, at the paths
they run from, together with the URL corpora used to check routing and
extraction against the real sites; `internal/extractor/live_test.go` holds the
general one. Its `link.sh` symlinks them into this checkout, where
`*live_test.go` is gitignored. A new live test is written in `hl-priv` and
linked, never left as a loose untracked file here.

```bash
git clone git@github.com:JohanLindvall/hl-priv.git ../hl-priv && ../hl-priv/link.sh .
make test-live                                              # all hosts
cd backend && go test -tags live ./internal/extractor/ -run 'TestLiveExtract/gofile' -v
```

Run these after touching anything under `internal/extractor/`. They are the
fastest way to find out which host broke.

Record what they were answered. Every live test, investigation or fix
against live data — direct or through a proxy or leased route — saves its
responses in `hl-priv` (the standing rule in [AGENTS.md](../../AGENTS.md)),
so that the result can be replayed and verified later without the site:
pages, API answers, redirects and helper output, with the request identity
and the route kind that answered, but no cookies, credentials, account
tokens, proxy addresses or media bodies. Use the audit runner's recording
modes (`hl-priv/README.md`) rather than a copy kept by hand.

Two conventions worth keeping to when writing one:

- A check that only calls `Extract` proves the API answered, not that the file
  is reachable. Fetch a range of the first file as well and look at the
  content type — gofile serves the site's HTML shell when a storage server is
  busy, with a `Content-Length` that agrees, and that records as a complete
  download. A `206` also confirms the host honours ranges, so the segmented
  engine will work on it.
- Take the URL, and any password, from the environment rather than writing it
  into the file. A literal would not be committed either way, but a file free
  of it can be pasted into a report or handed to someone else without leaking
  anything.

## Host notes

| Notes | Hosts |
| --- | --- |
| [hosts/file-hosts.md](hosts/file-hosts.md) | bunkr, gofile, pixeldrain, mega, emload, filejump, filester, suvobox, pixhost, imagepond |
| [hosts/keep2share.md](hosts/keep2share.md) | keep2share (both hosts), voyeurking, and the CAPTCHA reader (`internal/captcha`) |
| [hosts/video-hosts.md](hosts/video-hosts.md) | the player rows (pornhub, xhamster, tnaflix, drtuber, pornone, vidmoly, streamtape, mixdrop, lulustream, vids.st, sendvid, doodstream, knuthing, noodlemagazine, ok.ru, eporner), Kernel Video Sharing; pointer pages (yandex, alohatube); the yt-dlp handoffs (youtube, odysee, dailymotion, bilibili, soundcloud, mixcloud, rumble) |
| [hosts/broadcasters.md](hosts/broadcasters.md) | vimeo, arte, orf, svtplay, yle, drtv, rtbf, francetv, radio-canada, niconico |
| [hosts/collections.md](hosts/collections.md) | balbums, coomerfans, celeb.st, cum.st, erome, rajce, bandcamp, bandzoogle |

A host not listed has no notes beyond its code.
