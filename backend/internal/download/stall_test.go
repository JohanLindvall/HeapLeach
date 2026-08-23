package download

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A stalled item goes to the back of the queue rather than failing — with
// its state reset for a fresh attempt, a note saying why it moved, and the
// deferral counted against the retry budget.
func TestDeferStalledSendsTheItemToTheBack(t *testing.T) {
	m := &Manager{
		cfg: &config.Config{MaxRetries: 2},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	waiting := &Item{ID: "waiting", Status: StatusQueued}
	m.queue = []*Item{waiting}

	stalled := &Item{ID: "stalled", Status: StatusRunning}
	if !m.deferStalledLocked(stalled, &stalledError{name: "clip.mp4", after: time.Minute}) {
		t.Fatal("a first stall within the budget was not deferred")
	}
	if len(m.queue) != 2 || m.queue[1] != stalled {
		t.Fatalf("the stalled item is not at the back of the queue: %v", m.queue)
	}
	if stalled.Status != StatusQueued {
		t.Errorf("status = %q, want queued", stalled.Status)
	}
	if stalled.stallDefers != 1 {
		t.Errorf("stallDefers = %d, want 1", stalled.stallDefers)
	}
	if !strings.Contains(stalled.Note, "back of the queue") {
		t.Errorf("note = %q, want it to say where the item went", stalled.Note)
	}

	// The budget is the retry budget: past it, the stall is a failure.
	spent := &Item{ID: "spent", Status: StatusRunning, stallDefers: 2}
	if m.deferStalledLocked(spent, &stalledError{name: "clip.mp4", after: time.Minute}) {
		t.Error("a stall past the budget was deferred rather than failed")
	}

	// A retry the user asked for meanwhile takes precedence: runItem's own
	// retryPending path re-queues with a clean slate, and deferring too
	// would queue the item twice.
	manual := &Item{ID: "manual", Status: StatusRunning, retryPending: true}
	if m.deferStalledLocked(manual, &stalledError{name: "clip.mp4", after: time.Minute}) {
		t.Error("a pending user retry was overridden by a deferral")
	}

	// Any other failure is not a stall's business.
	failed := &Item{ID: "failed", Status: StatusRunning}
	if m.deferStalledLocked(failed, io.ErrUnexpectedEOF) {
		t.Error("an ordinary failure was deferred as though it had stalled")
	}
}

// A user-initiated re-queue starts the stall patience over.
func TestEnqueueResetsStallDeferrals(t *testing.T) {
	m := &Manager{}
	it := &Item{ID: "a", Status: StatusFailed, stallDefers: 3}
	m.enqueueLocked(it)
	if it.stallDefers != 0 {
		t.Errorf("stallDefers = %d after a fresh enqueue, want 0", it.stallDefers)
	}
}

// The whole point of deferral, end to end: with one worker, a transfer that
// stalls must not pin the slot while the rest of the queue waits. The
// stalled item steps aside, the quick one finishes first, and only once the
// stall budget is spent does the staller fail.
func TestStalledTransferYieldsTheWorkerSlot(t *testing.T) {
	// Sends a little and then nothing, holding the connection open — the
	// shape of a host that throttled this address, which a read deadline
	// never catches.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(1<<20))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 8<<10))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)

	quick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("small and immediate payload"))
	}))
	t.Cleanup(quick.Close)

	cfg := &config.Config{
		DownloadDir:  t.TempDir(),
		Concurrency:  1,
		Streams:      1,
		MaxRetries:   1,
		SlowSpeed:    1,
		StallTimeout: 200 * time.Millisecond,
		UserAgent:    config.DefaultUserAgent,
		Language:     config.DefaultLanguage,
		GofileSecret: config.FallbackGofileSecret,
		Timeout:      10 * time.Second,
	}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), cfg.MaxRetries, cfg.Timeout)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(cfg, extractor.NewRegistry(cfg, client), client, log)
	m.Start()
	t.Cleanup(m.Close)

	// Extensions on the paths, so no sniff spends a request deciding what
	// these are, and the queue order is add order.
	slowJob, err := m.Add(hang.URL+"/stall.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	waitForItem(t, m, slowJob)
	quickJob, err := m.Add(quick.URL+"/quick.bin", "")
	if err != nil {
		t.Fatal(err)
	}

	status := func(jobID string) Status {
		for _, job := range m.Snapshot().Jobs {
			if job.ID == jobID && len(job.Items) == 1 {
				return job.Items[0].Status
			}
		}
		return ""
	}

	// The quick item finishes while the staller is still being waited out —
	// before the change, the staller burnt its retries in place and failed
	// first, and only then did the quick one get the worker.
	var sawDeferralNote atomic.Bool
	waitFor(t, 15*time.Second, func() bool {
		for _, job := range m.Snapshot().Jobs {
			if job.ID == slowJob && len(job.Items) == 1 &&
				strings.Contains(job.Items[0].Note, "back of the queue") {
				sawDeferralNote.Store(true)
			}
		}
		return status(quickJob) == StatusDone
	})
	if got := status(slowJob); got == StatusFailed {
		t.Fatal("the stalled item failed before the quick one finished, so it never yielded its slot")
	}
	if !sawDeferralNote.Load() {
		t.Error("the deferred item never said why it went to the back of the queue")
	}

	// Patience is still bounded: the next stall is a failure.
	waitFor(t, 15*time.Second, func() bool { return status(slowJob) == StatusFailed })
	for _, job := range m.Snapshot().Jobs {
		if job.ID == slowJob {
			if !strings.Contains(job.Items[0].Error, "stalled") {
				t.Errorf("failure = %q, want it named as a stall", job.Items[0].Error)
			}
		}
	}
}

