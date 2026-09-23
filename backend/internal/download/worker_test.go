package download

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

func TestWatchForStallAbortsSilentTransfer(t *testing.T) {
	item := &Item{}
	ctx, abort := context.WithCancel(context.Background())
	defer abort()

	var stalled atomic.Bool
	go watchForStall(ctx, item.downloaded.Load, abort, &stalled, 90*time.Millisecond, nil)

	select {
	case <-ctx.Done():
		if !stalled.Load() {
			t.Error("context was cancelled but the stall flag was not set")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a silent transfer was never aborted")
	}
}

func TestWatchForStallLeavesProgressingTransferAlone(t *testing.T) {
	item := &Item{}
	ctx, abort := context.WithCancel(context.Background())
	defer abort()

	var stalled atomic.Bool
	go watchForStall(ctx, item.downloaded.Load, abort, &stalled, 120*time.Millisecond, nil)

	// Trickle bytes more slowly than the poll interval but faster than the
	// timeout: a slow transfer must not be mistaken for a stalled one.
	done := time.After(500 * time.Millisecond)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			if stalled.Load() {
				t.Error("a slow but progressing transfer was aborted")
			}
			return
		case <-ctx.Done():
			t.Fatal("a progressing transfer was aborted")
		case <-tick.C:
			item.downloaded.Add(1024)
		}
	}
}

func TestWatchForStallExitsWithContext(t *testing.T) {
	item := &Item{}
	ctx, abort := context.WithCancel(context.Background())

	var stalled atomic.Bool
	stopped := make(chan struct{})
	go func() {
		watchForStall(ctx, item.downloaded.Load, abort, &stalled, time.Minute, nil)
		close(stopped)
	}()

	abort() // the transfer finished
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the watchdog outlived its attempt")
	}
	if stalled.Load() {
		t.Error("a completed transfer was flagged as stalled")
	}
}

func TestAlreadyOnDisk(t *testing.T) {
	dir := t.TempDir()
	const name = "clip.mp4"
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{}
	tests := []struct {
		label string
		file  string
		size  int64
		want  bool
	}{
		{"same name and length", name, 2048, true},
		{"same name, different length", name, 4096, false},
		{"unknown length", name, -1, false},
		{"zero length", name, 0, false},
		{"missing file", "other.mp4", 2048, false},
	}
	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			it := &Item{ID: "i", Name: tc.file}
			if got := m.alreadyOnDisk(it, dir, "", tc.file, tc.size); got != tc.want {
				t.Errorf("alreadyOnDisk = %v, want %v", got, tc.want)
			}
			if tc.want && it.downloaded.Load() != tc.size {
				t.Errorf("a skipped file should count as fully downloaded, got %d", it.downloaded.Load())
			}
		})
	}
}

// A directory sharing the destination name is not a finished download.
func TestAlreadyOnDiskIgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "clip.mp4"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{}
	if m.alreadyOnDisk(&Item{ID: "i"}, dir, "", "clip.mp4", 4096) {
		t.Error("a directory must never be mistaken for the downloaded file")
	}
}

// Sizes read off a listing page are rounded, so they must not be trusted to
// decide that a file on disk is already the one being fetched.
func TestApproximateSizeDoesNotSkip(t *testing.T) {
	f := extractor.File{Name: "clip.mp4", Size: 60607692, SizeApprox: true}
	m := &Manager{}
	it := m.newItem(&Job{ID: "j"}, f, "", 0)
	if !it.SizeApprox {
		t.Fatal("an approximate size must survive into the item")
	}
	if it.Size != f.Size {
		t.Errorf("Size = %d, want %d — the figure is still worth showing", it.Size, f.Size)
	}
}

// A file that is already in the destination is skipped without being
// touched: re-reading a job after a restart walks every file it ever
// downloaded, and each one reading as new afterwards would scramble any
// listing sorted by date. Both skip paths are covered — before connecting,
// when the length is known, and after the response headers, when it is not.
func TestSkippingAFinishedFileLeavesItsMtimeAlone(t *testing.T) {
	payload := []byte(strings.Repeat("finished file ", 512))
	srv := httptest.NewServer((&rangeServer{payload: payload}).handler())
	defer srv.Close()

	for name, size := range map[string]int64{"known length": int64(len(payload)), "length from headers": -1} {
		t.Run(name, func(t *testing.T) {
			m := busyManager(t)
			dest := filepath.Join(m.cfg.DownloadDir, "clip.mp4")
			if err := os.WriteFile(dest, payload, 0o644); err != nil {
				t.Fatal(err)
			}
			old := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
			if err := os.Chtimes(dest, old, old); err != nil {
				t.Fatal(err)
			}

			it := &Item{ID: newID(), Name: "clip.mp4", URL: srv.URL + "/clip.mp4", Size: size}
			if err := m.transfer(context.Background(), it); err != nil {
				t.Fatalf("transfer: %v", err)
			}
			if !it.Skipped {
				t.Fatal("the finished file was downloaded again rather than skipped")
			}
			fi, err := os.Stat(dest)
			if err != nil {
				t.Fatal(err)
			}
			if !fi.ModTime().Equal(old) {
				t.Errorf("mtime changed from %s to %s", old, fi.ModTime())
			}
			entries, _ := os.ReadDir(m.cfg.DownloadDir)
			if len(entries) != 1 {
				t.Errorf("destination holds %d entries, want only the original", len(entries))
			}
		})
	}
}

// An album can hold several different files under one name, saved as the
// name, (2) and (3). Each later read of the job has to recognise all of
// them, or the two that are not under the plain name are downloaded again
// as (4) and (5) — two more copies per restart, which is what happened.
func TestAFileSavedUnderANumberedNameIsRecognised(t *testing.T) {
	small := []byte(strings.Repeat("s", 1000))
	large := []byte(strings.Repeat("L", 3000))
	srv := httptest.NewServer((&rangeServer{payload: large}).handler())
	defer srv.Close()

	m := busyManager(t)
	dir := m.cfg.DownloadDir
	for name, body := range map[string][]byte{
		"clip.mov": small, "clip (2).mov": large, "clip (x).mov": large,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	it := &Item{ID: newID(), Name: "clip.mov", URL: srv.URL + "/clip.mov", Size: int64(len(large))}
	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if !it.Skipped {
		t.Fatal("the file already saved as clip (2).mov was downloaded again")
	}
	if it.Path != "clip (2).mov" {
		t.Errorf("path = %q, want the numbered copy it matched", it.Path)
	}
	if _, err := os.Stat(filepath.Join(dir, "clip (3).mov")); err == nil {
		t.Error("a third copy was written")
	}
}
