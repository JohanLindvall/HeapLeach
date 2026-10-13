// SPDX-License-Identifier: MIT

package download

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// recordingWriterAt is a file that remembers every write it was asked for,
// which is the thing under test: not what ends up in it, but how many
// separate writes it took — one inotify event apiece.
type recordingWriterAt struct {
	mu     sync.Mutex
	data   []byte
	writes []int64 // offset of each write
	sizes  []int
	fail   error
	short  int // when non-zero, accept only this many bytes, then fail
}

func (r *recordingWriterAt) WriteAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if r.fail != nil {
		n = r.short
	}
	if need := int(off) + n; need > len(r.data) {
		grown := make([]byte, need)
		copy(grown, r.data)
		r.data = grown
	}
	copy(r.data[off:], p[:n])
	r.writes = append(r.writes, off)
	r.sizes = append(r.sizes, n)
	if r.fail != nil {
		return n, r.fail
	}
	return n, nil
}

// The whole point: many small positional writes must reach the file as few
// large ones, with the bytes unchanged and in place. A connection this fast
// moves its first mebibyte through the starting buffer and then grows.
func TestBufferedWriterCoalescesASequenceOfSmallWrites(t *testing.T) {
	file := &recordingWriterAt{}
	var flushed int64
	w := newBufferedWriterAt(file, func(upTo int64) { flushed = upTo })

	const chunk = 16 << 10
	total := 2 * config.WriteBufferSize
	count := total / chunk
	payload := make([]byte, chunk)
	for i := range payload {
		payload[i] = byte(i)
	}
	for i := range count {
		if n, err := w.WriteAt(payload, int64(i*chunk)); n != chunk || err != nil {
			t.Fatalf("write %d: %d, %v", i, n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	small := config.WriteBufferSize / config.WriteBufferStart
	if len(file.sizes) != small+1 || file.sizes[small] != config.WriteBufferSize {
		t.Errorf("%d small writes reached the file as writes of %v; want %d of %d, then one of %d",
			count, file.sizes, small, config.WriteBufferStart, config.WriteBufferSize)
	}
	if flushed != int64(total) {
		t.Errorf("reported %d bytes durable, want %d", flushed, total)
	}
	for i := range count {
		if got := file.data[i*chunk : (i+1)*chunk]; string(got) != string(payload) {
			t.Fatalf("chunk %d came out wrong", i)
		}
	}
}

// A connection grows only once it has moved a mebibyte at 64 KiB a second
// or more. Free downloads arrive in bursts: one read can fill the starting
// buffer at once on a connection averaging 42 kB/s, so it is the average
// that is judged, and a slow one keeps 64 KiB however its bytes arrive.
func TestBufferedWriterGrowsOnlyForAFastConnection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		read     int           // bytes a read delivers
		interval time.Duration // between reads
		grows    bool
	}{
		{"steady and fast", 16 << 10, 0, true},
		{"fast, in large reads", 256 << 10, 0, true},
		{"steady and slow", 16 << 10, 400 * time.Millisecond, false}, // 40 KiB/s
		{"slow, in bursts", 256 << 10, 6 * time.Second, false},       // 43 KiB/s
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := &recordingWriterAt{}
			w := newBufferedWriterAt(file, nil)
			clock := time.Unix(0, 0)
			w.now = func() time.Time { return clock }
			payload := make([]byte, tc.read)
			for i := range 2 * config.WriteBufferSize / tc.read {
				clock = clock.Add(tc.interval)
				if n, err := w.WriteAt(payload, int64(i*tc.read)); n != tc.read || err != nil {
					t.Fatalf("write %d: %d, %v", i, n, err)
				}
			}
			if w.grown != tc.grows {
				t.Fatalf("grown = %v, want %v, with writes of %v", w.grown, tc.grows, file.sizes)
			}
			if !tc.grows && len(w.buf) != config.WriteBufferStart {
				t.Fatalf("a slow connection holds a %d-byte buffer, want %d", len(w.buf), config.WriteBufferStart)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Nothing may be reported as on disk before it is there: the sidecar is
// built on that answer, and a resume past a gap leaves a hole in the file.
func TestBufferedWriterReportsNothingUntilItFlushes(t *testing.T) {
	file := &recordingWriterAt{}
	flushed := int64(-1)
	w := newBufferedWriterAt(file, func(upTo int64) { flushed = upTo })
	defer w.Close()

	if _, err := w.WriteAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if len(file.writes) != 0 {
		t.Errorf("a partial buffer reached the file as %d writes, want none yet", len(file.writes))
	}
	if flushed != -1 {
		t.Errorf("reported %d bytes durable while still holding them", flushed)
	}

	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if flushed != 4096 {
		t.Errorf("reported %d bytes durable after flushing, want 4096", flushed)
	}
}

// A write that does not continue the run ends it. Segments are contiguous
// in practice, so this is the safety net rather than the common path.
func TestBufferedWriterFlushesWhenTheRunBreaks(t *testing.T) {
	file := &recordingWriterAt{}
	w := newBufferedWriterAt(file, nil)
	defer w.Close()

	if _, err := w.WriteAt([]byte("first"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte("second"), 4096); err != nil {
		t.Fatal(err)
	}
	if len(file.writes) != 1 || file.writes[0] != 0 {
		t.Fatalf("writes = %v, want the first run flushed on its own", file.writes)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(file.writes) != 2 || file.writes[1] != 4096 {
		t.Fatalf("writes = %v, want the second run at its own offset", file.writes)
	}
	if string(file.data[0:5]) != "first" || string(file.data[4096:4102]) != "second" {
		t.Error("the bytes did not land where they were addressed")
	}
}

// Something bigger than the buffer has nothing to wait for, so it goes
// straight through rather than being copied for nothing.
func TestBufferedWriterPassesOversizedWritesThrough(t *testing.T) {
	file := &recordingWriterAt{}
	var flushed int64
	w := newBufferedWriterAt(file, func(upTo int64) { flushed = upTo })
	defer w.Close()

	big := make([]byte, config.WriteBufferSize+4096)
	if n, err := w.WriteAt(big, 512); n != len(big) || err != nil {
		t.Fatalf("write = %d, %v", n, err)
	}
	if len(file.writes) != 1 || file.sizes[0] != len(big) {
		t.Fatalf("writes = %v sizes = %v, want one write of %d", file.writes, file.sizes, len(big))
	}
	if flushed != int64(512+len(big)) {
		t.Errorf("reported %d durable, want %d", flushed, 512+len(big))
	}
}

// A write that only half lands must report only the half: the position
// recorded as durable is never allowed ahead of the file.
func TestBufferedWriterReportsOnlyWhatLandedOnAFailedFlush(t *testing.T) {
	boom := errors.New("disk full")
	file := &recordingWriterAt{fail: boom, short: 100}
	var flushed int64
	w := newBufferedWriterAt(file, func(upTo int64) { flushed = upTo })

	if _, err := w.WriteAt(make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	err := w.Close()
	if !errors.Is(err, boom) {
		t.Fatalf("close = %v, want the write error", err)
	}
	if flushed != 100 {
		t.Errorf("reported %d bytes durable, want only the 100 that landed", flushed)
	}
}

// Callers defer Close and may also flush explicitly; both must be safe.
func TestBufferedWriterCloseIsSafeTwice(t *testing.T) {
	file := &recordingWriterAt{}
	w := newBufferedWriterAt(file, nil)
	if _, err := w.WriteAt([]byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close = %v, want nil", err)
	}
	if len(file.writes) != 1 {
		t.Errorf("the buffer was written %d times, want once", len(file.writes))
	}
}
