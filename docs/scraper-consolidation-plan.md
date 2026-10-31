# Scraper consolidation plan

[Documentation](README.md) · [Architecture](architecture.md) ·
[Extractor guide](agents/extractors.md)

Status: steps 1–3 implemented, with the extractor guide's choice table from
step 5; step 4 (discovery request admission) remains a separate follow-up,
and the rest of step 5 (file reorganisation) is optional and not started.
Reviewed on 2026-10-31 against
`52d5652ade12336e0efba53bbc76929985ff40f7`; the observations below describe
the code before the implementation.

## Recommendation

Consolidate the remaining copies of **shared responsibilities**: collecting
item outcomes, finishing collections, reading an already parsed document, and
carrying pagination completion between helpers. Keep host protocols in small,
explicit readers that compose those helpers.

Start with outcome accounting and document reuse. Both have concrete consumers
and reproduced gaps in the current code. Complete the pagination-to-folder
adapter next. Treat discovery request admission as a separate scheduling change
after measuring actual HTTP concurrency. File reorganization comes last.

The earlier consolidation is already present: Player and Share engines, common
pagination, ordered expansion, outcome summaries, immutable borrowed pages,
typed page-walk termination, renewable credentials, and the NPR/ABC listen
document-reuse pilots. These are the starting point, not work to rebuild.

## Existing code to build on

Paths below are relative to `backend/internal/extractor/` unless stated.

| Responsibility | Existing owner | What remains provider-specific |
| --- | --- | --- |
| Recognition, ordered dispatch and borrowed page data | [extractor.go](../backend/internal/extractor/extractor.go), [page.go](../backend/internal/extractor/page.go), [platform.go](../backend/internal/extractor/platform.go) | URL forms, fingerprints, target validation and bounded API questions |
| Ordinary player orchestration | [players.go](../backend/internal/extractor/players.go), [playersites.go](../backend/internal/extractor/playersites.go) | Page reader, canonical URL, referer, rendition candidates and signing behavior |
| Stateless share-page orchestration | [shares.go](../backend/internal/extractor/shares.go) | Ordered forms, recognition and extraction callback |
| External handoffs and pointer pages | [handoffs.go](../backend/internal/extractor/handoffs.go), [pointers.go](../backend/internal/extractor/pointers.go) | Metadata/probe requirements and destination discovery |
| Page and folder traversal | [pages.go](../backend/internal/extractor/pages.go), [listing.go](../backend/internal/extractor/listing.go), [tree.go](../backend/internal/extractor/tree.go) | Reading one page/folder, cursor updates, identity keys and stated end conditions |
| Ordered work and item accounting | [resolve.go](../backend/internal/extractor/resolve.go), [fanout.go](../backend/internal/extractor/fanout.go) | Reading one item and interpreting a provider's refusal |
| Collections and broadcaster series | [sources.go](../backend/internal/extractor/sources.go), [broadcast.go](../backend/internal/extractor/broadcast.go) | Provider schemas, season identity, stable naming and deliberate omission policy |
| Media payloads and refresh | [hls.go](../backend/internal/extractor/hls.go), `File.target`, `Target.file`, `refreshing`, `signedResult` | Selection policy, when to sign, and route-bound control hooks |
| HTML payloads, diagnostics and credentials | [html.go](../backend/internal/extractor/html.go), [notes.go](../backend/internal/extractor/notes.go), [renewable.go](../backend/internal/extractor/renewable.go) | Typed JSON schemas, display vocabulary, credential identity and refusal classification |

Keep these helpers in the existing package. Moving each host into a subpackage
would require exporting or relocating the helpers it already shares. Put truly
dependency-free utilities in `internal/util`, transport behavior in
`internal/httpx`, and tunable limits in `internal/config/limits.go`.

## Confirmed opportunities

The checkout contains 112 tracked production extractor Go files, totaling
42,113 physical lines, and 121 tracked Go test files. Those counts describe the
scope; reducing line count is not the objective.

