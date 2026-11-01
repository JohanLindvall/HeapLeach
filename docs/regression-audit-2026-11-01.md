# Regression audit: baseline to main, 2026-11-01

This audit found **four reproducible changes that lose baseline behavior**: two extractor/discovery compatibility regressions, one destination-folder regression and one player-parser regression. No P0/P1 defect was established. The first two arise from deliberate tightening of recognition rules; they are reported because previously working inputs stop working, not because the policy change itself must be accidental.

## Comparison and evidence standard

| Item | Revision / scope |
| --- | --- |
| Baseline | [`6fdde611eef9ade7cb14f4a26d4a5ab943f1b0c2`](https://github.com/JohanLindvall/HeapLeach/commit/6fdde611eef9ade7cb14f4a26d4a5ab943f1b0c2), 2026-10-29 |
| Reviewed main | [`85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7`](https://github.com/JohanLindvall/HeapLeach/commit/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7), fetched from `origin/main` on 2026-11-01 |
| Commit range | One commit after the baseline; this report does not include its own documentation commit in the target |
| Diff size | 519 files; 50,740 insertions, 24,070 deletions |
| File categories | 141 extractor Go paths, 51 other Go production paths, 13 frontend TypeScript paths including shared test helpers, 287 test paths, 25 Markdown paths, two ignore files; deleted paths are included |

Three subagents worked alongside the primary reviewer, with a later independent review of the findings. The session allowed four concurrent agents, so the requested ceiling of 32 was not approached. Go checks used `GOMAXPROCS=2` and `-p 1`; frontend verification used a Docker container limited to two CPUs and 1 GiB, with two test workers.

Findings require a concrete user-visible consequence and a baseline/target comparison. R1, R2 and R4 use **identical full-registry test source** at both revisions. R3 uses the same synthetic API payload and the real listing parser, with a small version-specific adapter for the changed internal API. Every reproduction uses local fixture servers. No live content pages or private recordings were used. The differential reproductions invoked no transfer-time file resolvers or media transfers; the existing suites also exercise synthetic transfers. Consequently, these results establish code regressions, **not verified outages on currently deployed sites**.

## Findings at a glance

P2 means a material loss of functionality for a bounded input or workflow, suitable for correction in the normal release process.

| ID | Priority | Regression | Evidence | User impact |
| --- | --- | --- | --- | --- |
| R1 | P2 | Explicit `links:` harvesting excludes canonical item links on the same site | Full registry: baseline passes, target fails | Bulk operation fails, or omits items from a mixed page |
| R2 | P2 | An unknown KVS install with a custom video path and plain-media flashvars loses recognition | Full registry: baseline passes, target fails | A previously downloadable video is rejected as an ordinary page |
| R3 | P2 | Existing install labels are replaced when forming collection titles | Listing parser: baseline title preserved, target renamed; destination flow verified in source | Re-reading a collection can create a different folder and download saved files again |
| R4 | P2 | A top-level HLS `file` value is no longer read by the migrated player parser | Full registry: baseline passes, target fails | Previously supported player pages fail to extract |

### R1. Explicit harvesting loses same-site bare item links

**Trigger.** An explicit `links:<page>` request reads a page that links to a supported item on its own site. The demonstrated case is Sendvid's canonical `/<id>` form. The local fixture's item contains the already supported `video_source` element and a synthetic MP4 URL.

**Baseline behavior.** `Links.Extract` filtered candidates through the host-based `Registry.Known`. The item was admitted and the collection resolved to one media file.

**Target behavior.** [`Registry.Follows`, lines 353–374](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/extractor/registry.go#L353-L374) accepts a `linkForms.follow` predicate only when `!home`. The Sendvid Shape does not include a bare `/<id>`. The explicit-harvest fallback, [`harvests`, lines 381–399](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/extractor/registry.go#L381-L399), permits only rows with a `harvest` predicate; [the canonical-item rows have `follow` only](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/extractor/linkforms.go#L221-L235).

The candidate is removed before its page is fetched. The one-item reproduction fails with `none of the 1 links ... point at a host this build knows`. On a mixed page, excluded candidates do not enter the source count, so a partial download can lack an omission count for these links.

**Extent.** Sendvid was reproduced through `Registry.Extract`. The same filtering arrangement is present for Streamable, Imgur bare items, LuluStream, Vidmoly, SoundCloud tracks and Mixcloud shows; these are source-supported affected candidates, not seven additional independently reproduced findings. Directly pasting an individual item is outside the demonstrated failure. The issue is specifically the lost bulk workflow.

**Policy qualification.** Avoiding automatic recursion into account, login and navigation pages is intentional and documented. That does not make explicit `links:` equivalent to its baseline behavior. The new `harvests` mechanism already restores certain same-site platform items, but not the demonstrated canonical form.

**Correction direction.** Add bounded canonical-item predicates to explicit harvesting on known installs, while retaining stricter automatic scanning. Verify both same-site item inclusion and navigation exclusion. If this loss is intentionally retained, document it as a breaking capability restriction and provide an alternative bulk workflow.

### R2. Custom-path KVS pages with valid plain-media flashvars lose recognition

**Trigger.** A previously unknown install serves a video at an invented custom path such as `/watch/synthetic`. Its page contains:

```html
<meta name="generator" content="Kernel Video Sharing">
<script>
var flashvars = {
  video_title: 'First Clip',
  video_url: 'https://cdn.example.test/first.mp4'
};
</script>
```

There is no `license_code`, `kt_player` literal or `list_videos` block. A plain media URL does not need the license permutation.

**Baseline behavior.** The fallback's [`platformPage`](https://github.com/JohanLindvall/HeapLeach/blob/6fdde611eef9ade7cb14f4a26d4a5ab943f1b0c2/backend/internal/extractor/platform.go#L72-L79) recognized the generator on arbitrary paths and called the KVS parser, which returned the MP4 file.

**Target behavior.** [`KVS.Recognise`, lines 272–283](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/extractor/kvs.go#L272-L283), first requires one of the three literals above before checking the fingerprint. Generator-based recognition is then available only under `kvsVideoPath`, covering the conventional video paths. The custom path fails that gate. The generic fallback does not parse these flashvars, so the identical fixture is rejected as a page with no recognized media.

**Extent and confidence.** The full registry test passes at the baseline and fails at the target. This establishes a supported-input compatibility loss without claiming that a public site currently uses this exact minimal template. Known/configured installs, conventional paths and pages carrying stronger markers can still work. The parser's ability to read the media is not the failing component; dispatch no longer reaches it.

**Correction direction.** Accept the platform generator together with a successfully parsed playable flashvars object on custom paths. That is stronger evidence than a generator alone and does not require restoring permissive recognition of every generator-marked page. Add positive plain-URL and negative generator-only fixtures.

### R3. Collection-title changes rename existing destination folders

**Trigger.** Re-read a multi-file collection after upgrading. The extractor derives its title from an install label whose old value differs from the host's first meaningful label.

The baseline's [explicit board names](https://github.com/JohanLindvall/HeapLeach/blob/6fdde611eef9ade7cb14f4a26d4a5ab943f1b0c2/backend/internal/extractor/booru.go#L79-L93) are replaced by [`booruLabel(u.Hostname())`](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/extractor/booru.go#L194-L202). The board name becomes the listing's `Result.Title` at lines 238–244.

| Input install, expressed as a host only | Baseline label | Target title prefix | Evidence |
| --- | --- | --- | --- |
| `yande.re` | `yandere` | `yande` | Differential parser test |
| `booru.borvar.art` | `booruvar` | `borvar` | Old table and new label function |
| `archive.palanq.win`, board-index jobs | `palanq` | `archive` | Old install table and new archive title construction |

The archive variant comes from [`FoolFuuka.Extract`, line 202](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/extractor/foolfuuka.go#L197-L203), and the board-index title at lines 460–463. The retained `familyInstalls` display-label mapping does not prevent that title change.

**Observed comparison.** A synthetic two-image listing, with unchanged IDs and file names, produced:

```text
baseline: title="yandere audit" files=2 names=1.jpg,2.jpg   PASS
target:   title="yande audit"   files=2 names=1.jpg,2.jpg   FAIL
```

**Why this affects downloads.** [`Manager.applyResultLocked`, lines 463–496](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/download/manager.go#L463-L496), makes a multi-file job's destination folder from `res.Title`. Previously saved-file matching uses the destination directory and filename. A new folder therefore prevents the old destination from satisfying that check. Submitting the same collection again or re-reading its source can create a second tree and transfer files that already exist under the baseline folder.

The upgrade path was independently traced: restoration retains the old items' `Dir` and `Path`; resuming or retrying an unfinished restored job calls `rereadLocked`, which records completed files through `finishedLocked`. Those records are keyed by the old directory and name. The replacement extraction constructs the new directory before looking them up. The worker also looks for existing and partial files beneath the new directory, so both completed-file reuse and partial-file resumption can be lost. See [restored-item handling](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/download/persist.go#L63-L76), [source re-reading](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/download/manager.go#L1088), and [worker destination checks](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/download/worker.go#L102-L124).

This is not a claim of destructive overwriting. Fully completed jobs remain complete on ordinary restoration, and ordinary in-process item retries keep their existing directories. The regression requires re-extraction. A single-file job ordinarily has no title-derived outer folder.

**Correction direction.** Preserve legacy title prefixes for existing installs; use the derived host label for newly discovered installs. Keep UI labels and filesystem identity aligned where the title forms a directory. Add an upgrade-oriented test that starts with baseline destinations and verifies that re-extraction recognizes them without moving or downloading them again.

### R4. The migrated player parser rejects a top-level HLS `file`

**Trigger.** A LuluStream page supplies the HLS source directly in its JW Player setup, without wrapping it in a `sources` array:

```javascript
jwplayer("player").setup({file: "https://media.example.test/master.m3u8"});
```

**Baseline behavior.** The removed [LuluStream parser](https://github.com/JohanLindvall/HeapLeach/blob/6fdde611eef9ade7cb14f4a26d4a5ab943f1b0c2/backend/internal/extractor/lulustream.go#L41-L43) searched the unpacked setup for an HLS `file` property. It accepted this representation and resolved the manifest to a downloadable segmented file.

**Target behavior.** [`luluSource`, lines 648–655](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/extractor/playersites.go#L648-L655), delegates both ordinary and unpacked setups to `jwSource`. That [shared helper, lines 23–49](https://github.com/JohanLindvall/HeapLeach/blob/85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7/backend/internal/extractor/jsdecode.go#L23-L49), searches exclusively inside `sources: [...]`. A top-level `file` never enters the loop. The known-host fallback does not rescue this case: dispatch can reach the right extractor, but the parser itself refuses the page.

**Observed comparison.** Identical full-registry test source supplies a local player page and a complete one-segment HLS manifest. Baseline extraction passes and reports `resolved "First Clip.ts" with 1 segment`. Target extraction fails with `no source list in the player configuration (the file may have been removed, or the player may have changed)`. Neither run downloads the segment or invokes the returned transfer-time resolver. The fixture is synthetic, not a claim about a current live page.

**Cause and correction direction.** The shared helper's source-array restriction prevents a thumbnail `file` property from being mistaken for media on other players. Applying that narrower grammar to the old LuluStream reader removes a representation it previously handled. Preserve the top-level HLS source for this reader, or parse the actual setup object's media property while excluding nested preview/thumbnail properties. Test both top-level and array forms, packed and unpacked, with thumbnail decoys retained as negative cases. The existing array-based fixtures and full passing suite did not catch this loss.

## Verification

### Existing suites

- Target extractor race suite: `GOMAXPROCS=2 go test -race -p 1 ./internal/extractor` — **passed**, 86.236 seconds.
- Target HTTP, server, proxy, tools, utility, CAPTCHA and command packages under `-race` — **passed**; results were cached where Go's test cache applied. Proxy and screenshots have no test files.
- Target download/CLI/config/main ordinary suite — **passed**; download 117.648 seconds, CLI 1.821 seconds.
- Target selected existing core tests with `-count=1` — **passed**, 4.168 seconds.
- Target `GOMAXPROCS=2 go vet -p 1 ./...` — **passed**.
- Frontend `npm run typecheck` and `npm test -- --maxWorkers=2` in `node:24-alpine` — **passed**, 15 files and 136 tests.
- Full baseline `GOMAXPROCS=2 go test -race -p 1 ./...` — **passed**, including download 122.854 seconds and extractor 37.560 seconds.
- Full target `GOMAXPROCS=2 go test -race -p 1 ./...` — **passed**, including download 124.428 seconds and extractor 86.685 seconds; some unchanged package results were cached. These timings are observations, not a controlled performance comparison.

A passing existing suite does not contradict the findings: the differential fixtures exercise gaps in the checked-in tests. The intentionally failing audit fixtures were removed before the full-suite checks. Production code was not changed by this audit.

### Differential checks

| Check | Baseline | Reviewed main |
| --- | --- | --- |
| `TestAuditCoreCompatibility/explicit_same_site_bare_item` | Pass | Fail during extraction |
| `TestAuditCoreCompatibility/custom_kvs_generator_and_plain_flashvars` | Pass | Fail during extraction |
| `TestAuditStableBoardFolder` | Pass | Fail: changed title |
| `TestAuditCoreLuluTopLevelFile` | Pass | Fail: no source list |

No failure above is a compile failure, missing helper, external network failure or timeout. R3's adapter limitation is explicit below: it isolates the label-to-title change and is not an end-to-end transfer test.

## Review coverage and limits

The review used the installed codebase-memory skill for structural searches, call traces and source snippets, with direct Git diffs and source reads for behavior and baseline verification. Exact evidence-path coverage was checked. Six broadcaster files had reported one-line parse gaps; those lines and their surrounding source were read directly. Excluded private live-test symlinks were outside this offline audit. Graph freshness is a best-effort signal, not proof of semantic completeness.

| Area | Review performed | Result |
| --- | --- | --- |
| Download manager and transfer engine | All 24 changed production diffs; queue/lifecycle, retry freshness, persistence, file naming, disk ownership, streams/ranges, playlists, remuxing, proxy admission and settings | No additional confirmed introduced regression |
| Config, CLI and startup | Five changed production diffs; environment/default handling, proxy validation, startup settings, incomplete-run exit accounting and formatting | No additional confirmed introduced regression |
| HTTP, API, proxy adapter and helpers | Retry/body/admission changes, route observations, challenge capture, headers, TLS setup, API actions/JSON/SSE, helper and OCR path lookup, inventory/screenshots commands | No additional confirmed introduced regression |
| Frontend | All changed application-source diffs; polling cleanup/timeouts, settings settlement, error reporting, retry/filter/clear behavior and wire types | No additional confirmed introduced regression |
| Extractor framework | Dispatch/claims, recognition, probe/cache identity, fallback, source expansion, limits, shared walkers, player migrations, handoffs, KVS, HLS, refresh/retry hooks | R1, R2 and R4 |
| Shared collection platforms | Booru, Fediverse and its three dialect files, MediaWiki, autoindex, feeds, FoolFuuka, FourChan, WordPress, links/filelinks | R3; no additional confirmed failure |
| Host adapters | 43 current host/inventory files and three removed listing implementations; parser/resolver migration, pagination, original media selection, authentication/token state, filenames, folders and caps | No additional independently confirmed failure |
| Broadcasters | 23 adapter/shared-helper diffs; list ordering, pagination, title/season handling, media selection, geo/refusal handling, headers and deferred resolution | No additional confirmed failure |
| Build/release surface | Compared changed-path inventory; Dockerfile, Makefile, Go modules, helpers and `.github` workflows have no diff in this range; inspected ignore-file changes | No release-code regression attributed to this range |

Host-adapter coverage includes `archiveorg`, `balbums`, `bandcamp`, `bandzoogle`, `celeb`, `chevereto`, `coomerfans`, `cum`, `erome`, `fapello`, `kemono`, `pornpics`; `bunkr`, `cyberdrop`, `emload`, `filehosts`, `filejump`, `filester`, `gofile`, `gofilewt`, `imagepond`, `imgur`, `keep2share`, `mega`, `pixeldrain`, `pixhost`, `rajce`, `suvobox`, `turbo`, `wetransfer`; `civitai`, `darkgram`, `eporner`, `knuthing`, `niconico`, `noodlemagazine`, `okru`, `peertube`, `redgifs`, `streamable`, `vimeo`, `voyeurking`; and `catalogue`. Removed `filesterfolder`, `knuthinglist` and `noodlelist` implementations were compared with their replacements.

Broadcaster coverage includes `broadcast`, `abclisten`, `ard`, `arte`, `bbcsounds`, `drtv`, `francetv`, `npostart`, `npr`, `nrk`, `orf`, `pbs`, `radiocanada`, `raiplay`, `rtbf`, `rtp`, `rtve`, `ruv`, `srf`, `svtplay`, `vrtmax`, `yle` and `zdf`.

A final migration pass also compared removed player, KVS, HLS, OCR and grouping implementations with their replacements: `drtuber.go`, `lulustream.go`, `vidmoly.go`, `vidsst.go`, `hlsdirect.go`, `kvsmembers.go`, `kvssearch.go`, `yandex.go`, `keep2shareocr.go` and `series.go`. This pass is separate from checking that a replacement file exists: it compares the input forms and parsing behavior of the removed code.

This was a thorough, task-directed review of a large rewrite, not an exhaustive proof of all endpoint/schema combinations. Changed tests and documentation were read selectively to check intended contracts; all 287 changed test files were not independently reviewed line by line. Live-host compatibility, real proxy routing, real helper/media processing and release publication were not revalidated. No claim is made that this report enumerates every regression.

## Suspicions examined but not promoted to findings

- **Gofile child IDs:** rebuilding the child map by `child.ID` could collapse entries if objects omit that field while the original map keys remain populated. No valid supported response demonstrating that omission was established. This is an unconfirmed robustness concern, not a fourth finding.
- **ARD cap/filter ordering:** the new teaser cap is applied before some episode/accessibility filtering. No supported-input differential failure was established; ordinary page-size/cap combinations preserve the previous result. This remains a targeted test opportunity.
- **Unbounded page retention:** no evidence showed refresh closures retaining the original extraction context and its entire page corpus. The shared cache has a charged-body/memo limit and active expiry. Parsed DOM storage is an uncharged multiplier, so the configured cache limit is not a process-RAM ceiling, but that alone does not establish a regression.
- **Known API hosts blocked by page probes:** host claims bypass probing for many API-driven services. A general claim that the rewrite requires every known host's HTML to work would be false.
- **Refresh name normalization / playlist-to-progressive state:** examined problems already existed in the baseline and were excluded from introduced findings.
- **New caps, partial-result notices and incomplete CLI results:** documented behavior changes with corresponding implementation/tests; no defect follows merely from a new limit or a nonzero exit for an incomplete listing.
- **Video-plus-playlist links:** the external handoff now deliberately chooses the individual video for that combined form while preserving genuine playlist inputs. Recorded as changed semantics, not a demonstrated accidental failure.
- **Folder sanitization:** deliberate slash/backslash normalization is separate from R3's avoidable replacement of established install names.

## Reproducing the findings

Use disposable copies of the two exact revisions, leaving the working checkout unchanged:

```sh
mkdir -p /tmp/heapleach-audit-replay/base /tmp/heapleach-audit-replay/target
git archive 6fdde611eef9ade7cb14f4a26d4a5ab943f1b0c2 | tar -x -C /tmp/heapleach-audit-replay/base
git archive 85f3ff1bd12718237d58b1f5ba26c4cc1fe26ab7 | tar -x -C /tmp/heapleach-audit-replay/target
```

### R1 and R2: identical full-registry fixture

Save this as `backend/internal/extractor/audit_core_regression_test.go` in each copy. The public host label in the first case is served entirely by a local HTTP proxy; the fixture does not connect to that host. Media URLs are returned as metadata only.

```go
package extractor

import (
	"context"
	"fmt"
	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAuditCoreCompatibility(t *testing.T) {
	t.Run("explicit_same_site_bare_item", func(t *testing.T) {
		host := "sendvid.com"
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			switch r.URL.Path {
			case "/collection":
				fmt.Fprint(w, `<html><title>Invented collection</title><a href="/synthetic1">First clip</a></html>`)
			case "/synthetic1":
				fmt.Fprint(w, `<html><title>Sendvid</title><meta property="og:site_name" content="Sendvid"><meta property="og:title" content="First Clip"><video><source id="video_source" src="https://cdn.example.test/first.mp4" type="video/mp4"></video></html>`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer proxy.Close()
		client, err := httpx.New("audit", "en-US", 0, 2*time.Second).ThroughProxy(proxy.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer client.CloseIdleConnections()
		reg := NewRegistry(&config.Config{}, client)
		res, _, err := reg.Extract(context.Background(), "links:http://"+host+"/collection", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Files) != 1 || res.Files[0].URL != "https://cdn.example.test/first.mp4" {
			t.Fatalf("unexpected result: %+v", res)
		}
	})
	t.Run("custom_kvs_generator_and_plain_flashvars", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/watch/synthetic" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><meta name="generator" content="Kernel Video Sharing"><title>First Clip</title><script>var flashvars = {video_title: 'First Clip', video_url: 'https://cdn.example.test/first.mp4'};</script></html>`)
		}))
		defer srv.Close()
		client := httpx.New("audit", "en-US", 0, 2*time.Second)
		defer client.CloseIdleConnections()
		reg := NewRegistry(&config.Config{}, client)
		res, _, err := reg.Extract(context.Background(), srv.URL+"/watch/synthetic", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Files) != 1 || res.Files[0].URL != "https://cdn.example.test/first.mp4" {
			t.Fatalf("unexpected result: %+v", res)
		}
	})
}
```

Run from each copy's `backend/`:

```sh
GOMAXPROCS=2 go test -p 1 ./internal/extractor -run '^TestAuditCoreCompatibility$' -count=1 -v
```

Expected at the reviewed revisions: both subtests pass on the baseline; both fail on the target. A future fix should make both pass without weakening the negative navigation/recognition tests.

### R3: same listing payload, version-specific label adapter

Save this as `backend/internal/extractor/audit_root_test.go` in the target copy:

```go
// SPDX-License-Identifier: MIT
package extractor

import (
	"context"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestAuditStableBoardFolder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			io.WriteString(w, `[{"id":1,"file_url":"https://media.example.test/first.jpg"},{"id":2,"file_url":"https://media.example.test/second.jpg"}]`)
		} else {
			io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse("https://yande.re")
	u.Path = "/post"
	u.RawQuery = "tags=audit"
	b := NewBooru(httpx.New("audit", "en", 0, time.Second))
	site := booruSite{name: booruLabel(u.Hostname()), root: srv.URL, api: apiMoebooru}
	res, err := b.extract(context.Background(), u, &site, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(res.Files))
	}
	t.Logf("title=%q files=%d names=%s,%s", res.Title, len(res.Files), res.Files[0].Name, res.Files[1].Name)
	if res.Title != "yandere audit" {
		t.Fatalf("existing destination folder renamed: %q", res.Title)
	}
}
```

For the baseline copy, replace the line beginning `site := booruSite` and the following extraction line with:

```go
site := *siteFor(u)
site.root = srv.URL
res, err := b.extract(context.Background(), u, &site)
```

The adapter follows each revision's actual label construction while redirecting the API to the same fixture server. It does not replace the listing parser. It intentionally excludes registry probing and actual disk/media operations; the destination and saved-file consequence is established by the manager source cited in R3.

Run from each copy's `backend/`:

```sh
GOMAXPROCS=2 go test -p 1 ./internal/extractor -run '^TestAuditStableBoardFolder$' -count=1 -v
```

Expected: the baseline passes; the target fails the preserved-title assertion with `yande audit`. The two files and their names remain equal.

### R4: identical player-page and HLS fixture

Save this as `backend/internal/extractor/audit_lulu_topfile_test.go` in each disposable copy. All requests are handled by the local proxy; an attempted segment transfer would fail the fixture's unexpected-request assertion.

```go
package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestAuditCoreLuluTopLevelFile(t *testing.T) {
	// A positive single-file JWPlayer setup, with no preview tracks or
	// source list: one HLS VOD stream and one segment, all synthetic.
	host := "lulustream.com"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/abcd1234wxyz":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><title>First Clip - LuluStream</title><h1>First Clip</h1><script>jwplayer("player").setup({file:"http://%s/hls/master.m3u8"});</script></html>`, host)
		case "/hls/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nfirst-segment.ts\n#EXT-X-ENDLIST\n")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()
	client, err := httpx.New("audit", "en-US", 0, 2*time.Second).ThroughProxy(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	reg := NewRegistry(&config.Config{}, client)
	res, _, err := reg.Extract(context.Background(), "http://"+host+"/abcd1234wxyz", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "First Clip.ts" || len(res.Files[0].Segments) != 1 || res.Files[0].Segments[0] != "http://"+host+"/hls/first-segment.ts" {
		t.Fatalf("unexpected result: %+v", res)
	}
	t.Logf("resolved %q with %d segment", res.Files[0].Name, len(res.Files[0].Segments))
}
```

Run from each copy's `backend/`:

```sh
GOMAXPROCS=2 go test -p 1 ./internal/extractor -run '^TestAuditCoreLuluTopLevelFile$' -count=1 -v
```

Observed: baseline **PASS**, 0.007 seconds; target **FAIL**, 0.009 seconds, with the missing-source-list error above. This final focused comparison ran in isolated copies after the broad suites, without modifying production code or adding a test to the working checkout.
