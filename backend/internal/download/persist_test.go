package download

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A logger that goes nowhere: these tests are about the file, and a warning
// about an unusable one is expected output rather than a failure.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newSavedManager(t *testing.T) (*Manager, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "state", "queue.json")
	return &Manager{stateFile: file, jobs: map[string]*Job{}, log: testLogger()}, file
}

func TestQueueSurvivesARoundTrip(t *testing.T) {
	m, file := newSavedManager(t)

	done := &Item{ID: "a", Name: "one.mp4", Status: StatusDone, Size: 400}
	part := &Item{ID: "b", Name: "two.mp4", Status: StatusRunning, Size: 900}
	part.downloaded.Store(123)
	job := &Job{
		ID: "j1", Source: "https://example.test/album", Title: "Album",
		Host: "example.test", CreatedAt: time.Now().Round(time.Second),
		Items: []*Item{done, part},
	}
	m.jobs[job.ID] = job
	m.order = []string{job.ID}

	m.persist()

	// Read back into a fresh manager, as a restart would.
	next := &Manager{stateFile: file, jobs: map[string]*Job{}, log: testLogger()}
	unfinished, err := next.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if unfinished != 1 {
		t.Errorf("unfinished = %d, want 1", unfinished)
	}

	got, ok := next.jobs["j1"]
	if !ok {
		t.Fatal("the job did not come back")
	}
	if got.Source != job.Source || got.Title != job.Title || got.Host != job.Host {
		t.Errorf("job came back as %+v", got)
	}
	if !got.restored {
		t.Error("a job with work left should be marked for re-resolution")
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(got.Items))
	}
	if got.Items[0].Status != StatusDone || got.Items[0].downloaded.Load() != 400 {
		t.Errorf("a finished file should come back finished: %+v", got.Items[0])
	}
	// The one that was mid-flight comes back queued, at zero: the part file
	// on disk is what says how far it really got.
	if got.Items[1].Status != StatusQueued {
		t.Errorf("an interrupted transfer came back as %q, want queued", got.Items[1].Status)
	}
	if got.Items[1].downloaded.Load() != 0 {
		t.Errorf("an interrupted transfer came back claiming %d bytes", got.Items[1].downloaded.Load())
	}
}

func TestRestoreLeavesAFinishedJobAlone(t *testing.T) {
	m, file := newSavedManager(t)
	m.jobs["j"] = &Job{ID: "j", Source: "https://example.test/x", Items: []*Item{
		{ID: "a", Status: StatusDone, Size: 10},
		{ID: "b", Status: StatusCanceled},
	}}
	m.order = []string{"j"}
	m.persist()

	next := &Manager{stateFile: file, jobs: map[string]*Job{}, log: testLogger()}
	unfinished, err := next.Restore()
	if err != nil {
		t.Fatal(err)
	}
	if unfinished != 0 {
		t.Errorf("unfinished = %d, want 0 — nothing there is worth doing again", unfinished)
	}
	if next.jobs["j"].restored {
		t.Error("a finished job should not be queued for re-resolution")
	}
}

// The queue names everything being downloaded, and carries the passwords for
// the sources that need one. It is the owner's business and nobody else's.
func TestStateFileIsReadableOnlyByItsOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	m, file := newSavedManager(t)
	m.jobs["j"] = &Job{ID: "j", Source: "https://example.test/x", Password: "hunter2"}
	m.order = []string{"j"}
	m.persist()

	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file is mode %04o, want 0600", perm)
	}
	dir, err := os.Stat(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("state directory is mode %04o, which lets others in", perm)
	}

	next := &Manager{stateFile: file, jobs: map[string]*Job{}, log: testLogger()}
	if _, err := next.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := next.jobs["j"].Password; got != "hunter2" {
		t.Errorf("password came back as %q — a protected job could not resume", got)
	}
}

