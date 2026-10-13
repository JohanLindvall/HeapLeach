// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
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

// addTestJob queues files of size bytes in a job of its own, each served at
// base/<name>/<n>. The jobs all come from one site, so a limit that held more
// than its own job would show.
func addTestJob(m *Manager, name, base string, files, size int) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := &Job{ID: newID(), Source: "http://site.example.test/" + name, Host: "site.example.test", limiter: newThrottle(0)}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	for i := range files {
		it := m.newItem(job, extractor.File{Name: fmt.Sprintf("%s-%d.bin", name, i),
			URL: fmt.Sprintf("%s/%s/%d", base, name, i), Size: int64(size)}, "", i)
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

func jobView(m *Manager, id string) JobView {
	for _, v := range m.Snapshot().Jobs {
		if v.ID == id {
			return v
		}
	}
	return JobView{}
}

// A job's limit on files holds its own beneath the queue's, while another
// job from the same site goes on alongside it.
func TestAJobsFilesAreLimitedBeneathTheQueue(t *testing.T) {
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
	if err := m.ApplySettings(Settings{Concurrency: &concurrency}); err != nil {
		t.Fatal(err)
	}
	limited, other := addTestJob(m, "limited", srv.URL, 4, len(payload)), addTestJob(m, "other", srv.URL, 3, len(payload))
	if err := m.SetJobLimit(limited.ID, JobLimit{Files: intRef(1)}); err != nil {
		t.Fatal(err)
	}
	m.Start()
	if !waitForCond(30*time.Second, func() bool { return jobDone(m, limited) && jobDone(m, other) }) {
		t.Fatal("the queue did not drain")
	}
	if n := peaks.most("limited"); n != 1 {
		t.Fatalf("the limited job ran %d files at once; want 1", n)
	}
	if n := peaks.most("other"); n < 2 {
		t.Fatalf("the other job ran %d at once; want it free of the first's limit", n)
	}
}

// A job set to no files starts none of its own, and says why, while a file
// already running finishes and other jobs go on. Lifting the limit lets the
// rest go, and takes the note with it.
func TestAJobSetToNoneHoldsItsNewDownloads(t *testing.T) {
	payload := bytes.Repeat([]byte{3}, 256<<10)
	release := make(chan struct{})
	var releaseOnce sync.Once
	let := func() { releaseOnce.Do(func() { close(release) }) }
	var started atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/held/") && started.Add(1) == 1 {
			<-release // the first file runs until the test lets it finish
		}
		(&rangeServer{payload: payload}).handler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	defer let() // before the server closes, which waits for the held request
	m := busyManager(t)
	concurrency := 2
	if err := m.ApplySettings(Settings{Concurrency: &concurrency}); err != nil {
		t.Fatal(err)
	}
	job := addTestJob(m, "held", srv.URL, 2, len(payload))
	if err := m.SetJobLimit(job.ID, JobLimit{Files: intRef(1)}); err != nil {
		t.Fatal(err)
	}
	m.Start()
	if !waitForCond(10*time.Second, func() bool { return started.Load() == 1 }) {
		t.Fatal("the first file never started")
	}
	if err := m.SetJobLimit(job.ID, JobLimit{Files: intRef(0)}); err != nil {
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
	other := addTestJob(m, "other", srv.URL, 1, len(payload))
	if !waitForCond(10*time.Second, func() bool { return jobDone(m, other) }) {
		t.Fatal("another job from the same site was held too")
	}
	m.mu.Lock()
	status, note := job.Items[1].Status, m.itemNoteLocked(job.Items[1])
	m.mu.Unlock()
	if status != StatusQueued || !strings.Contains(note, "Paused") {
		t.Fatalf("second file: %s, note %q; want it held, saying so", status, note)
	}
	if started.Load() != 1 {
		t.Fatalf("%d files were started; want only the first", started.Load())
	}
	if err := m.SetJobLimit(job.ID, JobLimit{}); err != nil {
		t.Fatal(err)
	}
	if !waitForCond(10*time.Second, func() bool { return jobDone(m, job) }) {
		t.Fatal("the held file did not run once the limit was lifted")
	}
	m.mu.Lock()
	note = m.itemNoteLocked(job.Items[1])
	m.mu.Unlock()
	if note != "" {
		t.Fatalf("note after the limit was lifted: %q", note)
	}
}

// A job's limit on streams replaces its host's, beneath the queue's: it
// caps a file the host would let split, and lets split one the host would
// not, never past the queue's own streams.
func TestAJobsStreamsReplaceItsHostsBeneathTheQueue(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pace, job     int
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
			item.Name, item.Size = "limited.bin", -1
			if tc.pace > 0 {
				item.pace = &extractor.Pace{Streams: tc.pace}
			}
			item.JobID = "streams"
			m.jobs[item.JobID] = &Job{ID: item.JobID, Limit: JobLimit{Streams: intRef(tc.job)}}
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

// A job's speed limit holds its own transfers to it while another job goes
// at full speed, and the queue's limit still holds over a looser one.
func TestAJobsSpeedLimitHoldsItsOwnTransfers(t *testing.T) {
	const size, slow = 768 << 10, 512 << 10
	// The bucket starts empty and refills at the limit: a file that takes
	// size/slow seconds at it cannot arrive in less than this.
	least := time.Duration(float64(size-slow) / slow * float64(time.Second))
	for _, tc := range []struct {
		name       string
		queue, job int64
		other      bool
	}{
		{"its own", 0, slow, true},
		{"the queue's over a looser one", slow, 100 << 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{5}, size)
			srv := httptest.NewServer((&rangeServer{payload: payload}).handler())
			defer srv.Close()
			m := busyManager(t)
			concurrency := 2
			if err := m.ApplySettings(Settings{Concurrency: &concurrency, SpeedLimit: &tc.queue}); err != nil {
				t.Fatal(err)
			}
			limited := addTestJob(m, "limited", srv.URL, 1, size)
			if err := m.SetJobLimit(limited.ID, JobLimit{SpeedLimit: tc.job}); err != nil {
				t.Fatal(err)
			}
			var other *Job
			if tc.other {
				other = addTestJob(m, "other", srv.URL, 1, size)
			}
			start := time.Now()
			m.Start()
			if other != nil {
				if !waitForCond(10*time.Second, func() bool { return jobDone(m, other) }) {
					t.Fatal("the other job did not finish")
				}
				if jobDone(m, limited) {
					t.Fatal("the limited job finished as soon as the one without a limit")
				}
			}
			if !waitForCond(30*time.Second, func() bool { return jobDone(m, limited) }) {
				t.Fatal("the limited job did not finish")
			}
			if elapsed := time.Since(start); elapsed < least {
				t.Fatalf("%d bytes arrived in %s; a limit of %d bytes a second allows no less than %s", size, elapsed, slow, least)
			}
			got, err := os.ReadFile(filepath.Join(m.DownloadDir(), "limited-0.bin"))
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("the file came out wrong: %v", err)
			}
		})
	}
}

