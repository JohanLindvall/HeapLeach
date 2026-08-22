package download

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"sync/atomic"
)

// Multi-connection transfers.
//
// A slow transfer is often slow because of a per-connection limit rather
// than a saturated link, so when throughput stays low the worker opens
// extra connections and gives each its own byte range.
//
// Ranges are chosen by repeatedly bisecting whatever is still outstanding:
// the first extra connection starts halfway through the file, the next two
// at the quarter points, then the eighths, and so on. Bisecting the
// *remaining* span of the busiest segment, rather than fixed fractions of
// the whole file, keeps the work balanced once some segments are ahead —
// at the first split, with nothing downloaded yet, the two are the same.

// segment is one contiguous range of the file, fetched by one connection.
//
// start is fixed. pos advances as bytes land. end is exclusive and shrinks
// when the segment is split, which is why both are atomic: the supervisor
// moves end while the segment's own goroutine is reading.
type segment struct {
	start int64
	pos   atomic.Int64
	end   atomic.Int64
}

func newSegment(start, pos, end int64) *segment {
	s := &segment{start: start}
	s.pos.Store(pos)
	s.end.Store(end)
	return s
}

// remaining is how many bytes this segment still owes.
func (s *segment) remaining() int64 {
	if r := s.end.Load() - s.pos.Load(); r > 0 {
		return r
	}
	return 0
}

// done reports whether the segment has reached its end.
func (s *segment) done() bool { return s.pos.Load() >= s.end.Load() }

// written is how many bytes this segment has contributed.
func (s *segment) written() int64 {
	pos, end := s.pos.Load(), s.end.Load()
	if pos > end {
		pos = end
	}
	if pos < s.start {
		return 0
	}
	return pos - s.start
}

// segmentTable owns the set of ranges covering one file.
type segmentTable struct {
	mu   sync.Mutex
	segs []*segment
	size int64
}

// newSegmentTable starts with a single segment covering the whole file,
// already positioned at whatever is on disk.
func newSegmentTable(size, offset int64) *segmentTable {
	return &segmentTable{segs: []*segment{newSegment(0, offset, size)}, size: size}
}

// restoreSegmentTable rebuilds a table from saved state.
func restoreSegmentTable(size int64, saved []segmentState) *segmentTable {
	t := &segmentTable{size: size}
	for _, s := range saved {
		t.segs = append(t.segs, newSegment(s.Start, s.Pos, s.End))
	}
	return t
}

func (t *segmentTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.segs)
}

// written totals the bytes held across every segment.
func (t *segmentTable) written() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var total int64
	for _, s := range t.segs {
		total += s.written()
	}
	return total
}

// complete reports whether every segment has reached its end.
func (t *segmentTable) complete() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.segs {
		if !s.done() {
			return false
		}
	}
	return true
}

// split bisects the segment with the most work left and returns the new
// tail segment, or nil when nothing is worth splitting.
//
// Both halves must be at least minPiece, which besides avoiding pointless
// tiny requests keeps the split point far ahead of the writer: a reader
// advances by at most one copy buffer between this decision and its next
// write, and minPiece is far larger than that, so a segment can never write
// past the end assigned to it here.
func (t *segmentTable) split(minPiece int64) *segment {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Widest first, and on a tie the earliest in the file. Ties are the
	// normal case — after the halves are split, every quarter is equal —
	// and resolving them by position is what makes the split points come
	// out in their natural order: 1/2, then 1/4 and 3/4, then the eighths
	// left to right.
	var widest *segment
	for _, s := range t.segs {
		switch {
		case widest == nil:
			widest = s
		case s.remaining() > widest.remaining():
			widest = s
		case s.remaining() == widest.remaining() && s.start < widest.start:
			widest = s
		}
	}
	if widest == nil {
		return nil
	}

	pos, end := widest.pos.Load(), widest.end.Load()
	remaining := end - pos
	if remaining < 2*minPiece {
		return nil
	}

	mid := pos + remaining/2
	tail := newSegment(mid, mid, end)
	widest.end.Store(mid)
	t.segs = append(t.segs, tail)
	return tail
}

