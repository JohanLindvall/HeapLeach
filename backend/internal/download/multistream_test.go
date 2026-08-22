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

// rangeServer serves a fixed payload, honouring byte ranges and pacing each
// connection so a single stream looks slow enough to be worth splitting.
type rangeServer struct {
	payload     []byte
	perChunk    time.Duration
	ignoreRange bool

	live atomic.Int32
	peak atomic.Int32
	hits atomic.Int32
}

func (s *rangeServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		n := s.live.Add(1)
		for {
			old := s.peak.Load()
			if n <= old || s.peak.CompareAndSwap(old, n) {
				break
			}
		}
		defer s.live.Add(-1)

		total := int64(len(s.payload))
		start, end := int64(0), total-1

		if hdr := r.Header.Get("Range"); hdr != "" && !s.ignoreRange {
			var err error
			if start, end, err = parseRange(hdr, total); err != nil {
				http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"fixed-etag"`)
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		if r.Header.Get("Range") != "" && !s.ignoreRange {
			w.WriteHeader(http.StatusPartialContent)
		}

		const chunk = 64 << 10
		for at := start; at <= end; at += chunk {
			stop := min(at+chunk-1, end)
			if _, err := w.Write(s.payload[at : stop+1]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(s.perChunk):
			}
		}
	})
}

func parseRange(header string, total int64) (start, end int64, err error) {
	spec := strings.TrimPrefix(header, "bytes=")
	lo, hi, _ := strings.Cut(spec, "-")
	if start, err = strconv.ParseInt(lo, 10, 64); err != nil {
		return 0, 0, err
	}
	end = total - 1
	if hi != "" {
		if end, err = strconv.ParseInt(hi, 10, 64); err != nil {
			return 0, 0, err
		}
	}
	if start < 0 || start >= total || end < start {
		return 0, 0, fmt.Errorf("unsatisfiable")
	}
	return start, min(end, total-1), nil
}

// The whole point of splitting is that the bytes still land in the right
// places. A wrong offset would corrupt the file silently, so the check is
// byte-for-byte against what was served.
func TestSegmentedTransferIsByteExact(t *testing.T) {
	payload := make([]byte, 12<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	origin := &rangeServer{payload: payload, perChunk: 3 * time.Millisecond}
	srv := httptest.NewServer(origin.handler())
	defer srv.Close()

	part, got := runSegmented(t, srv.URL, len(payload), 8)

	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes but they differ from the source", len(got))
	}
	if peak := origin.peak.Load(); peak < 2 {
		t.Errorf("peak concurrent connections = %d, want the transfer to have been split", peak)
	} else {
		t.Logf("split across %d concurrent connections, %d requests total", peak, origin.hits.Load())
	}
	if _, err := os.Stat(statePath(part)); err == nil {
		t.Error("resume sidecar was left behind after a clean finish")
	}
}

// A server that ignores Range must fall back to one connection and still
// produce the right file.
func TestSegmentedTransferHandlesNoRangeSupport(t *testing.T) {
	payload := make([]byte, 9<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	origin := &rangeServer{payload: payload, perChunk: time.Millisecond, ignoreRange: true}
	srv := httptest.NewServer(origin.handler())
	defer srv.Close()

	_, got := runSegmented(t, srv.URL, len(payload), 1)

	if !bytes.Equal(got, payload) {
		t.Fatal("file differs from the source")
	}
	if peak := origin.peak.Load(); peak != 1 {
		t.Errorf("used %d connections against a server without range support, want 1", peak)
	}
}

// An interrupted transfer must resume from its sidecar rather than starting
// over, and still finish byte-exact.
func TestSegmentedTransferResumesFromState(t *testing.T) {
	payload := make([]byte, 12<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	origin := &rangeServer{payload: payload, perChunk: 3 * time.Millisecond}
	srv := httptest.NewServer(origin.handler())
	defer srv.Close()

	dir := t.TempDir()
	part := filepath.Join(dir, "resume.part")
	manager, item := testTransferDeps(t, srv.URL)

	// First pass: abort partway through.
	ctx, cancel := context.WithCancel(context.Background())
	table := newSegmentTable(int64(len(payload)), 0)
	tr, partFile := newTestTransfer(manager, item, part, table, 8)
	go func() {
		for item.downloaded.Load() < int64(len(payload))/4 {
			time.Sleep(2 * time.Millisecond)
		}
		cancel()
	}()
	body := openPrimary(t, ctx, manager, srv.URL, table.snapshot()[0])
	_ = tr.run(ctx, table.snapshot()[0], body)
	tr.saveState()
	partFile.Close()

	state := loadTransferState(part)
	if state == nil {
		t.Fatal("no resume state was written")
	}
	done := int64(0)
	for _, s := range state.Segments {
		done += s.Pos - s.Start
	}
	if done == 0 || done >= int64(len(payload)) {
		t.Fatalf("resume state records %d of %d bytes; wanted a partial transfer", done, len(payload))
	}
	t.Logf("interrupted with %d of %d bytes across %d segments", done, len(payload), len(state.Segments))

	// Second pass: resume and finish.
	restored := restoreSegmentTable(state.Size, state.Segments)
	var primary *segment
	for _, s := range restored.snapshot() {
		if !s.done() {
			primary = s
			break
		}
	}
	tr2, part2File := newTestTransfer(manager, item, part, restored, 8)
	defer part2File.Close()
	item.downloaded.Store(restored.written())

	body2 := openPrimary(t, context.Background(), manager, srv.URL, primary)
	if err := tr2.run(context.Background(), primary, body2); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	part2File.Close()

	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("resumed file differs from the source")
	}
}

// --- helpers ---------------------------------------------------------------

func testTransferDeps(t *testing.T, url string) (*Manager, *Item) {
	t.Helper()
	cfg := &config.Config{
		DownloadDir:  t.TempDir(),
		Concurrency:  1,
		Streams:      8,
		SlowSpeed:    1 << 40, // always "slow", so splitting always applies
		UserAgent:    config.DefaultUserAgent,
		Language:     config.DefaultLanguage,
		GofileSecret: config.FallbackGofileSecret,
		MaxRetries:   2,
		Timeout:      30 * time.Second,
	}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), cfg.MaxRetries, cfg.Timeout)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := New(cfg, extractor.NewRegistry(cfg, client), client, log)

	item := &Item{ID: "test", URL: url}
	return manager, item
}

// newTestTransfer builds a transfer over a fresh part file, returning the
// file too: the transfer writes through an io.WriterAt, which a host serving
// ciphertext slots a decrypting layer into, so it is no longer the thing to
// close.
func newTestTransfer(m *Manager, it *Item, part string, table *segmentTable, streams int) (*segmentedTransfer, *os.File) {
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	tr := &segmentedTransfer{
		manager: m, item: it, file: f, table: table, part: part,
		name: "test", validator: `"fixed-etag"`, maxStreams: streams,
		slowBelow:     m.cfg.SlowSpeed,
		pollInterval:  5 * time.Millisecond,
		probeInterval: 25 * time.Millisecond,
		addCooldown:   25 * time.Millisecond,
		saveInterval:  20 * time.Millisecond,
	}
	return tr.withDefaults(), f
}

func openPrimary(t *testing.T, ctx context.Context, m *Manager, url string, seg *segment) io.ReadCloser {
	t.Helper()
	req, err := m.client.NewRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(httpx.HeaderRange,
		"bytes="+strconv.FormatInt(seg.pos.Load(), 10)+"-"+strconv.FormatInt(seg.end.Load()-1, 10))
	resp, err := m.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.Body
}

// runSegmented performs a whole transfer and returns the part path and its
// contents.
func runSegmented(t *testing.T, url string, size, streams int) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	part := filepath.Join(dir, "download.part")
	manager, item := testTransferDeps(t, url)

	table := newSegmentTable(int64(size), 0)
	tr, partFile := newTestTransfer(manager, item, part, table, streams)
	defer partFile.Close()
	tr.saveState()

	primary := table.snapshot()[0]
	body := openPrimary(t, context.Background(), manager, url, primary)

	if err := tr.run(context.Background(), primary, body); err != nil {
		t.Fatalf("transfer failed: %v", err)
	}
	if err := partFile.Close(); err != nil {
		t.Fatal(err)
	}
	clearTransferState(part)

	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != size {
		t.Fatalf("file is %d bytes, want %d", len(got), size)
	}
	return part, got
}

// The full path, from a submitted URL down to the finished file.
//
// The engine tests above build a segmentedTransfer directly, which is
// exactly how a real bug slipped through once: transferOnce sent no Range
// header when starting from byte zero, so every server answered 200, was
// judged incapable of ranges, and no transfer was ever split. This covers
// that seam.
func TestSlowDownloadSplitsThroughTheManager(t *testing.T) {
	payload := make([]byte, 12<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	origin := &rangeServer{payload: payload, perChunk: 4 * time.Millisecond}
	srv := httptest.NewServer(origin.handler())
	defer srv.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		DownloadDir:  dir,
		Concurrency:  1,
		Streams:      8,
		SlowSpeed:    1 << 40, // treat every transfer as slow
		UserAgent:    config.DefaultUserAgent,
		Language:     config.DefaultLanguage,
		GofileSecret: config.FallbackGofileSecret,
		MaxRetries:   1,
		Timeout:      30 * time.Second,
	}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), cfg.MaxRetries, cfg.Timeout)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	manager := New(cfg, extractor.NewRegistry(cfg, client), client, log)
	manager.timings = downloadTimings{
		poll:     5 * time.Millisecond,
		probe:    25 * time.Millisecond,
		cooldown: 25 * time.Millisecond,
		save:     20 * time.Millisecond,
	}
	manager.Start()
	defer manager.Close()

	if _, err := manager.Add(srv.URL+"/payload.bin", ""); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(60 * time.Second)
	var finished bool
	for time.Now().Before(deadline) && !finished {
		for _, job := range manager.Snapshot().Jobs {
			for _, item := range job.Items {
				switch item.Status {
				case StatusDone:
					finished = true
				case StatusFailed:
					t.Fatalf("download failed: %s", item.Error)
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !finished {
		t.Fatal("download did not finish in time")
	}

	if peak := origin.peak.Load(); peak < 2 {
		t.Errorf("peak connections = %d; a slow transfer was never split", peak)
	} else {
		t.Logf("split across %d concurrent connections", peak)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one finished file, got %v", entries)
	}
	got, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("finished file (%d bytes) differs from the source (%d bytes)", len(got), len(payload))
	}
}

// Hosts commonly allow only so many connections at once. When the extra
// ones are turned away the transfer must settle for what it has and still
// finish correctly, rather than failing a download that was working.
func TestRefusedExtraConnectionsStillFinish(t *testing.T) {
	payload := make([]byte, 12<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	origin := &rangeServer{payload: payload, perChunk: 4 * time.Millisecond}
	const allowed = 2 // this host tolerates two connections and no more

	var refusals atomic.Int32
	gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin.live.Load() >= allowed {
			refusals.Add(1)
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many connections", http.StatusTooManyRequests)
			return
		}
		origin.handler().ServeHTTP(w, r)
	})
	srv := httptest.NewServer(gated)
	defer srv.Close()

	manager, item := testTransferDeps(t, srv.URL)
	manager.hosts = newHostLimiter(8)

	dir := t.TempDir()
	part := filepath.Join(dir, "gated.part")
	table := newSegmentTable(int64(len(payload)), 0)
	tr, partFile := newTestTransfer(manager, item, part, table, 8)
	defer partFile.Close()

	primary := table.snapshot()[0]
	body := openPrimary(t, context.Background(), manager, srv.URL, primary)

	if err := tr.run(context.Background(), primary, body); err != nil {
		t.Fatalf("a download that hit the host's connection limit failed outright: %v", err)
	}
	if err := partFile.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("file differs from the source after connections were refused")
	}
	if refusals.Load() == 0 {
		t.Skip("the host was never pushed past its limit; nothing to prove here")
	}
	t.Logf("host refused %d extra connections; transfer completed byte-exact "+
		"and settled on a limit of %d", refusals.Load(), manager.hosts.limit(hostOf(srv.URL)))

	if limit := manager.hosts.limit(hostOf(srv.URL)); limit >= 8 {
		t.Errorf("host limit is still %d after refusals; nothing was learned", limit)
	}
}

// A busy host is not a failing one. Its attempts must not spend the retry
// budget, which exists for transfers that are genuinely going wrong —
// otherwise a gofile link on a busy storage server gives up after three
// tries instead of waiting for the server to free up. The number of busy
// answers here deliberately fits inside BusyRetryLimit, which is where a
// resolver-less item stops; TestBusyCap* pin the boundary itself from both
// sides.
func TestBusyHostRetriesBeyondTheRetryBudget(t *testing.T) {
	payload := make([]byte, 1<<20) // small enough that nothing is split
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	const busyResponses = 5 // far more than the retry budget below
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= busyResponses {
			// What gofile's 302-to-its-own-page looks like once followed.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<!doctype html><html><body>busy</body></html>"))
			return
		}
		(&rangeServer{payload: payload}).handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		DownloadDir:  dir,
		Concurrency:  1,
		Streams:      1,
		SlowSpeed:    0,
		UserAgent:    config.DefaultUserAgent,
		Language:     config.DefaultLanguage,
		GofileSecret: config.FallbackGofileSecret,
		MaxRetries:   1, // deliberately tiny: busy must not consume it
		Timeout:      30 * time.Second,
	}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), cfg.MaxRetries, cfg.Timeout)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	manager := New(cfg, extractor.NewRegistry(cfg, client), client, log)
	manager.timings.busyBase = 10 * time.Millisecond
	manager.timings.busyMax = 40 * time.Millisecond
	manager.Start()
	defer manager.Close()

	if _, err := manager.Add(srv.URL+"/video.mp4", ""); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(30 * time.Second)
	var status Status
	for time.Now().Before(deadline) {
		for _, job := range manager.Snapshot().Jobs {
			for _, item := range job.Items {
				status = item.Status
				if item.Status == StatusFailed {
					t.Fatalf("gave up on a busy host after %d attempts: %s",
						attempts.Load(), item.Error)
				}
			}
		}
		if status == StatusDone {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if status != StatusDone {
		t.Fatalf("download did not finish (status %q after %d attempts)", status, attempts.Load())
	}

	if got := attempts.Load(); got <= busyResponses {
		t.Errorf("made %d attempts, want more than the %d busy responses", got, busyResponses)
	} else {
		t.Logf("waited through %d busy responses on a retry budget of %d", busyResponses, cfg.MaxRetries)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one file, got %v", entries)
	}
	got, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the file that finally downloaded differs from the source")
	}
}
