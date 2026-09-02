package download

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// droppingServer serves a file the way a flaky CDN does: each response
// carries the next scripted number of bytes and then the connection is
// dropped. Ranges are honoured, so a retry resumes where it left off, and no
// length is ever stated, which keeps the transfer on the sequential path
// where every retry is the manager's own rather than a segment's.
type droppingServer struct {
	payload []byte
	// chunks scripts how many bytes each successive response carries before
	// the connection is dropped; the last entry repeats. Zero drops the
	// connection after the headers and before any of the body.
	chunks []int
	// ignoreRange answers every request from the start of the file, the way
	// a host without range support does, so a retry can never get further
	// than the first attempt did.
	ignoreRange bool
	hits        atomic.Int32
}

func (s *droppingServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(s.hits.Add(1)) - 1
		chunk := s.chunks[min(n, len(s.chunks)-1)]

		start := 0
		if hdr := r.Header.Get("Range"); hdr != "" && !s.ignoreRange {
			from, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(hdr, "bytes="), "-"))
			if err != nil || from < 0 || from >= len(s.payload) {
				http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start = from
			// A total of "*" is a resumable answer that still names no
			// length, which is what keeps this off the segmented engine.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", start, len(s.payload)-1))
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Header().Set("Content-Type", "application/octet-stream")

		rest := s.payload[start:]
		if chunk >= len(rest) {
			_, _ = w.Write(rest) // the remainder, whole: a clean finish
			return
		}
		_, _ = w.Write(rest[:chunk])
		w.(http.Flusher).Flush()
		// The documented way to drop the connection mid-response: the
		// client sees an unexpected EOF, as it would from a node going away.
		panic(http.ErrAbortHandler)
	})
}

// progressManager is a manager with the given retry budget and the wait
// after a productive attempt shrunk to test speed.
func progressManager(t *testing.T, retries int) *Manager {
	t.Helper()
	cfg := &config.Config{
		DownloadDir: t.TempDir(), Concurrency: 1, Streams: 1,
		UserAgent: config.DefaultUserAgent, Language: config.DefaultLanguage,
		GofileSecret: config.FallbackGofileSecret,
		MaxRetries:   retries, Timeout: 30 * time.Second,
	}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), cfg.MaxRetries, cfg.Timeout)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(cfg, extractor.NewRegistry(cfg, client), client, log)
	m.timings.progressRetry = time.Millisecond
	t.Cleanup(m.Close)
	return m
}

func randomPayload(t *testing.T, n int) []byte {
	t.Helper()
	payload := make([]byte, n)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// A connection that drops after moving bytes is resumed, not counted: with
// no retries at all in the budget, a file that arrives in four pieces still
// arrives, because each attempt got somewhere.
func TestDroppedConnectionAfterProgressResumesRatherThanFailing(t *testing.T) {
	payload := randomPayload(t, 100_000)
	origin := &droppingServer{payload: payload, chunks: []int{30_000}}
	srv := httptest.NewServer(origin.handler())
	defer srv.Close()

	m := progressManager(t, 0)
	// Long enough to be seen, short enough not to matter.
	m.timings.progressRetry = 120 * time.Millisecond
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/clip.mp4", Size: -1}

	// The wait is announced on the item, so the UI can say why a transfer
	// that just failed is still running.
	var sawNote atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				m.mu.Lock()
				if strings.Contains(it.Note, "resuming") {
					sawNote.Store(true)
				}
				m.mu.Unlock()
			}
		}
	}()

	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatalf("a transfer that moved bytes on every attempt failed: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(m.cfg.DownloadDir, "clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes, which differ from the %d served", len(got), len(payload))
	}
	if hits := origin.hits.Load(); hits != 4 {
		t.Errorf("server saw %d requests, want 4: one per 30 kB piece", hits)
	}
	if got := it.downloaded.Load(); got != int64(len(payload)) {
		t.Errorf("downloaded = %d, want every one of the %d bytes counted", got, len(payload))
	}
	if !sawNote.Load() {
		t.Error("the item never said it was waiting to resume")
	}
	m.mu.Lock()
	note := it.Note
	m.mu.Unlock()
	if note != "" {
		t.Errorf("note %q outlived the wait it described", note)
	}
}

