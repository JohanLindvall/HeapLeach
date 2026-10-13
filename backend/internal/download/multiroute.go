// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

// A multi-route transfer fetches one file through several proxy routes at
// once. It is for hosts whose free downloads are capped links, each allowing
// one connection from the address that obtained it: a second connection on
// a route would be refused, and a link is no use from another address. So
// each extra stream needs a route of its own and a free download of its own
// on it, and adds its own cap where splitting one link would add nothing.
//
// Extra streams take idle routes only. None starts while a file of the same
// service is due to start and not running (routeWanted), and a route still
// being obtained is given up the moment one is. Asking whether the dispatcher
// found a route would not do: most routes it can lease are untried, and most
// of those are dead, so it nearly always finds one, and the files cycling
// through them would be racing extra streams for the few that work. An extra
// stream already reading keeps its route: the address has spent its free
// download on it, and a waiting file given it would only be told to come
// back in an hour.
//
// Each connection asks for one range at a time, of at most RouteRangeSize,
// and reads every response to its end, so the next request goes out on the
// same connection and the host never sees a second one. Splits leave alone
// what a connection has asked for (segmentTable.splitUnrequested). The main
// route reads one range at a time, whatever a sidecar held, and a range
// nobody is reading, after a restart or from an extra stream that stopped,
// is claimed by the next connection to finish its own.

// streamSource is where an extra stream reads: its route's context, which
// carries the route to every request, and the link its free download there
// was issued.
type streamSource struct {
	ctx     context.Context
	url     string
	headers httpx.Header
	discard func()
	lease   *proxy.Lease
	release func()
	// bytes is what the stream has read, which is its route's to be scored
	// on rather than the item's own route's.
	bytes atomic.Int64
}

// errRangeDone is copy's word that the range a multi-route connection asked
// for has arrived, and its segment goes on in the next request.
var errRangeDone = errors.New("requested range done")

var errNoIdleRoute = errors.New("no idle route")

var errExtraStalled = errors.New("extra stream stalled")

type sourceResult struct {
	src    *streamSource
	cancel context.CancelFunc
	err    error
}

// routeSourceLocked returns the function that gives a transfer of it an
// extra stream on a route of its own, or nil when it cannot have one: it is
// not on a proxy route, its host's pacing is not per route, nothing can
// mint it a link, or the streams setting allows one connection. Caller
// holds mu.
func (m *Manager) routeSourceLocked(it *Item) func(context.Context) (*streamSource, error) {
	if it.route == nil || it.pace == nil || !it.pace.PerRoute || it.resolve == nil ||
		m.proxies == nil || !m.proxyEnabled || m.streams < 2 {
		return nil
	}
	pool, pace, resolve := m.proxies, *it.pace, it.resolve
	return func(ctx context.Context) (*streamSource, error) {
		m.mu.Lock()
		if !m.proxyEnabled || m.routeWantedLocked(pace.Group) {
			m.mu.Unlock()
			return nil, errNoIdleRoute
		}
		// Drawn like any file's route: the best proven one while one is
		// free, otherwise an untested one at random.
		lease := pool.AcquireFor(pace.Group, "", pace.Files, it.Size-it.downloaded.Load(), pace.WAF)
		m.mu.Unlock()
		if lease == nil {
			return nil, errNoIdleRoute
		}
		release := func() {
			lease.Release()
			// A file may be waiting for exactly this route.
			m.signal()
		}
		if err := m.claimExit(ctx, nil, lease); err != nil {
			release()
			if errors.Is(err, errExitInUse) {
				return nil, errNoIdleRoute
			}
			return nil, err
		}
		// The item's note is its main route's; an extra stream's CAPTCHA and
		// timer would only overwrite it.
		start := time.Now()
		target, err := resolve(extractor.WithResolveNote(lease.Context(ctx), func(string) {}))
		if err == nil && (target == nil || target.URL == "") {
			err = errors.New("resolver returned no download URL")
		}
		if err != nil {
			if ctx.Err() == nil {
				if wait, ok := errors.AsType[*extractor.WaitError](err); ok {
					lease.Cooldown(wait.Until)
				} else if _, refused := errors.AsType[*extractor.RefusedError](err); refused {
					lease.Failed(err, time.Since(start), false)
					lease.Cooldown(time.Now().Add(config.ProxyAddressRefusedWait))
				}
			}
			release()
			return nil, err
		}
		return &streamSource{ctx: lease.Context(ctx), url: target.URL, headers: target.Headers,
			discard: target.Discard, lease: lease, release: release}, nil
	}
}

// routeWant is routeWantedLocked's answer for a group, and when it was found.
type routeWant struct {
	at     time.Time
	wanted bool
}

