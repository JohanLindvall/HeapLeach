# Download manager

`backend/internal/download`; file names are relative to it. This guide covers
jobs, the queue and what reaches the disk. How a single transfer runs is in
[transfers.md](transfers.md), playlists and joined tracks in
[media.md](media.md), and routed transfers in [proxies.md](proxies.md).

## Ownership and locking

`download.Manager` owns all jobs and the worker pool.

**Locking rule:** every `Job` and `Item` field except the atomic progress
counters (`downloaded` and `streams`) is guarded by `Manager.mu`. `downloaded`
is atomic so the progress ticker can sample a running transfer without
contending with workers. Workers take `mu` only to publish state transitions,
never while blocked on I/O. A change that the browser should see and the
scheduler should act on ends in `commitLocked`, which releases `mu` and only
then marks the state dirty and wakes the dispatcher.

**Item ownership — do not regress this.** `Item.inFlight` marks an item from
dispatch until its worker has fully finished. Status alone cannot serve that
purpose: cancelling flips `Status` to `canceled` while the worker is still
winding down, so gating on status would let cancel-then-retry hand the same
item — and the same `.part` file — to two goroutines. A retry requested during
that window sets `retryPending`, and the exiting worker re-queues it.
`enqueueLocked` is the single entry point for queueing; regression tests live
in `ownership_test.go`. Settings change only through `ApplySettings`, which
validates the whole update before applying any of it; tests read them back
from `Snapshot()` or `CurrentSettings()`.

The package's tests build managers with `testManager` (`helpers_test.go`),
whose function argument tunes the configuration before anything is built from
it, and read the queue through `jobView`, `findItem`, `countStatus` and
`waitForStatus`; it waits with `waitFor`, not a loop of its own. Tests that
need the media tools skip without them through `requireFFmpeg` and
`requireFFprobe`, run a tool with `run`, and read a result's streams with
`ffprobeStreams`, the tests' own reading of ffprobe kept apart from
`probeMedia` so a test cannot share the mistake it should catch. A test's own differences — retries, timings, whether it calls
`Start()` — stay at the test. Accessors the code itself never needs, such as
`hostGate.quiet` or `throttle.take`, live in `testonly_test.go`.

**Clear finished only forgets successful jobs** (`Job.complete`): the source
has no error and every item is done. Terminal jobs can still have failures,
and even a done status can include canceled files or a partial listing, so
neither status alone is enough. Incomplete jobs and their pending retries
remain available. The UI's `isComplete` predicate mirrors this rule.

**Stopping is allowed during shutdown; starting is not.** The job actions look
their job up through `jobLocked`, which returns with `mu` held. Retries and
limit changes pass `refuseClosing` and get `ErrClosed` once `Close` has begun;
`CancelJob` and `RemoveJob` do not, since cancelling is what shutdown wants
anyway. Both share `cancelJobLocked`.

## Part files

Transfers resume from the `.part` file with a `Range` request.

**Part-file naming — do not regress this.** The `.part` file is named
`<file>.<hash of the job's source URL>.part`. Two properties depend on that
choice, and a change in either direction breaks one of them:

- It is **derived, not random.** An item id is fresh every process, so a name
  keyed on it could never be found again: every run would restart from zero
  and orphan the previous `.part`.
- It is keyed on the **job's source**, not the item's URL, because several
  hosts hand out signed media links that expire between runs. Hashing those
  would change the name every time and resume nothing.
  `jobSourceLocked` is that lookup, the item's own URL only where the job has
  no source, for the worker, `startedOnDisk` and the host key alike.

Collisions are handled by `Manager.claimPart`: the same URL queued twice gets
the deterministic name for whichever transfer claims it first, and the random
item id for the other, so two workers can never share a `.part`. `partNames`
names the part files for both `claimPart` and `startedOnDisk`, so they cannot
drift apart. Tests live in `paths_test.go`.

## Pause, speed and job limits

**Pause and the rate ceiling are one mechanism** (`throttle.go`). Every read
draws from a single shared token bucket; pausing is a gate on the same object,
and the dispatcher refuses to start new items while it is closed. Four
consequences worth keeping in mind:

