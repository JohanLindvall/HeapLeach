package download

import (
	"context"
	"os"
	"path/filepath"
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
