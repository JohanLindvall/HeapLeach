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

// outcome is what resolving one item came to: the item, its place in the
// listing, and what it resolved to or why it did not.
type outcome[T, R any] struct {
	index int
	item  T
	got   R
	err   error
}

// resolveOutcomes is the one ordered, bounded map every listing resolves
// its items through: several at a time (config.PageFetchConcurrency) or,
// inTurn, one after another, the outcomes in the listing's order with each
// item's error kept beside the values. Both modes stop the same way once
// the context ends: no item starts after that, work already started
// settles, and an item not reached has no outcome. What the caller makes of
// a cancelled map is settled by its finaliser, where the caller's
// cancellation outranks whatever resolved first (readListing,
// noneResolved, expandSeries).
func resolveOutcomes[T, R any](ctx context.Context, items []T, inTurn bool, resolve func(context.Context, T) (R, error)) []outcome[T, R] {
	if inTurn {
		var outcomes []outcome[T, R]
		for i, item := range items {
			if ctx.Err() != nil {
				break
			}
			got, err := resolve(ctx, item)
			outcomes = append(outcomes, outcome[T, R]{index: i, item: item, got: got, err: err})
		}
		return outcomes
	}
	// FanOut drops an item whose fetch errored, so the error travels as a
	// value instead.
	indices := make([]int, len(items))
	for i := range indices {
		indices[i] = i
	}
	return FanOut(ctx, indices, func(ctx context.Context, i int) ([]outcome[T, R], error) {
		got, err := resolve(ctx, items[i])
		return []outcome[T, R]{{index: i, item: items[i], got: got, err: err}}, nil
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
// (noneRead). Once the caller has cancelled, the error is that, whatever
// had resolved (noneResolved).
func readListing[T any](ctx context.Context, site, noun, where string, items []T,
	resolve func(context.Context, T) ([]File, error)) ([]File, string, error) {
	files, refusals, failed := resolveFiles(ctx, items, resolve)
	if err := noneResolved(ctx, files, site, len(items), noun, where, refusals, failed); err != nil {
		return nil, "", err
	}
	return files, refusedNote(refusals), nil
}

// noneResolved is how a listing of n items on where finishes: the caller's
// cancellation, which outranks everything that resolved before it — a
// listing cut short and returned as a success would be saved as the whole
// listing; else noneRead's error when no item yielded files; else nil.
func noneResolved(ctx context.Context, files []File, site string, n int, noun, where string, refusals []string, failed error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(files) > 0 {
		return nil
	}
	return noneRead(site, n, noun, where, refusals, failed)
}
