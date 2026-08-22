package download

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// throttle is the one place transfers are held back: a token bucket shared
// by every connection, plus a gate that parks them all when paused.
//
// Both controls live together because they are the same mechanism seen from
// two angles — pausing is a limit of zero that no amount of waiting will
// satisfy — and because a reader should only ever have to consult one thing
// before it is allowed to move bytes.
// A nil *throttle is a valid, inert one: unlimited and never paused. The
// zero Manager the tests build by hand relies on that.
type throttle struct {
	// active is read on every Read, so it is kept out of the mutex: when
	// nothing is paused and no limit is set, that load is the entire cost.
	active atomic.Bool

	mu     sync.Mutex
	limit  int64 // bytes per second; 0 means unlimited
	tokens float64
	last   time.Time
	paused bool

	// How close the bucket is running to its ceiling, measured over a
	// rolling window. This is what tells the stream supervisor "this
	// transfer is slow" apart from "the ceiling is doing its job", which
	// look identical from the outside.
	//
	// Whether a reader had to *wait* is not a usable signal: the bucket
	// hands out whatever it has rather than blocking, so a transfer pinned
	// exactly at the limit can go a long time without ever waiting.
	windowStart time.Time
	granted     float64
	lastRate    float64
	lastRoll    time.Time
	// gate is closed and replaced whenever pause or limit changes, which
	// is how waiters learn to re-evaluate rather than sleep out their
	// original estimate.
	gate chan struct{}
}

func newThrottle(limit int64) *throttle {
	t := &throttle{limit: limit, gate: make(chan struct{})}
	t.active.Store(limit > 0)
	return t
}

// setLimit sets the ceiling in bytes per second; zero or less is unlimited.
func (t *throttle) setLimit(limit int64) {
	if t == nil {
		return
	}
	if limit < 0 {
		limit = 0
	}
	t.mu.Lock()
	t.limit = limit
	// Tokens accrued under the old limit would let a burst through at the
	// new one, so the bucket restarts empty — and the measured rate, which
	// described the old ceiling, restarts with it.
	t.tokens, t.last = 0, time.Now()
	t.granted, t.lastRate = 0, 0
	t.windowStart, t.lastRoll = time.Time{}, time.Time{}
	t.wakeLocked()
	t.mu.Unlock()
}

func (t *throttle) setPaused(paused bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.paused = paused
	t.wakeLocked()
	t.mu.Unlock()
}

func (t *throttle) isPaused() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.paused
}

func (t *throttle) currentLimit() int64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.limit
}

// recordLocked folds granted bytes into the rolling rate. Caller holds mu.
func (t *throttle) recordLocked(now time.Time, n int) {
	if t.windowStart.IsZero() {
		t.windowStart = now
	}
	t.granted += float64(n)
	if elapsed := now.Sub(t.windowStart); elapsed >= config.ThrottleWindow {
		t.lastRate = t.granted / elapsed.Seconds()
		t.granted, t.windowStart, t.lastRoll = 0, now, now
	}
}

// limiting reports whether the ceiling is what is holding transfers back
// right now, rather than the hosts they are pulling from.
//
// Opening more connections in that state cannot make anything faster — the
// bucket is shared between all of them — and would spend connections a host
// may well refuse. With several files running it is worse still: every one
// of them would look slow and fan out at once.
func (t *throttle) limiting() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.limit <= 0 || t.lastRoll.IsZero() {
		return false
	}
	// A rate measured long ago says nothing about now.
	if time.Since(t.lastRoll) > config.ThrottleWindowStale {
		return false
	}
	return t.lastRate >= config.ThrottleSaturation*float64(t.limit)
}

// wakeLocked releases everyone waiting and refreshes the active flag.
// Caller holds mu.
func (t *throttle) wakeLocked() {
	t.active.Store(t.paused || t.limit > 0)
	close(t.gate)
	t.gate = make(chan struct{})
}

// take blocks until the caller may move at least one byte, and reports how
// many. The answer is never larger than want and never zero without an
// error, so callers can read straight into buf[:n].
func (t *throttle) take(ctx context.Context, want int) (int, error) {
	if want <= 0 {
		return 0, nil
	}
	if t == nil {
		return want, nil
	}
	if !t.active.Load() {
		return want, nil
	}
	for {
		n, wait, gate := t.reserve(want)
		if n > 0 {
			return n, nil
		}
		if wait <= 0 {
			// Paused: nothing accrues, so wait only on the gate.
			wait = time.Hour
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-gate:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// reserve takes what the bucket can spare right now. When it can spare
// nothing it reports how long to wait, and the gate to watch in case the
// settings change first.
func (t *throttle) reserve(want int) (n int, wait time.Duration, gate chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.paused {
		return 0, 0, t.gate
	}
	if t.limit <= 0 {
		return want, 0, nil
	}

	now := time.Now()
	if t.last.IsZero() {
		t.last = now
	}
	t.tokens += now.Sub(t.last).Seconds() * float64(t.limit)
	t.last = now

	// A burst of one second keeps the average honest without forcing a
	// sleep on every single buffer.
	if burst := float64(t.limit); t.tokens > burst {
		t.tokens = burst
	}

	if t.tokens >= 1 {
		n = want
		if float64(n) > t.tokens {
			n = int(t.tokens)
		}
		t.tokens -= float64(n)
		t.recordLocked(now, n)
		return n, 0, nil
	}

	wait = time.Duration((1 - t.tokens) / float64(t.limit) * float64(time.Second))
	if wait < config.ThrottleMinWait {
		wait = config.ThrottleMinWait
	}
	return 0, wait, t.gate
}

// throttledReader is a reader that asks the throttle for permission before
// each read, so a paused or limited transfer holds its connection open
// rather than tearing it down.
type throttledReader struct {
	r   io.Reader
	t   *throttle
	ctx context.Context
}

func (tr *throttledReader) Read(p []byte) (int, error) {
	n, err := tr.t.take(tr.ctx, len(p))
	if err != nil {
		return 0, err
	}
	return tr.r.Read(p[:n])
}

// throttled wraps r unless there is nothing to enforce.
func (m *Manager) throttled(ctx context.Context, r io.Reader) io.Reader {
	return &throttledReader{r: r, t: m.throttle, ctx: ctx}
}

// chargeThrottle pays for bytes that arrived in one piece, blocking until
// the bucket has covered them. A cancelled context simply stops the wait:
// the bytes are already in hand, and the caller's own context check will
// deal with the cancellation.
func (m *Manager) chargeThrottle(ctx context.Context, n int) {
	for n > 0 {
		took, err := m.throttle.take(ctx, n)
		if err != nil {
			return
		}
		n -= took
	}
}
