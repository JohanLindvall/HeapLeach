// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// peakCounter counts requests in flight per key, and the most there were.
type peakCounter struct {
	mu         sync.Mutex
	live, peak map[string]int
}

func (p *peakCounter) enter(key string) func() {
	p.mu.Lock()
	p.live[key]++
	p.peak[key] = max(p.peak[key], p.live[key])
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		p.live[key]--
		p.mu.Unlock()
	}
}

func (p *peakCounter) most(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak[key]
}

// addHostJob queues files of a job added from host, each served at
// base/<host>/<n>.
func addHostJob(m *Manager, host, base string, files int) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := &Job{ID: newID(), Source: "http://" + host + "/album", Host: host}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	for i := range files {
		it := m.newItem(job, extractor.File{Name: fmt.Sprintf("%s-%d.bin", host, i),
			URL: fmt.Sprintf("%s/%s/%d", base, host, i), Size: 256 << 10}, "", i)
		job.Items = append(job.Items, it)
		m.queue = append(m.queue, it)
	}
	m.signal()
	return job
}

func jobDone(m *Manager, job *Job) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range job.Items {
		if it.Status != StatusDone {
			return false
		}
	}
	return true
}

func intLimit(v int) *int { return &v }

// A group's limit on files holds its own beneath the queue's, while other
// groups go on alongside it.
func TestAGroupsFilesAreLimitedBeneathTheQueue(t *testing.T) {
	payload := bytes.Repeat([]byte{7}, 256<<10)
	peaks := &peakCounter{live: map[string]int{}, peak: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer peaks.enter(strings.Split(r.URL.Path, "/")[1])()
		time.Sleep(150 * time.Millisecond) // long enough to overlap
		(&rangeServer{payload: payload}).handler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	m := busyManager(t)
	concurrency := 4
	groups := map[string]GroupLimit{"slow.example.test": {Files: intLimit(1)}}
	if err := m.ApplySettings(Settings{Concurrency: &concurrency, Groups: &groups}); err != nil {
		t.Fatal(err)
	}
	m.Start()
	slow, other := addHostJob(m, "slow.example.test", srv.URL, 4), addHostJob(m, "other.example.test", srv.URL, 3)
	if !waitForCond(30*time.Second, func() bool { return jobDone(m, slow) && jobDone(m, other) }) {
		t.Fatal("the queue did not drain")
	}
	if n := peaks.most("slow.example.test"); n != 1 {
		t.Fatalf("the limited group ran %d files at once; want 1", n)
	}
	if n := peaks.most("other.example.test"); n < 2 {
		t.Fatalf("the other group ran %d at once; want it free of the first's limit", n)
	}
}

// A group set to no files starts none of its own, and says why, while a file
// already running finishes. Lifting the limit lets the rest go.
func TestAGroupSetToNoneHoldsItsNewDownloads(t *testing.T) {
	payload := bytes.Repeat([]byte{3}, 256<<10)
	release := make(chan struct{})
	var releaseOnce sync.Once
	let := func() { releaseOnce.Do(func() { close(release) }) }
	var started atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if started.Add(1) == 1 {
			<-release // the first file runs until the test lets it finish
		}
		(&rangeServer{payload: payload}).handler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	defer let() // before the server closes, which waits for the held request
	m := busyManager(t)
	concurrency := 2
	one := map[string]GroupLimit{"held.example.test": {Files: intLimit(1)}}
	if err := m.ApplySettings(Settings{Concurrency: &concurrency, Groups: &one}); err != nil {
		t.Fatal(err)
	}
	m.Start()
	job := addHostJob(m, "held.example.test", srv.URL, 2)
	if !waitForCond(10*time.Second, func() bool { return started.Load() == 1 }) {
		t.Fatal("the first file never started")
	}
	none := map[string]GroupLimit{"held.example.test": {Files: intLimit(0)}}
	if err := m.ApplySettings(Settings{Groups: &none}); err != nil {
		t.Fatal(err)
	}
	let()
	if !waitForCond(10*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return job.Items[0].Status == StatusDone
	}) {
		t.Fatal("the running file did not finish")
	}
	time.Sleep(300 * time.Millisecond)
	m.mu.Lock()
	status, note := job.Items[1].Status, m.itemNoteLocked(job.Items[1])
	m.mu.Unlock()
	if status != StatusQueued || !strings.Contains(note, "Paused") {
		t.Fatalf("second file: %s, note %q; want it held, saying so", status, note)
	}
	if started.Load() != 1 {
		t.Fatalf("%d files were started; want only the first", started.Load())
	}
	cleared := map[string]GroupLimit{}
	if err := m.ApplySettings(Settings{Groups: &cleared}); err != nil {
		t.Fatal(err)
	}
	if !waitForCond(10*time.Second, func() bool { return jobDone(m, job) }) {
		t.Fatal("the held file did not run once the limit was lifted")
	}
}

