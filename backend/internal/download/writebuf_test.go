package download

import (
	"errors"
	"sync"
	"testing"

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
// large ones, with the bytes unchanged and in place.
func TestBufferedWriterCoalescesASequenceOfSmallWrites(t *testing.T) {
	file := &recordingWriterAt{}
	var flushed int64
	w := newBufferedWriterAt(file, func(upTo int64) { flushed = upTo })

	const chunk = 16 << 10
	count := config.WriteBufferSize / chunk // exactly one buffer's worth
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

	if len(file.writes) != 1 {
		t.Errorf("%d small writes reached the file as %d writes, want 1", count, len(file.writes))
	}
	if file.sizes[0] != config.WriteBufferSize {
		t.Errorf("wrote %d bytes, want a full buffer of %d", file.sizes[0], config.WriteBufferSize)
	}
	if flushed != int64(config.WriteBufferSize) {
		t.Errorf("reported %d bytes durable, want %d", flushed, config.WriteBufferSize)
	}
	for i := range count {
		if got := file.data[i*chunk : (i+1)*chunk]; string(got) != string(payload) {
			t.Fatalf("chunk %d came out wrong", i)
		}
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