func TestPersistSkipsAQueueThatHasNotChanged(t *testing.T) {
	m, file := newSavedManager(t)
	m.jobs["j"] = &Job{ID: "j", Source: "https://example.test/x", Items: []*Item{
		{ID: "a", Status: StatusRunning, Size: 100},
	}}
	m.order = []string{"j"}

	m.persist()
	first, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}

	// Bytes moved, nothing else. That is the common case by far, and it must
	// not rewrite the file — the counter is never recorded in the first place.
	m.jobs["j"].Items[0].downloaded.Store(99)
	m.persist()
	second, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ModTime().Equal(first.ModTime()) {
		t.Error("the file was rewritten although only a byte counter had moved")
	}

	// A status change is a different matter.
	m.jobs["j"].Items[0].Status = StatusDone
	m.persist()
	third, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if third.ModTime().Equal(first.ModTime()) && third.Size() == first.Size() {
		t.Error("a finished transfer did not reach the file")
	}
}

// A file half-written, from a newer build, or simply nonsense must never stop
// the service starting. A lost queue is a nuisance; a lost service is not.
func TestRestoreSurvivesAnUnusableFile(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"truncated", `{"version":1,"jobs":[{"id":"a",`},
		{"nonsense", `this is not json at all`},
		{"from the future", `{"version":9999,"jobs":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "queue.json")
			if err := os.WriteFile(file, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			m := &Manager{stateFile: file, jobs: map[string]*Job{}, log: testLogger()}
			unfinished, err := m.Restore()
			if err == nil {
				t.Error("an unusable file should be reported")
			}
			if unfinished != 0 || len(m.jobs) != 0 {
				t.Errorf("started with %d jobs from an unusable file", len(m.jobs))
			}
		})
	}
}

func TestNoStateFileMeansNoPersistence(t *testing.T) {
	m := &Manager{jobs: map[string]*Job{}, log: testLogger()}
	m.jobs["j"] = &Job{ID: "j", Source: "https://example.test/x"}
	m.order = []string{"j"}
	m.persist() // must not panic, and must write nothing anywhere

	if _, err := m.Restore(); err != nil {
		t.Errorf("Restore with no state file: %v", err)
	}
}

// No item URL is written down. Several hosts sign their links for minutes,
// and the ones that do carry no URL until a resolver mints one, so a stored
// link would be a stale link — the reason an unfinished job is resolved
// afresh rather than replayed.
func TestNoItemURLsReachTheFile(t *testing.T) {
	m, file := newSavedManager(t)
	m.jobs["j"] = &Job{ID: "j", Source: "https://example.test/album", Items: []*Item{
		{ID: "a", Name: "one.mp4", Status: StatusQueued,
			URL: "https://cdn.example.test/signed?token=SECRETTOKEN&ex=1787912216"},
	}}
	m.order = []string{"j"}
	m.persist()

	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "SECRETTOKEN") {
		t.Error("a signed item URL was written to the queue file")
	}
	var st savedState
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Jobs) != 1 || len(st.Jobs[0].Items) != 1 {
		t.Fatalf("unexpected shape: %+v", st)
	}
	if st.Jobs[0].Items[0].Name != "one.mp4" {
		t.Errorf("the item did not survive: %+v", st.Jobs[0].Items[0])
	}
}

// Resolution appends to a job's items, so a restored job has to have its
// placeholders dropped before its source is read again — otherwise every
// file in it would come back twice. This is the whole reason resuming and
// retrying clear them rather than leaving them in place.
func TestResumingARestoredJobReplacesItsItemsRatherThanDoublingThem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte("data"))
	}))
	defer server.Close()

	m, _ := newTestManager(t)
	m.SetPaused(true)

	// A job as Restore would leave it: one placeholder item recording what
	// the last run knew, and a note that the source must be read afresh.
	source := server.URL + "/payload.bin"
	m.mu.Lock()
	job := &Job{
		ID: "restored", Source: source, Title: "payload.bin", Host: "direct",
		CreatedAt: time.Now(),
		Items:     []*Item{{ID: "stale", Name: "payload.bin", Status: StatusQueued, Size: 4}},
		restored:  true,
	}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	m.mu.Unlock()

	// Nothing runs while it is held, which is the point of restoring held.
	if !waitForCond(300*time.Millisecond, func() bool { return false }) {
		// deliberately waits out the window
	}
	m.mu.Lock()
	stillHeld := m.jobs["restored"].restored && len(m.queue) == 0
	m.mu.Unlock()
	if !stillHeld {
		t.Fatal("a restored job started before it was resumed")
	}

	m.SetPaused(false)

	if !waitForCond(20*time.Second, func() bool {
		return itemStatusesOf(m, "restored", StatusDone) == 1
	}) {
		t.Fatalf("the resumed job did not finish; items now: %d", itemCountOf(m, "restored"))
	}
	if n := itemCountOf(m, "restored"); n != 1 {
		t.Errorf("job holds %d items after re-resolution, want 1 — the placeholder was not dropped", n)
	}
	m.mu.Lock()
	restored := m.jobs["restored"].restored
	m.mu.Unlock()
	if restored {
		t.Error("the job is still marked for re-resolution after being resumed")
	}
}

func itemCountOf(m *Manager, jobID string) int {
	for _, job := range m.Snapshot().Jobs {
		if job.ID == jobID {
			return len(job.Items)
		}
	}
	return -1
}

func itemStatusesOf(m *Manager, jobID string, want Status) int {
	n := 0
	for _, job := range m.Snapshot().Jobs {
		if job.ID != jobID {
			continue
		}
		for _, it := range job.Items {
			if it.Status == want {
				n++
			}
		}
	}
	return n
}

// Shutting down must not record an interrupted transfer as cancelled.
//
// Close cancels every transfer in flight, and a worker winding down marks its
// item cancelled. Written down, that is indistinguishable from an item the
// user cancelled deliberately — and restores as a job with nothing left to
// do, which silently abandons the download. So the queue is written before
// the cancellation, while those items are still running, which is recorded
// as queued.
func TestShutdownRecordsAnInterruptedTransferAsUnfinished(t *testing.T) {
	// A body that never ends, so the transfer is unambiguously mid-flight
	// when the manager is closed under it.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "10000000")
		w.WriteHeader(http.StatusOK)
		for {
			select {
			case <-release:
				return
			default:
			}
			if _, err := w.Write(make([]byte, 32*1024)); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer server.Close()
	defer close(release)

	file := filepath.Join(t.TempDir(), "queue.json")
	m, _ := newTestManager(t)
	m.stateFile = file

	jobID, err := m.Add(server.URL+"/endless.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	itemID := waitForItem(t, m, jobID)
	if !waitForCond(10*time.Second, func() bool {
		return itemStatus(m, jobID, itemID) == StatusRunning
	}) {
		t.Fatalf("the transfer never started (status %q)", itemStatus(m, jobID, itemID))
	}

	m.Close() // idempotent; the harness closes it again on cleanup

	st, err := loadState(file)
	if err != nil {
		t.Fatalf("reading back the queue: %v", err)
	}
	if len(st.Jobs) != 1 || len(st.Jobs[0].Items) != 1 {
		t.Fatalf("unexpected queue: %+v", st.Jobs)
	}
	if got := st.Jobs[0].Items[0].Status; got != StatusQueued {
		t.Errorf("an interrupted transfer was written down as %q, want %q — "+
			"a job recorded that way never resumes", got, StatusQueued)
	}

	// And it must come back as work to do.
	next := &Manager{stateFile: file, jobs: map[string]*Job{}, log: testLogger()}
	unfinished, err := next.Restore()
	if err != nil {
		t.Fatal(err)
	}
	if unfinished != 1 {
		t.Errorf("unfinished = %d, want 1 — the interrupted download was abandoned", unfinished)
	}
}
