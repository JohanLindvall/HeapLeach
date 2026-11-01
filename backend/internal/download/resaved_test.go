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
	srv := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		fetched.Add(1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // headers out before the body: no length
		_, _ = w.Write(body())
	}))
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
		onDisk   bool
		recorded int64 // the length the record holds; 0 for the file's own
		fetches  int32
	}{
		"still there": {onDisk: true, fetches: 0},
		"gone since":  {onDisk: false, fetches: 1},
		// A playlist is counted at its transport stream's length and then
		// rewrapped as a smaller MP4: the record's length is not the file's.
		"rewrapped since": {onDisk: true, recorded: 30489464, fetches: 0},
	} {
		t.Run(name, func(t *testing.T) {
			fetched.Store(0)
			recorded := int64(len(photo))
			if tc.recorded != 0 {
				recorded = tc.recorded
			}
			m := testManager(t, nil)
			m.Start()
			dir := m.DownloadDir()
			_ = m.ApplySettings(Settings{Paused: new(true)})
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
					{ID: "saved", Name: "photo.jpg", Status: StatusDone, Path: "photo.jpg", Size: recorded},
				},
			}
			m.queueJobLocked(job)
			m.mu.Unlock()

			_ = m.ApplySettings(Settings{Paused: new(false)})
			if !waitForCond(20*time.Second, func() bool { return countStatus(m, "restored", StatusDone) == 1 }) {
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

// A file its resolver named otherwise than its listing did is still the
// job's own after a restart: the listing, read again, names it as before,
// and the record knows it by that name too. Recorded under the resolver's
// name alone, every such file was fetched again.
func TestARestartRecognisesAFileItsResolverRenamed(t *testing.T) {
	photo := []byte("jpeg bytes of a photo its resolver named")
	var fetched atomic.Int32
	srv := unsizedServer(t, func() []byte { return photo }, &fetched)
	m := testManager(t, nil)
	m.Start()
	dir := m.DownloadDir()
	_ = m.ApplySettings(Settings{Paused: new(true)})
	if err := os.WriteFile(filepath.Join(dir, "A Photo Named By Its Resolver.jpg"), photo, 0o644); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	job := &Job{
		ID: "restored", Source: srv.URL + "/album/photo.jpg", Title: "photo.jpg", Host: "direct",
		CreatedAt: time.Now(), restored: true,
		Items: []*Item{{
			ID: "saved", Name: "A Photo Named By Its Resolver.jpg", listed: "photo.jpg", Status: StatusDone,
			Path: "A Photo Named By Its Resolver.jpg", Size: int64(len(photo)),
		}},
	}
	m.queueJobLocked(job)
	m.mu.Unlock()

	_ = m.ApplySettings(Settings{Paused: new(false)})
	if !waitForCond(20*time.Second, func() bool { return countStatus(m, "restored", StatusDone) == 1 }) {
		t.Fatal("the re-read job did not finish")
	}
	if got := fetched.Load(); got != 0 {
		t.Errorf("fetched %d times; want the saved file recognised", got)
	}
	if got := namesIn(t, dir); len(got) != 1 {
		t.Errorf("destination holds %q; want the one file", got)
	}
}

// The name an item was listed under outlives a restart, where something
// renamed the item since, and is saved only there.
func TestTheListedNameIsSavedWithTheQueue(t *testing.T) {
	m, file := newSavedManager(t)
	job := &Job{ID: "job", Title: "A listing", Items: []*Item{
		{ID: "renamed", Status: StatusDone, Name: "Renamed.ts", listed: "Listed.mp4", Path: "Renamed.mp4", Size: 10},
		{ID: "kept", Status: StatusDone, Name: "Kept.mp4", listed: "Kept.mp4", Path: "Kept.mp4", Size: 10},
	}}
	m.jobs[job.ID], m.order = job, []string{job.ID}
	m.persist()
	state, _, err := loadState(file, "")
	if err != nil {
		t.Fatal(err)
	}
	if items := state.Jobs[0].Items; items[0].Listed != "Listed.mp4" || items[1].Listed != "" {
		t.Errorf("saved %+v; want the listed name where it differs alone", items)
	}

	restored, _ := newSavedManager(t)
	restored.stateFile = file
	if _, err := restored.Restore(); err != nil {
		t.Fatal(err)
	}
	restored.mu.Lock()
	defer restored.mu.Unlock()
	got := restored.jobs["job"].Items
	if got[0].listed != "Listed.mp4" || got[1].listed != "Kept.mp4" {
		t.Errorf("restored listed names %q and %q", got[0].listed, got[1].listed)
	}
}
