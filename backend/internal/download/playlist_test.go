package download

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// A playlist transfer's whole job is order: parts fetched concurrently must
// land in the file strictly in sequence, whatever order the responses come
// back in.
func TestPlaylistJoinsPartsInOrder(t *testing.T) {
	const parts = 24
	var payload [][]byte
	for i := 0; i < parts; i++ {
		payload = append(payload, bytes.Repeat([]byte{byte(i)}, 1000+i))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		i, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/seg/"))
		if err != nil || i < 0 || i >= parts {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload[i])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := busyManager(t)
	m.streams = 4
	var segments []string
	for i := range payload {
		segments = append(segments, fmt.Sprintf("%s/seg/%d", srv.URL, i))
	}
	it := &Item{ID: newID(), Name: "show.ts", URL: srv.URL, Size: -1, Segments: segments}

	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatalf("playlist transfer: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(m.cfg.DownloadDir, "show.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Join(payload, nil); !bytes.Equal(got, want) {
		t.Fatalf("joined file differs: got %d bytes, want %d, in-order join", len(got), len(want))
	}

	m.mu.Lock()
	done, total := it.SegmentsDone, it.SegmentsTotal
	size := it.Size
	m.mu.Unlock()
	if done != parts || total != parts {
		t.Errorf("segment progress = %d/%d, want %d/%d", done, total, parts, parts)
	}
	if size <= 0 {
		t.Errorf("size after joining = %d, want the joined length", size)
	}

	// Nothing half-finished may be left beside the result.
	entries, err := os.ReadDir(m.cfg.DownloadDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %v, want only the finished file", entries)
	}
}

// One part that keeps failing must fail the transfer with the part named,
// not hang it or write a file with a hole.
func TestPlaylistReportsTheFailingPart(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/2") {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("data"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "broken.ts", URL: srv.URL, Size: -1,
		Segments: []string{srv.URL + "/seg/1", srv.URL + "/seg/2", srv.URL + "/seg/3"}}

	err := m.transfer(context.Background(), it)
	if err == nil {
		t.Fatal("a playlist with a dead part completed")
	}
	if !strings.Contains(err.Error(), "segment 2 of 3") {
		t.Errorf("the failing part should be named: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(m.cfg.DownloadDir, "broken.ts")); statErr == nil {
		t.Error("a failed playlist left a finished-looking file behind")
	}
}

// An interrupted playlist resumes from its checkpoint rather than refetching
// every part from the start.
func TestPlaylistStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "x.part")

	st := playlistState{Index: 40, Bytes: 123456, Total: 100}
	if err := savePlaylistState(part, st); err != nil {
		t.Fatal(err)
	}
	got := loadPlaylistState(part, 100)
	if got == nil || *got != st {
		t.Fatalf("round trip = %+v, want %+v", got, st)
	}

	// A playlist that changed length is a different stream; resuming into
	// it would splice two versions together.
	if loadPlaylistState(part, 101) != nil {
		t.Error("state for a 100-part stream resumed a 101-part one")
	}

	clearPlaylistState(part)
	if loadPlaylistState(part, 100) != nil {
		t.Error("cleared state was still loadable")
	}
}

func TestLoadPlaylistStateRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "y.part")

	if loadPlaylistState(part, 10) != nil {
		t.Error("a missing sidecar should mean starting over, not a value")
	}
	if err := os.WriteFile(playlistStatePath(part), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if loadPlaylistState(part, 10) != nil {
		t.Error("a corrupt sidecar should mean starting over")
	}
	if err := os.WriteFile(playlistStatePath(part), []byte(`{"index":-1,"bytes":5,"total":10}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if loadPlaylistState(part, 10) != nil {
		t.Error("an impossible index should mean starting over")
	}
}

// fetchSegment streams through the throttle and counts every byte as it
// arrives — that counter is what the stall watchdog watches.
func TestFetchSegmentCountsBytesAsTheyArrive(t *testing.T) {
	payload := bytes.Repeat([]byte{7}, 300<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := busyManager(t)
	var fetched atomic.Int64
	data, err := m.fetchSegment(context.Background(), srv.URL, nil, &fetched)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("fetched %d bytes, want %d", len(data), len(payload))
	}
	if fetched.Load() != int64(len(payload)) {
		t.Errorf("counter = %d, want %d", fetched.Load(), len(payload))
	}
}

func TestFetchSegmentRetriesAndGivesUp(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	m := busyManager(t)
	var fetched atomic.Int64
	if _, err := m.fetchSegment(context.Background(), srv.URL, nil, &fetched); err == nil {
		t.Fatal("a 404 part fetched successfully")
	}
	// 404 is definitive: asking again gets the same answer.
	if hits.Load() != 1 {
		t.Errorf("server saw %d requests for a definitive 404, want 1", hits.Load())
	}
}
