// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"strings"
)

// What a result says of itself goes in its Note, which the job's shown title
// carries. These build its parts and join them, so that a note reads the
// same whichever extractor wrote it.

// partialPrefix opens a note on a result cut short.
const partialPrefix = "partial — "

// noteSeparator stands between the parts of a note.
const noteSeparator = "; "

// joinNotes joins the parts of a note: "; " between them, an empty part left
// out, and a part the note already holds not repeated (a part may itself be
// a joined note, whose parts count one by one). A note says "partial — " once,
// at its start: a later part that starts so drops it, as in "partial —
// invalid next page; file limit".
func joinNotes(parts ...string) string {
	var kept []string
	seen := make(map[string]bool)
	for _, part := range parts {
		if part == "" {
			continue
		}
		for _, p := range strings.Split(part, noteSeparator) {
			reason := strings.TrimPrefix(p, partialPrefix)
			if reason == "" || seen[reason] {
				continue
			}
			seen[reason] = true
			if len(kept) > 0 && strings.HasPrefix(kept[0], partialPrefix) {
				p = reason
			}
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, noteSeparator)
}

// partial marks a note as one on a result cut short, or is nothing when the
// note is.
func partial(note string) string {
	if note == "" {
		return ""
	}
	return partialPrefix + note
}

// cappedNote says a listing of shown items was cut to kept, or nothing when
// it was not. An empty unit leaves the count bare.
func cappedNote(kept, shown int, unit string) string {
	if shown <= kept {
		return ""
	}
	if unit == "" {
		return fmt.Sprintf("%d of %d", kept, shown)
	}
	return fmt.Sprintf("%d of %d %s", kept, shown, unit)
}

// resolvedNote says how many of the listed items resolved, in the caller's
// noun, or nothing when all of them did.
func resolvedNote(used, listed int, noun string) string {
	if used >= listed {
		return ""
	}
	return fmt.Sprintf("%d of %d %s resolved", used, listed, noun)
}

// leftOutNote says n items were left out, and why, or nothing when none
// were. why reads on from "left out": ": <reason>" or " that ...".
func leftOutNote(n int, why string) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d left out%s", n, why)
}

// reasonCount is a reason and how many times it came up.
type reasonCount struct {
	reason string
	n      int
}

// tally counts the reasons given, in the order they first came up.
func tally(reasons []string) []reasonCount {
	var counts []reasonCount
	at := make(map[string]int)
	for _, reason := range reasons {
		i, ok := at[reason]
		if !ok {
			i = len(counts)
			at[reason] = i
			counts = append(counts, reasonCount{reason: reason})
		}
		counts[i].n++
	}
	return counts
}

// refusedNote says how many were left out for each reason given, in the
// order the reasons first came up — "3 left out: gone; 1 left out: live"
// — or nothing when none were.
func refusedNote(reasons []string) string {
	var notes []string
	for _, c := range tally(reasons) {
		notes = append(notes, leftOutNote(c.n, ": "+c.reason))
	}
	return joinNotes(notes...)
}

// brokenNote says a listing's walk broke off partway, when it did.
func brokenNote(err error) string {
	if err == nil {
		return ""
	}
	return "the listing broke off partway"
}
