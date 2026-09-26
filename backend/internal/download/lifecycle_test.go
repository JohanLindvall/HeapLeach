package download

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

func TestRetryDoesNotStartASecondActiveExtraction(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetPaused(true)
	m.mu.Lock()
	m.jobs["job"] = &Job{ID: "job", Source: "https://example.test/file.bin", resolving: true}
	m.mu.Unlock()
	if err := m.RetryJob("job"); err == nil {
		t.Fatal("retry accepted while the source is still resolving")
	}
}

func TestSupersededExtractionCannotPublish(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetPaused(true)
	job := &Job{ID: "job", Source: "https://example.test/file.bin", resolving: true, resolveID: 2}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()
	m.wg.Add(1)
	m.resolve(context.Background(), job, 1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !job.resolving || len(job.Items) != 0 || len(m.queue) != 0 {
		t.Fatalf("old extraction changed its replacement: %+v", job)
	}
}

func TestCloseRejectsNewWork(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetPaused(true)
	id, err := m.Add("https://example.test/file.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	item := waitForItem(t, m, id)
	m.Close()
	if _, err := m.Add("https://example.test/next.bin", ""); !errors.Is(err, ErrClosed) {
		t.Errorf("Add after Close: %v", err)
	}
	if err := m.RetryJob(id); !errors.Is(err, ErrClosed) {
		t.Errorf("RetryJob after Close: %v", err)
	}
	if err := m.RetryItem(id, item); !errors.Is(err, ErrClosed) {
		t.Errorf("RetryItem after Close: %v", err)
	}
	m.Start() // Starting a closed manager must not leave new goroutines behind.
}

func TestCloseCanRaceSubmissions(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetPaused(true)
	var callers sync.WaitGroup
	for range 40 {
		callers.Go(func() {
			_, err := m.Add("https://example.test/file.bin", "")
			if err != nil && !errors.Is(err, ErrClosed) {
				t.Errorf("Add: %v", err)
			}
		})
	}
	m.Close()
	callers.Wait()
}

func TestResolverDeadlineIsFailedRatherThanUserCanceled(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetPaused(true)
	id, err := m.Add("https://example.test/file.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	itemID := waitForItem(t, m, id)
	m.mu.Lock()
	item, _ := m.findItemLocked(id, itemID)
	item.resolve = func(context.Context) (*extractor.Target, error) { return nil, context.DeadlineExceeded }
	m.mu.Unlock()
	m.SetPaused(false)
	waitFor(t, 5*time.Second, func() bool { return itemStatus(m, id, itemID).Terminal() })
	if got := itemStatus(m, id, itemID); got != StatusFailed {
		t.Fatalf("deadline reported as %s", got)
	}
}

func TestCancelingPausedQueueReleasesCanceledWork(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetPaused(true)
	id, err := m.Add("https://example.test/file.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	waitForItem(t, m, id)
	if err := m.CancelJob(id); err != nil {
		t.Fatal(err)
	}
	if m.Busy() {
		t.Fatal("canceled, paused queue still reports work")
	}
	m.ClearFinished()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.queue) != 0 {
		t.Fatal("cleared queue retained canceled items")
	}
}

func TestClearFinishedCancelsPendingRetry(t *testing.T) {
	item := &Item{ID: "item", Status: StatusCanceled, inFlight: true, retryPending: true}
	job := &Job{ID: "job", Items: []*Item{item}}
	m := &Manager{jobs: map[string]*Job{job.ID: job}, order: []string{job.ID}}
	if got := m.ClearFinished(); got != 1 {
		t.Fatalf("removed %d jobs", got)
	}
	if item.retryPending {
		t.Fatal("removed item can be requeued by its exiting worker")
	}
}
