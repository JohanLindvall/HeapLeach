package download

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

func TestThrottleUnlimitedTakesEverything(t *testing.T) {
	th := newThrottle(0)
	n, err := th.take(context.Background(), 4096)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if n != 4096 {
		t.Errorf("took %d bytes, want all 4096", n)
	}
}

// A nil throttle is the zero value a hand-built Manager gets; it must be
// inert rather than a panic.
func TestNilThrottleIsInert(t *testing.T) {
	var th *throttle
	if th.isPaused() {
		t.Error("a nil throttle is not paused")
	}
	if got := th.currentLimit(); got != 0 {
		t.Errorf("nil limit = %d, want 0", got)
	}
	n, err := th.take(context.Background(), 1024)
	if err != nil || n != 1024 {
		t.Errorf("take on nil = (%d, %v), want (1024, nil)", n, err)
	}
	th.setPaused(true)
	th.setLimit(1000)
}

func TestThrottleLimitsRate(t *testing.T) {
	const limit = 40 << 10 // 40 KB/s
	th := newThrottle(limit)
	ctx := context.Background()

	// Drain the initial second of burst so the measurement covers the
	// steady state rather than the bucket's head start.
	for drained := 0; drained < limit; {
		n, err := th.take(ctx, limit-drained)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		drained += n
	}

	start := time.Now()
	moved := 0
	for moved < limit/2 {
		n, err := th.take(ctx, 4096)
		if err != nil {
			t.Fatalf("take: %v", err)
		}
		moved += n
	}
	elapsed := time.Since(start)

	// Half a second of budget should take roughly half a second. The
	// bounds are loose: this asserts that a limit is enforced at all, not
	// that the scheduler is precise.
	if elapsed < 250*time.Millisecond {
		t.Errorf("moved %d bytes in %s; the limit is not being enforced", moved, elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("moved %d bytes in %s; far slower than the limit asks", moved, elapsed)
	}
}

func TestThrottlePauseBlocksUntilResumed(t *testing.T) {
	th := newThrottle(0)
	th.setPaused(true)

	done := make(chan int, 1)
	go func() {
		n, err := th.take(context.Background(), 1024)
		if err != nil {
			done <- -1
			return
		}
		done <- n
	}()

	select {
	case n := <-done:
		t.Fatalf("take returned %d while paused; it should have blocked", n)
	case <-time.After(120 * time.Millisecond):
	}

	th.setPaused(false)
	select {
	case n := <-done:
		if n != 1024 {
			t.Errorf("after resuming, take returned %d, want 1024", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("take did not return after the pause was lifted")
	}
}

func TestThrottlePauseRespectsContext(t *testing.T) {
	th := newThrottle(0)
	th.setPaused(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := th.take(ctx, 1024)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelling should end the wait with an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling did not release a paused reader")
	}
}

// Pausing must stop the dispatcher handing out work, or a pause would only
// stop the transfers that happened to be running.
func TestPausedQueueDispatchesNothing(t *testing.T) {
	m := &Manager{
		limit:    4,
		throttle: newThrottle(0),
		queue:    []*Item{{ID: "a", Status: StatusQueued}},
	}
	if it := m.nextLocked(); it == nil {
		t.Fatal("an unpaused queue should hand out its item")
	}

	m.queue = []*Item{{ID: "b", Status: StatusQueued}}
	m.throttle.setPaused(true)
	if it := m.nextLocked(); it != nil {
		t.Errorf("a paused queue handed out %q", it.ID)
	}
	if len(m.queue) != 1 {
		t.Error("a paused queue should leave its items queued")
	}
}

func TestThrottledReaderStopsAtTheLimit(t *testing.T) {
	th := newThrottle(1 << 20)
	// Spend the burst so the next read is the one being measured.
	if _, err := th.take(context.Background(), 1<<20); err != nil {
		t.Fatalf("drain: %v", err)
	}

	src := strings.NewReader(strings.Repeat("x", 1<<20))
	r := &throttledReader{r: src, t: th, ctx: context.Background()}

	buf := make([]byte, 1<<20)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n == 0 {
		t.Fatal("a throttled read returned nothing")
	}
	if n == len(buf) {
		t.Error("a throttled read handed over the whole buffer despite an empty bucket")
	}
}

// saturate drives the throttle flat out for d, which is how a transfer
// pinned at the ceiling behaves.
func saturate(t *testing.T, th *throttle, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := th.take(ctx, 4096); err != nil {
			t.Fatalf("take: %v", err)
		}
	}
}

// The supervisor must be able to tell a slow host from its own ceiling, or
// a rate-limited transfer would keep opening connections that cannot help.
func TestThrottleReportsWhenItIsTheBottleneck(t *testing.T) {
	th := newThrottle(0)
	if th.limiting() {
		t.Error("an unlimited throttle is never the bottleneck")
	}

	th.setLimit(8 << 10) // 8 KB/s
	if th.limiting() {
		t.Error("a limit nothing has run against yet is not yet binding")
	}

	saturate(t, th, config.ThrottleWindow+300*time.Millisecond)
	if !th.limiting() {
		t.Error("a throttle running at its ceiling is the bottleneck")
	}

	// Lifting the limit lifts the diagnosis with it.
	th.setLimit(0)
	if th.limiting() {
		t.Error("an unlimited throttle cannot be the bottleneck")
	}
}

// A ceiling nowhere near being reached must not be blamed for a slow host.
func TestThrottleIsNotTheBottleneckWellBelowTheCeiling(t *testing.T) {
	th := newThrottle(10 << 20) // 10 MB/s
	ctx := context.Background()

	deadline := time.Now().Add(config.ThrottleWindow + 300*time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := th.take(ctx, 4096); err != nil {
			t.Fatalf("take: %v", err)
		}
		time.Sleep(20 * time.Millisecond) // ~200 KB/s, far under the cap
	}
	if th.limiting() {
		t.Error("a transfer far below the ceiling is slow for its own reasons")
	}
}

func TestShouldAddStreamRespectsTheCeiling(t *testing.T) {
	newTransfer := func(th *throttle) *segmentedTransfer {
		return &segmentedTransfer{
			manager:    &Manager{throttle: th},
			item:       &Item{ID: "i"},
			table:      newSegmentTable(1<<20, 0),
			maxStreams: 8,
			slowBelow:  2 << 20,
		}
	}

	// A genuinely slow transfer still fans out.
	if tr := newTransfer(newThrottle(0)); !tr.shouldAddStream(1<<10, time.Time{}) {
		t.Error("a slow transfer with no ceiling should open another connection")
	}

	// One held at the ceiling does not.
	th := newThrottle(8 << 10)
	saturate(t, th, config.ThrottleWindow+300*time.Millisecond)
	if tr := newTransfer(th); tr.shouldAddStream(1<<10, time.Time{}) {
		t.Error("a transfer held at the ceiling should not open another connection")
	}

	// Nor does a paused one.
	paused := newThrottle(0)
	paused.setPaused(true)
	if tr := newTransfer(paused); tr.shouldAddStream(1<<10, time.Time{}) {
		t.Error("a paused transfer should not open another connection")
	}
}
