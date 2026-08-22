package cli

import (
	"fmt"
	"strings"
	"time"
)

// formatBytes renders a byte count the way a download client should: three
// significant figures at most, so the number stops jittering as it grows.
func formatBytes(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	format := "%.1f %cB"
	if value >= 100 {
		format = "%.0f %cB"
	}
	return fmt.Sprintf(format, value, "KMGTP"[exp])
}

// formatSpeed renders bytes per second.
func formatSpeed(bps float64) string {
	if bps <= 0 {
		return "—"
	}
	return formatBytes(int64(bps)) + "/s"
}

// formatDuration renders a coarse, fixed-vocabulary duration. Sub-second
// precision would only add noise to a display that repaints ten times a
// second.
func formatDuration(d time.Duration) string {
	if d < 0 {
		return "—"
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// formatETA turns remaining bytes and a rate into a time, refusing to guess
// when either is unknown.
func formatETA(remaining int64, bps float64) string {
	if remaining <= 0 || bps <= 0 {
		return ""
	}
	return formatDuration(time.Duration(float64(remaining)/bps) * time.Second)
}

// barParts splits a progress bar into the filled run and the empty track, so
// each can be coloured separately — the contrast is what makes the bar
// readable, not the difference in stroke weight.
//
// A negative fraction means the total is unknown, which draws an empty track
// rather than a lie.
func barParts(fraction float64, width int) (fill, track string) {
	if width < 1 {
		return "", ""
	}
	if fraction < 0 {
		return "", strings.Repeat(trackRune, width)
	}
	if fraction > 1 {
		fraction = 1
	}
	filled := int(fraction*float64(width) + 0.5)
	return strings.Repeat(fillRune, filled), strings.Repeat(trackRune, width-filled)
}

// bar renders both parts uncoloured, which is what the tests measure.
func bar(fraction float64, width int) string {
	fill, track := barParts(fraction, width)
	return fill + track
}

// truncate shortens a string to a display width, marking the cut. Width is
// counted in runes, which is close enough for the names hosts hand out.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

// pad right-pads to an exact rune width so columns line up.
func pad(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = truncate(s, width)
	if n := width - len([]rune(s)); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// alignRight right-pads a field to an exact width, keeping columns in line
// down the screen. Overlong values are truncated rather than allowed to push
// the layout sideways.
func alignRight(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = truncate(s, width)
	if n := width - len([]rune(s)); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}
