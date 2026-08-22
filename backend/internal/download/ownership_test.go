package download

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// An item a worker still owns must never be handed to a second worker.
// Cancelling flips Status to canceled while the worker is still winding
// down, so Status alone cannot gate re-dispatch: doing so gave two
// goroutines the same item and the same .part file, and whichever renamed
// first left the other with "no such file or directory".
func TestEnqueueDefersWhileInFlight(t *testing.T) {
	m := &Manager{}
	it := &Item{ID: "abc", Status: StatusCanceled, inFlight: true}
	it.downloaded.Store(4096)

	m.enqueueLocked(it)

	if len(m.queue) != 0 {
		t.Fatalf("an in-flight item was queued for a second worker")
	}
	if !it.retryPending {
		t.Error("the retry was dropped instead of being recorded")
	}
	if it.downloaded.Load() != 4096 {
		t.Error("progress was reset while a worker was still using the item")
	}

	// Once the worker releases it, the pending retry takes effect.
	it.inFlight = false
	it.retryPending = false
	m.enqueueLocked(it)

	if len(m.queue) != 1 || m.queue[0] != it {
		t.Fatalf("a released item was not queued: %d entries", len(m.queue))
	}
	if it.Status != StatusQueued {
		t.Errorf("status = %q, want %q", it.Status, StatusQueued)
	}
	if it.downloaded.Load() != 0 {
		t.Error("progress was not reset for the fresh attempt")
	}
}

func TestNextSkipsInFlightItems(t *testing.T) {
	owned := &Item{ID: "owned", Status: StatusQueued, inFlight: true}
	free := &Item{ID: "free", Status: StatusQueued}

	m := &Manager{limit: 4, queue: []*Item{owned, free}}
	got := m.nextLocked()

	if got != free {
		t.Fatalf("dispatched %v, want the item no worker owns", got)
	}
}

func TestCancelClearsPendingRetry(t *testing.T) {
	it := &Item{Status: StatusRunning, inFlight: true, retryPending: true}
	cancelItemLocked(it)

	if it.retryPending {
		t.Error("a cancel must override a retry that has not started")
	}
	if it.Status != StatusCanceled {
		t.Errorf("status = %q, want %q", it.Status, StatusCanceled)
	}
}

// Hammering cancel-then-retry must never produce two concurrent transfers of
// the same file, nor a duplicate output like "file (2).bin".
func TestCancelRetryNeverDoubleRuns(t *testing.T) {
	const body = 512 << 10

	var concurrent, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := concurrent.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		// Stop counting the moment the client goes away, rather than when
		// this handler next gets round to noticing. The test cancels and
		// retries in the same breath, so a handler still asleep between
		// chunks would otherwise overlap the next one and look like a
		// double dispatch when only one worker ever existed.
		var release sync.Once
		drop := func() { release.Do(func() { concurrent.Add(-1) }) }
		defer drop()

		finished := make(chan struct{})
		defer close(finished)
		go func() {
			select {
			case <-r.Context().Done():
				drop()
			case <-finished:
			}
		}()

		// Ignore Range so every attempt is a clean restart, and stream
		// slowly enough that a cancel lands mid-transfer.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(body))
		chunk := make([]byte, 16<<10)
		for sent := 0; sent < body; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(8 * time.Millisecond):
			}
		}
	}))
	defer server.Close()

	// Measure the invariant itself: how many workers hold the item at once.
	// Counting HTTP handlers cannot answer that, because a cancelled
	// handler lingers on the server after the worker has let go.
	var workers, workerPeak atomic.Int32
	itemHeld = func(_ *Item, held bool) {
		if !held {
			workers.Add(-1)
			return
		}
		n := workers.Add(1)
		for {
			old := workerPeak.Load()
			if n <= old || workerPeak.CompareAndSwap(old, n) {
				break
			}
		}
	}
	t.Cleanup(func() { itemHeld = nil })

	m, dir := newTestManager(t)
	jobID, err := m.Add(server.URL+"/payload.bin", "")
	if err != nil {
		t.Fatal(err)
	}

	itemID := waitForItem(t, m, jobID)

	for round := range 12 {
		if p := workerPeak.Load(); p > 1 {
			t.Fatalf("round %d: %d workers held the item at once, want 1 "+
				"(it was dispatched twice)", round, p)
		}
		if !waitForCond(5*time.Second, func() bool {
			return itemStatus(m, jobID, itemID) == StatusRunning
		}) {
			t.Fatalf("round %d: item never resumed running after cancel+retry "+
				"(status %q, peak concurrency %d)",
				round, itemStatus(m, jobID, itemID), peak.Load())
		}
		if err := m.CancelItem(jobID, itemID); err != nil {
			t.Fatal(err)
		}
		// Retry immediately: the worker is still winding down here, which
		// is exactly the window that used to double-dispatch.
		if err := m.RetryItem(jobID, itemID); err != nil {
			t.Fatalf("retry: %v", err)
		}
	}

	if !waitForCond(30*time.Second, func() bool {
		return itemStatus(m, jobID, itemID) == StatusDone
	}) {
		t.Fatalf("item never completed (status %q, peak concurrency %d)",
			itemStatus(m, jobID, itemID), peak.Load())
	}

	if p := workerPeak.Load(); p > 1 {
		t.Errorf("%d workers held the item at once, want 1", p)
	}
	t.Logf("peak workers=%d, peak server handlers=%d (handlers may overlap "+
		"while a cancelled one winds down)", workerPeak.Load(), peak.Load())

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 {
		t.Errorf("wrote %d files %v, want exactly one", len(names), names)
	}
	for _, n := range names {
		if strings.Contains(n, ".part") {
			t.Errorf("left a partial file behind: %s", n)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		DownloadDir:  dir,
		Concurrency:  1,
		UserAgent:    config.DefaultUserAgent,
		Language:     config.DefaultLanguage,
		GofileSecret: config.FallbackGofileSecret,
		MaxRetries:   0,
		Timeout:      30 * time.Second,
	}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), cfg.MaxRetries, cfg.Timeout)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	m := New(cfg, extractor.NewRegistry(cfg, client), client, log)
	m.Start()
	t.Cleanup(m.Close)
	return m, dir
}

func waitForItem(t *testing.T, m *Manager, jobID string) string {
	t.Helper()
	var id string
	waitFor(t, 10*time.Second, func() bool {
		for _, job := range m.Snapshot().Jobs {
			if job.ID == jobID && len(job.Items) == 1 {
				id = job.Items[0].ID
				return true
			}
		}
		return false
	})
	return id
}

func itemStatus(m *Manager, jobID, itemID string) Status {
	for _, job := range m.Snapshot().Jobs {
		if job.ID != jobID {
			continue
		}
		for _, it := range job.Items {
			if it.ID == itemID {
				return it.Status
			}
		}
	}
	return ""
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	if !waitForCond(timeout, cond) {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// waitForCond polls until cond holds, reporting whether it did.
func waitForCond(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
