# Scraper consolidation and code reuse plan

[Documentation](README.md) · [Architecture](architecture.md) ·
[Extractor guide](agents/extractors.md)

Investigation date: 2026-10-31. Source baseline:
`d27bb4c0f53c9bebc491ee1daa74454202aba0dd`.
Status: A, B, D and E implemented, with C's measurement; C's admission,
F's wider harness and G remain. See [Implementation status](#implementation-status).

The next consolidation should strengthen the shared extraction contracts and
finish adopting existing helpers. Much of the obvious consolidation is already
implemented. Replacing the remaining host implementations with a larger generic
scraper would hide meaningful differences in recognition, authentication,
rendition selection and download timing.

Start with shared-page ownership and incomplete-result reporting, then reuse
parsed documents. Address nested request concurrency as a separate behavior
change. Keep the existing Player, Share, broadcaster and platform boundaries.

## Implementation status

| ID | State | What landed |
| --- | --- | --- |
| A | Done | The page cache probes with a URL of its own; `probedDocument` and `fetchRoot` hand back copies of the final URL. The ownership rule is on `Page` and in the extractor guide; `TestSharedPageIsLeftAsItWas` runs concurrent extractions over one page under `-race`. |
| B | Done | `outcomeSummary` (`resolve.go`) keeps resolved, empty, refused, failed, unrecognised and not-started items apart. `readListing`, the Bandcamp discography and `expandSeries` note unreadable items ("N left out: could not be read"). The refusal type is now `itemRefusal`, beside the finishers, with unchanged wording. Clearing still judges transfers only; the download guide says so. |
| C | Measured | `TestNestedListingsMultiplyTheirBound` pins the baseline: 36 leaves in flight for one nested job, 72 for two. The batch factor is `config.SourceBatchRounds`. Extraction-wide admission is left for its own change. |
| D | Done for the pilots | NPR and ABC listen read the probe's tree (`nprRead`, `abcListenRead`, `nextDataIn`). `BenchmarkProbedReaders` recorded about a third fewer bytes and allocations per recognised read (NPR 54.8 kB/489 → 37.6 kB/323; ABC listen 22.8 kB/175 → 15.1 kB/116). Other readers are not yet migrated. |
| E | Done | `walkPagesFor` returns a `walkStop` reason; `pageWalk.render` gives the same note strings. `walkCut` and `pageFailed` are gone; `failed` hooks return whether they decided a note. The `quiet` audit and the tree adapter remain. |
| F | Partial | Each step added focused contract tests; no separate scenario harness was built. |
| G | Not started | Deferred until the remaining contracts settle. |

A pre-existing intermittent failure in the concurrent cancellation test was
fixed along the way: its callbacks now wait for the cancellation, so the
count of calls started after it no longer depends on scheduling.

The baseline below describes the code at the investigation's source
revision; names it cites, such as `broadcastRefusal`, have since changed.

## Investigation and evidence

The review followed registry dispatch through page probing, recognition,
listing expansion, media selection, deferred resolution and the download
manager. It examined shared helpers and representative file hosts, player rows,
broadcasters, collection readers, platform families and the generic fallback.
It also checked the corresponding area guides and existing regression tests.

Structural discovery used the codebase graph, followed by call traces and
source verification. Its indexed generation was `2026-10-31T14:35:31Z`.
Exact-path coverage checks were performed for the evidence files. Reported
parser gaps in broadcaster return expressions were inspected directly. Some
Go methods with the same name are combined in graph results, so graph fan-in
counts were not treated as reliable measurements of individual methods.
Private live tests are symlinks excluded from the graph.

This is a task-directed investigation, not a claim that every host protocol
or every line of the service was audited. No live extraction, live transfer
test or corpus refresh was performed. The private replay harness's documented
contract was inspected; the corpus itself was not replayed for this
documentation-only change.

### Measured baseline

