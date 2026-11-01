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

// The reasons a listing most often stops short for: bare, as a tree walk's
// note takes them, and as whole notes.
const (
	fileLimit   = "file limit"
	sourceLimit = "source limit"
	rateLimited = "rate limited"

	fileLimitNote    = partialPrefix + fileLimit
	sourceLimitNote  = partialPrefix + sourceLimit
	listingLimitNote = partialPrefix + "listing limit"
	// postLimitNote is a host's own bound on a long search
	// (config.MaxBooruPosts), below the user's file cap.
	postLimitNote     = partialPrefix + "post limit"
	invalidNextNote   = partialPrefix + "invalid next page"
	couldNotFetchNote = partialPrefix + "could not fetch next page"
)

// failedReasons are the words a note uses for a listing that stopped short
// because a request failed — a later page, a folder, a season that could
// not be read, a host that rate-limited or stayed overloaded — rather than
// at a cap or the listing's own end. Reading the source again may get past
// them, where it would only meet a cap again.
var failedReasons = []string{
	"could not fetch", "could not read", "could not be read", "unreadable",
	rateLimited, "stayed overloaded", "broke off partway",
}

// Incomplete reports whether the result is a listing cut short by a
// failure, which reading the source again may complete: a result kept
// partial rather than failed, so it is still worth a retry.
func (r *Result) Incomplete() bool {
	for _, part := range strings.Split(r.Note, noteSeparator) {
		reason := strings.TrimPrefix(part, partialPrefix)
		for _, failed := range failedReasons {
			if strings.Contains(reason, failed) {
				return true
			}
		}
	}
	return false
}

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

// countNote says there are n of what, in the caller's words — "3 album
// items unavailable" — or nothing when there are none. The words stand as
// given whatever n is.
func countNote(n int, what string) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d %s", n, what)
}

// leftOutNote says n items were left out, and why, or nothing when none
// were. why reads on from "left out": ": <reason>" or " that ...".
func leftOutNote(n int, why string) string {
	return countNote(n, "left out"+why)
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

// unreadNote says n items of a listing could not be read — a request or a
// parse that failed — or nothing when none were. It sits beside
// refusedNote's counts, in the same words: the items were left out.
func unreadNote(n int) string {
	return leftOutNote(n, ": could not be read")
}

// brokenNote says a listing's walk broke off partway, when it did.
func brokenNote(err error) string {
	if err == nil {
		return ""
	}
	return "the listing broke off partway"
}
