package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// segmentedTransfer fetches one file over one or more connections, writing
// each segment straight to its own offset in the part file.
type segmentedTransfer struct {
	manager *Manager
	item    *Item
	file    io.WriterAt
	table   *segmentTable
	part    string
	name    string

	// validator is the ETag or Last-Modified of the copy already on disk.
	// It is sent as If-Range so a file that changed underneath us produces
	// a plain 200 we can detect, rather than bytes from a different version.
	validator string

	maxStreams int
	slowBelow  int64

	// crowded records that opening a connection measurably slowed this
	// transfer down, so no more are opened. Touched only by the supervisor
	// goroutine.
	crowded bool

	// Timings, overridable so tests need not wait out the real intervals.
	pollInterval  time.Duration
	probeInterval time.Duration
	addCooldown   time.Duration
	saveInterval  time.Duration
}

// downloadTimings groups the intervals the download loop waits out, kept in
// one place so tests need not sit through the production values.
type downloadTimings struct {
	poll     time.Duration
	probe    time.Duration
	cooldown time.Duration
	save     time.Duration
	busyBase time.Duration
	busyMax  time.Duration
}

func defaultDownloadTimings() downloadTimings {
	return downloadTimings{
		poll:     config.SegmentPollInterval,
		probe:    config.StreamProbeInterval,
		cooldown: config.StreamAddCooldown,
		save:     config.SegmentStateInterval,
		busyBase: config.BusyRetryBase,
		busyMax:  config.BusyRetryMax,
	}
}

// withDefaults fills in any timing left unset.
func (t *segmentedTransfer) withDefaults() *segmentedTransfer {
	if t.pollInterval <= 0 {
		t.pollInterval = config.SegmentPollInterval
	}
	if t.probeInterval <= 0 {
		t.probeInterval = config.StreamProbeInterval
	}
	if t.addCooldown <= 0 {
		t.addCooldown = config.StreamAddCooldown
	}
	if t.saveInterval <= 0 {
		t.saveInterval = config.SegmentStateInterval
	}
	return t
}

// run drives the transfer to completion. primary is the already-open body
// of the request that discovered the file; it belongs to primarySeg, so
// that request is put to work rather than thrown away.
func (t *segmentedTransfer) run(ctx context.Context, primarySeg *segment, primary io.ReadCloser) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}

	// spawn is only ever called from run and from the supervisor, and the
	// supervisor holds a WaitGroup slot for its whole life, so the counter
	// cannot reach zero while another Add is possible.
	//
	// reserved names the host an extra connection's slot was charged against,
	// and "" marks a primary, which is never budgeted. It is captured at
	// reservation time and travels with the goroutine, because a retry may
	// re-resolve the item onto another mirror host mid-flight: releasing
	// whatever host the URL names by then would leak the slot that was
	// actually taken. Every reservation is released here and only here.
	spawn := func(seg *segment, body io.ReadCloser, reserved string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reserved != "" {
				defer t.manager.hosts.release(reserved)
			}
			if err := t.pump(ctx, seg, body, reserved); err != nil {
				fail(err)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		t.supervise(ctx, func(seg *segment, reserved string) { spawn(seg, nil, reserved) })
	}()

	spawn(primarySeg, primary, "")
	// Segments restored from a previous run need their own connections.
	for _, seg := range t.table.snapshot() {
		if seg != primarySeg && !seg.done() {
			spawn(seg, nil, "")
		}
	}

	wg.Wait()
	t.saveState()

	errMu.Lock()
	err := firstErr
	errMu.Unlock()
	if err != nil {
		return err
	}
	if !t.table.complete() {
		return fmt.Errorf("incomplete transfer: got %d of %d bytes", t.table.written(), t.table.size)
	}
	return nil
}

