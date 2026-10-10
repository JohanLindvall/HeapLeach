// SPDX-License-Identifier: MIT

package download

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

// unsizedServer serves body with no Content-Length, as DarkGram's photo
// endpoint does: nothing before the bytes themselves says how long it is.
func unsizedServer(t *testing.T, body func() []byte, fetched *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		fetched.Add(1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // headers out before the body: no length
		_, _ = w.Write(body())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func namesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// A restart re-reads every unfinished job, and a file whose length nobody
// states cannot be recognised on disk before it is fetched. The job's own
// record of what it saved is what keeps it from being fetched again and
// kept beside itself as "(2)".
func TestARestartDoesNotFetchAgainWhatTheJobSaved(t *testing.T) {
	photo := []byte("jpeg bytes of a photo with no stated length")
	var fetched atomic.Int32
	srv := unsizedServer(t, func() []byte { return photo }, &fetched)

	for name, tc := range map[string]struct {
		onDisk  bool
		fetches int32
	}{
		"still there": {onDisk: true, fetches: 0},
		"gone since":  {onDisk: false, fetches: 1},
	} {
		t.Run(name, func(t *testing.T) {
			fetched.Store(0)
			m, dir := newTestManager(t)
			m.SetPaused(true)
			if tc.onDisk {
				if err := os.WriteFile(filepath.Join(dir, "photo.jpg"), photo, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			m.mu.Lock()
			job := &Job{
				ID: "restored", Source: srv.URL + "/album/photo.jpg", Title: "photo.jpg", Host: "direct",
				CreatedAt: time.Now(), restored: true,
				Items: []*Item{
					{ID: "saved", Name: "photo.jpg", Status: StatusDone, Path: "photo.jpg", Size: int64(len(photo))},
				},
			}
			m.jobs[job.ID] = job
			m.order = append(m.order, job.ID)
			m.mu.Unlock()

			m.SetPaused(false)
			if !waitForCond(20*time.Second, func() bool { return itemStatusesOf(m, "restored", StatusDone) == 1 }) {
				t.Fatal("the re-read job did not finish")
			}
			m.mu.Lock()
			path := m.jobs["restored"].Items[0].Path
			m.mu.Unlock()
			if got := namesIn(t, dir); len(got) != 1 || got[0] != "photo.jpg" || path != "photo.jpg" {
				t.Fatalf("destination holds %q, item at %q; want photo.jpg once", got, path)
			}
			if got := fetched.Load(); got != tc.fetches {
				t.Errorf("fetched %d times, want %d", got, tc.fetches)
			}
		})
	}
}

// When it is fetched anyway (re-added, or finished just before a restart
// recorded it), a whole file identical to one already under its name is that
// file: kept once, and the item points at it. A different file of the same
// name is still kept beside it.
func TestAnIdenticalFileIsKeptOnceAndADifferentOneBesideIt(t *testing.T) {
	var fetched atomic.Int32
	body := []byte("the same bytes both times")
	srv := unsizedServer(t, func() []byte { return body }, &fetched)
	m := busyManager(t)
	dir := m.cfg.DownloadDir

	fetch := func() *Item {
		t.Helper()
		it := &Item{ID: newID(), Name: "clip.jpg", URL: srv.URL + "/clip.jpg", Size: -1}
		if err := m.transfer(t.Context(), it); err != nil {
			t.Fatal(err)
		}
		return it
	}
	fetch()
	again := fetch()
	if got := namesIn(t, dir); len(got) != 1 || got[0] != "clip.jpg" {
		t.Fatalf("destination holds %q after the same file twice; want clip.jpg once", got)
	}
	if !again.Skipped || again.Path != "clip.jpg" {
		t.Errorf("second fetch: skipped=%v path=%q; want it pointed at the file already there", again.Skipped, again.Path)
	}

	body = []byte("different bytes, same name")
	other := fetch()
	if got := namesIn(t, dir); len(got) != 2 || got[1] != "clip.jpg" || got[0] != "clip (2).jpg" {
		t.Fatalf("destination holds %q; want a different file of the same name kept beside it", got)
	}
	if other.Skipped {
		t.Error("a different file was taken for the one already there")
	}
}