Source inspection was supplemented with temporary Go test overlays using
synthetic fixtures. The probes ran with `-race`, made no live-site requests,
and left no instrumentation or tests in the checkout.

| Observation | Evidence | Implication |
| --- | --- | --- |
| An archive index with one readable thread and one HTTP 403 returns one file and an empty `Result.Note` | `foolFuukaArchive.index` / `expand` in [foolfuuka.go](../backend/internal/extractor/foolfuuka.go); reproduced through its local HTTP fixture | This path still uses the reduced `resolveFiles` result instead of retaining the full outcome summary |
| Expanding one good source and one failed source retains the first error, but the cap-only note is empty; `expansionNote` says only `1 of 2 sources resolved` | `expandSourcesNamed`, `partialNote`, `expansionNote` in [sources.go](../backend/internal/extractor/sources.go); reproduced | Source expansion has a second, less complete accounting/finalization path |
| A successful child with a file-limit note increments `expansion.partial`, while both shared note helpers can return an empty string | Same source-expansion probe; [wordpress.go](../backend/internal/extractor/wordpress.go) separately adds child-note counts | Child reports need a shared finishing policy; callers currently have to remember it independently |
| CoomerFans posts, PornPics galleries and an ordinary Chevereto album each parse the same document twice: once during recognition, once during extraction | Counter added to `parseHTML` only through an overlay; actual recognizers and extractors called with a supplied `Page` and nil HTTP clients | Three concrete adopters can use the existing `fetchRoot` contract |
| NPR and ABC listen each parse once across recognition and extraction on the same probe | Same counter probe, plus [docreuse_test.go](../backend/internal/extractor/docreuse_test.go) | The existing root-reader pattern is a working reference |
| Nested maps reach 36 active leaf callbacks for one job and 72 for two | `TestNestedListingsMultiplyTheirBound` in [resolve_test.go](../backend/internal/extractor/resolve_test.go), rerun successfully | A per-map bound is not an extraction-wide request bound; actual HTTP admission requires separate measurement |

These observations distinguish missing information from changed wording. The
link harvester already reports a resolved-source count; WordPress already
reports child notes. The proposed work should preserve those useful behaviors
while removing the need for each caller to assemble its own incomplete account.

## Work order

| Order | Work | First consumers | Risk and completion gate |
| --- | --- | --- | --- |
| 1 | Reuse complete outcome accounting and collection finalization | FoolFuuka index; source expansion used by Balbums and PornPics | Medium: intentional note changes; unchanged files, names, order and cap behavior |
| 2 | Reuse parsed documents | CoomerFans posts and PornPics galleries, then Chevereto | Low to medium: one parse per supplied HTML page, same results and request timing |
| 3 | Carry typed walk completion into folder traversal | Mediafire and Gofile folders, then Filester | Medium: exact current notes and termination behavior preserved |
| Throughout | Extend shared contract tests as consumers migrate | Existing recognition, document, source and media tests | Each migrated boundary exercised by at least two real consumers |
| 4, separate follow-up | Decide and implement discovery request admission | Nested registry/source expansion | High: measured HTTP bound, cancellation, fairness and no deadlock |
| 5, optional | Remove obsolete adapters and improve file organization | Central contracts and migrated helpers | Low: no behavior change, no new package/API boundary |

### 1. Finish adopting outcome summaries

**Current duplication.** `resolveListing` already returns `outcomeSummary`, and
`readListing` uses it to report unreadable items. FoolFuuka's index still goes
through `resolveFiles`, then finishes the result itself. Source expansion
collects another list of failures and reduces it through `sortOutcomes`.
Its callers combine caps, resolved counts and child notes in different ways.

Examples worth migrating after the pilots:

- [erome.go](../backend/internal/extractor/erome.go) and
  [pornpics.go](../backend/internal/extractor/pornpics.go) finish successful
  expansions with the walk and file-limit notes.
