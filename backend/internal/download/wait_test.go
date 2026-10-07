// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// A host that names a time to come back puts the item back in the queue as
// waiting, says how long is left, and is not asked again before then.
func TestAHostsWaitReturnsTheItemToTheQueueUntilItIsOver(t *testing.T) {
	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4", Status: StatusRunning}
	wait := &extractor.WaitError{
		Until:  time.Now().Add(time.Hour + 3*time.Minute + 35*time.Second),
		Reason: "test host: waiting between free downloads",
	}

	m.mu.Lock()
	deferred := m.deferWaitLocked(it, fmt.Errorf("resolve: %w", wait))
	status, note := it.Status, m.itemNoteLocked(it)
	early := m.nextLocked()
	m.mu.Unlock()
	if !deferred || status != StatusQueued {
		t.Fatalf("deferred=%v status=%s; want the item back in the queue", deferred, status)
	}
	if want := "test host: waiting between free downloads — 1h4m left"; note != want {
		t.Errorf("note = %q, want %q", note, want)
	}
	if early != nil {
		t.Error("the dispatcher started an item before the time its host named")
	}

	// Once the time has come it is an ordinary queued item again.
	m.mu.Lock()
	it.notBefore = time.Now().Add(-time.Second)
	note = m.itemNoteLocked(it)
	next := m.nextLocked()
	m.mu.Unlock()
	if next != it {
		t.Errorf("dispatched %v, want the item whose wait is over", next)
	}
	if note != "" {
		t.Errorf("note = %q after the wait, want none", note)
	}

	// A retry the user asked for meanwhile takes precedence, and no other
	// failure is a wait.
	pending := &Item{ID: newID(), Status: StatusRunning, retryPending: true}
	other := &Item{ID: newID(), Status: StatusRunning}
	m.mu.Lock()
	if m.deferWaitLocked(pending, wait) {
		t.Error("a pending user retry was overridden by the host's wait")
	}
	if m.deferWaitLocked(other, io.ErrUnexpectedEOF) {
		t.Error("an ordinary failure was deferred as though the host had asked for a wait")
	}
	m.mu.Unlock()
}

func TestMinutesLeftRoundsUp(t *testing.T) {
	for in, want := range map[time.Duration]string{
		time.Second:               "1m",
		time.Minute:               "1m",
		time.Minute + time.Second: "2m",
		time.Hour:                 "1h0m",
		time.Hour + 3*time.Minute + 35*time.Second: "1h4m",
	} {
		if got := minutesLeft(in); got != want {
			t.Errorf("minutesLeft(%s) = %q, want %q", in, got, want)
		}
	}
}

// The point of it, end to end: with one worker, an item told to wait must not
// hold that worker. The file behind it finishes first, while the waiting one
// shows as queued rather than downloading, and it runs once its time comes.
func TestAWaitingItemGivesItsWorkerToTheRestOfTheQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "payload for "+r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	m, _ := newTestManager(t)
	var resolves atomic.Int32
	until := time.Now().Add(2 * time.Second)
	waits := extractor.File{Name: "waits.bin", Size: -1, Resolve: func(context.Context) (*extractor.Target, error) {
		if resolves.Add(1) == 1 {
			return nil, &extractor.WaitError{Until: until, Reason: "test host: waiting between free downloads"}
		}
		return &extractor.Target{URL: srv.URL + "/waits.bin"}, nil
	}}
	quick := extractor.File{Name: "quick.bin", URL: srv.URL + "/quick.bin"}

	m.mu.Lock()
	job := &Job{ID: newID(), Source: srv.URL + "/list", CreatedAt: time.Now()}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	first, second := m.newItem(job, waits, "", 0), m.newItem(job, quick, "", 1)
	job.Items = []*Item{first, second}
	m.enqueueLocked(first)
	m.enqueueLocked(second)
	m.mu.Unlock()
	m.signal()

	view := func(id string) ItemView {
		for _, job := range m.Snapshot().Jobs {
			for _, it := range job.Items {
				if it.ID == id {
					return it
				}
			}
		}
		return ItemView{}
	}

	waitFor(t, 10*time.Second, func() bool { return view(second.ID).Status == StatusDone })
	if v := view(first.ID); time.Now().Before(until) &&
		(v.Status != StatusQueued || !strings.Contains(v.Note, "waiting between free downloads")) {
		t.Fatalf("waiting item is %s, %q; want it queued and saying why", v.Status, v.Note)
	}
	waitFor(t, 10*time.Second, func() bool { return view(first.ID).Status == StatusDone })
	if time.Now().Before(until) {
		t.Error("the item ran before the time its host named")
	}
	if got := resolves.Load(); got != 2 {
		t.Errorf("resolved %d times, want the turn that was told to wait and the one after", got)
	}
}
