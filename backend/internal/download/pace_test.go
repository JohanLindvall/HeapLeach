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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// A host that punishes parallelism instead of refusing it defeats every
// defence the downloader already has: nothing is refused so the host budget
// never drops, bytes keep moving so the stall watchdog is satisfied, and the
// transfer merely looks slow — which is what opens more connections. Pace is
// how an extractor says "not this one".
func TestPaceCapsConnectionsToOneHost(t *testing.T) {
	payload := make([]byte, 24<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	var live, peak atomic.Int32
	origin := &rangeServer{payload: payload, perChunk: 3 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := live.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		defer live.Add(-1)
		origin.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	manager, item := testTransferDeps(t, srv.URL)
	// testTransferDeps leaves SlowSpeed enormous, so the supervisor always
	// wants to split — which is exactly the pressure Pace has to resist.
	item.Name = "paced.bin"
	item.Size = -1
	item.pace = &extractor.Pace{Streams: 1}

	if err := manager.transfer(context.Background(), item); err != nil {
		t.Fatalf("paced transfer: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(manager.cfg.DownloadDir, item.Name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("a paced transfer produced the wrong bytes")
	}
	if peak.Load() > 1 {
		t.Errorf("host saw %d simultaneous connections, want 1 — Pace.Streams was ignored", peak.Load())
	}
}

// Capping connections per file is not enough on its own: four files at one
// connection each is still four connections to the same host.
func TestPaceCapsSimultaneousTransfersToOneHost(t *testing.T) {
	payload := bytes.Repeat([]byte{9}, 256<<10)

	var live, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := live.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		defer live.Add(-1)
		// Held open long enough that the pool would overlap them if it could.
		time.Sleep(120 * time.Millisecond)
		(&rangeServer{payload: payload}).handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	manager := busyManager(t)
	manager.limit = 4
	manager.Start()

	job := &Job{ID: newID(), Source: srv.URL + "/album"}
	manager.mu.Lock()
	manager.jobs[job.ID] = job
	manager.order = append(manager.order, job.ID)
	for i := range 4 {
		it := manager.newItem(job, extractor.File{
			Name: fmt.Sprintf("file-%d.bin", i),
			URL:  fmt.Sprintf("%s/f/%d", srv.URL, i),
			Size: int64(len(payload)),
			Pace: &extractor.Pace{Streams: 1, Files: 1},
		}, "", i)
		job.Items = append(job.Items, it)
		manager.queue = append(manager.queue, it)
	}
	manager.mu.Unlock()
	manager.signal()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		done := 0
		for _, j := range manager.Snapshot().Jobs {
			for _, it := range j.Items {
				if it.Status == StatusDone {
					done++
				}
			}
		}
		if done == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, j := range manager.Snapshot().Jobs {
		for _, it := range j.Items {
			if it.Status != StatusDone {
				t.Fatalf("item %s ended %q (%s) — a paced host must still drain its queue",
					it.Name, it.Status, it.Error)
			}
		}
	}
	if peak.Load() > 1 {
		t.Errorf("host saw %d simultaneous transfers, want 1 — Pace.Files was ignored", peak.Load())
	}
}

// A gentle host must not idle the pool behind it: its items keep their place
// in the queue while later items from other hosts start.
func TestPacedItemsDoNotBlockTheQueue(t *testing.T) {
	m := busyManager(t)
	m.limit = 4

	job := &Job{ID: newID(), Source: "https://slow.example.test/a"}
	m.jobs[job.ID] = job

	paced := func(n int) *Item {
		return m.newItem(job, extractor.File{
			Name: fmt.Sprintf("slow-%d.bin", n),
			URL:  fmt.Sprintf("https://slow.example.test/f/%d", n),
			Pace: &extractor.Pace{Files: 1},
		}, "", n)
	}
	free := m.newItem(job, extractor.File{
		Name: "free.bin", URL: "https://other.example.test/f",
	}, "", 9)

	m.queue = []*Item{paced(0), paced(1), free}

	first := m.nextLocked()
	if first == nil || !strings.HasPrefix(first.Name, "slow-0") {
		t.Fatalf("first = %v, want the first paced item", first)
	}
	m.hostActive[m.hostKeyLocked(first)]++
	first.inFlight = true

	// The second paced item is now over its host's ceiling, so the item
	// behind it must be started instead — and the paced one must survive.
	second := m.nextLocked()
	if second == nil || second.Name != "free.bin" {
		t.Fatalf("second = %v, want the unpaced item to overtake", second)
	}
	if len(m.queue) != 1 || m.queue[0].Name != "slow-1.bin" {
		t.Fatalf("queue = %v, want the held item kept in place", m.queue)
	}
}

// A rejection from the extractor's own guard is final: the resource is gone,
// so repeating the request only gets the same answer.
func TestRejectByHostIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	// The shape imgur actually has: a deleted image redirects to a real PNG
	// that is correct in every respect except being the wrong file. Nothing
	// in the response is malformed, so no general rule can catch it.
	removed := bytes.Repeat([]byte{0x89}, 503)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/removed.png" {
			w.Header().Set("Content-Type", "image/png")
			(&rangeServer{payload: removed}).handler().ServeHTTP(w, r)
			return
		}
		hits.Add(1)
		http.Redirect(w, r, "/removed.png", http.StatusFound)
	}))
	defer srv.Close()

	m := busyManager(t)
	it := &Item{
		ID: newID(), Name: "photo.jpg", URL: srv.URL + "/gone.jpg", Size: -1,
		reject: func(finalURL string, _ http.Header) error {
			if strings.HasSuffix(finalURL, "/removed.png") {
				return fmt.Errorf("this image has been removed")
			}
			return nil
		},
	}

	err := m.transfer(context.Background(), it)
	if err == nil {
		t.Fatal("a removed image downloaded successfully")
	}
	if !strings.Contains(err.Error(), "removed") {
		t.Errorf("error should carry the host's own explanation: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("made %d attempts, want 1 — a gone resource must not be retried", hits.Load())
	}
	if _, statErr := os.Stat(filepath.Join(m.cfg.DownloadDir, "photo.jpg")); statErr == nil {
		t.Error("a rejected response was written to disk")
	}
}

func TestRejectByHostPassesGoodResponses(t *testing.T) {
	payload := bytes.Repeat([]byte{3}, 4096)
	srv := httptest.NewServer((&rangeServer{payload: payload}).handler())
	defer srv.Close()

	m := busyManager(t)
	var asked atomic.Int32
	it := &Item{
		ID: newID(), Name: "fine.bin", URL: srv.URL + "/fine.bin", Size: -1,
		reject: func(string, http.Header) error { asked.Add(1); return nil },
	}
	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if asked.Load() == 0 {
		t.Error("the guard was never consulted")
	}
	got, err := os.ReadFile(filepath.Join(m.cfg.DownloadDir, "fine.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("file = %d bytes, %v", len(got), err)
	}
}