- [balbums.go](../backend/internal/extractor/balbums.go) uses `partialNote`,
  which intentionally reports only caps.
- [links.go](../backend/internal/extractor/links.go) adds a resolved count;
  WordPress additionally counts child reports.

**Implementation.**

1. Preserve `outcomeSummary` through the archive's expansion. Keep its special
   distinction between an entirely text-only board and threads that could not
   be read. Share the success-note construction without replacing that useful
   error with a generic one.
2. Use `summarise` for source-resolution outcomes instead of independently
   classifying failures. Keep source admission separate: successfully fetched
   sources excluded by a whole-album cap are not failed sources. Keep counts
   for discovered, attempted, admitted, empty, refused, failed and unrecognised
   sources distinct where they affect the result.
3. Consolidate expansion finishing around the existing note helpers. Keep
   `partialNote` narrowly about caps; give collection callers one composition
   point for cap information, outcomes and child reports. Supply vocabulary
   explicitly, with small family adapters where existing wording differs.
4. Migrate Balbums and PornPics, then the harvester, WordPress and Erome.
   Preserve WordPress's existing child-report wording without reporting it
   twice. Preserve Erome's `eromeFolders` hook and every existing directory.
5. Remove reduced-result adapters only after their production callers have
   migrated and their remaining test uses have been reviewed.

First make outcome retention a behavior-preserving change. Add the intended
note improvements in a separate commit with explicit expectations and guide
updates. No API, queue persistence or UI completeness field is needed here.
Do not infer a new machine-readable completeness state by inspecting note
text; the current child counter records nonempty notes, which needs a defined
policy before being treated as a stronger completeness guarantee.

**Acceptance.** Cover all-success, mixed failure, all-failed, only-empty,
explicit refusal, unrecognised navigation, partial children, cancellation,
duplicate titles, an oversized first album, and later albums excluded by caps.
Successful mixed results report what was omitted; cancellation remains an error.
Filenames, directories, source order, exact sizes and transfer hooks are
unchanged. Count admitted sources independently of speculative batch work.

### 2. Reuse the document and keep one parser per representation

**Current duplication.** Recognition calls the page's HTML reader, but
`CoomerFans.postFiles` then calls `coomerFansPost(string, base)`,
`PornPics.gallery` calls `pornpicsGallery(string, base)`, and
`cheveretoFetch` calls `parseHTML` after `fetchDocument`.

**Implementation.**

1. Give the CoomerFans and PornPics parsers root-based entry points, as
   `nprRead` and `abcListenRead` already do. Obtain the root through `fetchRoot`.
   Keep existing string entry points as tiny fixture adapters that parse once
   and delegate; there must be only one implementation of each parser.
2. Extend `docreuse_test.go` to these consumers. Check borrowed-root ownership,
   output parity and the absence of another document request. Keep the
   CoomerFans video resolver's fresh page read at transfer time: an earlier
   signed link must not become a permanent cached answer.
3. Convert Chevereto's ordinary GET path after checking which callers still
   need its raw body as well as its root. Its password-unlock POST returns a
   different representation and must parse that new response. Do not reuse
   the gate's DOM as the unlocked document.
4. Only if two actual consumers need body, root and final URL together, add
   one small page-reading helper in `page.go`. Otherwise use `fetchRoot` and
   preserve the exceptional raw-body path locally. Do not add another cache.
5. Consider further readers individually. `Direct.pageSniff` in
   [filelinks.go](../backend/internal/extractor/filelinks.go) is a candidate,
   but must retain its HTML content-type gate and fallback ordering.
   [celeb.go](../backend/internal/extractor/celeb.go) demonstrates per-Page
   memoization when both recognition and extraction need the decoded payload;
   memoize only when there are multiple consumers.

