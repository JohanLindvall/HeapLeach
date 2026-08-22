// Package cli runs the downloader without a browser: it queues the URLs it
// is given, animates their progress on the terminal, and exits when the last
// one is finished.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/download"
)

// Options configure a headless run.
type Options struct {
	// URLs to download, in the order they were given.
	URLs []string
	// Password is applied to every URL that needs one.
	Password string
	// Out receives the progress display.
	Out io.Writer
	// Animate paints an in-place display instead of plain progress lines.
	// Callers turn it off for pipes, CI logs and -debug runs.
	Animate bool
	// Colour enables SGR colour in the display.
	Colour bool
	// Width is the terminal width in columns.
	Width int
	// TermWidth, when set, is asked for the current width on every frame,
	// so the display follows the terminal through a resize instead of
	// painting for the size it happened to have at startup — a shrink
	// would otherwise wrap every line and corrupt the repaint arithmetic
	// for the rest of the run.
	TermWidth func() int
}

// ErrIncomplete reports that the run finished with work that did not
// succeed. It carries no detail of its own: the display has already listed
// every failure by name.
var ErrIncomplete = errors.New("some downloads did not complete")

// Run queues opts.URLs, renders progress until every job reaches a terminal
// state, and returns. A canceled context ends the run early; the manager's
// own shutdown leaves .part files behind so a later run resumes them.
func Run(ctx context.Context, mgr *download.Manager, opts Options) error {
	r := newRenderer(opts.Out, opts.Width, opts.Animate, opts.Colour)
	defer r.stop()

	accepted := 0
	for _, raw := range opts.URLs {
		if _, err := mgr.Add(raw, opts.Password); err != nil {
			r.note(r.paintColour(red, "✗ ") + raw + r.paintColour(dim, "  "+err.Error()))
			continue
		}
		accepted++
	}
	if accepted == 0 {
		return errors.New("no usable URLs")
	}

	started := time.Now()
	tracker := newTracker()

	ticker := time.NewTicker(config.CLIFrameInterval)
	defer ticker.Stop()

	// Plain mode has no live region, so progress has to be announced on a
	// slower beat or it would flood the log.
	var lastPlain time.Time

	for {
		select {
		case <-ctx.Done():
			r.stop()
			fmt.Fprintln(opts.Out, r.paintColour(yellow, "interrupted")+
				r.paintColour(dim, " — partial files are kept; run again to resume"))
			return ctx.Err()
		case <-ticker.C:
		}

		snap := mgr.Snapshot()
		for _, line := range tracker.events(snap, r) {
			r.note(line)
		}

		if opts.Animate {
			if opts.TermWidth != nil {
				if w := opts.TermWidth(); w > 0 {
					r.setWidth(w)
				}
			}
			r.paint(frame(snap, r, started))
		} else if time.Since(lastPlain) >= config.CLIPlainInterval {
			lastPlain = time.Now()
			if line := plainLine(snap, started); line != "" {
				r.note(line)
			}
		}

		if finished(snap) {
			r.stop()
			return summary(snap, opts.Out, r, started)
		}
	}
}

// finished reports whether every job has stopped moving. A job that resolved
// to nothing counts as finished; it has no items that could ever complete.
func finished(snap download.Snapshot) bool {
	for _, job := range snap.Jobs {
		if job.Status.Terminal() {
			continue
		}
		if job.Total == 0 && job.Status == download.StatusQueued {
			continue // extractor returned no files
		}
		return false
	}
	return true
}

// tracker turns successive snapshots into the events worth a permanent line:
// a job resolving, and each file reaching its end state.
type tracker struct {
	jobs  map[string]bool
	items map[string]download.Status
}

func newTracker() *tracker {
	return &tracker{jobs: make(map[string]bool), items: make(map[string]download.Status)}
}

func (t *tracker) events(snap download.Snapshot, r *renderer) []string {
	var out []string
	// Snapshots arrive newest-first; announce in submission order.
	for i := len(snap.Jobs) - 1; i >= 0; i-- {
		job := snap.Jobs[i]
		if !t.jobs[job.ID] && job.Status != download.StatusResolving {
			t.jobs[job.ID] = true
			out = append(out, r.jobLine(job))
		}
		for _, it := range job.Items {
			key := job.ID + "/" + it.ID
			if prev, seen := t.items[key]; seen && prev == it.Status {
				continue
			}
			t.items[key] = it.Status
			if it.Status.Terminal() {
				out = append(out, r.itemDoneLine(it))
			}
		}
	}
	return out
}

