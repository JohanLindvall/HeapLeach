package util

import (
	"io"
	"strings"
	"testing"
)

func TestBoundedBufferDrainsPastItsLimit(t *testing.T) {
	for _, limit := range []int{0, 1, 5, 10} {
		b := &BoundedBuffer{Limit: limit}
		for _, part := range []string{"ab", "cdefgh"} {
			if n, err := io.Copy(b, strings.NewReader(part)); err != nil || n != int64(len(part)) {
				t.Fatalf("limit %d stopped draining: %d, %v", limit, n, err)
			}
		}
		if b.String() != "abcdefgh"[:min(limit, 8)] || b.Truncated != (limit < 8) {
			t.Fatalf("limit %d: %q, truncated %v", limit, b.String(), b.Truncated)
		}
	}
}