| Measurement | Result | Method |
| --- | --- | --- |
| Tracked extractor production Go files | 112 files, 41,910 lines | `git ls-files`; physical lines including comments and blanks |
| Tracked extractor Go test files | 117 files, 38,834 lines | Same count; excludes ignored private live-test symlinks |
| Registered extractors | 96, plus the Direct fallback | Constructed `NewRegistry` with a zero configuration and nil client |
| Existing adapter families | 17 Players, 13 Shares, 7 Handoffs, 2 Pointers | Counted concrete types in that registry |
| Extractor and HTTP baseline tests | Passed with the race detector | `go test ./internal/extractor ./internal/httpx -race`, from `backend/` |

Line counts describe scope; reducing them is not an acceptance criterion.

Three small synthetic probes exercised the actual implementation through a
temporary Go test overlay. They used invented data, made no live requests,
and left no test file in the checkout. All three probes passed, confirming
these observations rather than asserting the proposed future behavior:

| Probe | Observed result | What it establishes |
| --- | --- | --- |
| `readListing` with one successful item and one ordinary error | One file, empty note, nil error | A mixed listing can lose the indication that an item failed |
| Six outer `resolveOutcomes` callbacks, each starting six inner callbacks | 36 simultaneously active leaf callbacks | The limit of six applies to each map invocation, not to the whole extraction |
| `rajceExtract` reading a successful `Page` supplied through `withPage`, with a fragment on its final URL | The cached final URL's fragment was cleared | A consumer mutates a URL borrowed from the shared page |

The concurrency probe measures callbacks, not live HTTP concurrency. The URL
probe demonstrates mutation, not a reproduced data race. The race-enabled
baseline passing does not establish that those compositions are safe.

## What is already consolidated