// snapshot returns the current segments. The slice is a copy; the segments
// themselves are shared and update as the transfer proceeds.
func (t *segmentTable) snapshot() []*segment {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*segment(nil), t.segs...)
}

// finish confirms a segment has reached its end.
//
// It is the only way a reader leaves its loop, and it runs under the table
// lock so the decision cannot race a retire that would have extended the
// segment. Together with retire's refusal to extend a finished segment,
// this guarantees no range is ever left without a reader.
func (t *segmentTable) finish(seg *segment) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return seg.pos.Load() >= seg.end.Load()
}

// retire hands an untouched segment's range back to the segment before it
// and drops it from the table, for when the extra connection it was created
// for turned out to be unwelcome.
//
// Only a segment that has downloaded nothing can be retired, since its range
// is given away whole, and only into a neighbour still working, since a
// finished neighbour's reader has gone.
func (t *segmentTable) retire(seg *segment) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.segs) < 2 || seg.pos.Load() != seg.start {
		return false
	}
	var left *segment
	for _, candidate := range t.segs {
		if candidate != seg && candidate.end.Load() == seg.start && candidate.pos.Load() < candidate.end.Load() {
			left = candidate
			break
		}
	}
	if left == nil {
		return false
	}

	left.end.Store(seg.end.Load())
	for i, candidate := range t.segs {
		if candidate == seg {
			t.segs = append(t.segs[:i], t.segs[i+1:]...)
			break
		}
	}
	return true
}

// state captures the table for the sidecar file, in file order.
//
// Segments accumulate in the order they were split off, which is not
// ascending; the sidecar is sorted so it reads as a plain tiling of the
// file, which is also what loading validates against.
func (t *segmentTable) state() []segmentState {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]segmentState, 0, len(t.segs))
	for _, s := range t.segs {
		out = append(out, segmentState{Start: s.start, Pos: s.pos.Load(), End: s.end.Load()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// transferState is the sidecar written next to a .part file so a partly
// finished multi-connection transfer can be resumed.
//
// It is always written before any data, and re-written after data lands, so
// it can only ever under-report progress. Under-reporting costs a little
// re-download; over-reporting would leave holes in the file.
type transferState struct {
	Size      int64          `json:"size"`
	Validator string         `json:"validator,omitempty"`
	Segments  []segmentState `json:"segments"`
}

type segmentState struct {
	Start int64 `json:"start"`
	Pos   int64 `json:"pos"`
	End   int64 `json:"end"`
}

// statePath is the sidecar path for a part file.
func statePath(part string) string { return part + ".state" }

// loadTransferState reads the sidecar, returning nil when it is absent or
// unusable. A missing or broken sidecar simply means starting over.
func loadTransferState(part string) *transferState {
	raw, err := os.ReadFile(statePath(part))
	if err != nil {
		return nil
	}
	var st transferState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	if st.Size <= 0 || len(st.Segments) == 0 {
		return nil
	}
	if !st.valid() {
		return nil
	}
	return &st
}

// valid rejects a sidecar whose segments do not tile the file exactly, which
// would otherwise resume into a file with holes.
func (st *transferState) valid() bool {
	next := int64(0)
	for _, s := range st.Segments {
		if s.Start != next || s.Pos < s.Start || s.Pos > s.End || s.End > st.Size {
			return false
		}
		next = s.End
	}
	return next == st.Size
}

// saveTransferState writes the sidecar atomically, so an interrupted write
// cannot leave a half-parsed file behind.
func saveTransferState(part string, st *transferState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := statePath(part) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(part))
}

// clearTransferState removes the sidecar once the file is complete.
func clearTransferState(part string) {
	_ = os.Remove(statePath(part))
	_ = os.Remove(statePath(part) + ".tmp")
}

// errFileChanged means the server answered a ranged request with the whole
// entity, so what is already on disk belongs to a different version of the
// file and must be discarded.
var errFileChanged = errors.New("the file changed on the server; restarting")