// jobLine announces what a source turned out to contain.
func (r *renderer) jobLine(job download.JobView) string {
	title := job.Title
	if title == "" {
		title = job.Source
	}
	switch {
	case job.Status == download.StatusFailed && job.Total == 0:
		return r.paintColour(red, "✗ ") + title + r.paintColour(dim, "  "+job.Error)
	case job.Total == 0:
		return r.paintColour(yellow, "· ") + title + r.paintColour(dim, "  no files found")
	}
	files := fmt.Sprintf("%d file", job.Total)
	if job.Total != 1 {
		files += "s"
	}
	detail := files
	if job.SizeKnown && job.Size > 0 {
		detail += ", " + formatBytes(job.Size)
	}
	return r.paintColour(cyan, "▸ ") + r.paintColour(bold, title) +
		r.paintColour(dim, "  "+job.Host+" · "+detail)
}

// itemDoneLine records one file's outcome.
func (r *renderer) itemDoneLine(it download.ItemView) string {
	switch it.Status {
	case download.StatusDone:
		detail := formatBytes(it.Downloaded)
		switch {
		case it.Skipped:
			// A file that was already on disk moved no bytes; quoting a
			// rate for it would be arithmetic on a zero denominator.
			detail += " · already downloaded"
		case it.Elapsed > 0:
			rate := float64(it.Downloaded) / it.Elapsed
			detail += " in " + formatDuration(time.Duration(it.Elapsed*float64(time.Second))) +
				" · " + formatSpeed(rate)
		}
		return r.paintColour(green, "  ✓ ") + it.Name + r.paintColour(dim, "  "+detail)
	case download.StatusFailed:
		return r.paintColour(red, "  ✗ ") + it.Name + r.paintColour(dim, "  "+firstLine(it.Error))
	default:
		return r.paintColour(dim, "  – "+it.Name+"  canceled")
	}
}

// frame builds the live region: a status line, then a row per moving file.
// Frame renders one frame of the live display, coloured, as a terminal would
// show it. It exists for the documentation screenshots: a picture of the
// terminal output should come from the code that draws it rather than from
// an artist's impression, and this is that code with the animation and the
// terminal detection taken out.
func Frame(snap download.Snapshot, width int, elapsed time.Duration) []string {
	r := newRenderer(io.Discard, width, false, true)
	// What a run this far along would already have printed above the live
	// region, then the live region itself — the two together are what the
	// screen actually looks like.
	lines := newTracker().events(snap, r)
	return append(lines, frame(snap, r, time.Now().Add(-elapsed))...)
}

func frame(snap download.Snapshot, r *renderer, started time.Time) []string {
	head := fmt.Sprintf("%s %s  %s",
		r.paintColour(cyan, r.tick()),
		r.paintColour(bold, formatSpeed(snap.Speed)),
		r.paintColour(dim, headDetail(snap, started)))

	lines := []string{head}
	shown := 0
	for _, job := range snap.Jobs {
		for _, it := range job.Items {
			if it.Status != download.StatusRunning {
				continue
			}
			if shown == config.CLIMaxRows {
				lines = append(lines, r.paintColour(dim,
					fmt.Sprintf("  … %d more running", runningCount(snap)-shown)))
				return lines
			}
			lines = append(lines, r.itemRow(it))
			// A note explains a wait the numbers cannot: without it a
			// transfer being patient with a busy host is indistinguishable
			// from one that has died.
			if it.Note != "" {
				lines = append(lines, r.paintColour(yellow, "      "+it.Note))
			}
			shown++
		}
	}
	if shown == 0 && snap.Queued > 0 {
		lines = append(lines, r.paintColour(dim, "  waiting for a free slot…"))
	}
	return lines
}

// headDetail is the counts half of the status line.
func headDetail(snap download.Snapshot, started time.Time) string {
	var done, total, failed int
	for _, job := range snap.Jobs {
		done += job.Done
		failed += job.Failed
		total += job.Total
	}
	parts := []string{
		fmt.Sprintf("%d/%d files", done, total),
		fmt.Sprintf("%d running", snap.Active),
	}
	if snap.Queued > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", snap.Queued))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	parts = append(parts, formatDuration(time.Since(started)))
	return strings.Join(parts, " · ")
}

// itemRow renders one running file: name, bar, and the numbers that matter
// while it is moving.
func (r *renderer) itemRow(it download.ItemView) string {
	fraction := -1.0
	if it.Size > 0 {
		fraction = float64(it.Downloaded) / float64(it.Size)
	} else if it.SegmentsTotal > 0 {
		fraction = float64(it.SegmentsDone) / float64(it.SegmentsTotal)
	}

	right := r.itemStats(it, fraction)
	// The stats block is a fixed width, so the name column — and with it
	// every bar — starts at the same place on every row.
	nameWidth := r.width - config.CLIBarWidth - statsWidth - 5
	if nameWidth < 12 {
		nameWidth = 12
	}

	fill, track := barParts(fraction, config.CLIBarWidth)
	return "  " + pad(it.Name, nameWidth) + " " +
		r.paintColour(magenta, fill) + r.paintColour(dim, track) + " " + right
}

