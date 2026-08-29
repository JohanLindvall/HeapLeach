package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/download"
)

// timeNowMinus is a run's start a moment ago, so durations render non-zero.
func timeNowMinus(t *testing.T) time.Time {
	t.Helper()
	return time.Now().Add(-90 * time.Second)
}

// The renderer's whole contract is repaint arithmetic: the moment its
// cursor-up count disagrees with the rows on screen, the display eats
// itself. These tests read the control codes it actually emits.

func TestPaintReplacesExactlyItsOwnRows(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, 80, true, false)

	r.paint([]string{"one", "two", "three"})
	out.Reset()
	r.paint([]string{"four"})

	// Replacing a three-row region means climbing exactly two rows: the
	// cursor sits on the region's last row, not below it.
	if got := strings.Count(out.String(), "\x1b[1A"); got != 2 {
		t.Errorf("repaint climbed %d rows, want 2", got)
	}
	if !strings.Contains(out.String(), "four") {
		t.Error("the new frame was not written")
	}

	out.Reset()
	r.stop()
	// The frame on screen is one row now; stopping clears it without
	// climbing, and gives the cursor back.
	if got := strings.Count(out.String(), "\x1b[1A"); got != 0 {
		t.Errorf("stop climbed %d rows over a one-row region, want 0", got)
	}
	if !strings.Contains(out.String(), ansiShowCursor) {
		t.Error("stop did not restore the cursor")
	}
}

func TestNoteScrollsAboveTheLiveRegion(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, 80, true, false)

	r.paint([]string{"live one", "live two"})
	out.Reset()
	r.note("done: file.bin")

	s := out.String()
	// The note clears the two-row region (one climb), writes itself with a
	// newline, and leaves the region empty for the next paint.
	if got := strings.Count(s, "\x1b[1A"); got != 1 {
		t.Errorf("note climbed %d rows, want 1", got)
	}
	if !strings.HasSuffix(s, "done: file.bin\n") {
		t.Errorf("note output ends %q, want the permanent line and a newline", s)
	}
	if r.rows != 0 {
		t.Errorf("rows = %d after a note, want 0", r.rows)
	}
}

func TestNoteWithoutAnimationIsAPlainLine(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, 80, false, false)

	r.note("plain")
	r.paint([]string{"never shown"})

	if got := out.String(); got != "plain\n" {
		t.Errorf("plain-mode output = %q, want just the note", got)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Error("plain mode must carry no control codes")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, 80, true, false)
	r.paint([]string{"x"})
	r.stop()
	first := out.String()
	r.stop()
	if out.String() != first {
		t.Error("a second stop wrote more output")
	}
}

func TestSetWidthKeepsTheFloor(t *testing.T) {
	r := newRenderer(&bytes.Buffer{}, 80, true, false)
	r.setWidth(20)
	if r.width != 40 {
		t.Errorf("width = %d after asking for 20, want the 40 floor", r.width)
	}
	r.setWidth(120)
	if r.width != 120 {
		t.Errorf("width = %d, want 120", r.width)
	}
}

func TestSummaryExitCodes(t *testing.T) {
	item := func(status download.Status, name string, size int64) download.ItemView {
		return download.ItemView{Name: name, Status: status, Downloaded: size, Size: size}
	}
	snapOf := func(items ...download.ItemView) download.Snapshot {
		return download.Snapshot{Jobs: []download.JobView{{Items: items}}}
	}

	t.Run("clean run reports nil", func(t *testing.T) {
		var out bytes.Buffer
		r := newRenderer(&out, 80, false, false)
		err := summary(snapOf(item(download.StatusDone, "a.bin", 10)), &out, r, timeNowMinus(t))
		if err != nil {
			t.Errorf("summary = %v, want nil", err)
		}
		if !strings.Contains(out.String(), "1 file,") {
			t.Errorf("summary output %q does not count the file", out.String())
		}
	})

	t.Run("several files pluralise", func(t *testing.T) {
		var out bytes.Buffer
		r := newRenderer(&out, 80, false, false)
		_ = summary(snapOf(
			item(download.StatusDone, "a.bin", 10),
			item(download.StatusDone, "b.bin", 10),
		), &out, r, timeNowMinus(t))
		if !strings.Contains(out.String(), "2 files,") {
			t.Errorf("summary output %q does not pluralise", out.String())
		}
	})

	t.Run("failures name the files and fail the run", func(t *testing.T) {
		var out bytes.Buffer
		r := newRenderer(&out, 80, false, false)
		err := summary(snapOf(
			item(download.StatusDone, "ok.bin", 10),
			item(download.StatusFailed, "zeta.bin", 0),
			item(download.StatusFailed, "alpha.bin", 0),
		), &out, r, timeNowMinus(t))
		if err != ErrIncomplete {
			t.Errorf("summary = %v, want ErrIncomplete", err)
		}
		// Sorted, so the report reads the same run to run.
		if !strings.Contains(out.String(), "alpha.bin, zeta.bin") {
			t.Errorf("failed files not listed in order: %q", out.String())
		}
	})

	t.Run("a cancellation alone still fails the run", func(t *testing.T) {
		var out bytes.Buffer
		r := newRenderer(&out, 80, false, false)
		err := summary(snapOf(item(download.StatusCanceled, "c.bin", 0)), &out, r, timeNowMinus(t))
		if err != ErrIncomplete {
			t.Errorf("summary = %v, want ErrIncomplete — cancelled work is not done work", err)
		}
	})
}

func TestPlainLine(t *testing.T) {
	idle := download.Snapshot{}
	if got := plainLine(idle, timeNowMinus(t)); got != "" {
		t.Errorf("an idle queue printed %q, want nothing", got)
	}
	busy := download.Snapshot{Active: 2, Queued: 3, Speed: 1_000_000}
	got := plainLine(busy, timeNowMinus(t))
	if !strings.Contains(got, "2 running") || !strings.Contains(got, "3 queued") {
		t.Errorf("plain line %q misses the counts", got)
	}
}
