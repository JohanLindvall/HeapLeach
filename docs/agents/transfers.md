# Transfers

How one file moves from a host to its part file: `Manager.transfer`
(`worker.go`), the multi-connection engine (`multistream.go`, `segments.go`,
`writebuf.go`, `ranges.go`, `speedmeter.go`), and the per-host admission it
answers to (`hostgate.go`, `hostlimit.go`), all in
`backend/internal/download`. Playlists take a path of their own
([media.md](media.md)), and routed transfers add the route handling in
[proxies.md](proxies.md).

## Retries and stalls

**Deferred resolution has its own bounded retries** (`resolveTarget`). A
request can exhaust the HTTP client's retries while obtaining a download link,
before any transfer starts or while refreshing a link to resume one. Dial,
TLS and request timeouts, dropped connections, temporary HTTP statuses and
`extractor.TransientError` repeat the resolution up to `MaxRetries`, using the
transfer backoff and an item note. Parser errors, missing files, certificate
failures and permanent refusals stop immediately. Check the caller's context
separately: an HTTP client deadline is retryable while that context is alive;
a canceled or expired caller stops the attempt and its backoff. Leased routes,
challenges, host waits and enabled proxy fallback (`proxyRecovers`) keep their
existing recovery paths rather than spending another retry budget on the same
route. Tests in `resolve_retry_test.go` cover both initial resolution and
refresh after a partial transfer.

**The retry budget counts attempts in a row that moved nothing.** An attempt
that received bytes and then lost its connection — a CDN node dropping every
few dozen megabytes, a NAT forgetting a long connection — is one connection
failing on a host that was serving the file a moment ago, not the transfer
going nowhere. `Manager.transfer` therefore resumes it from the part file
after a flat `config.ProgressRetryDelay` (30s) and starts the attempt count
over, so `MaxRetries` bounds consecutive unproductive attempts rather than the
whole life of a large file on a flaky host. The waits that are not failures
— a busy host, a connection limit, a dropped connection after progress — go
through `retryUnspent`, which logs, waits in `waitThenRefresh` (sleep under a
note, clear it, resolve the link again) and returns the attempt counter the
loop carries on from: one back for a refusal, so it is not counted, and `-1`
after progress, so the count starts over. A failed attempt waits through
`retryAfter`, which every
retry loop of the transfer engine shares. The judgement is made against the
disk, not the wire: `onDisk` reads what the part file held before the attempt
— the segment sidecar's tally, the playlist checkpoint, or the file's length —
and the position afterwards has to be further. Bytes arriving is not enough,
and `TestStalledTransferYieldsTheWorkerSlot` is why: its server ignores
`Range` and serves the same first 8 KB on every turn, which receives plenty
and gets nowhere, and judged by bytes alone would be deferred forever. Nor can
`Item.downloaded` from before the attempt serve as the baseline, since
`enqueueLocked` zeroes it on every re-queue. This cannot cycle forever on a
finite file, since every such attempt left more on disk than it found. Tests
live in `progress_retry_test.go`.

**A stall watchdog** aborts an attempt whose byte counter stops moving
(`config.StallTimeout`). Streaming requests keep a header deadline, while body
reads have no deadline of their own, so without the watchdog a silent server
would pin a worker forever. Paused time does not count (see the throttle in
[download.md](download.md)). The aborted item is **not retried in place**:
`deferStalledLocked` sends it to the back of the queue with its part file
intact, so a host that has stopped serving does not hold a worker slot while
the queue waits. That is bounded by the retry budget, past which the next
stall is a failure, so a headless run still terminates. Its next turn resumes
from disk, and a stall that came after bytes had landed starts the count over,
as above.

The stream supervisor treats stalled and slow as opposites: a probe window in
which no bytes arrived opens no extra connection, and discards any pending
before/after verdict rather than reading the stall as
"parallelism made it slower", a verdict that would penalise the host's budget
for the rest of the process over a condition the host imposed on everyone.

