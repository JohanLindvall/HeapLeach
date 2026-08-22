package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/download"
)

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{-1, "?"},
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{102400, "100 KB"},
		{1048576, "1.0 MB"},
		{1610612736, "1.5 GB"},
	}
	for _, tc := range cases {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatSpeed(t *testing.T) {
	if got := formatSpeed(0); got != "—" {
		t.Errorf("idle speed = %q, want an em dash", got)
	}
	if got := formatSpeed(1048576); got != "1.0 MB/s" {
		t.Errorf("formatSpeed(1MiB) = %q", got)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "—"},
		{900 * time.Millisecond, "1s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m30s"},
		{3900 * time.Second, "1h05m"},
	}
	for _, tc := range cases {
		if got := formatDuration(tc.in); got != tc.want {
			t.Errorf("formatDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatETARefusesToGuess(t *testing.T) {
	if got := formatETA(1000, 0); got != "" {
		t.Errorf("no rate should give no ETA, got %q", got)
	}
	if got := formatETA(0, 1000); got != "" {
		t.Errorf("nothing remaining should give no ETA, got %q", got)
	}
	if got := formatETA(3000, 1000); got != "3s" {
		t.Errorf("formatETA = %q, want 3s", got)
	}
}

func TestBar(t *testing.T) {
	if got := bar(0.5, 10); got != strings.Repeat(fillRune, 5)+strings.Repeat(trackRune, 5) {
		t.Errorf("half bar = %q", got)
	}
	// An unknown total draws an empty track rather than inventing progress.
	if got := bar(-1, 6); got != strings.Repeat(trackRune, 6) {
		t.Errorf("unknown bar = %q", got)
	}
	// Overshoot is clamped, never wider than the track.
	if got := len([]rune(bar(2, 8))); got != 8 {
		t.Errorf("clamped bar width = %d, want 8", got)
	}
}

func TestTruncateAndPad(t *testing.T) {
	if got := truncate("abcdefgh", 4); got != "abc…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("short strings pass through, got %q", got)
	}
	if got := pad("ab", 5); got != "ab   " {
		t.Errorf("pad = %q", got)
	}
	if got := len([]rune(pad("abcdefgh", 4))); got != 4 {
		t.Errorf("pad width = %d, want 4", got)
	}
}

func TestVisibleWidthIgnoresEscapes(t *testing.T) {
	if got := visibleWidth("\x1b[36mabc\x1b[39m"); got != 3 {
		t.Errorf("visibleWidth = %d, want 3", got)
	}
}

// fit must count only printable cells, or a coloured line would be trimmed
// far too early and the cursor arithmetic would drift.
func TestRendererFitCountsCellsNotBytes(t *testing.T) {
	r := newRenderer(discardWriter{}, 40, false, true)
	line := "\x1b[36m" + strings.Repeat("x", 30) + "\x1b[39m"
	if got := r.fit(line); got != line {
		t.Errorf("a 30-cell line should survive a 40-column fit, got %q", got)
	}
	long := strings.Repeat("y", 80)
	if got := visibleWidth(r.fit(long)); got > 40 {
		t.Errorf("fit left %d visible cells, want <= 40", got)
	}
}

func TestIsURL(t *testing.T) {
	for _, in := range []string{"http://example.com/a", "HTTPS://example.com/a"} {
		if !IsURL(in) {
			t.Errorf("%q should be a URL", in)
		}
	}
	for _, in := range []string{"~/Downloads", "/mnt/media", "example.com", "ftp://example.com"} {
		if IsURL(in) {
			t.Errorf("%q should not be a URL", in)
		}
	}
}

func TestReadURLs(t *testing.T) {
	got, err := ReadURLs(strings.NewReader(
		"# a list\n\nhttps://example.com/a\n  https://example.com/b  \n\n"))
	if err != nil {
		t.Fatalf("ReadURLs: %v", err)
	}
	want := []string{"https://example.com/a", "https://example.com/b"}
	if len(got) != len(want) {
		t.Fatalf("got %d URLs, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("URL %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFinished(t *testing.T) {
	running := download.Snapshot{Jobs: []download.JobView{
		{Status: download.StatusDone, Total: 1},
		{Status: download.StatusRunning, Total: 1},
	}}
	if finished(running) {
		t.Error("a running job means the run is not finished")
	}

	allDone := download.Snapshot{Jobs: []download.JobView{
		{Status: download.StatusDone, Total: 1},
		{Status: download.StatusFailed, Total: 1},
	}}
	if !finished(allDone) {
		t.Error("terminal jobs mean the run is finished")
	}

	// A source that resolved to nothing would otherwise hang the run: it
	// reports queued forever because it has no items to complete.
	empty := download.Snapshot{Jobs: []download.JobView{
		{Status: download.StatusQueued, Total: 0},
	}}
	if !finished(empty) {
		t.Error("a job with no files can never progress; it is finished")
	}
}

func TestTrackerAnnouncesEachEventOnce(t *testing.T) {
	r := newRenderer(discardWriter{}, 80, false, false)
	tr := newTracker()

	resolving := download.Snapshot{Jobs: []download.JobView{
		{ID: "j1", Title: "Album", Host: "example", Status: download.StatusResolving},
	}}
	if got := tr.events(resolving, r); len(got) != 0 {
		t.Fatalf("a resolving job is not announced yet, got %v", got)
	}

	running := download.Snapshot{Jobs: []download.JobView{{
		ID: "j1", Title: "Album", Host: "example", Status: download.StatusRunning,
		Total: 2, SizeKnown: true, Size: 2048,
		Items: []download.ItemView{
			{ID: "i1", Name: "one.bin", Status: download.StatusRunning},
			{ID: "i2", Name: "two.bin", Status: download.StatusQueued},
		},
	}}}
	lines := tr.events(running, r)
	if len(lines) != 1 || !strings.Contains(lines[0], "Album") {
		t.Fatalf("expected one job announcement, got %v", lines)
	}
	if got := tr.events(running, r); len(got) != 0 {
		t.Fatalf("nothing changed, expected no lines, got %v", got)
	}

	done := running
	done.Jobs[0].Items = []download.ItemView{
		{ID: "i1", Name: "one.bin", Status: download.StatusDone, Downloaded: 1024, Elapsed: 2},
		{ID: "i2", Name: "two.bin", Status: download.StatusFailed, Error: "404\nsecond line"},
	}
	lines = tr.events(done, r)
	if len(lines) != 2 {
		t.Fatalf("expected two completion lines, got %v", lines)
	}
	if !strings.Contains(lines[0], "one.bin") || !strings.Contains(lines[0], "1.0 KB") {
		t.Errorf("done line = %q", lines[0])
	}
	if !strings.Contains(lines[1], "two.bin") || strings.Contains(lines[1], "second line") {
		t.Errorf("failure line should carry only the first line of the error: %q", lines[1])
	}
}

// The live region's height must equal the number of lines painted, or the
// cursor-up count on the next frame is wrong.
func TestFrameRowsAreBounded(t *testing.T) {
	r := newRenderer(discardWriter{}, 100, true, false)
	var items []download.ItemView
	for i := 0; i < 40; i++ {
		items = append(items, download.ItemView{
			ID: string(rune('a' + i%26)), Name: "file.bin",
			Status: download.StatusRunning, Size: 100, Downloaded: 50,
		})
	}
	snap := download.Snapshot{Jobs: []download.JobView{{Items: items}}, Active: 40}
	lines := frame(snap, r, time.Now())
	// One status line, the capped rows, and the "… n more" summary.
	if len(lines) != 1+12+1 {
		t.Fatalf("frame produced %d lines, want %d", len(lines), 1+12+1)
	}
	for i, line := range lines {
		if visibleWidth(r.fit(line)) > 100 {
			t.Errorf("line %d exceeds the terminal width", i)
		}
	}
}

// discardWriter swallows the display so tests can exercise the renderer.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
