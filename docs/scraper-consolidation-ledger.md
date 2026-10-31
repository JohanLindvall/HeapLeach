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

## W6 — ordered outcomes

| Caller | Disposition | Proof |
| --- | --- | --- |
| `FanOut` | removed; its scheduling moved into `resolveOutcomes`, which keeps every started item's index, value and error and leaves an unstarted one absent | `TestResolveOutcomesKeepsEveryOutcomeInOrder` (the old goroutine-bound and order test, now with errors kept), `TestResolveOutcomesStopsAtCancellation`, `TestResolveOutcomesIsBounded`, under `-race` |
| `resolveOutcomes` concurrent mode | adopt: no singleton slices or disguised errors | Same tests |
| `Autoindex.readAll` | adopt: explicit outcomes; breadth-first levels and in-place node filling unchanged; a read cut short by cancellation is not a failure | `TestAutoindexDoesNotCountACancelledReadAsUnreadable`, existing crawl tests |
| `resolveListing`, `resolveEach`, `readSeasons`, source expansion | already shared (`resolveOutcomes`) | Existing tests |
| Private live test using `FanOut` | updated in `hl-priv` to `resolveOutcomes` | `go vet -tags live` |

## W10 — tunables and headers

| Constant | Classification | Disposition |
| --- | --- | --- |
| `gelbooru01Pace` | application wait | moved: `config.Gelbooru01Pace` |
| `gofileRejectionCooldown` | application cooldown | moved: `config.GofileRejectionCooldown` |
| `pornpicsPageSize` | request size chosen below the route's clamp; also the block offset step, which moves with it | moved: `config.PornPicsPageSize` |
| `orfPageSize`, `redgifsPageSize` | request sizes asked for | moved: `config.ORFPageSize`, `config.RedGifsPageSize` |
| `gelbooru01PageSize` | protocol: the board's fixed page, the unit `pid` counts in | retain |
| `booruPageSize`, `civitaiPageSize`, `niconicoPageSize`, `mediaWikiPageSize`, `bitchutePageSize`, `rtbfPage`, `abcListenPageSize`, fediverse strides, `balbumsPerPage` | protocol: the server's documented ceiling | retain |
| `gofileSlot` | protocol: the signing window | retain |

Values are unchanged; request sequences are those the existing host tests
pin (offsets, limits, page numbers). No flag or environment setting was added.

### Naming and size helpers

| Site of use | Disposition |
| --- | --- |
| RedGifs file name (`path.Ext(util.NameFromURL(link))`) | adopt `linkExtAsIs(link, "")`, its exact equivalent; `TestRedgifsItemFile` |
| `fediExt` (drops an "extension" longer than six), `bitchuteExt` (letter rule, `.mp4` fallback), media-page extension check (lower case, set lookup) | retain: each rule differs from `linkExt`/`linkExtAsIs` |
| Autoindex size column (`0` and listed numbers kept as given) | retain: `parsedSize` maps zero to unknown |
| Bunkr listing size (`parseHumanSize`, approximate) | already shared |
| `util.Unescape`, `util.TitleName`, `sourceFolder` | already shared; no inline copies (`feedSlashes` maps to `-`, a different rule kept for feed names) |

## W5 — collection accounting

| Caller | Disposition |
| --- | --- |
| Source expansion finishers (balbums, links, VoyeurKing, Erome, PornPics, Chevereto, WordPress) | already shared: each checks cancellation first, through `expansion.unresolved` (`noneResolved`) or an explicit `ctx.Err()` |
| `expandSourcesNamed` partial count (`r.res.Note != ""`) | retain: counts a child that said anything, not why; a structured partial flag would add a field to the exported `Result` |
| Cum creator note (`note == ""` before `cappedNote`) | retain: composition of its own notes, in its existing order (an invalid-posts note suppresses the capped one) |
| RTP capped season title (`more || walkNote != ""`) | retain: the plan's named legacy exception; the folder name it produces is part of resume behaviour. A request-sequence characterization needs endpoint injection RTP does not have, so it is not pinned here |
| Bandzoogle continuation | already shared (`walkPages`); now pinned by `TestBandzoogleFollowsEveryListThatHasMore` |