**A web page served instead of bytes** is caught by `rejectWebPage`; gofile
does this for busy storage servers. Without it, following the redirect writes
the page shell to disk and, since `Content-Length` agrees, records it as a
complete file. Those attempts sit outside the retry budget, but only for an
item that can re-resolve: one with a resolver re-signs its link or rotates to
the next mirror, so trying again can genuinely land somewhere else. An item
without one asks the same URL the same way, and a URL that keeps answering
with a page almost always *is* a page — something pasted that no extractor
recognised — so those stop at `config.BusyRetryLimit` and say that instead of
retrying forever. A host whose dead end looks like a real file gets
`File.Reject` from its extractor, which sees the final URL and headers and may
refuse the response; a rejection is final and never retried.

## Several connections for one file

`segments.go` holds the range table and its resume sidecar, `multistream.go`
the engine and its supervisor. Points worth knowing before changing them:

- Every resume checkpoint — the segment table's `.state`, a playlist's
  `.playlist` — goes through `writeSidecar`, `readSidecar` and `clearSidecar`
  in `files.go`, which write JSON whole under a `.tmp` name and rename it
  into place (`writeFile`, which the merged subtitles use too). A write that
  fails removes its `.tmp`; a missing or unreadable checkpoint reads as none,
  which means starting over. Every file operation of a transfer takes the
  `transferFiles` it was started under; the zero value is for trusted local
  paths only (remuxing, tests). Both supervisors, single- and multi-route,
  write the segment `.state` from their tick through `saveDue`, so the two
  checkpoint at the same interval.
- The primary connection and every extra one ask for their range through
  `rangedRequest`, so they send the same headers, `Accept-Encoding:
  identity` and the same `If-Range` rules.

- Every hot-path copy and decrypt draws its 256 KiB buffer from one shared
  pool (`buffers.go`, `borrowChunk`). They all want exactly `CopyBufferSize`,
  so keep new ones on the pool rather than allocating per attempt. A buffer
  of another size gets a `bytePool` of its own, as the write buffers have.
- A transfer reads its intervals from `Manager.timings` as it runs rather
  than copying them. Every default lives in `downloadTimings.withDefaults`,
  so a test that replaces the timings wholesale passes them through it
  (`fastTimings`), and one that builds a `Manager` by hand for a supervisor
  sets them there; a zero interval would stop the supervisor's ticker cold.
- Splitting bisects the *remaining* span of the widest segment, with ties
  broken leftmost-first. That is what produces the halves, quarters and
  eighths in their natural order, and it self-balances once some segments are
  ahead.
- `MinSegmentSize` is not just about avoiding tiny requests: it guarantees a
  split point stays ahead of the writer, so a reader can never write past the
  end reassigned beneath it.
- A reader leaves its loop only through `table.finish`, which runs under the
  table lock. Paired with `retire` refusing to extend a finished segment, that
  is what stops a range being left with no reader.
- Decisions use `speedMeter`, a windowed average that reports nothing until
  the window fills. A single interval swings far too much to act on.
- Extra connections are budgeted per host (`hostLimiter`) and never retry: a
  refusal is the answer we want, so the range goes back and the host's budget
  drops. Primary connections are never budgeted, so a tight budget cannot
  deadlock.
- `ranges.go` validates every partial response before it reaches the file: the
  offset, span, body length and known total must agree. A 416 only proves
  completion when it states the exact length of a sequential part. For a
  partial response with an unknown total, the next range confirms the end.
  Resume also checks that the part still contains the bytes its checkpoint
  records; a missing or truncated part or a corrupt checkpoint starts over.
  These regressions live in `ranges_test.go`.

### Write buffers

**Writes are buffered, and that is what separates "received" from "on disk"**
(`writebuf.go`). A read returns whatever the socket has, and writing each one
straight through would mean a write syscall per read — several hundred a
second across a busy queue, every one of them an inotify event for whatever is
watching the download directory. So a connection accumulates up to
`config.WriteBufferSize` before touching the file.

A connection starts on `config.WriteBufferStart` (64 KiB) and grows to the
full size only once it has moved a mebibyte at an average of
`WriteBufferGrowRate` (64 KiB/s) or more. A slow connection, a free download
capped at tens of kB/s, would take the better part of half a minute to fill a
full buffer: a mebibyte apiece held for nothing across hundreds of transfers.
Worse, a connection dropped before its buffer filled would have written
nothing, so its attempt would look unproductive to the retry budget, which
judges by what is on disk. It is the average over a mebibyte that is judged,
not how fast one buffer filled: slow downloads arrive in bursts, and one read
can fill 64 KiB at once. A write of a full buffer or more goes straight
through, and a fast transfer pays sixteen small writes before it grows.