// Job limits are checked, ride in the job's view as flat fields, and outlive
// the process with the job.
func TestJobLimitsAreCheckedShownAndKept(t *testing.T) {
	file := filepath.Join(t.TempDir(), "queue.json.zst")
	first := runManager(t, file, 10, 8, 0)
	job := addTestJob(first, "kept", "http://127.0.0.1:1", 0, 0)
	for _, bad := range []JobLimit{
		{Files: intRef(-1)},
		{Files: intRef(first.maxConcurrency() + 1)},
		{Streams: intRef(0)},
		{Streams: intRef(config.MaxStreams + 1)},
		{SpeedLimit: -1},
	} {
		if err := first.SetJobLimit(job.ID, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := first.SetJobLimit("no-such-job", JobLimit{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a job that does not exist: %v; want ErrNotFound", err)
	}
	limit := JobLimit{Files: intRef(0), Streams: intRef(2), SpeedLimit: 1 << 20}
	if err := first.SetJobLimit(job.ID, limit); err != nil {
		t.Fatal(err)
	}
	view := jobView(first, job.ID)
	if !view.JobLimit.same(limit) {
		t.Fatalf("limits shown: %+v", view.JobLimit)
	}
	body, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"fileLimit":0`, `"streamLimit":2`, `"speedLimit":1048576`} {
		if !bytes.Contains(body, []byte(field)) {
			t.Errorf("the job's view lacks %s: %s", field, body)
		}
	}
	first.Close()

	next := runManager(t, file, 10, 8, 0)
	defer next.Close()
	next.mu.Lock()
	restored := next.jobs[job.ID]
	got, speed := restored.Limit, restored.limiter.currentLimit()
	next.mu.Unlock()
	if !got.same(limit) || speed != limit.SpeedLimit {
		t.Fatalf("after a restart: %+v at %d bytes a second; want the limits chosen before it", got, speed)
	}
	if err := next.SetJobLimit(job.ID, JobLimit{}); err != nil {
		t.Fatal(err)
	}
	body, err = json.Marshal(jobView(next, job.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("Limit")) {
		t.Fatalf("a job without limits still shows some: %s", body)
	}
}

// A saved limit is taken back under the ceiling in force now, which a
// configuration may have lowered, and what no build would accept is dropped.
func TestASavedJobLimitIsHeldUnderTheCeilingInForceNow(t *testing.T) {
	got := restoredJobLimit(JobLimit{Files: intRef(10), Streams: intRef(0), SpeedLimit: -5}, 4)
	if got.Files == nil || *got.Files != 4 || got.Streams != nil || got.SpeedLimit != 0 {
		t.Fatalf("restored %+v; want 4 files, the queue's streams and no speed limit", got)
	}
}