## W7 — resolver construction

| Item | Disposition |
| --- | --- |
| `File.target`, `Target.file`, `hlsMedia.target` | already shared: no hand-built partial copy of an existing `File`/`Target` remains; other `Target` literals are built from host API data |
| Transfer-time context (`refreshing`, `Player` deferred resolve) | adopt: one `transferRead`; identical composition. `TestTransferReadDropsOnlyThePageAndAdmission` |
| Bunkr albums, lazy broadcaster episodes, Niconico lists, Keep2Share tickets, `cumPostLoader`, `Mirrored` | retain: their lifecycles differ (plan W7 table); no unconditional fresh read or invalidation added |
| `refreshFile` identity and downloader partial-update contract | unchanged; existing `refresh_media_test.go`, `download/refresh_tracks_test.go` |

## W8 — media selection

| Item | Disposition |
| --- | --- |
| `peerTubeBest` | retain: video presence ranks first and "progressive" is provenance, not `IsHLS`; folding it into `bestCandidate` would need a switch or a false `IsHLS` |
| `jwSource`, `jwPlayerBest` | retain: first usable source versus a ranked feed (plan W8.4) |
| Broadcaster file tails | already shared (`broadcastFile`, `progressiveFile`), adopted where the tails agree before this work |
| BitChute playlist test (case-insensitive `.m3u8` in the link) | adopt `isPlaylistLink`, its exact equivalent |
| `renditions.go`, KVS, RTP `.m3u8` checks (case-sensitive), knuthing (path extension), Noodlemagazine and media-page checks (declared type as well) | retain: inputs or case rules differ from `isPlaylistLink` |
| Generic fallback confidence order | unchanged |

## W9 — state scopes

| State | Disposition |
| --- | --- |
| `Page.memo` network questions (`pageJSON`, player canonical probe, Bandcamp own-agent read) | **fixed**: an answer computed after the asker's context ended was kept on the shared page, so another job reading the same URL within the cache's lifetime got "no answer" and could miss a recognition. Requirement: the page cache keeps no failure (extractors guide, "Fetching a page once"); a cancellation is the caller's, not the page's. `memoAsked` drops such an answer and a waiter asks again with its own context; real failures and the probe's own timeouts are still kept. `TestPageAnswerAskedByACancelledCallerIsNotKept` failed before the fix |
| `Page.memo` decodes (Cum servers, Celeb props, LuluStream source) | retain plain `memo`: no network, so no cancellation |
| `renewable[T]` (Gofile account, RedGifs token, VRT token) | retain: its cancelable waiter, detached shared fetch and refused-generation renewal are its own; `renewable_test.go` |
| `hostMemo` | retain: success-only, extractor lifetime; concurrent misses may each compute, as documented |
| `cumPostLoader` | retain: one post's metadata, read under its lock and checked for cancellation once the lock is held; a failed read is not kept |
| ORF configuration, RTBF device session, Filester protected-folder jar, Keep2Share tickets, `routeWaits` | retain: scopes and invalidation differ (plan W9 table); none shares a key, lifetime and invalidation rule with another |
| Gofile signing secret (`gofilewt.go`) and guest account | retain: different state, kept apart |

No universal cache interface or background refresh was introduced.

## W11 — files and adapters

| Item | Disposition |
| --- | --- |
| `extractor.go` | split into `contract.go`, `errors.go`, `registry.go` with no symbol changes; `extractor.go` keeps `Result`, the interfaces, `Options`/`Limits` and `about` |
| `fanout.go`, `wirePointers`, `playerSites`, `francetvPageTitle` | removed: no remaining caller in this repository or the private overlay |
| String adapters kept for synthetic parser tests (`imgurMediaFrom` and the NPR, ABC listen, CoomerFans, PornPics adapters) | retain: each only parses and delegates, so there is one parser per page |
