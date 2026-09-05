package extractor

import (
	"context"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// withExtractRetries repeats attempt up to config.ExtractRetries times,
// backing off between calls, for the hosts whose answer is sometimes wrong
// rather than absent: a listing that reports itself overloaded, a player
// page served with its rendition list empty. It stops as soon as again says
// the answer is worth keeping, and hands back the last one either way — the
// caller decides what an answer that never improved is worth.
//
// The budget is shared with the transport's own retries deliberately: those
// cover a request that failed, and this covers one that succeeded and said
// something the extractor can see is not the whole truth.
func withExtractRetries[T any](ctx context.Context, attempt func() (T, error), again func(T, error) bool) (T, error) {
	var (
		out T
		err error
	)
	for n := range config.ExtractRetries {
		if n > 0 {
			wait := util.Backoff(n-1, config.RequestRetryBase, config.RequestRetryMax)
			if err := util.SleepCtx(ctx, wait); err != nil {
				var zero T
				return zero, err
			}
		}
		if out, err = attempt(); !again(out, err) {
			break
		}
	}
	return out, err
}