The buffer belongs to **one connection**, not to the file: eight connections
fill eight disjoint ranges at once, so a shared buffer would have nothing to
coalesce and would need a lock to say so.

The consequence is the part that must not be regressed. `segment.pos` is what
has been *received* and decides what to read next; `segment.flushed` is what
the file actually holds, and it is the only thing `state()` writes to the
sidecar. A sidecar that counted buffered bytes would resume past a gap and
finish a file with a hole in it — the sidecar may only ever under-report.
A connection whose flush failed goes back to `flushed` before it retries
(`segment.rewind`), on the single-route and the multi-route path alike.
Reads stay bounded by `CopyBufferSize` whatever the buffer holds, because the
promise that keeps a split point ahead of the writer is stated in those terms;
see `MinSegmentSize`. Tests live in `writebuf_test.go`, and the invariant
itself in `TestSegmentStateRecordsWhatIsOnDiskNotWhatWasReceived`.

Two more consequences. An attempt that dies loses whatever its connections
were holding, which is bytes re-fetched rather than bytes lost. And since the
retry budget judges an attempt by what it left *on disk*, a buffer much larger
than what a flaky host manages between drops would start making productive
attempts look unproductive. The playlist path is deliberately left alone: it
writes whole parts, which are already megabytes apiece, and an external
download is yt-dlp's own file to write.

### Encrypted payloads

**Encrypted payloads** (`crypt.go`) are decrypted on the way in rather than in
a second pass over the finished file, and the reason that is possible is worth
keeping in mind before changing either side. Every write in the engine is
positional — `WriteAt` with an absolute offset, from the sequential path and
from eight connections filling ranges out of order alike — and the mode
(`File.Cipher`, AES-CTR) is a counter-mode stream cipher, whose keystream at
an offset depends on nothing but the offset. So a decrypting `io.WriterAt`
slots under the file and the rest of the engine does not know it is there. Two
consequences: `segmentedTransfer.file` is an interface rather than an
`*os.File`, and the part file holds **plaintext**, which is what keeps resume
working, since the sidecar's byte counts and the file's length go on meaning
the same thing. Decrypting in place would be free but is forbidden:
`io.WriterAt` may not touch the caller's slice, and both callers reuse one
read buffer for the whole transfer. Tests live in `crypt_test.go`.

## Hosts that push back

Three refusals are waited out on the busy schedule, outside the retry budget,
because each is the host saying *not now* rather than *no*: an overloaded
host, a limit on open connections, and (above) a web page from a busy server.
A budget spent in a few seconds is no time at all for a backend to recover in.

Before any of those, a host's own reading of the refusal is asked
(`File.Refused`, through `refusedByHost`), since only the extractor knows what
some statuses mean on its host: mega's 509 is an address whose quota is used
up, with the time left in a header of its own. What it reads is returned as it
is, never retried in place, since the address would only be refused again: a
`WaitError` sends the file back to the queue, or to another route. An extra
connection refused that way gives its range back like any refused extra, and
the connection that was already reading goes on.

### Overloaded hosts (503)

**A host that answers 503 is overloaded, not broken**, and the answer is a
global admission queue per host (`hostgate.go`), not a longer retry loop. The
refusal is waited out up to `config.OverloadRetries`.

- **The note is rendered per snapshot** from the host's current answer
  (`Manager.itemNoteLocked`), not stored when the item is turned away. What a
  host is taking changes while an item waits, so a stored sentence would go on
  stating whatever was true when it was written, and a queue behind one host
  would show two different numbers for it at once. One wording covers every
  item that is waiting, refused or merely not yet at the front: all are
  waiting for a turn at a host taking fewer downloads than the queue would
  give it. The refusal count behind them is bookkeeping and stays out of the
  row; if the patience runs out the item fails and says so, which is where the
  count is worth something.