| Responsibility | Existing implementation | Direction |
| --- | --- | --- |
| Page recognition and dispatch | [`Registry`](../backend/internal/extractor/extractor.go#L489), [`Page`](../backend/internal/extractor/page.go#L33), [`platform.go`](../backend/internal/extractor/platform.go) | Preserve ordered, content-based recognition and route-scoped probing |
| Ordinary video players | [`Player`](../backend/internal/extractor/players.go), [`playersites.go`](../backend/internal/extractor/playersites.go) | Add matching sites as rows; reuse signed, withheld and peeked resolution |
| Stateless share pages | [`Share`](../backend/internal/extractor/shares.go) | Reuse forms and extraction callbacks where recognition fits the contract |
| External tools and pointer pages | [`Handoff`](../backend/internal/extractor/handoffs.go), [`Pointer`](../backend/internal/extractor/pointers.go) | Keep one engine per behavior family |
| Pagination | [`walkPages`](../backend/internal/extractor/pages.go#L85), [`walkListing`](../backend/internal/extractor/listing.go#L95), [`walkEpisodes`](../backend/internal/extractor/broadcast.go) | Extend the shared outcome contract; do not add another page walker |
| Ordered concurrent expansion | [`resolveOutcomes`](../backend/internal/extractor/resolve.go#L34), [`FanOut`](../backend/internal/extractor/fanout.go#L35) | Preserve order and cancellation; improve composition and result accounting |
| Sources and folder trees | [`sources.go`](../backend/internal/extractor/sources.go), [`tree.go`](../backend/internal/extractor/tree.go) | Preserve whole-source caps, stable folders and traversal limits |
| Broadcaster listings | [`broadcast.go`](../backend/internal/extractor/broadcast.go) | Keep provider schemas and selection policies outside the common machinery |
| Playlist and refresh payloads | [`hls.go`](../backend/internal/extractor/hls.go), [`File.target` / `Target.file`](../backend/internal/extractor/extractor.go#L246), [`hooks.go`](../backend/internal/extractor/hooks.go) | Keep every key, audio track, subtitle and control hook intact |
| Expiring credentials and route waits | [`renewable.go`](../backend/internal/extractor/renewable.go), [`routewaits.go`](../backend/internal/extractor/routewaits.go) | Reuse matching lifetimes; preserve provider-specific refusal rules |
| HTTP, notes and raw payloads | [`httpx/client.go`](../backend/internal/httpx/client.go), [`notes.go`](../backend/internal/extractor/notes.go), [`html.go`](../backend/internal/extractor/html.go#L246) | Keep retry, diagnostic wording and payload handling centralized |

There is no missing universal pagination engine, token cache, HLS pipeline or
player abstraction to build from scratch. In particular, the current code
already shares cancellation handling, tree traversal, signed-file conversion,
raw script extraction and note formatting.

## Ranked work

Effort estimates are rough engineering time, including focused tests and guide
updates, assuming the relevant recordings exist. They are not commitments.
P1 is the first implementation tranche; P2 follows once its contracts are
stable; P3 is optional maintenance.

| ID | Priority | Change | Expected value | Effort / risk |
| --- | --- | --- | --- | --- |
| A | P1 | Establish immutable borrowed page data | Make existing and expanded cache reuse safe | 1–2 days / low to medium |
| B | P1 | Preserve typed item outcomes through listing finalization | One fix improves incomplete-result reporting across scraper families | 2–4 days / medium; visible note changes |
| C | P1 investigation, P2 rollout | Share metadata request admission across nested extraction | Prevent locally bounded helpers from multiplying load | 3–5 days / high; scheduling changes |
| D | P2 | Finish DOM and decoded-payload reuse | Remove repeated parsing and redundant parser paths | 1–3 days / medium; naming and ownership risks |
| E | P2 | Return structured pagination completion | Separate traversal decisions from displayed note strings | 2–4 days / medium; many callers |
| F | P2 | Reuse a focused extraction-contract test harness | Verify family composition and request timing consistently | 2–3 days, spread across A–E / low |
| G | P3 | Split the central contract file by responsibility | Make the existing reuse boundaries easier to find | About 1 day / low; navigation-only |

### A. Make borrowed page data immutable before widening reuse

**Evidence.** [`pageCache.get`](../backend/internal/extractor/page.go#L120)
returns a shared `*Page`. `probedDocument` and `fetchDocument` return its
`Final` pointer, and `fetchRoot` may return its cached HTML tree.
[`rajceExtract`](../backend/internal/extractor/rajce.go#L62) clears the
fragment on that borrowed URL. The synthetic probe confirmed the mutation.
The same cache intentionally serves concurrent callers.

**Plan.**

1. State an ownership rule: `Page` URLs, headers, DOM and memoized values are
   borrowed and immutable. A consumer copies the part it needs to modify.
2. Copy the final URL before referer normalization in `rajceExtract`; check
   the other consumers of `fetchDocument` and `fetchRoot` for writes to
   borrowed data. A small URL-copy convention is preferable to cloning the
   entire document on every read.
3. Keep a private tree for readers that edit nodes. In particular,
   [`wordPressPost`](../backend/internal/extractor/wordpress.go#L350) removes
   related-content nodes from the tree produced by `wordPressFetch`. Its
   current private parse must not be replaced with the shared root without
   first copying the relevant subtree or making filtering non-mutating.
4. Keep representation identity explicit. `fetchDocument` reuses the
   submitted probe; it is not a general cache of arbitrary header or session
   variants. `pageJSON` currently keys by endpoint within a Page. Any broader
   response cache must distinguish route, method, body and relevant
   representation/session identity before it is introduced.

**Acceptance.** Two extractions sharing a Page leave its original and final
URLs, DOM and decoded values unchanged. Concurrent variants pass `-race`.
Redirect referers and source matching remain identical. This should land
before D; it is useful even if D is deferred.

### B. Keep item outcomes until the result is finalized

**Evidence.** [`sortOutcomes`](../backend/internal/extractor/resolve.go#L60)
retains refusal strings and only the first ordinary failure.
[`readListing`](../backend/internal/extractor/resolve.go#L115) checks that
something succeeded, then returns only `refusedNote(refusals)`. Consequently,
one successful item plus one ordinary failure produces an empty note.
`PeerTube` and `CoomerFans` use this finalizer; the discography path in
[`bandcamp.go`](../backend/internal/extractor/bandcamp.go#L131) independently
finishes with the same refusal-only note pattern. `expandSeries` also receives
the first failure but does not include ordinary failures in a successful
series's note.

The outcome classification is already shared, but the information retained
and presented is incomplete. Also, the generic resolver's refusal check is
coupled to `broadcastRefusal`, a type declared in `broadcast.go` and already
used by a non-broadcaster platform in `peertube.go`.

**Plan.**

1. Introduce a small internal outcome summary over `outcome[T, R]`: attempted,
   succeeded, empty, deliberately refused, failed and not started. Count
   items separately from files; one item may yield many files or none.
2. Retain ordered refusal reasons and the first diagnostic error. Give the
   shared refusal contract a neutral home/name without changing its public
   wording. Preserve `errors.Is`/`errors.As` behavior.
3. Let `readListing`, series expansion and source expansion use that summary.
   Supply caller vocabulary and any deliberate omission policy explicitly.
   A failed metadata request must not be indistinguishable from an empty
   post or an intentionally excluded live stream.
4. Render concise notes in `notes.go`, such as an unreadable-item count, while
   continuing to return usable files. Cancellation and all-failed behavior
   retain their current precedence. Navigation rejected as `errUnrecognised`
   remains outside the failure count.
5. Migrate `readListing` consumers first, then the manual finishers. Do not
   replace them all with an elaborate generic result builder.

**Acceptance.** Pin success plus network failure, success plus parser failure,
success plus refusal, empty items, all failures and cancellation after partial
success. Preserve item order, folder names, successful file metadata and
whole-source cap behavior. Notes change only where specified by the new
omission policy.

**Service implication.** Extraction completeness and transfer completion are
currently different facts, but only the latter is structured downstream.
[`applyResultLocked`](../backend/internal/download/manager.go#L445) folds
`Result.Note` into the displayed title. [`Job.complete`](../backend/internal/download/types.go#L480)
and [`isComplete`](../frontend/src/status.ts#L23) inspect transfer status,
counts and source errors, not extraction completeness. Merely adding a note
will not change whether a successfully transferred partial listing can be
cleared. The guides' references to partial listings should be reconciled with
that implementation.

Make any change to clearing/retry behavior a separate follow-up: carry an
explicit extraction-completeness field through persistence and snapshots,
define how intentional caps differ from unexpected failures, and render/use
it in the UI and CLI. Do not infer this state from note text, or add an unused
wire field as part of the internal refactor.

### C. Bound metadata requests across nested expansion

**Evidence.** [`Manager.extractSource`](../backend/internal/download/manager.go#L362)
limits active top-level source resolutions to four. Each `FanOut` invocation
independently starts up to six workers; `resolveOutcomes` delegates to it.
`expandSourcesNamed` can recursively enter the registry, and WordPress
category/post expansion can nest those operations. Limits are inherited as
values, not as a shared request-admission object. The synthetic nested-map
probe reached 36 simultaneous leaf callbacks.

The HTTP client's idle connection settings and retry logic do not make the
per-map worker bound an extraction-wide request bound. Existing source,
file and recursion caps still constrain work; this finding does not mean
the service has no limits.

**Plan.**

1. Add a synthetic nested-listing workload that measures active metadata
   requests, total requests and work started after a cap/cancellation. Include
   several simultaneous top-level jobs and multiple origins.
2. Introduce one extraction request-admission mechanism shared by descendants.
   Separate the per-root bound from any per-origin bound shared across roots.
   Put new tunables in `config/limits.go`; choose values from these measurements.
3. Apply admission around actual metadata I/O, including response consumption,
   rather than around a parent callback that waits for child work. Holding
   every permit in parents while children need the same permits can deadlock.
4. Preserve context propagation through routed clients, redirects and retries.
   Release permits on body close, errors and cancellation. Reacquire for retry
   attempts; backoff should not occupy a request slot. Do not add an upward
   dependency from `httpx` into `extractor` or `download`.
5. Keep media transfers and deferred transfer resolvers under their existing
   worker/route policies. Define their admission explicitly before sharing an
   extraction-only context with them.

Source/file caps express which results to return; admission controls how much
I/O overlaps. Do not silently reinterpret the user's source cap as a new
cumulative request budget. Similarly, retain bounded speculative batches and
whole-album admission in `expandSourcesNamed` unless a separate experiment
justifies changing them. Its current batch is `4 * PageFetchConcurrency`;
that independent tuning factor belongs in `limits.go` if it remains tunable.

**Acceptance.** Nested maps and concurrent jobs respect the chosen request
bound, finish without starvation or deadlock, stop promptly on cancellation,
and preserve output ordering and cap notes. Report peak requests and request
counts before/after on the same synthetic workload; claim no live performance
improvement without measurements.

### D. Reuse the parsed document and the host's decoded payload

**Evidence.** `Page.index` already parses HTML once, and `fetchRoot` can return
that tree. However, [`NPR.Recognise`](../backend/internal/extractor/npr.go#L108)
uses `Page.Named`, while extraction takes the body through `fetchDocument`
and `nprResult` parses it again. [`ABCListen.Recognise`](../backend/internal/extractor/abclisten.go#L114)
also uses the page index, then `abcListenParse` reparses the body for its
title. The watch-page path in [`niconico.go`](../backend/internal/extractor/niconico.go#L269)
has another string-to-DOM path, with an important exception for metadata
embedded in an HTTP error body.

[`celebProbedData`](../backend/internal/extractor/celeb.go#L195) is the useful
existing example: one host-specific decoder, a root-based entry point, and
per-Page memoization when recognition and extraction need the same payload.

**Plan.**

1. Pilot root-based parser entry points in NPR and ABCListen. Keep their
   existing string entry points as small synthetic-fixture adapters.
2. Reuse `fetchRoot`, `rawText`, `scriptTexts`, `linkedData` and `jsonAttr`.
   Where a parser also needs raw JavaScript, pass the body alongside the root
   rather than serialize and parse the DOM again.
3. Memoize a decoded host payload only when multiple stages need it. Reuse
   Celeb's immutable-value pattern rather than adding a second cache. Cache
   only on the Page actually read; a changed canonical URL needs its own data.
4. Extend to other readers after verifying their error-body, redirect,
   whitespace and representation behavior. Preserve Niconico's useful 4xx
   metadata, Bandcamp's alternate User-Agent document, and WordPress's
   deliberately private editable subtree.

Do not replace typed provider schemas with `map[string]any` or turn all JSON
islands into one schema. Payload location and raw-text handling are reusable;
field meanings, required fields and target validation belong to each parser.

**Acceptance.** Benchmark representative synthetic documents with `-benchmem`
and record parse/allocation reductions. No extra requests; byte-identical
titles, names and directory components; unchanged negative recognition and
canonical-URL behavior. Preserve non-breaking spaces and repeated whitespace
inside JSON strings. No speedup estimate is assumed in advance.

### E. Separate pagination state from note wording

**Evidence.** `walkPages` returns items, a note string and an error.
`walkEpisodes` communicates a cap through the `walkCut` string sentinel;
`pageWalk.failed` can return the `pageFailed` sentinel to request default
handling. `treeWalk.partial` strips a display prefix from another walk's
note. These adapters are translating presentation text back into state.

`quiet` also suppresses several different termination notes, including the
page cap. That is documented current behavior, not something to change
incidentally while modifying the return type.

**Plan.**

1. Introduce a small internal walk result containing entries, stop reason,
   available counts and an optional provider note. Start with explicit
   normal-end, item-limit, page-limit, repeated-page and later-fetch-failure
   reasons. Avoid a general workflow/state-machine framework.
2. Keep thin compatibility adapters that produce today's exact strings.
   Replace `walkCut` and `pageFailed` with typed decisions; let the episode
   adapter derive `more` and the tree adapter propagate a reason directly.
3. Audit `quiet` callers individually after parity is established. Distinguish
   a site's expected end-of-list response from an unexpected truncation and
   document any resulting note changes separately.
4. Use B's outcome summaries at the listing finalizer. Page completion and
   individual item failures remain distinct inputs to one displayed note.

**Acceptance.** Preserve first-page error versus later-page partial success,
short-page continuation, explicit totals/page counts, duplicate suppression,
weighted limits, exact-cap behavior and cancellation precedence. Every
behavior-preserving migration must produce the same files and note strings
before any deliberate policy change.

### F. Reuse contract tests at family boundaries

Existing coverage is substantial: [`recognise_test.go`](../backend/internal/extractor/recognise_test.go),
[`players_test.go`](../backend/internal/extractor/players_test.go),
[`shares_test.go`](../backend/internal/extractor/shares_test.go),
[`resolve_test.go`](../backend/internal/extractor/resolve_test.go), and
[`refresh_media_test.go`](../backend/internal/extractor/refresh_media_test.go)
already protect important shared behavior. Extend those facilities instead of
building a competing replay framework.

Add a compact, opt-in synthetic scenario harness for the cross-stage contracts:
submitted and redirected page, recorded test requests, expected selection,
result metadata, and explicit resolver invocations. Assert stable outputs and
request behavior, not the helper call sequence used to implement them.

Cover these cases across the relevant engine families:

- Recognition on an unrelated host, a misleading page with the right name,
  overlapping forms, redirects and strict link-following rejection.
- No duplicate document request, immutable shared data, and bounded optional
  recognition requests even when the response is a challenge or failure.
- Listings with item failures, repeated/short pages, duplicate titles,
  nested expansion, exact caps and cancellation.
- Signed versus withheld versus peeked files: when metadata is read, when
  signing happens, and which page supplies the referer.
- Refresh of segments, keys, audio and subtitles together, including clearing
  stale tracks, while preserving pacing, cipher and transfer control hooks.

Use local synthetic fixtures for tests that invoke `Resolve` or exercise
transfers. The private parsing replay deliberately does neither. Its current
baseline checks selection, outcomes, counts and deferred/external entries;
full filename, directory, track and header parity needs synthetic assertions
or an explicitly extended private comparison.

### G. Make the existing boundaries easier to navigate

`extractor.go` combines transfer payloads, options, error types, registration,
recognition dispatch and small adapters. Once A–E settle, split these into
coherent files within the same package: for example, transfer contracts,
registry/dispatch, identity/options and errors. These are suggested file
responsibilities, not new public APIs.

Keep host parsers in their own files and keep the common helpers unexported.
Moving every host into a subpackage now would force a broad export surface
or dependency inversion merely to regain helpers the current package shares.
Defer such a split until a measured build, ownership or isolation problem
justifies it. Update the area guides and source links with any file moves.

## Boundaries that should remain explicit

The inspected implementations do not justify another broad host-to-row
conversion. Keep adopting Player and Share for compatible new sites, and
consolidate the remaining implementations through the smaller contracts above.

| Tempting consolidation | Why it should not be part of this plan |
| --- | --- |
| Convert every stateless extractor to Share | Share recognizes only its forms. Erome profile pages and PornPics listings intentionally distinguish recognizability from forms worth harvesting. Widening Shape would follow navigation; narrowing recognition would lose supported pages. |
| Convert every video extractor to Player | Vimeo prefers HLS, falls back to progressive and preserves the chosen kind on refresh. PeerTube ranks video presence and progressive candidates using richer metadata. The Player comparator is not their policy. |
| Give every rendition one universal ranking | `bestCandidate`, `peerTubeBest`, `jwPlayerBest` and HLS `bestVariant` have different rules. `jwSource` deliberately takes the first usable non-DASH source. Reuse already-shared primitives such as `rankIn` and `cmpBool`; preserve each ordering. |
| Replace all credential/session state with renewable | Gofile, RedGifs and VRT already use it. RTBF keeps a device/session pair; ORF discovers API credentials with no equivalent TTL; per-post metadata and address-bound tickets have other identities. Broader adoption needs matching invalidation semantics, not just similar mutexes. |
| Wrap every authenticated request in one retry helper | RedGifs renews on 401; Gofile recognizes envelope/token errors and currently renews at the root; VRT's refusal handling differs. Keep classification local. A small one-renewal runner is worth considering only when two callers can adopt it without new policy switches or changed scope. |
| Merge all tree traversal | `walkTree` reads nested folder APIs. Mega reconstructs a tree from one decrypted listing, and Autoindex crawls directory pages with different traversal and bounds. Reuse naming/limits where semantics match. |
| Route by domain for speed | Content-based recognition is an enforced contract. Registry order and platform fingerprints let unknown installations and domain changes work. Optimize parsing and bounded probes first. |

These distinctions are supported by the current
[extractor guide](agents/extractors.md), [file-host notes](agents/hosts/file-hosts.md),
[collection notes](agents/hosts/collections.md), [broadcaster notes](agents/hosts/broadcasters.md),
and the selection logic in [`vimeo.go`](../backend/internal/extractor/vimeo.go),
[`peertube.go`](../backend/internal/extractor/peertube.go),
[`renditions.go`](../backend/internal/extractor/renditions.go) and
[`jsdecode.go`](../backend/internal/extractor/jsdecode.go).

## Implementation sequence and decision gates

Each step is a separately reviewable commit on the repository's normal main
workflow. Keep behavior-preserving refactors separate from changed omission,
admission or completion policies. Do not combine host protocol updates with
helper migrations unless they are necessary for the same regression.

| Step | Work | Gate before continuing |
| --- | --- | --- |
| 1 | A: fix borrowed-URL mutation, document ownership, add the focused regression | Shared Page remains unchanged; race checks pass |
| 2 | B: introduce outcome accounting; migrate `readListing` and two real consumers | Mixed failures are visible, ordering/naming preserved, all-failed and cancellation errors unchanged |
| 3 | D: migrate NPR and ABCListen to root-based readers using A's contract | Recorded parse/allocation reduction and full metadata parity; expand only if useful |
| 4 | C: measure nested request fan-out, then implement admission separately | Measured bounds under nested/multiple jobs, no deadlock, no request leaks or ordering changes |
| 5 | E: introduce typed walk completion and compatibility adapters | Existing walker behavior and note strings preserved; review quiet-policy changes separately |
| 6 | Extend B/D/E to proven consumers; apply F throughout | Every new abstraction has actual adopters and removes a duplicated responsibility |
| 7 | Decide whether structured extraction completeness should affect clearing/retry; consider G | Separate service-contract decision and migration tests, or explicit deferral |

Stop a proposed abstraction when it needs many host flags, obscures a provider
protocol, changes established names, or saves less code than the adaptation
layer adds. Two compatible consumers are the minimum evidence for a new
production helper. Existing helpers can be adopted by one reader at a time.

## Validation for implementation

For each code change, first run the focused contract and affected-host tests.
Then run the applicable repository checks; the broad gate remains `make check`.
For extractor/shared-HTTP changes, the useful backend commands are:

```sh
# From backend/
go test ./internal/extractor ./internal/httpx ./internal/download -race
go vet ./...
go vet -tags live ./...
```

For changed parser/recognition paths, replay the affected cases through the
existing private corpus runner, then broaden replay for changes to shared
helpers. Run from the sibling private checkout:

```sh
cd ../hl-priv
./support/audit-corpus.sh ../HeapLeach
```

Its default is offline replay and missing recordings must fail. Keep default
replay for routine checks; explicitly fetch missing responses or refresh only
when investigating a site change. Preserve request identity and redirects.
Keep recordings, real links, filenames and per-case reports in that private
repository. Keep public fixtures synthetic.

Parser replay does not validate deferred signing or media transfer. Follow
the extractor guide's private live-test requirement for extractor code changes,
and use affected transfer tests when changing resolver behavior. Do not turn
the parsing audit into a media downloader. UI/API/persistence changes in the
optional completeness follow-up need their matching area guides and frontend
tests, using Docker on this machine.

Success means fewer independently implemented policies with the same supported
recognition, output order, folder layout and transfer behavior, plus explicitly
reviewed improvements to omission reporting and request bounds. It does not
mean fewer supported extractors, a larger configuration language, or a target
percentage reduction in source lines.
