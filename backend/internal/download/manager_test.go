// SPDX-License-Identifier: MIT

package download

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func BenchmarkDispatchQueue(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			items := make([]Item, count)
			queue := make([]*Item, count)
			for i := range items {
				items[i].Status = StatusQueued
			}
			b.ResetTimer()
			for b.Loop() {
				for i := range items {
					queue[i] = &items[i]
				}
				m := &Manager{limit: 1, queue: queue}
				for range items {
					if m.nextLocked() == nil {
						b.Fatal("lost a queued item")
					}
				}
			}
		})
	}
}

// A manager with a resolved job in each terminal state and one still
// queued, so the housekeeping calls have something to distinguish.
func seededManager(t *testing.T) (m *Manager, done, failed, queued string) {
	t.Helper()
	m = testManager(t, nil)
	m.Start()
	add := func(status Status) string {
		job := &Job{ID: newID(), Source: "https://example.test/" + newID(), Title: "t", CreatedAt: time.Now()}
		job.Items = []*Item{{ID: newID(), JobID: job.ID, Name: "f", Status: status}}
		m.mu.Lock()
		m.queueJobLocked(job)
		m.mu.Unlock()
		return job.ID
	}
	return m, add(StatusDone), add(StatusFailed), add(StatusQueued)
}

func TestClearFinishedForgetsOnlyCompleteJobs(t *testing.T) {
	m := &Manager{jobs: map[string]*Job{}}
	cases := []struct {
		name      string
		statuses  []Status
		err       string
		resolving bool
		canceled  bool
		remove    bool
	}{
		{name: "failed", statuses: []Status{StatusFailed}},
		{name: "complete", statuses: []Status{StatusDone}, remove: true},
		{name: "partly-failed", statuses: []Status{StatusDone, StatusFailed}},
		{name: "canceled", statuses: []Status{StatusCanceled}},
		{name: "partly-canceled", statuses: []Status{StatusDone, StatusCanceled}},
		{name: "queued", statuses: []Status{StatusDone, StatusQueued}},
		{name: "running", statuses: []Status{StatusDone, StatusRunning}},
		{name: "empty"},
		{name: "source-failed", err: "source failed"},
		{name: "partial-source", statuses: []Status{StatusDone}, err: "listing incomplete"},
		{name: "resolving", statuses: []Status{StatusDone}, resolving: true},
		{name: "job-canceled", statuses: []Status{StatusDone}, canceled: true},
		{name: "all-complete", statuses: []Status{StatusDone, StatusDone}, remove: true},
	}
	var wantKept []string
	var wantRemoved int
	for _, tc := range cases {
		job := &Job{ID: tc.name, Err: tc.err, resolving: tc.resolving, canceled: tc.canceled}
		for i, status := range tc.statuses {
			job.Items = append(job.Items, &Item{ID: fmt.Sprint(i), JobID: job.ID, Status: status})
		}
		m.queueJobLocked(job)
		if tc.remove {
			wantRemoved++
		} else {
			wantKept = append(wantKept, job.ID)
		}
	}

	if got := m.ClearFinished(); got != wantRemoved {
		t.Errorf("ClearFinished removed %d jobs, want %d", got, wantRemoved)
	}
	var gotKept []string
	for _, job := range m.Snapshot().Jobs {
		gotKept = append(gotKept, job.ID)
	}
	slices.Reverse(wantKept) // Snapshots show the newest jobs first.
	if !slices.Equal(gotKept, wantKept) {
		t.Errorf("kept jobs = %v, want %v", gotKept, wantKept)
	}
	for _, tc := range cases {
		if _, exists := m.jobs[tc.name]; exists == tc.remove {
			t.Errorf("job %s exists = %v, want %v", tc.name, exists, !tc.remove)
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
	job := jobView(m, queued)
	if job.Status != StatusCanceled {
		t.Errorf("job status = %s, want canceled", job.Status)
	}
	for _, it := range job.Items {
		if it.Status != StatusCanceled {
			t.Errorf("item status = %s, want canceled", it.Status)
		}
	}
	if err := m.CancelJob("nope"); err != ErrNotFound {
		t.Errorf("cancelling a stranger: %v", err)
	}
}

// The settings each refuse what the UI's controls cannot produce, and say so
// rather than clamping silently.
func TestSettingsRefuseTheOutOfRange(t *testing.T) {
	m := testManager(t, nil)
	m.Start()

	if err := m.ApplySettings(Settings{Concurrency: new(0)}); err == nil {
		t.Error("concurrency 0 was accepted")
	}
	if err := m.ApplySettings(Settings{Concurrency: new(config.DefaultMaxConcurrency + 1)}); err == nil {
		t.Error("concurrency above the ceiling was accepted")
	}
	if got := m.Snapshot().MaxConcur; got != config.DefaultMaxConcurrency {
		t.Errorf("snapshot ceiling = %d, want %d for the slider", got, config.DefaultMaxConcurrency)
	}
	// The ceiling is the configured one, live settings included. Set on a
	// manager that has not started, so nothing else is reading it.
	set := busyManager(t)
	set.cfg.MaxConcurrency = 40
	if err := set.ApplySettings(Settings{Concurrency: new(41)}); err == nil {
		t.Error("concurrency above a configured ceiling was accepted")
	}
	if err := set.ApplySettings(Settings{Concurrency: new(40)}); err != nil {
		t.Errorf("concurrency at a configured ceiling: %v", err)
	}
	if got := set.Snapshot().MaxConcur; got != 40 {
		t.Errorf("snapshot ceiling = %d, want the configured 40", got)
	}
	if err := m.ApplySettings(Settings{Concurrency: new(3)}); err != nil {
		t.Errorf("concurrency 3: %v", err)
	}
	if got := m.Snapshot().Concurrency; got != 3 {
		t.Errorf("snapshot concurrency = %d, want 3", got)
	}

	if err := m.ApplySettings(Settings{Streams: new(0)}); err == nil {
		t.Error("streams 0 was accepted")
	}
	if err := m.ApplySettings(Settings{Streams: new(config.MaxStreams + 1)}); err == nil {
		t.Error("streams above the ceiling was accepted")
	}
	if err := m.ApplySettings(Settings{Streams: new(2)}); err != nil {
		t.Errorf("streams 2: %v", err)
	}
	if got := m.Snapshot().Streams; got != 2 {
		t.Errorf("snapshot streams = %d, want 2", got)
	}

	if err := m.ApplySettings(Settings{SpeedLimit: new(int64(-1))}); err == nil {
		t.Error("a negative speed limit was accepted")
	}
	if err := m.ApplySettings(Settings{SpeedLimit: new(int64(1_000_000))}); err != nil {
		t.Errorf("speed limit: %v", err)
	}
	if got := m.Snapshot().SpeedLimit; got != 1_000_000 {
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
	m := testManager(t, nil)
	m.Start()
	if m.Busy() {
		t.Fatal("an empty manager is busy")
	}

	// A page that never answers keeps the job resolving.
	hold := make(chan struct{})
	srv := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
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
	m := testManager(t, nil)
	m.Start()
	m.Close()

	events, _, unsubscribe := m.Subscribe()
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
	m := testManager(t, nil)
	m.Start()

	m.markDirty()
	time.Sleep(3 * config.ProgressTick)
	if !m.dirty.Load() {
		t.Fatal("the broadcaster spent the dirty flag with nobody listening")
	}

	events, _, unsubscribe := m.Subscribe()
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
	log := testLogger()
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

// A user's own action goes out at once rather than on the next beat. A row
// that sits unchanged for most of a second after a click reads as the click
// not having taken.
func TestAUserActionIsPublishedAtOnce(t *testing.T) {
	m := testManager(t, nil)
	m.Start()
	events, _, unsubscribe := m.Subscribe()
	defer unsubscribe()

	// Have an ordinary frame go out first. The ceiling then holds the
	// ordinary path off for a whole FrameInterval, so a frame inside that
	// could only have come from the nudge — whereas with no recent frame, a
	// tick falling just after the action would pass for one.
	m.markDirty()
	select {
	case <-events:
	case <-time.After(3 * config.FrameInterval):
		t.Fatal("no ordinary frame to start from")
	}

	// An action that changes something: one that changes nothing has
	// nothing to show and sends nothing.
	if err := m.ApplySettings(Settings{SpeedLimit: new(int64(1 << 20))}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-time.After(config.FrameInterval / 2):
		t.Fatal("the action was not published at once")
	}
}

// An action that changed nothing — here, an unknown job — costs no frame.
func TestARefusedActionSendsNothing(t *testing.T) {
	m := testManager(t, nil)
	m.Start()
	events, _, unsubscribe := m.Subscribe()
	defer unsubscribe()

	m.markDirty()
	select {
	case <-events:
	case <-time.After(3 * config.FrameInterval):
		t.Fatal("no ordinary frame to start from")
	}

	if err := m.CancelJob("no-such-job"); err == nil {
		t.Fatal("cancelling an unknown job succeeded")
	}
	select {
	case <-events:
		t.Error("a refused action sent a frame")
	case <-time.After(config.FrameInterval / 2):
	}
}

// Pasting a list of links is an Add per link. Those nudges arrive together
// and must go out as one frame, not one snapshot of the whole queue each.
func TestNudgesArrivingTogetherAreOneFrame(t *testing.T) {
	m := testManager(t, nil)
	m.Start()
	events, _, unsubscribe := m.Subscribe()
	defer unsubscribe()
	select { // let the ordinary beat send, so it is held off for a second
	case <-events:
	case <-time.After(5 * config.FrameInterval):
	}

	for i := range 10 {
		if err := m.ApplySettings(Settings{SpeedLimit: new(int64(1_000_000 * (i + 1)))}); err != nil {
			t.Fatal(err)
		}
	}
	frames := 0
	deadline := time.After(config.FrameInterval / 2)
collect:
	for {
		select {
		case <-events:
			frames++
		case <-deadline:
			break collect
		}
	}
	if frames != 1 {
		t.Errorf("ten actions together sent %d frames, want 1", frames)
	}
}