- A paused transfer is idle *on purpose*, so `watchForStall` is given the
  pause predicate and does not count that time against it. Otherwise pausing
  would abort exactly the transfers the user asked to keep.
- The stream supervisor asks `throttle.limiting()` before opening another
  connection. A capped transfer and a slow one look identical from the outside
  — bytes are not moving — and splitting a capped one eight ways cannot help,
  because the bucket is shared. `limiting()` measures the bucket's own
  throughput over a rolling window rather than whether anyone *blocked*: the
  bucket hands out whatever it has, so a transfer pinned exactly at the
  ceiling can go a long time without ever waiting.
- A nil `*throttle` is valid and inert (unlimited, never paused). The tests
  build `&Manager{}` by hand, and that has to keep working.
- A job with a speed limit of its own has a second bucket (`Job.limiter`), and
  `throttledReader` draws on it first: waiting there holds back that job
  alone, where waiting with the queue's tokens in hand would hold back every
  other job with it. The supervisors ask its `limiting()` beside the queue's,
  and a job held to its own speed constrains its route measurements as a
  capped queue does. The bucket is made with the job and changed in place, so
  a running transfer is held to a new limit at once, and every item takes the
  pointer when it is made, so readers need no lock to find it.

**A job can be held beneath the queue's limits** (`joblimits.go`). `JobLimit`
sets how many of the job's files download at once, how many streams each may
use, and how fast the job may go. The limits belong to the job rather than to
a host or service, because the job is what someone adds and watches. The
queue's own settings still apply over them, and the job's streams replace what
the host asks for itself within them: a host that allows one connection can be
allowed three, and one that splits freely capped at two. Files of 0 holds the
job's new downloads, and those running finish. The dispatcher counts each
job's running transfers (`jobActive`) beside `hostActive`. The note on a held
file (`jobHoldNoteLocked`) is rendered per snapshot rather than stored, so it
follows the limit as it changes and is gone once it is lifted.
`POST /api/jobs/{id}/limits` replaces a job's limits whole. They are saved
with the job: no configuration sets them, so they are kept whatever it says,
under the ceiling in force when they are read back. The job's view carries
them as flat fields (`fileLimit`, `streamLimit`, `speedLimit`), since an
object would be a new one in every frame and defeat the cards' memo.

## Scheduling

`nextLocked` picks the next item to start. It passes over an item whose time
has not come (`Item.notBefore`), one its host, job or route cannot take, and
everything while the free-space floor is reached.

**An item told "later" waits in the queue, not in a worker.** A resolver that
returns `extractor.WaitError` does not sleep it out. `deferWaitLocked` puts
the item back in the queue as `queued`, which the UI shows as Waiting,
`nextLocked` passes over it until then, and a timer wakes the dispatcher when
the time comes. The note's time left is rendered per snapshot
(`itemNoteLocked`), rounded up to the minute so the row changes once a minute,
not every frame.

**A file already started goes back to the front after a route failure.** A
route that failed a file is deferred the same way, and says as little about
the file. So a file whose part file holds bytes (`startedOnDisk`, statted by
`runItem` outside `mu` through `os.Root`) goes back at the front of the queue,
not the back, and the next route that frees up finishes what is started; at
the back, a large transfer would wait behind every queued file for each new
route. The part file is the evidence because nothing else is: a job re-read
after a restart has items that have never reached the transfer. It cannot
spin: `nextLocked` leases a route before starting an item, and passes over one
it cannot. Stalls still go to the back, on purpose (`deferStalledLocked`,
[transfers.md](transfers.md)).

`resolveSlots` bounds active source listings across jobs independently of
download concurrency. Waiting resolvers observe cancellation before calling
the registry. Nested registry expansion carries a path-local ancestry list;
cycles and chains beyond `config.MaxExtractionDepth` fail without sharing
mutable traversal state between parallel sources.

### Free space

**The free-space floor** (`Config.MinFreeDisk`: `HEAPLEACH_MIN_FREE`,
`-min-free`, default `config.DefaultMinFreeDisk`, 10 GiB; zero disables it)
gates `nextLocked`, which is to say it gates *starting* a transfer. Three
choices in that: running transfers are left alone, because their bytes are on
disk either way and abandoning one near its end frees less than it wastes; the
queue waits rather than failing, so making room is the whole remedy and
nothing has to be re-added; and a destination that could not be measured is
never treated as full, since refusing to download because the check itself
failed is a worse failure than the one it guards. It reads the sampled figure
rather than asking the filesystem, so the dispatcher's hot path costs nothing
and never blocks under `mu`. `DiskMinFree` rides along in the snapshot because
a held queue and a slow one look identical without it.