// supervise watches throughput and opens another connection while the
// transfer is slow, up to the configured ceiling.
func (t *segmentedTransfer) supervise(ctx context.Context, spawn func(*segment, string)) {
	ticker := time.NewTicker(t.pollInterval)
	defer ticker.Stop()

	var (
		sinceProbe time.Duration
		sinceSave  time.Duration
		lastAdd    time.Time
		// speedBeforeAdd is what the transfer was managing when the last
		// connection was opened, so the next stable reading can be compared
		// against it. Zero means no addition is awaiting judgement.
		speedBeforeAdd float64
		// lastAddHost is the host that connection's slot was charged against,
		// so a verdict on it penalises the host that was actually asked even
		// if the item has since rotated to another mirror.
		lastAddHost string
	)
	lastCount := t.item.downloaded.Load()
	meter := newSpeedMeter(config.SpeedWindow, lastCount)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Checked on the short tick so finishing is not delayed by the
			// much longer probe interval.
			if t.table.complete() {
				return
			}

			sinceSave += t.pollInterval
			if sinceSave >= t.saveInterval {
				sinceSave = 0
				t.saveState()
			}

			sinceProbe += t.pollInterval
			if sinceProbe < t.probeInterval {
				continue
			}

			current := t.item.downloaded.Load()
			moved := current != lastCount
			lastCount = current
			elapsed := sinceProbe
			sinceProbe = 0

			// A whole probe window with nothing arriving is a stall, not
			// slowness, and the two call for opposite answers. Slow means
			// the path to this host has more room than one connection uses;
			// stalled means the host is serving nothing on the connections
			// it already has, and will serve nothing on one more — opening
			// it just spends a budget slot proving so. The pending verdict
			// is thrown out rather than delivered for the same reason:
			// judged against a stalled window, the last addition would read
			// as a collapse and penalise the host's budget for the rest of
			// the process, when what actually happened is that the host
			// stopped serving everyone. If the connection is truly dead,
			// the stall watchdog aborts the whole attempt; nothing here has
			// to.
			if !moved {
				meter.reset(current)
				speedBeforeAdd = 0
				continue
			}

			speed, stable := meter.observe(current, elapsed)
			if !stable {
				continue
			}

			// Did the last connection actually help? A host that punishes
			// parallelism rather than refusing it answers every request with
			// a clean 206 and simply serves everyone slower, so nothing else
			// here notices: the budget never drops, the stall watchdog sees
			// bytes moving, and the transfer looks slow — which is the very
			// condition that opens another connection. Left alone that is a
			// feedback loop into the worst case. Measuring the answer breaks
			// it, and generalises past the one host that prompted it.
			if speedBeforeAdd > 0 {
				if speed < speedBeforeAdd*config.ThroughputRegressionRatio {
					t.crowded = true
					limit := t.manager.hosts.penalise(lastAddHost)
					t.manager.log.Debug("host is slower with more connections; stopping",
						"item", t.item.ID, "host", lastAddHost, "before", int64(speedBeforeAdd),
						"after", int64(speed), "limit", limit)
				}
				speedBeforeAdd = 0
			}

			if !t.shouldAddStream(speed, lastAdd) {
				continue
			}
			// Only attempt what this host has shown it will tolerate. The
			// host is captured here so the release — and any penalty — hits
			// the budget the reservation came out of, whatever host the item
			// has rotated to by then.
			host := t.host()
			if !t.manager.hosts.reserve(host) {
				continue
			}
			seg := t.table.split(config.MinSegmentSize)
			if seg == nil {
				t.manager.hosts.release(host)
				continue
			}
			lastAdd = time.Now()
			speedBeforeAdd = speed
			lastAddHost = host
			// The rate is about to change; judge the new one from scratch.
			meter.reset(current)
			t.manager.log.Debug("opening another connection",
				"item", t.item.ID, "streams", t.table.count(),
				"speed", int64(speed), "from", seg.start)
			spawn(seg, host)
		}
	}
}

// shouldAddStream decides whether another connection is worth opening.
func (t *segmentedTransfer) shouldAddStream(speed float64, lastAdd time.Time) bool {
	if t.table.count() >= t.maxStreams {
		return false
	}
	// Adding a connection already made this host slower once. Asking again
	// would only repeat the experiment with a worse starting point.
	if t.crowded {
		return false
	}
	// A paused or rate-limited transfer looks exactly like a slow one: the
	// bytes are not moving. Splitting it further would open connections
	// that cannot help, because the ceiling is shared across all of them —
	// and with several files running, every one of them would fan out at
	// once for the same wrong reason.
	if t.manager.throttle.isPaused() || t.manager.throttle.limiting() {
		return false
	}
	if speed >= float64(t.slowBelow) {
		return false
	}
	// Let the previous addition settle before judging the rate again.
	return lastAdd.IsZero() || time.Since(lastAdd) >= t.addCooldown
}

// pump fetches one segment, retrying it on its own so a single dropped
// connection does not sink the whole transfer. reserved is the host an extra
// connection's slot was charged against, and "" for a primary; the slot
// itself is released by spawn, never here.
func (t *segmentedTransfer) pump(ctx context.Context, seg *segment, body io.ReadCloser, reserved string) error {
	t.item.streams.Add(1)
	defer t.item.streams.Add(-1)

	extra := reserved != ""
	for attempt := 0; ; attempt++ {
		if body == nil {
			opened, err := t.open(ctx, seg, extra)
			if err != nil {
				if errors.Is(err, errFileChanged) || ctx.Err() != nil {
					return err
				}
				// The host is turning away extra connections. Give the
				// range back and settle for the connections we have,
				// rather than failing a download that is working.
				if extra && refusedExtraConnection(err) && t.table.retire(seg) {
					limit := t.manager.hosts.penalise(reserved)
					t.manager.log.Debug("host refused another connection",
						"item", t.item.ID, "host", reserved, "limit", limit, "err", err)
					return nil
				}
				if attempt >= t.manager.cfg.MaxRetries || !retryableTransfer(err) {
					return err
				}
				if err := util.SleepCtx(ctx, transferRetryDelay(attempt)); err != nil {
					return err
				}
				// An expired signed link is the usual cause; refresh it.
				_ = t.manager.resolveTarget(ctx, t.item)
				continue
			}
			body = opened
		}

		err := t.copy(ctx, seg, body)
		body = nil
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt >= t.manager.cfg.MaxRetries || !retryableTransfer(err) {
			return err
		}
		if err := util.SleepCtx(ctx, transferRetryDelay(attempt)); err != nil {
			return err
		}
		_ = t.manager.resolveTarget(ctx, t.item)
	}
}

