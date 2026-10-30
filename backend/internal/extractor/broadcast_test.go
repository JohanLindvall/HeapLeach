// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestBroadcastRefusalReadsAsBefore(t *testing.T) {
	tests := map[string]struct {
		err  *broadcastRefusal
		want string
	}{
		"titled":   {&broadcastRefusal{"site", "A Programme", "it is gone"}, "site: A Programme: it is gone"},
		"untitled": {&broadcastRefusal{site: "site", reason: "refused (CODE)"}, "site: refused (CODE)"},
	}
	for name, tc := range tests {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%s: Error() = %q, want %q", name, got, tc.want)
		}
	}
	wrapped := fmt.Errorf("outer: %w", &broadcastRefusal{"site", "x", "why"})
	if reason, ok := refusalReason(wrapped); !ok || reason != "why" {
		t.Errorf("refusalReason(wrapped) = %q, %v", reason, ok)
	}
	if _, ok := refusalReason(errors.New("plain")); ok {
		t.Error("a plain error read as a refusal")
	}
}

// TestResolveListedKeepsOrderAndReasons pins what every series expansion
// leans on: files in the listing's order, refusals kept in order, and the
// first other failure kept beside them.
func TestResolveListedKeepsOrderAndReasons(t *testing.T) {
	items := []string{"a", "refused-1", "b", "failed", "refused-2", "c"}
	files, refusals, failed := resolveListed(context.Background(), items, func(_ context.Context, item string) (*File, error) {
		switch item {
		case "refused-1", "refused-2":
			return nil, &broadcastRefusal{"site", item, "reason " + item}
		case "failed":
			return nil, errors.New("transport")
		}
		return &File{Name: item}, nil
	})
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if fmt.Sprint(names) != "[a b c]" {
		t.Errorf("files = %v, want [a b c]", names)
	}
	if fmt.Sprint(refusals) != "[reason refused-1 reason refused-2]" {
		t.Errorf("refusals = %v", refusals)
	}
	if failed == nil || failed.Error() != "transport" {
		t.Errorf("failed = %v", failed)
	}
}

// TestResolveInTurnAsksOneAtATime pins the sequential mode the stateful
// series lean on: never two resolves at once, each after the one before it
// in listing order, with the same files and reasons as the parallel mode.
func TestResolveInTurnAsksOneAtATime(t *testing.T) {
	items := []string{"a", "refused-1", "b", "failed", "refused-2", "c"}
	var running, seen int
	files, refusals, failed := resolveInTurn(context.Background(), items, func(_ context.Context, item string) (*File, error) {
		running++
		defer func() { running-- }()
		if running > 1 {
			t.Errorf("%s resolved while another was running", item)
		}
		seen++
		switch item {
		case "refused-1", "refused-2":
			return nil, &broadcastRefusal{"site", item, "reason " + item}
		case "failed":
			return nil, errors.New("transport")
		}
		return &File{Name: fmt.Sprint(item, seen)}, nil
	})
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if fmt.Sprint(names) != "[a1 b3 c6]" {
		t.Errorf("files = %v, want [a1 b3 c6]", names)
	}
	if fmt.Sprint(refusals) != "[reason refused-1 reason refused-2]" {
		t.Errorf("refusals = %v", refusals)
	}
	if failed == nil || failed.Error() != "transport" {
		t.Errorf("failed = %v", failed)
	}
}

// TestResolveEachKeepsItems pins that each kept file stays beside the item
// it came from, in either mode, for a caller that names files after them.
func TestResolveEachKeepsItems(t *testing.T) {
	for _, inTurn := range []bool{false, true} {
		resolved, _, _ := resolveEach(context.Background(), []int{1, 2, 3, 4}, inTurn, func(_ context.Context, n int) (*File, error) {
			if n == 2 {
				return nil, &broadcastRefusal{site: "site", reason: "no"}
			}
			return &File{Name: fmt.Sprint("f", n)}, nil
		})
		var got []string
		for _, r := range resolved {
			got = append(got, fmt.Sprint(r.item, "=", r.file.Name))
		}
		if fmt.Sprint(got) != "[1=f1 3=f3 4=f4]" {
			t.Errorf("inTurn=%v: resolved = %v", inTurn, got)
		}
	}
}

func TestListingNote(t *testing.T) {
	tests := []struct {
		got, want string
	}{
		{listingNote(cappedNote(5, 5, "episodes"), brokenNote(nil), leftOutNote(0, " that x"), refusedNote(nil)), ""},
		{listingNote(cappedNote(5, 9, "episodes"), brokenNote(errors.New("x"))), "5 of 9 episodes; the listing broke off partway"},
		{listingNote(leftOutNote(2, " that are DRM-protected"), refusedNote([]string{"gone", "late"})),
			"2 left out that are DRM-protected; 2 left out: gone"},
	}
	for i, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%d: note = %q, want %q", i, tc.got, tc.want)
		}
	}
}
