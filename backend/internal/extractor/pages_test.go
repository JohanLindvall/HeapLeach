// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"strconv"
	"testing"
)

// TestWalkPagesWeighsTheLimit counts the limit in what entries hold: pages
// of entries holding nothing walk on, the page that passes the limit is kept
// whole and noted, and one that reaches it exactly ends the walk, noted only
// while the listing goes on.
func TestWalkPagesWeighsTheLimit(t *testing.T) {
	// Each entry weighs its last digit.
	weigh := func(entries []int) int {
		total := 0
		for _, e := range entries {
			total += e % 10
		}
		return total
	}
	for _, tc := range []struct {
		name  string
		pages [][]int
		want  int // entries walked
		read  int // pages read
		note  string
	}{
		{"weightless pages walk on", [][]int{{10, 20}, {30, 40}, {51}, {62}}, 6, 4, ""},
		{"passing the limit keeps the page", [][]int{{11}, {22, 33}, {44}}, 3, 2, "partial — file limit"},
		{"reaching it while more follow", [][]int{{12}, {23}, {34}}, 2, 2, "partial — file limit"},
		{"reaching it on the last page", [][]int{{12}, {23}}, 2, 2, ""},
	} {
		read := 0
		got, note, err := walkPages(context.Background(), pageWalk[int]{
			key:   func(e int) string { return strconv.Itoa(e) },
			limit: 5, limitNote: "partial — file limit", weigh: weigh,
			fetch: func(_ context.Context, n int) (pageOf[int], error) {
				read++
				return pageOf[int]{items: tc.pages[n], next: n+1 < len(tc.pages)}, nil
			},
		})
		if err != nil || len(got) != tc.want || read != tc.read || note != tc.note {
			t.Errorf("%s: %d entries over %d pages, note %q, err %v; want %d over %d, %q",
				tc.name, len(got), read, note, err, tc.want, tc.read, tc.note)
		}
	}
}

// TestWalkPagesEndsAtTheStatedPageCount ends a listing on the page its
// stated count names, without asking past it.
func TestWalkPagesEndsAtTheStatedPageCount(t *testing.T) {
	read := 0
	got, note, err := walkPages(context.Background(), pageWalk[string]{
		key: func(s string) string { return s }, limit: -1,
		fetch: func(_ context.Context, n int) (pageOf[string], error) {
			read++
			return pageOf[string]{items: []string{strconv.Itoa(n)}, next: true, pages: 3}, nil
		},
	})
	if err != nil || len(got) != 3 || read != 3 || note != "" {
		t.Errorf("%d entries over %d pages, note %q, err %v; want 3 over 3 and no note", len(got), read, note, err)
	}
}

// TestWalkPagesFailedHookDefers leaves the note for a failed later page to
// the walk when the hook declines to decide it: its own note, or none when
// the walk is quiet; a decided "" stays no note at all.
func TestWalkPagesFailedHookDefers(t *testing.T) {
	for _, tc := range []struct {
		hook    string
		decided bool
		quiet   bool
		want    string
	}{
		{"", false, false, "partial — could not fetch next page"},
		{"", false, true, ""},
		{"", true, false, ""},
		{"partial — rate limited", true, true, "partial — rate limited"},
	} {
		_, note, err := walkPages(context.Background(), pageWalk[string]{
			key: func(s string) string { return s }, limit: -1, quiet: tc.quiet,
			failed: func(error) (string, bool) { return tc.hook, tc.decided },
			fetch: func(_ context.Context, n int) (pageOf[string], error) {
				if n > 0 {
					return pageOf[string]{}, errors.New("gone")
				}
				return pageOf[string]{items: []string{"a"}, next: true}, nil
			},
		})
		if err != nil || note != tc.want {
			t.Errorf("hook %q, quiet %v: note = %q, err = %v; want %q", tc.hook, tc.quiet, note, err, tc.want)
		}
	}
}

// Each way a walk ends is a reason of its own, rendered into the note the
// walk has always given; walkEpisodes reads the limit from the reason.
func TestWalkPagesReportsWhyItEnded(t *testing.T) {
	gone := errors.New("gone")
	for _, tc := range []struct {
		name  string
		limit int
		pages []pageOf[string]
		stop  walkStop
		note  string
	}{
		{"own end", -1, []pageOf[string]{{items: []string{"a"}, next: true}, {items: []string{"b"}, note: "said by the page"}},
			walkEnded, "said by the page"},
		{"limit", 1, []pageOf[string]{{items: []string{"a", "b"}, next: true}}, walkLimit, "partial — file limit"},
		{"page cap", -1, []pageOf[string]{{items: []string{"a"}, next: true}, {items: []string{"b"}, next: true}},
			walkPageLimit, "partial — page limit"},
		{"repeated", -1, []pageOf[string]{{items: []string{"a"}, next: true}, {items: []string{"a"}, next: true}},
			walkRepeated, "partial — repeated or empty page"},
		{"stop page", -1, []pageOf[string]{{items: []string{"a"}, next: true}, {stop: "partial — redirected"}},
			walkStopped, "partial — redirected"},
		{"later page failed", -1, []pageOf[string]{{items: []string{"a"}, next: true}, {}}, walkFetchFailed,
			couldNotFetchNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := pageWalk[string]{
				key: func(s string) string { return s }, limit: tc.limit, limitNote: fileLimitNote, pages: 2,
				fetch: func(_ context.Context, n int) (pageOf[string], error) {
					if tc.stop == walkFetchFailed && n == 1 {
						return pageOf[string]{}, gone
					}
					return tc.pages[n], nil
				},
			}
			out, err := walkPagesFor(context.Background(), w)
			if err != nil || out.stop != tc.stop || w.render(out) != tc.note {
				t.Errorf("stop %v, note %q, err %v; want %v, %q", out.stop, w.render(out), err, tc.stop, tc.note)
			}
			_, note, _ := walkPages(context.Background(), w)
			if note != tc.note {
				t.Errorf("walkPages note %q, want %q", note, tc.note)
			}
			_, more, _, _ := walkEpisodes(context.Background(), tc.limit, w)
			if more != (tc.stop == walkLimit) {
				t.Errorf("walkEpisodes more = %v", more)
			}
		})
	}
}