// routeWantedLocked reports a file of the group due to start and not
// running: one a route given to an extra stream could have served. Every
// multi-route transfer asks, several times a second, and a queue can hold
// tens of thousands of files, so the answer is kept for wantedRecheck.
// Caller holds mu.
func (m *Manager) routeWantedLocked(group string) bool {
	now := time.Now()
	if w, ok := m.routeWanted[group]; ok && now.Sub(w.at) < m.timings.wantedRecheck {
		return w.wanted
	}
	wanted := false
	// Nor is any file while the queue can start none: at its concurrency
	// limit, paused or short of room, a waiting file waits for a slot, not a
	// route, and the transfer whose end frees the slot gives its route back
	// with it. Counted, fifty files queued behind the limit kept 254 running
	// ones on a stream apiece, with routes to spare.
	if m.running < m.limit && !m.throttle.isPaused() && !m.lowOnSpace() {
		for _, it := range m.queue {
			// A file its job holds back is not due to start: counted, a job
			// paused with hundreds of files queued kept every other file of
			// the service from extra streams for as long as it stayed paused.
			if it.Status == StatusQueued && !it.inFlight && !it.notBefore.After(now) &&
				it.pace != nil && it.pace.Group == group && !m.jobHeldLocked(it) {
				wanted = true
				break
			}
		}
	}
	if m.routeWanted == nil {
		m.routeWanted = make(map[string]routeWant)
	}
	m.routeWanted[group] = routeWant{at: now, wanted: wanted}
	return wanted
}

func (m *Manager) routeIsWanted(group string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routeWantedLocked(group)
}

// nextSegment is what a multi-route connection reads once its range is
// done: a range nobody is reading, or else half of what is left of the
// widest one beyond what its connection has asked for.
func (t *segmentedTransfer) nextSegment() *segment {
	if seg := t.table.claim(); seg != nil {
		return seg
	}
	return t.table.splitUnrequested(config.MinSegmentSize)
}

