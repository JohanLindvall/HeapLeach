// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
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
// the walk accepts the failure; a decided "" stays no note at all.
func TestWalkPagesFailedHookDefers(t *testing.T) {
	accepting := func(accept bool) walkAccepts {
		if accept {
			return walkAccepts{statuses: []int{http.StatusNotFound}}
		}
		return walkAccepts{}
	}
	for _, tc := range []struct {
		hook    string
		decided bool
		accept  bool
		want    string
	}{
		{"", false, false, "partial — could not fetch next page"},
		{"", false, true, ""},
		{"", true, false, ""},
		{"partial — rate limited", true, true, "partial — rate limited"},
	} {
		_, note, err := walkPages(context.Background(), pageWalk[string]{
			key: func(s string) string { return s }, limit: -1, ends: accepting(tc.accept),
			failed: func(error) (string, bool) { return tc.hook, tc.decided },
			fetch: func(_ context.Context, n int) (pageOf[string], error) {
				if n > 0 {
					return pageOf[string]{}, &httpx.StatusError{Code: http.StatusNotFound}
				}
				return pageOf[string]{items: []string{"a"}, next: true}, nil
			},
		})
		if err != nil || note != tc.want {
			t.Errorf("hook %q, accepted %v: note = %q, err = %v; want %q", tc.hook, tc.accept, note, err, tc.want)
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
			if err != nil || out.stop != tc.stop || out.render() != tc.note {
				t.Errorf("stop %v, note %q, err %v; want %v, %q", out.stop, out.render(), err, tc.stop, tc.note)
			}
			_, note, _ := walkPages(context.Background(), w)
			if note != tc.note {
				t.Errorf("walkPages note %q, want %q", note, tc.note)
			}
			// A tree takes the same end as a bare reason, and marks its
			// note partial itself, once.
			want := strings.TrimPrefix(tc.note, partialPrefix)
			if out.reason() != want {
				t.Errorf("reason %q, want %q", out.reason(), want)
			}
			tree := &treeWalk{res: &Result{}}
			tree.ended(out.walkEnd)
			if tree.res.Note != partial(want) {
				t.Errorf("tree note %q, want %q", tree.res.Note, partial(want))
			}
			_, more, _, _ := walkEpisodes(context.Background(), tc.limit, w)
			if more != (tc.stop == walkLimit) {
				t.Errorf("walkEpisodes more = %v", more)
			}
		})
	}
}

// The completion matrix: what a walk says for each way a listing ends,
// under the ends its caller accepts (walkAccepts) and under none.
func TestWalkEndPolicies(t *testing.T) {
	status := func(code int) error { return &httpx.StatusError{Code: code} }
	sentinel := walkAccepts{repeated: true, missing: true, statuses: []int{http.StatusNotFound, http.StatusGone}}
	for _, tc := range []struct {
		name    string
		ends    walkAccepts
		pages   []pageOf[string]
		failAt  error
		limit   int
		want    string
		wantErr bool
	}{
		{name: "stated total reached", pages: []pageOf[string]{{items: []string{"a"}, next: true, total: 2}, {items: []string{"b"}, next: true, total: 2}}},
		{name: "end flag", pages: []pageOf[string]{{items: []string{"a"}, end: true}}},
		{name: "short page that goes on", pages: []pageOf[string]{{items: []string{"a"}, next: true}, {items: []string{"b"}}}},
		{name: "repeated page, accepted", ends: sentinel, pages: []pageOf[string]{{items: []string{"a"}, next: true}, {items: []string{"a"}, next: true}}},
		{name: "repeated page, not accepted", pages: []pageOf[string]{{items: []string{"a"}, next: true}, {items: []string{"a"}, next: true}},
			want: "partial — repeated or empty page"},
		{name: "last page of seen entries is the end", pages: []pageOf[string]{{items: []string{"a", "b"}, next: true}, {items: []string{"b"}}}},
		{name: "first page served again as the last, not accepted", pages: []pageOf[string]{{items: []string{"a"}, next: true}, {items: []string{"a"}}},
			want: "partial — repeated or empty page"},
		{name: "missing listing, accepted", ends: sentinel, pages: []pageOf[string]{{items: []string{"a"}, next: true}, {stop: "partial — listing is missing on next page", stopped: stopMissing}}},
		{name: "redirect, not accepted by the sentinel", ends: sentinel, pages: []pageOf[string]{{items: []string{"a"}, next: true}, {stop: "partial — redirected", stopped: stopRedirected}},
			want: "partial — redirected"},
		{name: "later 404, accepted", ends: sentinel, pages: []pageOf[string]{{items: []string{"a"}, next: true}}, failAt: status(http.StatusNotFound)},
		{name: "later 404, not accepted", pages: []pageOf[string]{{items: []string{"a"}, next: true}}, failAt: status(http.StatusNotFound),
			want: couldNotFetchNote},
		{name: "later 5xx", ends: sentinel, pages: []pageOf[string]{{items: []string{"a"}, next: true}}, failAt: status(http.StatusBadGateway),
			want: couldNotFetchNote},
		{name: "later 429", ends: sentinel, pages: []pageOf[string]{{items: []string{"a"}, next: true}}, failAt: status(http.StatusTooManyRequests),
			want: "partial — rate limited"},
		{name: "page cap", ends: sentinel, pages: []pageOf[string]{{items: []string{"a"}, next: true}, {items: []string{"b"}, next: true}},
			want: "partial — page limit"},
		{name: "item cap", ends: sentinel, limit: 1, pages: []pageOf[string]{{items: []string{"a", "b"}, next: true}}, want: fileLimitNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := tc.limit
			if limit == 0 {
				limit = -1
			}
			w := pageWalk[string]{
				key: func(s string) string { return s }, limit: limit, limitNote: fileLimitNote, pages: 2, ends: tc.ends,
				fetch: func(_ context.Context, n int) (pageOf[string], error) {
					if n >= len(tc.pages) {
						return pageOf[string]{}, tc.failAt
					}
					return tc.pages[n], nil
				},
			}
			out, err := walkPagesFor(context.Background(), w)
			if err != nil {
				t.Fatal(err)
			}
			if got := out.render(); got != tc.want {
				t.Errorf("note %q, want %q (stop %v)", got, tc.want, out.stop)
			}
			if tc.failAt != nil && !errors.Is(out.err, tc.failAt) {
				t.Errorf("the failure was not kept: %v", out.err)
			}
		})
	}

	// Cancellation outranks everything collected, accepted end or not.
	ctx, cancel := context.WithCancel(context.Background())
	_, err := walkPagesFor(ctx, pageWalk[string]{
		key: func(s string) string { return s }, limit: -1, ends: sentinel,
		fetch: func(_ context.Context, n int) (pageOf[string], error) {
			cancel()
			return pageOf[string]{items: []string{"a"}, end: true}, nil
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation", err)
	}
}