Free space is sampled on its own cadence (`config.DiskSampleInterval`, seconds
rather than the progress tick's milliseconds) and never under `mu`: `Statfs`
is a syscall and the destination may be a network mount, so it obeys the same
rule workers do. `diskSpace` is split per platform in this package; the unix
half reads `Bavail`, not `Bfree`, since the blocks reserved for root are not
room a download can use. A reading that moved marks the state dirty in its own
right — a disk filling from elsewhere is exactly what the figure is for, and
an idle queue would otherwise never mention it, while a disk that is not
moving keeps that queue quiet. Both `DiskFree` and `DiskTotal` are published
because zero free is ambiguous on its own: a full disk and an unreadable
directory report it alike, and only the total separates them.

## The saved queue

**The queue outlives the process** (`persist.go`, `state.go`). It is written
every `config.StateSaveInterval` (ten minutes, since it carries a line per
file and a large queue is megabytes to rewrite) and on shutdown, atomically
and 0600, as **zstd-compressed JSON** (klauspost, about 9:1 on a real queue).
A save is skipped entirely when nothing but byte counters has moved: the
fingerprint covers all persisted fields, never progress, because the part file
on disk is the authority on how far a transfer got.

Reading goes by content, not by name: a file without zstd's magic number is
read as plain JSON, the legacy format. The default path is `queue.json.zst`;
when it does not exist, `loadState` reads a legacy `queue.json` beside it, and
the first successful save removes that file, since a queue deleted later to
start afresh would otherwise come back from it. A path named explicitly
(`HEAPLEACH_STATE`) is read and written as named. `Config.LegacyStateFile`
enables that default-only migration; a custom `.zst` path must never infer
ownership of a neighbouring file. A failed restore disables persistence for
that process, including shutdown saving, to preserve an unreadable or
newer-version queue. Reads check the file and decoded size against
`config.MaxStateBytes`.

Shutdown closes admission before capturing the queue, and serializes that
snapshot with the periodic saver, so a late save cannot overwrite it with the
workers' shutdown cancellations. Extractor retries carry a generation number
so a superseded result cannot publish into the new attempt.

Three rules here are easy to break:

- **No item URL is stored.** Signed links expire in minutes, and a `Resolve`
  host has no URL until one is minted — it is a closure, which no file holds.
  So `Restore` marks an unfinished job `restored`, and its source is read
  again on resume; `alreadyOnDisk` skips what is complete and the part files
  carry the rest. That is also why **`RetryItem` re-reads a restored job**
  rather than re-queueing the one item: a restored item has nothing to fetch
  from, and enqueued alone it would fail with "no download URL", a reason that
  says nothing about the file. Both retries funnel through `rereadLocked`.
  Which jobs that applies to is `unfetchable`, not `restored`: `restored`
  means *held*, and a job whose files were all done or cancelled comes back
  unheld while its items are just as URL-less.
- **Resolution appends to `job.Items`**, so a restored job's items must be
  dropped before it is re-read or every file in it doubles. Both resuming
  (`ApplySettings` with `Paused` false) and `RetryJob` clear them, and
  `TestResumingARestoredJobReplacesItsItemsRatherThanDoublingThem` holds that
  in place.
- **`Close` persists before `stop()`, not after `wg.Wait()`.** Stopping
  cancels every transfer, and a worker winding down marks its item cancelled,
  which would restore as a job with nothing left to do, silently abandoning
  the download. Recorded before the cancellation, they are still running,
  which is written down as queued. The cost is that a file completing during
  the wind-down is recorded queued and re-checked next run, which is the
  harmless direction.

Restored jobs are held by default, because resuming is the user's word to
give. A service replaced by a deploy tool has nobody to give it, and every new
image would stop the queue until noticed, so `HEAPLEACH_RESUME` / `-resume`
says it at startup instead (`ApplySettings` unpausing after `Start`), and
`/api/health` reports the build's `version` so the tool can tell the new
process from the one it replaced.

### Settings saved with the queue

**Parallel downloads, streams and proxy protocols chosen in Settings outlive
the process.** Every release restarts the service, so a choice held only in
memory would be lost at each one. They are saved with the queue
(`savedSettings`), written the moment they change rather than at the next
save, and taken back by `Restore` before `Start`. Each is stored with the
configured value it overrode, and is kept only while the configuration still
says that: a configuration changed since is the later word, or a deployment
changing it would see nothing happen. A queue saved without settings leaves
the configuration standing. A kept concurrency is held under the current
ceiling. Pause is not kept: resuming stays the user's word, or
`HEAPLEACH_RESUME`'s. The groups' own protocols are saved even when there are
none (`proxyTypes: {}`), which is a choice, unlike a queue saved without that
field, which has none to restore.

## Files already on disk

**Skipping what is already downloaded** happens twice, on purpose: once before
connecting, when the extractor gave an exact length, and once after the
response headers arrive, which is the only check available for hosts that
publish no size. A length marked `File.SizeApprox` is never enough.
`Item.Skipped` records the outcome and, unlike `Note`, survives completion,
because `runItem` clears notes when an item finishes.

`separateNames` reserves destinations across each resolved listing before any
worker starts. Sanitized names can collide, and equal lengths are common;
without the reservation a second entry would be skipped as though the first
were its bytes. All original names are reserved before numbering duplicates,
and `fixedName` prevents a late resolver or disposition header undoing that
allocation.

Neither check works for a file whose length nobody states — a photo with no
size in its listing and no `Content-Length`, say — and every restart re-reads
each unfinished job, so each such file would be fetched again and kept beside
itself as "(2)", then "(3)". Two things cover it:

- **The job's own record.** `rereadLocked` records the items the job had
  already saved, by destination (`finishedLocked`, `destinationKey`, the key
  `separateNames` uses). A file is recorded under the name its listing gave it
  too (`Item.listed`, saved with the queue as `listed` where it differs),
  because a resolver, a `Content-Disposition` header or yt-dlp can rename an
  item after listing, and a listing read again names it as before; an
  extractor can list `.mp4` and resolve `.ts`, for one. A listed name never
  displaces another file's own. `resolve` stats them outside `mu`
  (`stillOnDisk`), and `applyResultLocked` marks an item done, without
  queueing it, when its destination was saved before and that file is still
  there. The recorded path is the evidence, not the length: a playlist is
  counted at its transport stream's length and then rewrapped as a smaller
  MP4, so matching on length would pass over every rewrapped video. The record
  is added to rather than replaced, so a job read again before an earlier
  reading lands keeps it.
- **A byte-for-byte check at the end** (`identicalOnDisk`, before `reserve`).
  A whole file identical to one already under its name, or a numbered name
  beside it, is that file: the part is dropped and the item points at it,
  marked skipped. That catches a re-added job, and a file finished after the
  shutdown save recorded it. A different file with the same name still gets
  its own "(2)". A playlist's part is a `.ts` while the copy on disk is the
  `.mp4` it became, so `remuxToMP4` makes the same check on its output;
  rewrapping is deterministic, so the copies are byte for byte the same.
  `identicalOnDisk` skips the new file itself, which by then already sits
  under a numbered name. It and `alreadyOnDisk`'s `onDiskAs` share one scan
  of the name and its numbered siblings (`numberedSiblings`, beside
  `numberedName`), and both record the find through `markSkipped`.
  `finishTransfer` is this end of `Manager.transfer`: join, the identical-file
  check, `reserve`, rename and remux.

## Names and paths

`SafeName`/`SafeRelPath` sanitize remote path components. Native transfers and
checkpoints use `os.Root` (`files.go`) to confine actual filesystem operations
too, including symlinks. Finished names are reserved with exclusive creation,
so simultaneous completions cannot overwrite each other. `SafeName` first
decodes HTML entities (`util.Unescape`, repeated until stable), since some
pages escape a title twice; decoding comes before the separator rules, so an
encoded slash is treated like a plain one. A title's own slashes are turned
into lookalikes before they get here (`resolveTarget`; see titles in
[extractors.md](extractors.md)).
