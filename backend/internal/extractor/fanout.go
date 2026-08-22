package extractor

import (
	"context"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Expanding a listing into the pages behind it.
//
// Several hosts list things but keep the media on each thing's own page, so
// resolving one URL means fetching many. Doing that serially leaves a large
// profile sitting in "resolving" for minutes before a byte moves, which is
// why coomerfans fetched concurrently first — and why every listing-shaped
// host since has wanted the same three properties:
//
//   - bounded concurrency, because this is a burst of small requests at one
//     host rather than a download, and PageFetchConcurrency is deliberately
//     far below the transfer concurrency;
//   - results collected BY INDEX, so the job lists its files in the order the
//     listing did however the requests interleave;
//   - one page that will not load skipped rather than failing the job, since
//     on a listing of hundreds the rest are still worth having.
//
// All three are load-bearing and each was arrived at for a reason, so they
// live here once rather than being reproduced — slightly differently, in the
// end — by every host that needs them.

// FanOut maps fetch over items, several at a time, and returns the results in
// the input's order. An item whose fetch fails or returns nothing contributes
// nothing, and the rest still arrive.
func FanOut[In, Out any](ctx context.Context, items []In, fetch func(context.Context, In) ([]Out, error)) []Out {
	groups := make([][]Out, len(items))

	var wg sync.WaitGroup
	slots := make(chan struct{}, config.PageFetchConcurrency)
	for i, item := range items {
		wg.Add(1)
		go func(i int, item In) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			if out, err := fetch(ctx, item); err == nil {
				groups[i] = out
			}
		}(i, item)
	}
	wg.Wait()

	var out []Out
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}