Keep JSON schemas typed and provider-owned. Shared `rawText`, `scriptTexts`,
`linkedData` and `jsonAttr` locate payloads without changing their whitespace;
they do not decide which payload describes the requested media.

**Acceptance.** The three reproduced ordinary-page cases go from two parses
to one across recognition and extraction. Measure allocations and time with
`-benchmem`; do not assume a percentage speedup. Preserve result metadata byte
for byte, redirects, canonical URL behavior, negative recognition and request
counts. Shared URLs, DOM nodes and memoized values remain unchanged under
concurrent extraction with `-race`.

Keep these exceptions explicit: Niconico reads useful metadata from HTTP error
bodies; Bandcamp asks for a different User-Agent representation; WordPress
edits a private tree; deferred resolvers need fresh metadata. None should be
forced into the initial migration.

### 3. Finish the typed pagination-to-folder boundary

**Current duplication.** `walkPagesFor` already returns `walked` with a
`walkStop`, and `walkEpisodes` consumes it. Folder readers still call the
string-returning `walkPages` adapter and pass its note to `treeWalk.partial`,
which strips `partialPrefix` before adding it again. This is remaining
coupling to display formatting, not a missing pagination engine.

**Implementation.**

1. Separate the non-generic completion information from the page items only
   as far as needed to pass it to a tree. Retain `walkPages` as a compatibility
   adapter for callers that only need items and a displayed note.
2. Migrate Mediafire's folder chunks in
   [filehosts.go](../backend/internal/extractor/filehosts.go) and Gofile's
   subfolder listing in [gofile.go](../backend/internal/extractor/gofile.go).
   Have the tree consume completion information directly and render through
   the shared note code.
3. Migrate [filester.go](../backend/internal/extractor/filester.go), preserving
   its page budget across the entire tree, per-folder cookie jars, password
   failures and room-based stopping. Preserve Gofile's separately read root
   and Mediafire's files-before-subfolders order.
4. Review `quiet` policies separately after parity. A repeated end page can
   be expected; exhausting a page budget or a later rate limit may mean
   truncation. Record each caller's intended behavior before changing it.

**Acceptance.** Preserve exact note strings in the refactor, including quiet
walks, prefix deduplication and provider notes. Test first-page error versus
later partial success, short pages, explicit end/total/page-count signals,
repeats, weighted limits, exact caps, folder cycles and cancellation. No host
gets a new page loop, and note text is not parsed to decide control flow.

### 4. Share discovery admission only after measuring the I/O

This is a separate follow-up. Reusing a worker-map helper has already reduced
code duplication, but nesting it multiplies its bound. The reproduced 36/72
baseline measures callbacks, not simultaneous HTTP requests or production load.

1. Build a synthetic nested-registry workload with a local HTTP server that
   records active requests, totals and cancellation. Include two root jobs,
   redirects, retries and two callers sharing a cached probe.
2. Define the unit and scope: the recommended first scope is active discovery
   requests within one top-level extraction, shared by its nested extractions.
   Two independent jobs may each use their own bound. A process-wide bound or
   per-origin fairness is a separate policy decision, not an accidental effect.
3. Put admission at the metadata I/O boundary, with its primitive below
   `extractor` in the dependency graph if `httpx` needs it. Acquire only while
   doing an actual request/body read. Do not hold a permit around a parent
   waiting for child extraction, a cache wait, or retry backoff; those designs
   can deadlock or consume all capacity without doing I/O.
4. Specify permit ownership for redirects, retry attempts, body close/error,
   shared in-flight fetches, helper subprocesses and cancellation. Ensure a
   transport wrapper cannot acquire twice for one active operation. Carry the
   same scope into nested registry calls and detached shared fetches.
5. Keep deferred resolution and media transfers under their existing policies.
   Do not reinterpret source/file caps as cumulative request budgets. Keep
   `SourceBatchRounds` as batch sizing, independent of request admission.

