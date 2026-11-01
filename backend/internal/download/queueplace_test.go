// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

// A route failing a file says nothing about the file. One that has started
// goes back to the front of the queue; one that has not, to the back.
func TestAStartedFileGoesBackToTheFrontOfTheQueue(t *testing.T) {
	m := busyManager(t)
	ahead, behind := &Item{ID: "ahead", Status: StatusQueued}, &Item{ID: "behind", Status: StatusQueued}
	fresh, started := &Item{ID: "fresh", Status: StatusRunning}, &Item{ID: "started", Status: StatusRunning}
	retry := &extractor.WaitError{Until: time.Now(), Reason: "Retrying through another download route"}

	m.mu.Lock()
	m.queue = []*Item{ahead, behind}
	m.deferWaitLocked(fresh, retry, false)
	m.deferWaitLocked(started, retry, true)
	queue := slices.Clone(m.queue)
	m.mu.Unlock()
	if want := []*Item{started, ahead, behind, fresh}; !slices.Equal(queue, want) {
		var got []string
		for _, it := range queue {
			got = append(got, it.ID)
		}
		t.Fatalf("queue %v; want started, ahead, behind, fresh", got)
	}
}

// Started means bytes in a part file the transfer would resume from: the one
// keyed on the job's source, or the item's own when that one was taken.
func TestStartedOnDiskReadsThePartFiles(t *testing.T) {
	m := busyManager(t)
	job := &Job{ID: "job", Source: "http://service.example.test/collection"}
	it := &Item{ID: "item", JobID: job.ID, Name: "clip.bin", Dir: "album"}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.mu.Unlock()
	dir := filepath.Join(m.DownloadDir(), "album")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	parts := partNames("clip.bin", job.Source, it.ID)
	for _, tc := range []struct {
		name    string
		write   map[string]string // part name -> content
		started bool
	}{
		{"nothing on disk", nil, false},
		{"an empty part", map[string]string{parts[0]: ""}, false},
		{"bytes in the shared part", map[string]string{parts[0]: "synthetic bytes"}, true},
		{"bytes in the item's own part", map[string]string{parts[1]: "synthetic bytes"}, true},
		{"another file's part", map[string]string{partNames("other.bin", job.Source, it.ID)[0]: "bytes"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, part := range append(parts[:], partNames("other.bin", job.Source, it.ID)[0]) {
				_ = os.Remove(filepath.Join(dir, part))
			}
			for part, content := range tc.write {
				if err := os.WriteFile(filepath.Join(dir, part), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := m.startedOnDisk(it); got != tc.started {
				t.Fatalf("started = %v, want %v", got, tc.started)
			}
		})
	}
}

// Through the dispatcher: the started file's route drops the connection, and
// the next route goes to it before the files queued behind it, which waited
// at the back. It resumes from what it had.
func TestAStartedFileTakesTheNextRouteAfterOneFailsIt(t *testing.T) {
	m := busyManager(t)
	payload := bytes.Repeat([]byte("synthetic bytes "), 1024)
	dropping := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, r *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close() // the route's own failure
		}
	})
	working := proxyServer(t, "127.0.0.3", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file", time.Time{}, bytes.NewReader(payload))
	})
	pool := attachPool(t, m, []string{dropping.URL, working.URL})

	var mu sync.Mutex
	var attempts []string
	m.mu.Lock()
	job := &Job{ID: newID(), Source: "http://service.example.test/collection"}
	var files []extractor.File
	for _, name := range []string{"started.bin", "second.bin", "third.bin"} {
		files = append(files, extractor.File{Name: name, Size: -1,
			Resolve: func(context.Context) (*extractor.Target, error) {
				mu.Lock()
				attempts = append(attempts, name)
				mu.Unlock()
				return &extractor.Target{URL: "http://storage.example.test/" + name, Size: -1}, nil
			},
			Pace: &extractor.Pace{Files: 1, Streams: 1, Group: config.GroupKeep2Share, PerRoute: true}})
	}
	started := m.queueJobLocked(job, files...)[0]
	started.preferredRoute = pool.Page(config.GroupKeep2Share, proxy.Query{Search: dropping.URL}).Rows[0].ID
	m.mu.Unlock()
	half := len(payload) / 2
	part := filepath.Join(m.DownloadDir(), partNames("started.bin", job.Source, started.ID)[0])
	if err := os.WriteFile(part, payload[:half], 0o644); err != nil {
		t.Fatal(err)
	}

	m.Start()
	if !waitForCond(20*time.Second, func() bool { return countStatus(m, job.ID, StatusDone) == 3 }) {
		m.mu.Lock()
		defer m.mu.Unlock()
		t.Fatalf("not all done: %s %s %s (%s)", job.Items[0].Status, job.Items[1].Status, job.Items[2].Status, job.Items[0].Err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"started.bin", "started.bin", "second.bin", "third.bin"}; !slices.Equal(attempts, want) {
		t.Fatalf("attempts %v; want the started file again before the others", attempts)
	}
	got, err := os.ReadFile(filepath.Join(m.DownloadDir(), "started.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("started.bin holds %d bytes (%v), want the %d-byte file", len(got), err, len(payload))
	}
}
