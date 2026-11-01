// SPDX-License-Identifier: MIT

package download

import (
	"net/http"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// A listing a failure cut short is kept, its files queued, and the job says
// it is incomplete; retrying it reads the source again for what was left
// out, even once every file it holds is done. One cut at a cap is complete
// as far as a retry can take it, and retries as it always has.
func TestAJobCutShortByAFailureRetriesItsSource(t *testing.T) {
	for _, tc := range []struct {
		note       string
		incomplete bool
	}{
		{"partial — could not fetch next page", true},
		{"partial — file limit; could not read a folder page", true},
		{"partial — rate limited", true},
		{"partial — unreadable folders", true},
		{"partial — file limit", false},
		{"partial — repeated or empty page", false},
		{"2 of 3 posts", false},
		{"", false},
	} {
		t.Run(tc.note, func(t *testing.T) {
			srv := testServer(t, http.NotFoundHandler())
			m := testManager(t, nil)
			job := &Job{ID: "job", Source: srv.URL + "/listing"}
			res := &extractor.Result{Title: "A Listing", Note: tc.note, Files: []extractor.File{
				{Name: "one.mp4", URL: srv.URL + "/1"}, {Name: "two.mp4", URL: srv.URL + "/2"},
			}}
			m.mu.Lock()
			m.jobs[job.ID] = job
			m.order = append(m.order, job.ID)
			m.applyResultLocked(job, "stub", res, nil)
			for _, it := range job.Items {
				it.Status = StatusDone
			}
			view := job.view(func(*Item) string { return "" })
			m.mu.Unlock()
			if view.Incomplete != tc.incomplete {
				t.Fatalf("view.Incomplete = %v, want %v", view.Incomplete, tc.incomplete)
			}

			if err := m.RetryJob(job.ID); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			reread, items := job.resolving || job.resolveID > 0, len(job.Items)
			m.mu.Unlock()
			if reread != tc.incomplete {
				t.Fatalf("retry re-read the source: %v, want %v (items %d)", reread, tc.incomplete, items)
			}
			if tc.incomplete {
				// The re-read finds the source no longer cut short, and the
				// job no longer says it is incomplete.
				waitLocked(t, m, 5*time.Second, func() bool { return !job.resolving })
				m.mu.Lock()
				defer m.mu.Unlock()
				if job.incomplete {
					t.Fatal("the re-read found nothing cut short, yet the job still says it is incomplete")
				}
			}
		})
	}
}

// Retrying an incomplete job while some of its files are still queued or
// owned by a worker re-queues the failed ones rather than reading the source
// again: a re-read drops the job's items, and those would go on downloading
// outside it while the re-read listed the same files again.
func TestRetryOfAnIncompleteJobWithLiveItemsKeepsThem(t *testing.T) {
	m := testManager(t, nil)
	job := &Job{ID: "job", Source: "https://example.test/listing", incomplete: true}
	m.mu.Lock()
	m.queueJobLocked(job,
		extractor.File{Name: "running.mp4", URL: "https://example.test/1"},
		extractor.File{Name: "failed.mp4", URL: "https://example.test/2"},
		extractor.File{Name: "queued.mp4", URL: "https://example.test/3"},
	)
	running, failed, queued := job.Items[0], job.Items[1], job.Items[2]
	running.Status, running.inFlight = StatusRunning, true
	failed.Status = StatusFailed
	m.mu.Unlock()

	if err := m.RetryJob(job.ID); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if job.resolving || job.resolveID > 0 {
		t.Fatal("the source was read again while files were still in flight")
	}
	if len(job.Items) != 3 || job.Items[0] != running || job.Items[2] != queued {
		t.Fatalf("the job's items changed: %d", len(job.Items))
	}
	if failed.Status != StatusQueued {
		t.Errorf("failed file is %s; want it queued again", failed.Status)
	}
	if !job.incomplete {
		t.Error("the job no longer offers the retry of what its listing left out")
	}
}

// What a failure left out of a listing is still offered after a restart.
func TestIncompleteIsSavedWithTheQueue(t *testing.T) {
	m, file := newSavedManager(t)
	job := &Job{ID: "job", Title: "A listing", incomplete: true, Items: []*Item{
		{ID: "one", Status: StatusDone, Name: "One.mp4", Path: "One.mp4", Size: 10},
	}}
	m.jobs[job.ID], m.order = job, []string{job.ID}
	m.persist()

	restored, _ := newSavedManager(t)
	restored.stateFile = file
	if _, err := restored.Restore(); err != nil {
		t.Fatal(err)
	}
	restored.mu.Lock()
	got := restored.jobs["job"]
	view := got.view(func(*Item) string { return "" })
	restored.mu.Unlock()
	if !got.incomplete || !view.Incomplete {
		t.Fatal("the restored job no longer says its listing was cut short")
	}
	if n := restored.ClearFinished(); n != 0 {
		t.Errorf("Clear finished removed %d jobs; want the incomplete one kept", n)
	}
}
