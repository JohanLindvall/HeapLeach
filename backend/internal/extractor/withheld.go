// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"net/http"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// probeWithheld reports a probe the site did not answer with its page: a
// challenge, a refusal (401, 403), a rate limit, a server error, or a
// request that failed in transport. A page too large to read whole was
// served, and a missing page (404, 410) is an answer about the link, so
// neither counts.
//
// An extractor that reads an open API rather than the page may take such a
// probe of its own link form by a valid answer from that API (withheldAsked):
// the page being kept from a client says nothing of whether the API is, and
// the API answering for the link's id is what its extraction depends on.
func probeWithheld(p *Page) bool {
	if p == nil {
		return false
	}
	if p.Err != nil {
		return !httpx.IsTooLarge(p.Err) && !errors.Is(p.Err, errNoProbe)
	}
	switch {
	case p.Challenged, p.Status == http.StatusUnauthorized, p.Status == http.StatusForbidden,
		p.Status == http.StatusTooManyRequests, p.Status >= http.StatusInternalServerError:
		return true
	}
	return false
}

// withheldAsked asks, once per probed page and only when its probe was
// withheld (probeWithheld), the bounded question ask puts to an
// extractor's own API, and reports its answer. The question is admitted
// before its deadline starts (probeContext) and is memoised on the page
// under key, so every recogniser and the extractor after it share the one
// answer, as pageJSON's are shared.
func withheldAsked(ctx context.Context, p *Page, key string, ask func(ctx context.Context) bool) bool {
	if !probeWithheld(p) {
		return false
	}
	return memoAsked(ctx, p, "withheld "+key, func() bool {
		ctx, done, ok := probeContext(ctx)
		if !ok {
			return false
		}
		defer done()
		return ask(ctx)
	})
}