// A group's limit on streams replaces its host's, beneath the queue's: it
// caps a file the host would let split, and lets split one the host would
// not, never past the queue's own streams.
func TestAGroupsStreamsReplaceItsHostsBeneathTheQueue(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pace, group   int
		least, most   int
		queueSettings int
	}{
		{"capped beneath the queue", 0, 2, 2, 2, 8},
		{"raised above the host's own", 1, 3, 2, 3, 8},
		{"never past the queue's", 1, 6, 2, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := make([]byte, 24<<20)
			if _, err := rand.Read(payload); err != nil {
				t.Fatal(err)
			}
			var live, peak atomic.Int32
			origin := &rangeServer{payload: payload, perChunk: 3 * time.Millisecond}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := live.Add(1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				defer live.Add(-1)
				origin.handler().ServeHTTP(w, r)
			}))
			defer srv.Close()
			m, item := testTransferDeps(t, srv.URL)
			m.timings.probe, m.timings.cooldown = 30*time.Millisecond, 30*time.Millisecond
			m.streams = tc.queueSettings
			item.Name, item.Size = "grouped.bin", -1
			if tc.pace > 0 {
				item.pace = &extractor.Pace{Streams: tc.pace}
			}
			m.groupLimits = map[string]GroupLimit{"127.0.0.1": {Streams: intLimit(tc.group)}}
			if err := m.transfer(context.Background(), item); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(m.cfg.DownloadDir, item.Name)); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("the file came out wrong: %v", err)
			}
			if n := int(peak.Load()); n < tc.least || n > tc.most {
				t.Fatalf("the host saw %d connections at once; want %d to %d", n, tc.least, tc.most)
			}
		})
	}
}

// Group limits are checked, offered with the groups the queue has files
// of, and outlive the process like the queue's own settings.
func TestGroupLimitsAreCheckedShownAndKept(t *testing.T) {
	file := filepath.Join(t.TempDir(), "queue.json.zst")
	first := runManager(t, file, 10, 8, 0)
	for _, bad := range []map[string]GroupLimit{
		{"keep2share": {Files: intLimit(-1)}},
		{"keep2share": {Files: intLimit(first.maxConcurrency() + 1)}},
		{"keep2share": {Streams: intLimit(0)}},
		{" ": {Files: intLimit(1)}},
	} {
		if err := first.ApplySettings(Settings{Groups: &bad}); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	groups := map[string]GroupLimit{"keep2share": {Files: intLimit(0), Streams: intLimit(2)}, "empty": {}}
	if err := first.ApplySettings(Settings{Groups: &groups}); err != nil {
		t.Fatal(err)
	}
	addHostJob(first, "site.example.test", "http://127.0.0.1:1", 1)
	snap := first.Snapshot()
	if !slices.Equal(snap.Groups, []string{"keep2share", "site.example.test"}) {
		t.Fatalf("groups offered: %v; want the limited one and the queue's own", snap.Groups)
	}
	if got := snap.GroupLimits["keep2share"]; got.Files == nil || *got.Files != 0 || got.Streams == nil || *got.Streams != 2 {
		t.Fatalf("limits shown: %+v", snap.GroupLimits)
	}
	if _, kept := snap.GroupLimits["empty"]; kept {
		t.Fatal("a group setting nothing was kept")
	}
	first.Close()
	next := runManager(t, file, 10, 8, 0)
	defer next.Close()
	if got := (*next.CurrentSettings().Groups)["keep2share"]; got.Files == nil || *got.Files != 0 || got.Streams == nil || *got.Streams != 2 {
		t.Fatalf("after a restart: %+v; want the limits chosen before it", *next.CurrentSettings().Groups)
	}
}
