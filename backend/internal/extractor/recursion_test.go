package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

type recursiveFixture struct {
	registry *Registry
	calls    int
	cycle    bool
}

func (*recursiveFixture) Name() string        { return "fixture" }
func (*recursiveFixture) Match(*url.URL) bool { return true }
func (f *recursiveFixture) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	f.calls++
	if f.calls > config.MaxExtractionDepth+1 {
		return nil, fmt.Errorf("fixture safety stop")
	}
	next := u.String()
	if !f.cycle {
		next = fmt.Sprintf("https://example.test/source/%d", f.calls)
	}
	res, _, err := f.registry.Extract(ctx, next, opts)
	return res, err
}

func TestRegistryBoundsRecursiveSources(t *testing.T) {
	for _, cycle := range []bool{true, false} {
		t.Run(fmt.Sprintf("cycle=%t", cycle), func(t *testing.T) {
			f := &recursiveFixture{cycle: cycle}
			r := &Registry{fallback: f}
			f.registry = r
			_, _, err := r.Extract(t.Context(), "https://example.test/source/0", Options{})
			want, calls := "nesting exceeds", config.MaxExtractionDepth
			if cycle {
				want, calls = "recursive source", 1
			}
			if err == nil || !strings.Contains(err.Error(), want) || f.calls != calls {
				t.Fatalf("Extract = %v after %d calls, want %q after %d", err, f.calls, want, calls)
			}
		})
	}
}
