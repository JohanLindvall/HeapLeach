package download

import (
	"github.com/JohanLindvall/HeapLeach/internal/config"
	"os"
	"path/filepath"
	"testing"
)

// The requested placement: the first extra connection starts halfway
// through the file, the next two at the quarter points, then the eighths.
func TestSplitBisectsInHalvesThenQuartersThenEighths(t *testing.T) {
	const size = 64 << 20 // 64 MiB, big enough for three rounds of splitting

	table := newSegmentTable(size, 0)
	want := []int64{
		size / 2,
		size / 4,
		size * 3 / 4,
		size / 8,
		size * 3 / 8,
		size * 5 / 8,
		size * 7 / 8,
	}

	for i, wantStart := range want {
		seg := table.split(config.MinSegmentSize)
		if seg == nil {
			t.Fatalf("split %d returned nil, want a segment starting at %d", i+1, wantStart)
		}
		if seg.start != wantStart {
			t.Errorf("split %d starts at %d (%.3f of the file), want %d (%.3f)",
				i+1, seg.start, float64(seg.start)/size, wantStart, float64(wantStart)/size)
		}
	}

	if got := table.count(); got != len(want)+1 {
		t.Errorf("table holds %d segments, want %d", got, len(want)+1)
	}
	assertTiles(t, table, size)
}

// Splitting has to follow the work that is left, not fixed fractions of the
// file: a segment already near its end is not the one worth dividing.
func TestSplitFollowsRemainingWork(t *testing.T) {
	const size = 64 << 20

	table := newSegmentTable(size, 0)
	first := table.split(config.MinSegmentSize) // [size/2, size)
	if first == nil {
		t.Fatal("first split failed")
	}

	// Drive the head segment almost to its end; the tail is now the widest.
	head := table.snapshot()[0]
	head.pos.Store(size/2 - config.MinSegmentSize)

	next := table.split(config.MinSegmentSize)
	if next == nil {
		t.Fatal("second split failed")
	}
	if next.start <= size/2 {
		t.Errorf("split at %d, but the outstanding work was all above %d", next.start, size/2)
	}
	assertTiles(t, table, size)
}

func TestSplitRefusesWhenTooSmall(t *testing.T) {
	// Below two minimum pieces there is nothing worth handing to another
	// connection.
	table := newSegmentTable(2*config.MinSegmentSize-1, 0)
	if seg := table.split(config.MinSegmentSize); seg != nil {
		t.Errorf("split a %d byte file into %d and %d", table.size, seg.start, table.size-seg.start)
	}
}

func TestSplitRefusesWhenFinished(t *testing.T) {
	const size = 64 << 20
	table := newSegmentTable(size, size)
	if seg := table.split(config.MinSegmentSize); seg != nil {
		t.Error("split a finished transfer")
	}
}

// Every split must leave the segments tiling the file exactly, or the
// finished file would have holes.
func assertTiles(t *testing.T, table *segmentTable, size int64) {
	t.Helper()

	segs := table.snapshot()
	bounds := map[int64]int64{}
	for _, s := range segs {
		bounds[s.start] = s.end.Load()
	}
	next := int64(0)
	for range segs {
		end, ok := bounds[next]
		if !ok {
			t.Fatalf("no segment starts at %d; segments do not tile the file", next)
		}
		if end <= next {
			t.Fatalf("segment [%d,%d) is empty or inverted", next, end)
		}
		next = end
	}
	if next != size {
		t.Errorf("segments cover %d bytes, want %d", next, size)
	}
}

func TestWrittenCountsAcrossSegments(t *testing.T) {
	const size = 64 << 20
	table := newSegmentTable(size, 0)
	table.split(config.MinSegmentSize)

	segs := table.snapshot()
	segs[0].pos.Store(1000)
	segs[1].pos.Store(segs[1].start + 2000)

	if got, want := table.written(), int64(3000); got != want {
		t.Errorf("written = %d, want %d", got, want)
	}
	if table.complete() {
		t.Error("reported complete with work outstanding")
	}

	for _, s := range segs {
		s.pos.Store(s.end.Load())
	}
	if !table.complete() {
		t.Error("reported incomplete once every segment finished")
	}
	if got := table.written(); got != size {
		t.Errorf("written = %d, want %d", got, size)
	}
}

func TestTransferStateRoundTrip(t *testing.T) {
	part := filepath.Join(t.TempDir(), "file.part")

	const size = 64 << 20
	table := newSegmentTable(size, 0)
	table.split(config.MinSegmentSize)
	table.snapshot()[0].pos.Store(4096)

	want := &transferState{Size: size, Validator: `"abc"`, Segments: table.state()}
	if err := saveTransferState(part, want); err != nil {
		t.Fatal(err)
	}

	got := loadTransferState(part)
	if got == nil {
		t.Fatal("state did not survive the round trip")
	}
	if got.Size != want.Size || got.Validator != want.Validator || len(got.Segments) != len(want.Segments) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got.Segments[0].Pos != 4096 {
		t.Errorf("progress = %d, want 4096", got.Segments[0].Pos)
	}

	clearTransferState(part)
	if loadTransferState(part) != nil {
		t.Error("state survived being cleared")
	}
}

// A sidecar that does not tile the file exactly would resume into a file
// with holes, so it must be rejected outright.
func TestTransferStateRejectsGaps(t *testing.T) {
	part := filepath.Join(t.TempDir(), "file.part")

	bad := []*transferState{
		{Size: 100, Segments: []segmentState{{Start: 0, Pos: 0, End: 40}}},                                 // short
		{Size: 100, Segments: []segmentState{{Start: 0, Pos: 0, End: 40}, {Start: 50, Pos: 50, End: 100}}}, // gap
		{Size: 100, Segments: []segmentState{{Start: 0, Pos: 60, End: 40}}},                                // pos past end
		{Size: 100, Segments: []segmentState{{Start: 0, Pos: 0, End: 200}}},                                // beyond size
	}
	for i, st := range bad {
		if err := saveTransferState(part, st); err != nil {
			t.Fatal(err)
		}
		if loadTransferState(part) != nil {
			t.Errorf("case %d: accepted a sidecar that does not tile the file: %+v", i, st.Segments)
		}
	}

	if err := os.WriteFile(statePath(part), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if loadTransferState(part) != nil {
		t.Error("accepted a corrupt sidecar")
	}
}