- **A refused transfer gives its slot up there and then.** It does not wait
  and try again holding it: an item that has been refused has no open
  connection, only a place in a queue better given to something else. Waiting
  in place would keep slots that lowering the cap cannot take back, with
  several rows on one struggling host retrying while the cap says one at a
  time. The turn is counted on the *item* (`overloadWaits`), since the waiting
  happens in the queue and each turn is a separate transfer.
- **Only refusals in a row count**, the way the retry budget counts only
  unproductive attempts: a turn after which the host got some other transfer
  going (`hostGate.lastServed` against the item's `refusedAt`) starts the
  count over, so an overloaded host still working through its queue is never
  given up on — only one that has stopped serving is. `enqueueLocked` clears
  both, so a retry starts the patience over; carried across, a file that had
  run out of it would fail on the first refusal after being retried, without
  waiting at all. `deferHostQueuedLocked` restores them, being the one caller
  continuing a wait.
- **One at a time, from the first refusal, and the host is left alone between
  turns.** Walking the cap down a step per refusal sounds gentler and is not:
  while it walks, everything admitted before it started is still going. And a
  cap alone slows nothing when the queue behind it is long — the next item
  takes the freed slot the instant it is given back — so the gate also holds a
  host quiet for a growing interval after each refusal, cleared the moment a
  transfer there gets going.
- **The cap is learned**, which is what separates it from the `Pace` an
  extractor declares: it drops to one at the first refusal and comes back one
  slot for each transfer that runs to the end (`eased`), and is forgotten once
  it reaches what the queue would run anyway. Transfers already running are
  left alone, for the reason the free-space floor leaves them alone.
- **The queue does the waiting, never a worker.** An item that cannot have a
  slot is handed straight back to the queue (`hostQueuedError`,
  `deferHostQueuedLocked`), where `hostFullLocked` passes over it until there
  is room. `tryAdmit` never blocks, and that is the point: a worker parked on
  a struggling host is a worker not downloading from anywhere else. Freeing a
  slot signals the dispatcher, since it is asleep until told there is
  something to look at.
- **The turn comes before the signature.** A link is signed for minutes and an
  item may sit in that queue for hours, so `transfer` takes the slot first —
  keyed on wherever the item last resolved to — and only then mints a link,
  swapping the slot if what it resolved to is somewhere else. Minted before
  the wait, the link would expire where it stood, and a signature spent on a
  transfer that is then turned away is a request made of a host that has just
  asked for fewer of them. An item that has never run knows no host, so it
  signs once and takes its turn against what that produced; the worker pool
  bounds that, which is a far smaller number than the queue behind it.
- **It is keyed on the host that actually answered**, which is where the link
  resolved to and not the site the job was submitted from, and it is keyed
  that way in all three places, the queue, the evidence and the cap, or the
  throttle can never fire. A search on an index site is the case that proves
  the point: every file in that queue lives on somebody else's storage, so
  throttling the index would hold back the queue without touching the host
  that is struggling, under a message naming the wrong machine. That is why
  `hostFullLocked` alone cannot serve here: it runs at dispatch, and nothing
  has resolved yet.
- **The throttle waits for proof the host serves anything.** A 503 from a host
  that has never got a transfer going is a host that is *down*: cutting it
  back neither helps it nor gets the file. That case still waits, and the note
  says "unavailable" where the other says "overloaded". Only 503 does any of
  this. 500, 502 and 504 are a server that broke or a gateway that could not
  reach what it fronts, which the ordinary budget already repeats and which
  say nothing about how much the host is being asked for.

### Limits on open connections

**A host that limits how many downloads one caller may have open** is neither
a failure nor a rate limit: `refusedForConnectionCount` (`hostlimit.go`)
recognises it and `Manager.transfer` waits it out on the busy schedule, up to
`config.ConnectionLimitRetries`. What clears it is *our own* other transfers
finishing, which is why the patience is measured in minutes and why it is
capped: the identical refusal arrives when the connections belong to something
else on this address, and then no amount of waiting here frees one. The body
has to be read, not the status: these hosts answer 403, which otherwise means
forbidden and must stay fatal, so `connectionLimitMarkers` holds each host's
own error identifier rather than a phrase from its prose. A refusal of a
connection a transfer *needed* is also the strongest evidence there is about
the host, so it calls `hostLimiter.saturated` to close the extra-connection
budget at once instead of letting every sibling learn it by being refused.
