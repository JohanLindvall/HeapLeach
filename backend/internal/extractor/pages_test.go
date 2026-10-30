// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"testing"
)

// TestWalkPagesFailedHookDefers leaves the note for a failed later page to
// the walk when the hook returns pageFailed: its own note, or none when the
// walk is quiet; "" stays no note at all.
func TestWalkPagesFailedHookDefers(t *testing.T) {
	for _, tc := range []struct {
		hook  string
		quiet bool
		want  string
	}{
		{pageFailed, false, "partial — could not fetch next page"},
		{pageFailed, true, ""},
		{"", false, ""},
		{"partial — rate limited", true, "partial — rate limited"},
	} {
		_, note, err := walkPages(context.Background(), pageWalk[string]{
			key: func(s string) string { return s }, limit: -1, quiet: tc.quiet,
			failed: func(error) string { return tc.hook },
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
