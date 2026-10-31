# Scraper consolidation ledger

The migration record for [the consolidation plan](scraper-consolidation-plan.md):
one entry per changed caller or audited operation, with its disposition and
proof. It is not a source of registration data; the registry is. Private
replay details stay in `hl-priv`; this file names only what a public reader
needs to find them.

Dispositions: **adopt** (moved to the shared operation), **already shared**
(nothing to change), **retain** (an explicit exception with its reason), or
**fixed** (a demonstrated defect corrected under the plan's bug-fix gate).

## Baseline

| Item | Value |
| --- | --- |
| Starting commit | `1b0423b` (plan), code as of `efa21c7` |
| Ordinary suite | `go test ./internal/extractor ./internal/httpx ./internal/download ./internal/tools ./internal/captcha ./internal/util ./internal/config` passed |
| Live-tag vet | `go vet -tags live ./internal/extractor` passed with the private tests linked |
| Registry | 96 extractors plus the `direct` fallback; 97 catalogue entries; rows: 17 Player, 13 Share, 2 Pointer, 7 Handoff |
| Private replay | `CACHE_MODE=replay ./support/audit-corpus.sh` over 1332 URLs: 0 network requests, 0 cache misses, 0 resolvers or transfers started, every helper call a cache hit |
| Known replay failures | 27 outcomes differ from the private repository's stored parse baseline, which predates several earlier commits on this branch: 21 `unrecognised → extract_error` and 6 `parsed → extract_error`, all on the `direct` fallback. They are pre-existing, not introduced here; later replays are compared with this run's report rather than with the stored baseline |

## W1 — registration

| Caller | Disposition | Proof |
| --- | --- | --- |
| `NewRegistry` order | adopt: one explicit list in `registry.go`, the registry allocated first and handed to the extractors that resolve through it | Order and engine types identical before and after (dumped from both builds); `TestRegistryOrder` pins it independently in `rows_test.go` |
| `wirePointers` second pass | adopt: removed; `NewPointer(client, registry, name)` | `TestRegistryHandsItselfToWhatResolvesThroughIt` |
| `playerSites` duplicate order | adopt: removed | `TestEveryDeclaredRowIsRegistered` reads the package source for every declared row; a removed registration fails it |
| Row validation (Player, Share, Pointer, Handoff) | adopt: one validator per engine, run over the assembled registry in tests | `TestRegisteredRowsAreComplete`; no production construction failure added |
| Catalogue | already shared | `make hosts` leaves README unchanged; 97 entries |

## W2 — document acquisition

| Caller | Disposition | Proof |
| --- | --- | --- |
| `fetchDocument`, `readDocument`, `fetchRoot` | adopt: adapters over `acquire`; tree via `parseDocument` with its error | `TestAcquisitionReusesOnlyACompleteProbe`, `TestProbeAnswersOnlyItsOwnRequest` (now over all three adapters), passing on the old and new code; `BenchmarkProbedReaders` allocations identical |
| Registry probe, `platformJSON`, player canonical probe, media sniff | retain: distinct attempt, status and content-type policies | Unchanged |
| KVS member and search first page | retain: takes the probe by URL and route (`probed`) without the identity check, as before | Unchanged behaviour; carries the tree now (W3) |

## W3 — parsed-document reuse

| Caller | Disposition | Proof |
| --- | --- | --- |
| Bandzoogle first page | adopt: `bandzoogleRead` takes the probe's tree | `TestProbedReadersReuseTheParsedDocument`, `TestBandzoogleFollowsEveryListThatHasMore` (passes on the old code); allocations 1730 → 1033 |
| KVS browse to listing | adopt: `kvsListing.first` is a `document` | `TestKVSListingWalksTheFirstPageInHand` |
| Imgur bare media page | adopt: `imgurMediaIn`; string adapter only parses and delegates | `TestImgurSingleMediaReadsTheProbedTree` |
| france.tv page title | adopt: `francetvTitleOf` | Existing france.tv tests |
| Noodlemagazine watch page | adopt: `noodleRead` takes the tree | Existing tests |
| Cum media servers | adopt: decoded once per probed page (`cumProbedEdges`, `memo`) | `TestCumDecodesAProbedPagesServersOnce` |
| Erome album, VoyeurKing, Rajce | retain: recognisers scan the body only, so no tree exists to reuse | — |
| Niconico, WordPress, Bandcamp, password answers, feeds | retain: the plan's listed exceptions | — |

## W4 — complete reads and request construction

| Caller | Purpose | Disposition |
| --- | --- | --- |
| Niconico grant | cookie-setting POST, complete JSON | **fixed**: read with `httpx.ReadBody`; an answer past the limit was taken from its prefix. `TestNiconicoGrantIsReadWholeOrRefused` failed before the fix |
| `feedCharset` | rest of an already bounded feed | adopt `httpx.ReadBody`; unreachable difference, decoded text is at most twice the bytes read |
| Registry probe (`probePage`) | complete document with prefix kept for sniffing | already shared (`ReadBody`) |
| Media sniff (`mediaPageFetchURL`) | type check before body | already shared (`ReadBody`) |
| FoolFuuka remote HEAD, PBS HEAD | status-only probes | retain |
| WeTransfer short link | redirect follow, body drained | retain |
| AIA, challenge peek, `StatusError` sample, download streams | separate bounds | retain |
| Header names | `orf.go` literal `Authorization` → `httpx.HeaderAuthorization`; no other request-header literal outside `httpx` | adopt |