// superviseRoutes is supervise for a multi-route transfer. It opens extra
// streams, a route at a time and only while the transfer is slow, gives up a
// route still being obtained when a file wants one, credits each stream's
// bytes to its own route, drops a stream gone silent, and sets the main
// route reading again whenever a range is left with nobody reading it.
func (t *segmentedTransfer) superviseRoutes(ctx context.Context,
	spawnPrimary func(*segment, io.ReadCloser), spawnSource func(*segment, *streamSource, chan struct{})) {
	ticker := time.NewTicker(t.pollInterval)
	defer ticker.Stop()

	type extra struct {
		src     *streamSource
		cancel  context.CancelFunc
		done    chan struct{}
		counted int64     // src.bytes when last credited to the route
		sampled time.Time // when that was
		movedAt time.Time // when its bytes last moved
	}
	var (
		extras                []*extra
		pending               chan sourceResult // while a route is being obtained
		pendingCancel         context.CancelFunc
		sinceProbe, sinceSave time.Duration
		lastAdd               time.Time
		// Each extra stream costs a CAPTCHA, so ones that get nowhere are
		// tried less and less often: once per addCooldown, doubling with
		// each in a row, up to RouteExtraBackoffMax.
		failures    int
		nextAcquire time.Time
	)
	backOff := func() {
		failures++
		wait := min(t.addCooldown<<min(failures, 20), config.RouteExtraBackoffMax)
		nextAcquire = time.Now().Add(wait)
	}
	defer func() {
		// A route being obtained as the transfer ends is given back.
		if pending != nil {
			pendingCancel()
			if res := <-pending; res.src != nil {
				res.src.release()
			}
		}
		for _, e := range extras {
			e.cancel()
		}
	}()
	lastCount := t.item.downloaded.Load()
	meter := newSpeedMeter(config.SpeedWindow, lastCount)
	stall := t.manager.stallTimeout()

	for {
		select {
		case <-ctx.Done():
			return
		case res := <-pending:
			pending, pendingCancel = nil, nil
			if res.err != nil {
				res.cancel()
				if !errors.Is(res.err, errNoIdleRoute) && ctx.Err() == nil {
					// A route that could not carry the request has cost the
					// host nothing, and the pool has already set it aside:
					// the next draw is another one, and most untested
					// routes are dead.
					if !routeBroken(res.err) {
						backOff()
					}
					t.manager.log.Debug("no extra stream from that route", "item", t.item.ID, "err", res.err)
				}
				continue
			}
			var seg *segment
			if !t.manager.routeIsWanted(t.group) {
				seg = t.nextSegment()
			}
			if seg == nil {
				res.cancel()
				res.src.release()
				continue
			}
			now := time.Now()
			e := &extra{src: res.src, cancel: res.cancel, done: make(chan struct{}), sampled: now, movedAt: now}
			extras = append(extras, e)
			t.manager.log.Debug("reading a range through a route of its own",
				"item", t.item.ID, "route", res.src.lease.ID(), "streams", 1+len(extras), "from", seg.pos.Load())
			spawnSource(seg, res.src, e.done)
		case <-ticker.C:
			if t.table.complete() {
				return
			}
			sinceSave += t.pollInterval
			if sinceSave >= t.saveInterval {
				sinceSave = 0
				t.saveState()
			}
			live := extras[:0]
			for _, e := range extras {
				select {
				case <-e.done:
					e.cancel()
					switch {
					case e.src.bytes.Load() > 0:
						failures = 0
					case !t.table.complete():
						backOff() // a link that served nothing
					}
				default:
					live = append(live, e)
				}
			}
			clear(extras[len(live):])
			extras = live

			wanted := t.manager.routeIsWanted(t.group)
			if wanted && pending != nil {
				// Its result, arriving on pending, gives the route back.
				pendingCancel()
			}
			if !t.primaryActive.Load() {
				if seg := t.table.claim(); seg != nil {
					spawnPrimary(seg, nil)
				}
			}

			sinceProbe += t.pollInterval
			if sinceProbe < t.probeInterval {
				continue
			}
			elapsed := sinceProbe
			sinceProbe = 0
			now := time.Now()
			paused := t.manager.throttle.isPaused()
			limited := paused || t.manager.throttle.currentLimit() > 0 || t.item.limiter.currentLimit() > 0
			for _, e := range extras {
				n := e.src.bytes.Load()
				if n != e.counted || paused {
					e.movedAt = now
				}
				e.src.lease.Progress(now, n-e.counted, now.Sub(e.sampled), limited)
				e.counted, e.sampled = n, now
				// The transfer's own watchdog waits for every connection
				// to go quiet; one silent stream would otherwise hold its
				// range until the rest of the file was done.
				if now.Sub(e.movedAt) >= stall {
					t.manager.log.Debug("an extra stream's route went silent", "item", t.item.ID, "route", e.src.lease.ID())
					e.src.lease.Failed(errExtraStalled, now.Sub(e.movedAt), true)
					e.cancel()
					e.movedAt = now
				}
			}

			current := t.item.downloaded.Load()
			moved := current != lastCount
			lastCount = current
			if !moved {
				meter.reset(current)
				continue
			}
			speed, stable := meter.observe(current, elapsed)
			if !stable || wanted || pending != nil || 1+len(extras) >= t.maxStreams ||
				speed >= float64(t.slowBelow) || paused || t.manager.throttle.limiting() || t.item.limiter.limiting() {
				continue
			}
			if !lastAdd.IsZero() && time.Since(lastAdd) < t.addCooldown || now.Before(nextAcquire) {
				continue
			}
			// The queue sets up only so many at once: each is a hunt and a
			// CAPTCHA, which the files' own routes need first.
			most := int32(t.manager.timings.extraAcquisitions)
			if most <= 0 {
				most = config.RouteExtraAcquisitions
			}
			if t.manager.extraAcquiring.Add(1) > most {
				t.manager.extraAcquiring.Add(-1)
				continue
			}
			lastAdd = time.Now()
			meter.reset(current)
			acquire, cancel := context.WithCancel(ctx)
			pending, pendingCancel = make(chan sourceResult, 1), cancel
			go func(out chan<- sourceResult) {
				src, err := t.routeSource(acquire)
				t.manager.extraAcquiring.Add(-1)
				out <- sourceResult{src: src, cancel: cancel, err: err}
			}(pending)
		}
	}
}

// pumpSource reads ranges through an extra stream's route until nothing is
// left for it, its route fails it, or the transfer ends. None of those fails
// the file: what it had not read is claimed again by another connection.
func (t *segmentedTransfer) pumpSource(seg *segment, src *streamSource) {
	t.item.streams.Add(1)
	t.item.routeExtras.Add(1)
	defer t.item.streams.Add(-1)
	defer t.item.routeExtras.Add(-1)
	for seg != nil {
		body, err := t.open(src.ctx, seg, true, src)
		if err == nil {
			err = t.copy(src.ctx, seg, body, &src.bytes)
		}
		if errors.Is(err, errRangeDone) {
			continue
		}
		if err != nil {
			// What the failed flush was holding is not on disk.
			if flushed := seg.flushed.Load(); seg.pos.Load() > flushed {
				lost := seg.pos.Swap(flushed) - flushed
				t.item.downloaded.Add(-lost)
				t.item.extraBytes.Add(-lost)
				src.bytes.Add(-lost)
			}
			// The link is bound to an address this route did not present: a
			// route whose egress moves, which is the route's fault.
			if refusedForAddress(err) {
				if src.discard != nil {
					src.discard()
				}
				src.lease.Failed(err, 0, false)
			}
			t.table.release(seg)
			if src.ctx.Err() == nil {
				t.manager.log.Debug("an extra stream's route stopped", "item", t.item.ID, "route", src.lease.ID(), "err", err)
			}
			return
		}
		seg = t.nextSegment()
	}
}
