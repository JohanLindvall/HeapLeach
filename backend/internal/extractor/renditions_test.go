// SPDX-License-Identifier: MIT

package extractor

import (
	"testing"
)

func TestRankIn(t *testing.T) {
	order := []string{"best", "good", "fair"}
	tests := []struct {
		name    string
		unknown int
		want    int
	}{
		{"best", -1, 0},
		{"GOOD", -1, 1},
		{"Fair", len(order), 2},
		{"other", -1, -1},
		{"other", len(order), 3},
		{"", -1, -1},
	}
	for _, tc := range tests {
		if got := rankIn(order, tc.name, tc.unknown); got != tc.want {
			t.Errorf("rankIn(%q, unknown %d) = %d, want %d", tc.name, tc.unknown, got, tc.want)
		}
	}
}
