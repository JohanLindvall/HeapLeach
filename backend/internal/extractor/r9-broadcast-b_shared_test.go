// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"slices"
	"testing"
)

// TestRegionGate covers the probe's three outcomes: refused, which leaves
// every region-only item out; allowed, which keeps them; and a probe that
// cannot tell, which asks the next one, up to the limit.
func TestRegionGate(t *testing.T) {
	items := []string{"a", "x1", "b", "x2", "x3"}
	only := func(s string) bool { return s[0] == 'x' }
	cases := map[string]struct {
		answers []int // per probe: 0 cannot tell, 1 refused, 2 allowed
		probes  int
		kept    []string
		asked   int
	}{
		"refused":           {[]int{1}, 3, []string{"a", "b"}, 1},
		"allowed":           {[]int{2}, 3, items, 1},
		"unknown then":      {[]int{0, 1}, 3, []string{"a", "b"}, 2},
		"never known":       {[]int{0, 0, 0}, 3, items, 3},
		"limit before tell": {[]int{0, 1}, 1, items, 1},
	}
	for name, tc := range cases {
		asked := 0
		kept, left := regionGate(context.Background(), items, only, tc.probes, func(_ context.Context, s string) (bool, bool) {
			answer := tc.answers[asked]
			asked++
			return answer == 1, answer != 0
		})
		if !slices.Equal(kept, tc.kept) || left != len(items)-len(tc.kept) || asked != tc.asked {
			t.Errorf("%s: kept %v, left out %d, asked %d; want %v, %d", name, kept, left, asked, tc.kept, tc.asked)
		}
	}
}

// TestNextData reads the island wherever the script names its id.
func TestNextData(t *testing.T) {
	for _, doc := range []string{
		`<script id="__NEXT_DATA__" type="application/json">{"a":1}</script>`,
		`<script type="application/json" id="__NEXT_DATA__">{"a":1}</script>`,
	} {
		if got, ok := nextData(doc); !ok || got != `{"a":1}` {
			t.Errorf("nextData(%q) = %q, %v", doc, got, ok)
		}
	}
	if _, ok := nextData(`<script id="other">{}</script>`); ok {
		t.Error("read an island from a page with none")
	}
}