// An attempt that moves nothing is the case the budget exists for, and it
// still spends it: headers and then a dropped connection, MaxRetries+1 times,
// is a failure.
func TestAttemptsThatMoveNothingStillSpendTheBudget(t *testing.T) {
	origin := &droppingServer{payload: randomPayload(t, 100_000), chunks: []int{0}}
	srv := httptest.NewServer(origin.handler())
	defer srv.Close()

	m := progressManager(t, 1)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/clip.mp4", Size: -1}

	if err := m.transfer(context.Background(), it); err == nil {
		t.Fatal("a server that never sends a byte produced a download")
	}
	if hits := origin.hits.Load(); hits != 2 {
		t.Errorf("server saw %d requests, want MaxRetries+1 = 2", hits)
	}
	if got := it.downloaded.Load(); got != 0 {
		t.Errorf("downloaded = %d bytes from a server that sent none", got)
	}
}

// Progress starts the count over rather than merely excusing its own
// attempt: after one productive attempt the transfer is owed a full budget
// of unproductive ones again, and only then does it fail — with what it did
// get kept on disk for a retry to resume from.
func TestProgressRestartsTheRetryCount(t *testing.T) {
	origin := &droppingServer{payload: randomPayload(t, 100_000), chunks: []int{30_000, 0}}
	srv := httptest.NewServer(origin.handler())
	defer srv.Close()

	m := progressManager(t, 1)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/clip.mp4", Size: -1}

	if err := m.transfer(context.Background(), it); err == nil {
		t.Fatal("a server that stops serving after one piece produced a download")
	}
	// One that moved bytes, then MaxRetries+1 that moved nothing.
	if hits := origin.hits.Load(); hits != 3 {
		t.Errorf("server saw %d requests, want 1 + (MaxRetries+1) = 3", hits)
	}

	parts, _ := filepath.Glob(filepath.Join(m.cfg.DownloadDir, "*.part"))
	if len(parts) != 1 {
		t.Fatalf("part files left for a retry to resume from: %v, want one", parts)
	}
	if info, err := os.Stat(parts[0]); err != nil || info.Size() != 30_000 {
		t.Errorf("the part file holds %d bytes, want the 30000 that arrived", info.Size())
	}
}

// Bytes arriving is not progress unless the file gets further. A host that
// ignores Range serves the same first piece on every attempt, so each one
// receives plenty and leaves the part file exactly where it was; treating
// that as progress would retry it forever. It spends the budget like any
// other attempt that got nowhere.
func TestRestartingFromZeroIsNotProgress(t *testing.T) {
	origin := &droppingServer{payload: randomPayload(t, 100_000), chunks: []int{30_000}, ignoreRange: true}
	srv := httptest.NewServer(origin.handler())
	defer srv.Close()

	m := progressManager(t, 1)
	it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/clip.mp4", Size: -1}

	if err := m.transfer(context.Background(), it); err == nil {
		t.Fatal("a server that restarts from zero every time produced a download")
	}
	// The first attempt did get the file from nothing to 30 kB, which is
	// progress; every one after it got exactly the same 30 kB, which is not.
	if hits := origin.hits.Load(); hits != 3 {
		t.Errorf("server saw %d requests, want 1 + (MaxRetries+1) = 3", hits)
	}
	if got := it.downloaded.Load(); got != 30_000 {
		t.Errorf("downloaded = %d, want the 30000 the file never got past", got)
	}
}

// A stall after bytes had landed is the same shape as a dropped connection
// after progress, and starts the deferral count over the same way.
func TestStallAfterProgressRestartsTheDeferralCount(t *testing.T) {
	m := &Manager{
		cfg: &config.Config{MaxRetries: 2},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Two deferrals in, with the budget about to run out.
	it := &Item{ID: "stalled", Status: StatusRunning, stallDefers: 2}
	if m.deferStalledLocked(it, &stalledError{name: "clip.mp4", after: time.Minute}) {
		t.Fatal("a stall past the budget was deferred although nothing had moved")
	}

	it = &Item{ID: "moving", Status: StatusRunning, stallDefers: 2}
	if !m.deferStalledLocked(it, &stalledError{name: "clip.mp4", after: time.Minute, moved: true}) {
		t.Fatal("a stall after progress was failed instead of deferred")
	}
	if it.stallDefers != 1 {
		t.Errorf("stallDefers = %d after a productive stall, want the count started over at 1", it.stallDefers)
	}
	if !strings.Contains(it.Note, "(1 of 2)") {
		t.Errorf("note = %q, want it to show the count starting over", it.Note)
	}
}