// statsWidth is the width of the fixed columns itemStats emits: a
// percentage, the sizes, the rate, the stream count and the ETA, each
// separated by one space.
const statsWidth = 4 + 1 + config.CLISizeWidth + 1 + config.CLISpeedWidth +
	1 + config.CLIStreamWidth + 1 + config.CLIETAWidth

// itemStats is the right-hand block: percentage, sizes, rate, streams, ETA.
// Every field is padded to a constant width — see statsWidth.
func (r *renderer) itemStats(it download.ItemView, fraction float64) string {
	pct := " --%"
	if fraction >= 0 {
		pct = fmt.Sprintf("%3.0f%%", fraction*100)
	}

	size := formatBytes(it.Downloaded)
	if it.Size > 0 {
		size += "/" + formatBytes(it.Size)
	} else if it.SegmentsTotal > 0 {
		size = fmt.Sprintf("seg %d/%d", it.SegmentsDone, it.SegmentsTotal)
	}

	streams := ""
	if it.Streams > 1 {
		streams = fmt.Sprintf("×%d", it.Streams)
	}

	eta := ""
	if it.Size > 0 {
		eta = formatETA(it.Size-it.Downloaded, it.Speed)
	}

	return strings.Join([]string{
		pct,
		alignRight(size, config.CLISizeWidth),
		alignRight(formatSpeed(it.Speed), config.CLISpeedWidth),
		r.paintColour(cyan, alignRight(streams, config.CLIStreamWidth)),
		r.paintColour(dim, alignRight(eta, config.CLIETAWidth)),
	}, " ")
}

// plainLine is the whole display for terminals that cannot be animated.
func plainLine(snap download.Snapshot, started time.Time) string {
	if snap.Active == 0 && snap.Queued == 0 {
		return ""
	}
	return fmt.Sprintf("… %s · %s", formatSpeed(snap.Speed), headDetail(snap, started))
}

// summary prints the closing report and reports whether it was clean.
func summary(snap download.Snapshot, out io.Writer, r *renderer, started time.Time) error {
	var done, failed, canceled int
	var bytes int64
	names := map[download.Status][]string{}
	for _, job := range snap.Jobs {
		for _, it := range job.Items {
			switch it.Status {
			case download.StatusDone:
				done++
				bytes += it.Downloaded
			case download.StatusFailed:
				failed++
				names[download.StatusFailed] = append(names[download.StatusFailed], it.Name)
			case download.StatusCanceled:
				canceled++
			}
		}
	}

	elapsed := time.Since(started)
	rate := ""
	if secs := elapsed.Seconds(); secs > 0 && bytes > 0 {
		rate = " · " + formatSpeed(float64(bytes)/secs)
	}

	mark, tint := "✓", green
	if failed > 0 {
		mark, tint = "✗", red
	}
	fmt.Fprintf(out, "%s %s%s\n",
		r.paintColour(tint, mark),
		fmt.Sprintf("%d file(s), %s in %s", done, formatBytes(bytes), formatDuration(elapsed)),
		r.paintColour(dim, rate))

	if failed > 0 {
		sort.Strings(names[download.StatusFailed])
		fmt.Fprintf(out, "%s\n", r.paintColour(red,
			fmt.Sprintf("  %d failed: %s", failed,
				truncate(strings.Join(names[download.StatusFailed], ", "), 200))))
		return ErrIncomplete
	}
	if canceled > 0 {
		return ErrIncomplete
	}
	return nil
}

// runningCount totals the files currently moving.
func runningCount(snap download.Snapshot) int {
	n := 0
	for _, job := range snap.Jobs {
		for _, it := range job.Items {
			if it.Status == download.StatusRunning {
				n++
			}
		}
	}
	return n
}

// firstLine keeps an error to one row of the display.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 120)
}

// IsURL reports whether an argument is a download source rather than a
// directory. Only the schemes the extractors can actually fetch count.
func IsURL(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// ReadURLs collects one URL per line, ignoring blanks and # comments, so a
// list can be piped in or kept in a file.
func ReadURLs(r io.Reader) ([]string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urls = append(urls, line)
	}
	return urls, nil
}

// IsTerminal reports whether f is an interactive terminal.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
