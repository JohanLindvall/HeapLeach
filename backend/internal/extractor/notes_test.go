// SPDX-License-Identifier: MIT

package extractor

import (
	"errors"
	"testing"
)

// TestJoinNotes pins how the parts of a note read together.
func TestJoinNotes(t *testing.T) {
	tests := []struct {
		got, want string
	}{
		{joinNotes(cappedNote(5, 5, "episodes"), brokenNote(nil), leftOutNote(0, " that x"), refusedNote(nil)), ""},
		{joinNotes(cappedNote(5, 9, "episodes"), brokenNote(errors.New("x"))), "5 of 9 episodes; the listing broke off partway"},
		{joinNotes(leftOutNote(2, " that are DRM-protected"), refusedNote([]string{"gone", "late"})),
			"2 left out that are DRM-protected; 1 left out: gone; 1 left out: late"},
		// Refusals are counted by reason, in the order the reasons came up.
		{refusedNote([]string{"live", "gone", "live", "live"}), "3 left out: live; 1 left out: gone"},
		// A part the note already holds is not repeated, a joined part
		// counting part by part; "partial — " opens the note once.
		{joinNotes("partial — file limit", "partial — file limit"), "partial — file limit"},
		{joinNotes("partial — invalid next page; file limit", "partial — file limit"), "partial — invalid next page; file limit"},
		{joinNotes("partial — invalid posts", "partial — file limit", "2 left out: gone"),
			"partial — invalid posts; file limit; 2 left out: gone"},
		{joinNotes("3 of 5 releases", "partial — file limit"), "3 of 5 releases; partial — file limit"},
		{joinNotes("1 left out: gone", "11 left out: gone"), "1 left out: gone; 11 left out: gone"},
		{partial(cappedNote(3, 5, "")), "partial — 3 of 5"},
		{partial(cappedNote(5, 5, "posts")), ""},
		{resolvedNote(2, 3, "links"), "2 of 3 links resolved"},
		{resolvedNote(3, 3, "links"), ""},
	}
	for i, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%d: note = %q, want %q", i, tc.got, tc.want)
		}
	}
}