**Acceptance.** Observed discovery requests respect the chosen per-extraction
bound, complete without deadlock or starvation, and release admission on every
failure/cancellation path. Files, order, notes and request totals stay the same
apart from explicitly justified speculation changes. Measure latency as well
as peaks. Land this independently so it can be reverted without undoing the
parser and outcome consolidation.

### 5. Make the shared boundaries easier to maintain

After adoption, remove redundant wrappers and stale comments. If it improves
navigation, split `extractor.go` into files for transfer contracts,
registry/dispatch, options/identity and queue-steering errors, within the same
package. This is optional organization work, not a prerequisite or a substitute
for removing duplicated policy.

Keep local duplication local when its semantics are local. For example,
Niconico's `seriesPage` and `uploadsPage` resemble each other but decode
different envelopes and derive titles differently. They may share a small
provider-local conversion if that helps; they do not justify a universal JSON
endpoint DSL.

Update the extractor guide with a short choice table: Player row, Share row,
handoff/pointer, platform family, or dedicated stateful extractor. Make the
existing helper catalogue the place to look before adding another walker,
resolver, formatter or cache.

## Boundaries to preserve

| Tempting consolidation | Required boundary |
| --- | --- |
| Put every stateless reader into Share | `Share` recognizes only matching forms. Erome profiles and PornPics categories intentionally include recognizable pages whose shapes should not be harvested as navigation. Preserve the distinction. |
| Use one rendition comparator everywhere | Player ranking, Vimeo's HLS preference, PeerTube selection and `jwSource`'s first usable non-DASH source are different policies. Reuse primitives without changing their ordering. |
| Use one token cache for all state | `renewable` fits existing process-wide renewable credentials. Device/session pairs, per-post metadata, page memoization and address-bound tickets have different identities and invalidation rules. |
| Merge every folder/tree walk | `walkTree` reads nested folder APIs. Mega reconstructs a decrypted listing; Autoindex traverses directory pages by levels. Share compatible naming/limits without changing traversal. |
| Cache every response under its URL | Route, method, headers and session can change the representation. Borrowed page reuse does not authorize sharing a password-unlock response or a new signed result. |
| Simplify recognition to a hostname table | Dispatch remains ordered and content-based. Unknown installations, aliases, strict followed-link failures and the Direct fallback must keep working. |
| Convert files and targets with fresh struct literals | Use `File.target`, `Target.file` and HLS conversion helpers so segments, keys, audio and subtitles travel together. Pacing, cipher, checks, refusals, discard and refresh hooks retain their owners. |
| Normalize titles while moving helpers | Titles determine persisted paths. Preserve whitespace inside payloads, `sourceFolder`, duplicate-title disambiguation and the distinction between `Result.Title` and `Result.Note`. |

The relevant invariants are in the [extractor guide](agents/extractors.md),
[HTTP guide](agents/http.md), and host notes for
[collections](agents/hosts/collections.md), [file hosts](agents/hosts/file-hosts.md),
[players](agents/hosts/video-hosts.md) and
[broadcasters](agents/hosts/broadcasters.md).

## Tests and migration gates

Extend the existing tests as each responsibility moves. Prefer a small shared
fixture/request recorder or assertion helper once two tests need it; a new
scenario language or replay framework is unnecessary.

| Boundary | Existing starting point | Required additional checks |
| --- | --- | --- |
| Recognition and dispatch | `recognise_test.go`, `registry_test.go`, `shares_test.go`, `players_test.go` | Unrelated host, misleading branding, overlapping forms, redirects and strict navigation rejection |
| Page and payload ownership | `page_test.go`, `docreuse_test.go` | Three new root-reader consumers, full metadata parity, no extra request, shared data unchanged |
| Collection outcomes | `resolve_test.go`, `sources_test.go`, affected host tests | Mixed failures and partial children reach the final result; admitted, empty and unrecognised sources stay distinct |
| Traversal | `pages_test.go`, `tree_test.go`, folder-host tests | Typed completion composes without changing order, caps, error precedence or notes |
| Transfer handoff | `refresh_media_test.go`, player and affected resolver tests | Deferred timing, referer, route identity, every media field refreshed, obsolete tracks cleared, control hooks retained |
| Admission, if implemented | Nested-listing baseline plus local HTTP scenarios | Nested and concurrent jobs, shared fetches, retries, cancelled waiters and bounded active I/O |

