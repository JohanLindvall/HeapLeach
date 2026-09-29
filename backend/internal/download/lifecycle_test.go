package download

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
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

// A job is shown with its extractor's note but filed under its title alone.
// The note changes when a cap is raised or a rate limit lifts; a folder that
// changed with it filed the next run somewhere new and downloaded every file
// again.
func TestAJobIsFiledUnderItsTitleNotItsNote(t *testing.T) {
	m := &Manager{jobs: map[string]*Job{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	job := &Job{ID: "job", Source: "https://example.test/?search=a+band"}
	res := &extractor.Result{
		Title: "a band",
		Note:  "224 of 2700 albums",
		Files: []extractor.File{
			{Name: "one.mp4", Dir: "First Album", URL: "https://example.test/1"},
			{Name: "two.mp4", Dir: "Second Album", URL: "https://example.test/2"},
		},
	}

	m.mu.Lock()
	m.applyResultLocked(job, "stub", res)
	m.mu.Unlock()

	if job.Title != "a band (224 of 2700 albums)" {
		t.Errorf("title = %q, want the note shown beside the name", job.Title)
	}
	for _, it := range job.Items {
		if !strings.HasPrefix(it.Dir, "a band"+string(filepath.Separator)) {
			t.Errorf("%s filed under %q, want the plain title's folder", it.Name, it.Dir)
		}
	}
}