// copy drains one response body into the segment's slice of the file.
//
// WriteAt is a positional write, so concurrent segments writing to
// non-overlapping offsets of the same handle is safe and needs no locking.
func (t *segmentedTransfer) copy(ctx context.Context, seg *segment, body io.ReadCloser) error {
	defer body.Close()

	buf, release := borrowChunk()
	defer release()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pos, end := seg.pos.Load(), seg.end.Load()
		if pos >= end {
			// Confirm under the table lock: the segment may have just been
			// extended to take back a range another connection gave up.
			if t.table.finish(seg) {
				return nil
			}
			continue
		}

		// Never read past this segment's end: the supervisor may have
		// shrunk it to hand the tail to another connection.
		limit := int64(len(buf))
		if remaining := end - pos; remaining < limit {
			limit = remaining
		}

		// Every connection draws from the one shared bucket, so the cap is
		// a total across the queue rather than a per-stream allowance.
		// This is also where a paused transfer parks.
		allowed, err := t.manager.throttle.take(ctx, int(limit))
		if err != nil {
			return err
		}
		limit = int64(allowed)

		n, readErr := body.Read(buf[:limit])
		if n > 0 {
			if _, err := t.file.WriteAt(buf[:n], pos); err != nil {
				return fmt.Errorf("write at %d: %w", pos, err)
			}
			seg.pos.Store(pos + int64(n))
			t.item.downloaded.Add(int64(n))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if t.table.finish(seg) {
					return nil
				}
				return io.ErrUnexpectedEOF
			}
			return readErr
		}
	}
}

// open requests a segment's remaining range. An extra connection does not
// retry: if the host turns it away, that answer is wanted immediately so the
// range can be handed back rather than held while the retry budget drains.
func (t *segmentedTransfer) open(ctx context.Context, seg *segment, extra bool) (io.ReadCloser, error) {
	m := t.manager

	m.mu.Lock()
	rawURL := t.item.URL
	headers := maps.Clone(t.item.Headers)
	m.mu.Unlock()

	if rawURL == "" {
		return nil, errors.New("no download URL")
	}
	req, err := m.client.NewRequest(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.Header.Set(httpx.HeaderAccept, httpx.AcceptAny)
	req.Header.Set(httpx.HeaderAcceptEncoding, httpx.EncodingIdentity)

	pos, end := seg.pos.Load(), seg.end.Load()
	req.Header.Set(httpx.HeaderRange,
		"bytes="+strconv.FormatInt(pos, 10)+"-"+strconv.FormatInt(end-1, 10))
	if t.validator != "" {
		req.Header.Set(httpx.HeaderIfRange, t.validator)
	}

	do := m.client.Do
	if extra {
		do = m.client.DoOnce
	}
	resp, err := do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return resp.Body, nil
	case http.StatusOK:
		defer resp.Body.Close()
		// A web page here means the host bounced us, not that the file
		// changed; saying so makes the retry message honest.
		if err := rejectWebPage(resp, t.name); err != nil {
			return nil, err
		}
		// Otherwise If-Range did not match: this is the whole file from a
		// different version, so everything on disk is suspect.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, config.ErrorBodySample))
		return nil, errFileChanged
	default:
		defer resp.Body.Close()
		return nil, statusError(req.URL, resp)
	}
}

// host is the remote this transfer is talking to.
func (t *segmentedTransfer) host() string {
	t.manager.mu.Lock()
	defer t.manager.mu.Unlock()
	return hostOf(t.item.URL)
}

// saveState records segment progress so an interrupted transfer resumes
// rather than starting over.
func (t *segmentedTransfer) saveState() {
	err := saveTransferState(t.part, &transferState{
		Size:      t.table.size,
		Validator: t.validator,
		Segments:  t.table.state(),
	})
	if err != nil {
		t.manager.log.Debug("could not save resume state", "item", t.item.ID, "err", err)
	}
}
