package download

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A manager with a resolved job in each terminal state and one still
// queued, so the housekeeping calls have something to distinguish.
func seededManager(t *testing.T) (m *Manager, done, failed, queued string) {
	t.Helper()
	m, _ = newTestManager(t)
	add := func(status Status) string {
		job := &Job{ID: newID(), Source: "https://example.test/" + newID(), Title: "t", CreatedAt: time.Now()}
		job.Items = []*Item{{ID: newID(), JobID: job.ID, Name: "f", Status: status}}
		m.mu.Lock()
		m.jobs[job.ID] = job
		m.order = append(m.order, job.ID)
		m.mu.Unlock()
		return job.ID
	}
	return m, add(StatusDone), add(StatusFailed), add(StatusQueued)
}

func TestClearFinishedForgetsOnlyTheTerminal(t *testing.T) {
	m, done, failed, queued := seededManager(t)

	if got := m.ClearFinished(); got != 2 {
		t.Errorf("ClearFinished removed %d jobs, want 2", got)
	}
	snap := m.Snapshot()
	if len(snap.Jobs) != 1 || snap.Jobs[0].ID != queued {
		t.Errorf("left %v, want only the queued job %s", snap.Jobs, queued)
	}
	for _, id := range []string{done, failed} {
		if err := m.CancelJob(id); err != ErrNotFound {
			t.Errorf("cleared job %s still answers: %v", id, err)
		}
	}
	// Nothing left to clear is not an error, and not a change either.
	if got := m.ClearFinished(); got != 0 {
		t.Errorf("a second clear removed %d", got)
	}
}

func TestRemoveJobForgetsWhateverState(t *testing.T) {
	m, _, _, queued := seededManager(t)

	if err := m.RemoveJob(queued); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveJob(queued); err != ErrNotFound {
		t.Errorf("removing twice: %v, want ErrNotFound", err)
	}
	for _, job := range m.Snapshot().Jobs {
		if job.ID == queued {
			t.Error("the removed job is still in the snapshot")
		}
	}
}

func TestCancelJobMarksEveryItem(t *testing.T) {
	m, _, _, queued := seededManager(t)

	if err := m.CancelJob(queued); err != nil {
		t.Fatal(err)
	}
	for _, job := range m.Snapshot().Jobs {
		if job.ID != queued {
			continue
		}
		if job.Status != StatusCanceled {
			t.Errorf("job status = %s, want canceled", job.Status)
		}
		for _, it := range job.Items {
			if it.Status != StatusCanceled {
				t.Errorf("item status = %s, want canceled", it.Status)
			}
		}
	}
	if err := m.CancelJob("nope"); err != ErrNotFound {
		t.Errorf("cancelling a stranger: %v", err)
	}
}

// The settings each refuse what the UI's controls cannot produce, and say so
// rather than clamping silently.
func TestSettingsRefuseTheOutOfRange(t *testing.T) {
	m, _ := newTestManager(t)

	if err := m.SetConcurrency(0); err == nil {
		t.Error("concurrency 0 was accepted")
	}
	if err := m.SetConcurrency(config.MaxConcurrency + 1); err == nil {
		t.Error("concurrency above the ceiling was accepted")
	}
	if err := m.SetConcurrency(3); err != nil {
		t.Errorf("concurrency 3: %v", err)
	}
	if got := m.Snapshot().Concurrency; got != 3 {
		t.Errorf("snapshot concurrency = %d, want 3", got)
	}

	if err := m.SetStreams(0); err == nil {
		t.Error("streams 0 was accepted")
	}
	if err := m.SetStreams(config.MaxStreams + 1); err == nil {
		t.Error("streams above the ceiling was accepted")
	}
	if err := m.SetStreams(2); err != nil {
		t.Errorf("streams 2: %v", err)
	}
	if got := m.Snapshot().Streams; got != 2 {
		t.Errorf("snapshot streams = %d, want 2", got)
	}

	if err := m.SetSpeedLimit(-1); err == nil {
		t.Error("a negative speed limit was accepted")
	}
	if err := m.SetSpeedLimit(1_000_000); err != nil {
		t.Errorf("speed limit: %v", err)
	}
	if got := m.SpeedLimit(); got != 1_000_000 {
		t.Errorf("SpeedLimit = %d, want 1000000", got)
	}
	if got := m.Snapshot().SpeedLimit; got != 1_000_000 {
		t.Errorf("snapshot speed limit = %d", got)
	}
}

// Busy is what a bare desktop run asks before deciding nobody needs it. A job
// still being read counts, and has to: it has no items yet, so the queue is
// empty at exactly the moment work is about to arrive.
func TestBusyCountsAResolvingJob(t *testing.T) {
	m, _ := newTestManager(t)
	if m.Busy() {
		t.Fatal("an empty manager is busy")
	}

	// A page that never answers keeps the job resolving.
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(hold)

	id, err := m.Add(srv.URL+"/slow", "")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Busy() {
		t.Error("a resolving job does not count as busy")
	}
	if err := m.CancelJob(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return !m.Busy() })
}

// A stream subscribed after the manager has closed must end at once, or a
// browser reloading during shutdown would hold the server to its deadline.
func TestSubscribeAfterCloseIsAlreadyClosed(t *testing.T) {
	m, _ := newTestManager(t)
	m.Close()

	events, unsubscribe := m.Subscribe()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("a closed manager delivered a snapshot")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the channel from a closed manager stayed open")
	}
	unsubscribe() // must not panic on a channel that was never registered
}

// With nobody subscribed the broadcaster leaves the state alone: the dirty
// flag stays up for the next arrival, who is sent the snapshot as their
// first frame, and nothing is encoded for no reader in the meantime.
func TestBroadcastWaitsForASubscriber(t *testing.T) {
	m, _ := newTestManager(t)

	m.markDirty()
	time.Sleep(3 * config.ProgressTick)
	if !m.dirty.Load() {
		t.Fatal("the broadcaster spent the dirty flag with nobody listening")
	}

	events, unsubscribe := m.Subscribe()
	defer unsubscribe()
	select {
	case payload := <-events:
		if len(payload) == 0 {
			t.Error("an empty snapshot was published")
		}
	case <-time.After(5 * config.ProgressTick):
		t.Fatal("no snapshot reached the subscriber")
	}
	if m.dirty.Load() {
		t.Error("publishing did not clear the dirty flag")
	}
}

// The snapshot's host count is a fact about the build, not about the queue,
// and a manager built without a registry still has to be able to say so.
func TestSnapshotHostCountIsTheRegistrySize(t *testing.T) {
	cfg := &config.Config{DownloadDir: t.TempDir(), Concurrency: 1,
		UserAgent: config.DefaultUserAgent, Language: config.DefaultLanguage}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), 0, time.Second)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := extractor.NewRegistry(cfg, client)

	m := New(cfg, reg, client, log)
	t.Cleanup(m.Close)
	if got, want := m.Snapshot().HostCount, len(reg.Hosts()); got != want {
		t.Errorf("HostCount = %d, want %d", got, want)
	}

	bare := New(cfg, nil, client, log)
	t.Cleanup(bare.Close)
	if got := bare.Snapshot().HostCount; got != 0 {
		t.Errorf("HostCount without a registry = %d, want 0", got)
	}
}
