// SPDX-License-Identifier: MIT

package extractor

import (
	"context"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// withExtractRetries repeats attempt up to config.ExtractRetries times,
// backing off between calls, for the hosts whose answer is sometimes wrong
// rather than absent: a listing that reports itself overloaded, a player
// page served with its rendition list empty. It stops as soon as again says
// the answer is worth keeping, and hands back the last one either way — the
// caller decides what an answer that never improved is worth.
//
// Each attempt is handed the context to read with, and must read with it
// rather than one it captured: the first may take the page the registry
// probed, and every later one gets freshRead's, so a retry asks the site
// again instead of being handed the same wrong answer.
//
// The budget is separate from the transport's own retries deliberately:
// those cover a request that failed (httpx.Client.Do, 429 waits included),
// and this covers one that succeeded and said something the extractor can
// see is not the whole truth.
func withExtractRetries[T any](ctx context.Context, attempt func(context.Context) (T, error), again func(T, error) bool) (T, error) {
	var (
		out T
		err error
	)
	for n := range config.ExtractRetries {
		read := ctx
		if n > 0 {
			if err := util.SleepCtx(ctx, httpx.RetryDelay(n-1)); err != nil {
				var zero T
				return zero, err
			}
			read = freshRead(ctx)
		}
		if out, err = attempt(read); !again(out, err) {
			break
		}
	}
	return out, err
}