Use synthetic names and reserved domains in public fixtures. Synthetic tests
may explicitly invoke resolvers against local servers. The private parsing
corpus must continue to exercise the real registry and extractors with no file
resolvers, transfers, media bodies or implicit network fallback.

For each implementation commit:

1. Read the applicable area/host guides and add the focused characterization
   cases before changing behavior. Migrate one small family at a time.
2. Run affected tests with `-race`; benchmark root-reader migrations with
   `-benchmem`. Check metadata, names, directories, note policy, request timing
   and counts, not merely the number of returned files.
3. Replay affected private recordings, then broaden replay when a shared
   helper changes. From the sibling private checkout:

   ```sh
   ./support/audit-corpus.sh ../HeapLeach
   ```

   Its default is offline replay; missing recordings fail. Its current
   baseline checks outcomes, extractor selection, counts and deferred/external
   entries, so synthetic tests must cover the fuller filename, track and
   header contracts. Preserve real pages, links, filenames and reports only
   in `hl-priv`. Refresh recordings explicitly when investigating site changes.
4. Run the extractor guide's private live tests for extractor code changes,
   plus affected resolver/transfer checks. Parsing replay alone does not
   establish that a deferred link can be downloaded.
5. Run the relevant repository checks, with `make check` as the broad gate.
   Useful backend checks, from `backend/`, are:

   ```sh
   go test ./internal/extractor ./internal/httpx ./internal/download -race
   go vet ./...
   go vet -tags live ./...
   ```

6. Update area guides in the same commit as the code; update user reference
   material when visible behavior changes. Regenerate the supported inventory
   with `make hosts` if registration/descriptions change. Commit small changes
   on `main` using generic behavior descriptions, following repository policy.

Keep refactors, deliberate note changes, scheduling changes and provider
protocol fixes in separate commits. Revert a problematic consumer migration
independently; retain an old adapter only during migration and remove it when
its replacement is established.

## Definition of done

- Each new production helper has at least two compatible real consumers and
  removes an independently implemented responsibility. Existing helpers can
  be adopted one reader at a time.
- The archive and collection pilots retain shared outcome accounting through
  result construction, with omissions and child reports handled deliberately.
- The three measured document readers use one parse of the supplied page,
  with unchanged metadata and no extra requests.
- The migrated folder readers consume typed walk completion; display wording
  is no longer an input to their orchestration.
- Host-specific recognition, naming, ordering, signing, media selection and
  route behavior remain covered. Any visible change is separately documented.
- Obsolete implementation paths are removed, guides describe the result, and
  relevant tests/replay pass. Admission is either independently validated or
  explicitly deferred; it does not block the core reuse work.

Stop expanding an abstraction when it needs a growing collection of host
switches, hides protocol decisions, or adds more adaptation code than shared
responsibility. Fewer independently maintained policies is the success measure;
a target percentage of deleted lines is not.

## Investigation limits

This is a task-directed review, not an exhaustive protocol audit. Structural
discovery used the codebase graph, call traces and source checks. Exact-path
coverage was checked for the evidence files; reported broadcaster parse gaps
were inspected directly. Graph counts for same-named Go methods were not used
as per-method impact measurements.

The focused ownership, document-reuse, outcome, pagination and nested-map tests
passed with the race detector, as did the temporary accounting and parse-count
probes described above. No live transfer, corpus refresh, complete private
replay or full `make check` was run for this documentation-only change. The
future migration gates above remain work for the implementation commits.
