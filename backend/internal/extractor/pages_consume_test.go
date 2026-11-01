// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestWalkPagesConsumesOnlyAdmittedChildren(t *testing.T) {
	var consumed []int
	pages := 0
	_, note, err := walkPages(t.Context(), pageWalk[int]{
		key: func(n int) string { return fmt.Sprint(n) }, limit: 3, limitNote: sourceLimitNote,
		consume: func(items []int) (bool, bool) { consumed = append(consumed, items...); return false, false },
		fetch: func(_ context.Context, n int) (pageOf[int], error) {
			pages++
			if n == 0 {
				return pageOf[int]{items: []int{1, 1, 2}, next: true}, nil
			}
			return pageOf[int]{items: []int{2, 3, 4}, next: true}, nil
		},
	})
	if err != nil || pages != 2 || !slices.Equal(consumed, []int{1, 2, 3}) || note != sourceLimitNote {
		t.Fatalf("consumed=%v pages=%d note=%q err=%v", consumed, pages, note, err)
	}
}

func TestWalkPagesConsumedFileBudgetKeepsTerminalAndCancellationRules(t *testing.T) {
	for _, ending := range []string{"end flag", "total", "page count", "no next", "more pages", "cut", "cancelled"} {
		t.Run(ending, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pages := 0
			items, note, err := walkPages(ctx, pageWalk[int]{
				key: func(n int) string { return fmt.Sprint(n) }, limit: 10, limitNote: sourceLimitNote,
				consumeNote: fileLimitNote,
				consume: func([]int) (bool, bool) {
					if ending == "cancelled" {
						cancel()
					}
					return true, ending == "cut"
				},
				fetch: func(context.Context, int) (pageOf[int], error) {
					pages++
					return pageOf[int]{items: []int{1, 2}, next: ending != "no next", end: ending == "end flag",
						total: map[string]int{"total": 2}[ending], pages: map[string]int{"page count": 1}[ending]}, nil
				},
			})
			if pages != 1 {
				t.Fatalf("fetched %d pages after the file budget filled", pages)
			}
			if ending == "cancelled" {
				if !errors.Is(err, context.Canceled) || items != nil {
					t.Fatalf("cancelled walk returned items=%v err=%v", items, err)
				}
				return
			}
			want := ""
			if ending == "more pages" || ending == "cut" {
				want = fileLimitNote
			}
			if err != nil || note != want {
				t.Fatalf("note=%q err=%v; want %q", note, err, want)
			}
		})
	}
}
