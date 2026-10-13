// SPDX-License-Identifier: MIT

package download

import (
	"io"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Buffering what the engine writes.
//
// Every write here is positional, and until this layer existed every one of
// them went straight to the file: a read returned whatever the socket had,
// and that became a write syscall. It works, and it is wasteful in a way
// that is invisible from inside the process — each write is an inotify
// event, so a download directory being watched by a media server, a sync
// client or a backup tool saw several hundred events a second and rescanned
// through the lot. See config.WriteBufferSize for the sizing.
//
// A buffer belongs to one connection rather than to the file. Eight
// connections fill eight disjoint ranges at once, so a shared buffer would
// have nothing to coalesce and would need a lock to say so; a per-connection
// one accumulates a naturally contiguous run and needs neither.
//
// The important consequence is that "received" and "on disk" are no longer
// the same number, and the resume machinery is built entirely on the second.
// So a flush reports what it durably wrote, and the segment records *that*
// as its position — never what it is still holding. The sidecar may only
// ever under-report: a short read costs a little re-downloading, while
// claiming a byte that never landed leaves a hole in the file.

// bufferedWriterAt accumulates a contiguous run of positional writes and
// passes them on in one piece.
//
// It satisfies io.WriterAt for its callers' convenience, but it is not safe
// for concurrent use and does not pretend to be: one connection owns one of
// these for one run at one segment.
type bufferedWriterAt struct {
	dst io.WriterAt
	// onFlush is told the offset just past everything now durably written.
	// Nil where the caller has no use for it — the sequential path resumes
	// from the file's own length and has nothing to record.
	onFlush func(upTo int64)

	buf     []byte
	release func()
	// start is the file offset buf[0] belongs at; held is how much of buf
	// is occupied.
	start int64
	held  int
	// filling is when the run now held began, which tells a connection that
	// needs the full-size buffer from one that does not; grown says buf is
	// that one. now is time.Now, but for tests.
	filling time.Time
	grown   bool
	now     func() time.Time
}

// newBufferedWriterAt wraps dst, starting on a WriteBufferStart buffer (see
// grow). Close returns the buffer to its pool, so every caller must call it.
func newBufferedWriterAt(dst io.WriterAt, onFlush func(upTo int64)) *bufferedWriterAt {
	buf, release := borrowStartBuffer()
	return &bufferedWriterAt{dst: dst, onFlush: onFlush, buf: buf, release: release, now: time.Now}
}

// grow swaps the starting buffer, empty after a flush, for a full-size one,
// for a connection that filled it within WriteBufferGrowWithin. A slower one
// keeps it: see config.WriteBufferSize for what a full-size buffer costs a
// connection that cannot fill it.
func (w *bufferedWriterAt) grow() {
	w.release()
	w.buf, w.release = borrowWriteBuffer()
	w.grown = true
}

// WriteAt takes p as the bytes at off, writing through only when it has to.
//
// A write that does not continue the run being held ends it: the run is
// flushed and a new one starts at off. Nothing here reorders anything, so
// bytes reach the file in the order the caller wrote them.
func (w *bufferedWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if w.held > 0 && off != w.start+int64(w.held) {
		if err := w.Flush(); err != nil {
			return 0, err
		}
	}
	// A full buffer's worth or more, with nothing held: buffering it would
	// be a copy for nothing, since it cannot wait for company. Measured
	// against the full size, so a starting buffer still fills, and grows,
	// from a connection fast enough to deliver large reads.
	if w.held == 0 && len(p) >= config.WriteBufferSize {
		n, err := w.dst.WriteAt(p, off)
		if n > 0 {
			w.note(off + int64(n))
		}
		return n, err
	}
	if w.held == 0 {
		w.start, w.filling = off, w.now()
	}

	written := 0
	for written < len(p) {
		n := copy(w.buf[w.held:], p[written:])
		w.held += n
		written += n
		if w.held == len(w.buf) {
			fast := !w.grown && w.now().Sub(w.filling) < config.WriteBufferGrowWithin
			if err := w.Flush(); err != nil {
				return written - n, err
			}
			w.start, w.filling = off+int64(written), w.now()
			if fast {
				w.grow()
			}
		}
	}
	return written, nil
}

// Flush writes out whatever is held.
//
// A partial write reports only what actually landed, both to the caller and
// to onFlush: the whole point of this layer is that the position recorded as
// durable is never ahead of the file.
func (w *bufferedWriterAt) Flush() error {
	if w.held == 0 {
		return nil
	}
	n, err := w.dst.WriteAt(w.buf[:w.held], w.start)
	if n > 0 {
		w.note(w.start + int64(n))
	}
	if err != nil {
		// What did not land stays unaccounted for; the attempt is over and
		// the next one re-fetches from the recorded position.
		w.held = 0
		return err
	}
	w.start += int64(n)
	w.held = 0
	return nil
}

// Close flushes and returns the buffer to the pool. It is safe to call
// twice, which lets a caller flush explicitly and still defer this.
func (w *bufferedWriterAt) Close() error {
	err := w.Flush()
	if w.release != nil {
		w.release()
		w.release = nil
		w.buf = nil
	}
	return err
}

func (w *bufferedWriterAt) note(upTo int64) {
	if w.onFlush != nil {
		w.onFlush(upTo)
	}
}
