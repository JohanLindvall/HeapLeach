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
