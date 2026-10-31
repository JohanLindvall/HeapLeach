// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"strings"
)

// Resolving a listing's items: a page, a release or an episode that has to
// be opened before it says what it holds. Each costs a request, so they are
// asked several at a time; one refused or failing must not sink the rest,
// but when none resolves the reason why is the whole answer, so it is kept.

// outcome is what resolving one item came to.
type outcome[T, R any] struct {
	item T
	got  R
	err  error
}

// resolveOutcomes resolves each item, several at a time or, inTurn, one
// after another, keeping the listing's order. An item not reached because
// the context ended has no outcome.
func resolveOutcomes[T, R any](ctx context.Context, items []T, inTurn bool, resolve func(context.Context, T) (R, error)) []outcome[T, R] {
	if inTurn {
		var outcomes []outcome[T, R]
		for _, item := range items {
			got, err := resolve(ctx, item)
			outcomes = append(outcomes, outcome[T, R]{item: item, got: got, err: err})
		}
		return outcomes
	}
	// FanOut drops an item whose fetch errored, so the error travels as a
	// value instead.
	return FanOut(ctx, items, func(ctx context.Context, item T) ([]outcome[T, R], error) {
		got, err := resolve(ctx, item)
		return []outcome[T, R]{{item: item, got: got, err: err}}, nil
	})
}

// sortOutcomes is the reasons the items refused were refused for, in the
// listing's order, and the first other failure.
func sortOutcomes[T, R any](outcomes []outcome[T, R]) (refusals []string, failed error) {
	for _, out := range outcomes {
		switch reason, refused := refusalReason(out.err); {
		case out.err == nil:
		case refused:
			refusals = append(refusals, reason)
		case failed == nil:
			failed = out.err
		}
	}
	return refusals, failed
}

// resolveFiles resolves each item into the files it holds, several at a
// time, keeping the files in the listing's order. Beside them come the
// reasons of the items refused, in order, and the first other failure, for
// the caller to report when nothing resolved (noneRead) or to note what was
// left out when something did.
func resolveFiles[T any](ctx context.Context, items []T, resolve func(context.Context, T) ([]File, error)) (files []File, refusals []string, failed error) {
	outcomes := resolveOutcomes(ctx, items, false, resolve)
	for _, out := range outcomes {
		if out.err == nil {
			files = append(files, out.got...)
		}
	}
	refusals, failed = sortOutcomes(outcomes)
	return files, refusals, failed
}

// firstReason is why nothing of a listing resolved: the first refusal,
// which outranks a failure, being the one the caller can act on; else the
// first failure, less the site's name it opens with, as the sentence it is
// folded into does; else fallback.
func firstReason(site string, refusals []string, failed error, fallback string) string {
	switch {
	case len(refusals) > 0:
		return refusals[0]
	case failed != nil:
		return strings.TrimPrefix(failed.Error(), site+": ")
	}
	return fallback
}

// noneRead is the error for a listing of n items on where, none of which
// could be read, giving the first reason (firstReason) there was.
func noneRead(site string, n int, noun, where string, refusals []string, failed error) error {
	return fmt.Errorf("%s: none of the %d %s on %s could be read: %s",
		site, n, noun, where, firstReason(site, refusals, failed, "they hold nothing to download"))
}

// readListing resolves a listing's items into their files, in the
// listing's order, with the note counting the items refused beside others
// that resolved. When none resolves, the error is the first reason why
// (noneRead) — or, once the caller has cancelled, that.
func readListing[T any](ctx context.Context, site, noun, where string, items []T,
	resolve func(context.Context, T) ([]File, error)) ([]File, string, error) {
	files, refusals, failed := resolveFiles(ctx, items, resolve)
	if len(files) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		return nil, "", noneRead(site, len(items), noun, where, refusals, failed)
	}
	return files, refusedNote(refusals), nil
}