// superviseHarness runs the stream supervisor over a synthetic transfer whose
// byte counter the test moves by hand.
func superviseHarness(t *testing.T, cooldown time.Duration) (*segmentedTransfer, *atomic.Int32, func()) {
	t.Helper()
	m := &Manager{
		hosts: newHostLimiter(4),
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	tr := &segmentedTransfer{
		manager:    m,
		item:       &Item{ID: "x", URL: "http://cdn.example.test/f.bin"},
		table:      newSegmentTable(64<<20, 0),
		name:       "x",
		maxStreams: 8,
		slowBelow:  1 << 30, // everything is "slow", so only the stall guard can refuse

		pollInterval:  time.Millisecond,
		probeInterval: 4 * time.Millisecond,
		addCooldown:   cooldown,
		saveInterval:  time.Hour, // never; there is no part file behind this transfer
	}

	var spawns atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tr.supervise(ctx, func(*segment, string) { spawns.Add(1) })
	}()
	return tr, &spawns, func() { cancel(); <-done }
}

// A transfer that is not moving at all must not fan out: the host is serving
// nothing on the connection it has, and another one only spends budget.
func TestSuperviseDoesNotFanOutWhileStalled(t *testing.T) {
	_, spawns, stop := superviseHarness(t, time.Millisecond)
	defer stop()

	time.Sleep(100 * time.Millisecond) // many probe windows, no bytes
	if got := spawns.Load(); got != 0 {
		t.Errorf("opened %d connections against a transfer with no bytes moving", got)
	}
}

// A stall arriving after a connection was added must not be read as that
// connection's fault: the verdict is discarded, and the host's budget keeps
// its limit for when it serves again.
func TestSuperviseDiscardsTheVerdictAcrossAStall(t *testing.T) {
	tr, spawns, stop := superviseHarness(t, time.Hour) // one addition at most
	defer stop()

	var moving atomic.Bool
	moving.Store(true)
	go func() {
		for moving.Load() {
			tr.item.downloaded.Add(512)
			time.Sleep(time.Millisecond)
		}
	}()

	// Slow but moving: the supervisor should open a connection...
	waitFor(t, 5*time.Second, func() bool { return spawns.Load() == 1 })
	// ...and then the host stops serving entirely.
	moving.Store(false)
	time.Sleep(80 * time.Millisecond)

	if tr.crowded {
		t.Error("a stall was judged as crowding, though nothing was being served to anyone")
	}
	if got := tr.manager.hosts.limit("cdn.example.test"); got != 4 {
		t.Errorf("host limit = %d after a stall, want the untouched 4 — a stall must not poison the budget", got)
	}
}
