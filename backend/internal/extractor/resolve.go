// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
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

// itemRefusal is a host declining one item for a reason of its own: a
// programme gone, live, protected or shown only somewhere else, a video
// still being transcoded. It is a type so that a listing can leave such
// items out, saying why once, rather than queue them all to fail, and so
// that the reason outranks an ordinary failure when nothing resolved. site
// is the extractor's prefix and title the item's name, or its id; a refusal
// that names neither leaves title empty.
type itemRefusal struct{ site, title, reason string }

func (e *itemRefusal) Error() string {
	if e.title == "" {
		return e.site + ": " + e.reason
	}
	return e.site + ": " + e.title + ": " + e.reason
}

// refusalReason is the reason a host gave, when err is its refusal.
func refusalReason(err error) (string, bool) {
	if refusal, ok := errors.AsType[*itemRefusal](err); ok {
		return refusal.reason, true
	}
	return "", false
}

// outcomeSummary is what resolving a listing's items came to, item by item
// rather than file by file: an item may hold many files or none. A listing
// is finished from it, so that what was refused, what failed and what held
// nothing stay distinct facts until the note is written.
type outcomeSummary struct {
	// listed is how many items were asked for, attempted how many started.
	// The difference is the items the caller's cancellation never reached.
	listed, attempted int
	// succeeded resolved to something, empty resolved without error to
	// nothing, failures is how many failed for some other reason than a
	// refusal, and unrecognised how many turned out to be no extractor's
	// page (errUnrecognised): navigation, never a failure.
	succeeded, empty, failures, unrecognised int
	// refusals are the reasons of the items refused, in the listing's
	// order; failed is the first other failure.
	refusals []string
	failed   error
}

// summarise counts outcomes of a listing of listed items. yielded says
// whether an item that resolved without error holds anything.
func summarise[T, R any](listed int, outcomes []outcome[T, R], yielded func(R) bool) outcomeSummary {
	s := outcomeSummary{listed: listed, attempted: len(outcomes)}
	for _, out := range outcomes {
		switch reason, refused := refusalReason(out.err); {
		case out.err == nil && yielded(out.got):
			s.succeeded++
		case out.err == nil:
			s.empty++
		case refused:
			s.refusals = append(s.refusals, reason)
		case errors.Is(out.err, errUnrecognised):
			s.unrecognised++
		default:
			s.failures++
			if s.failed == nil {
				s.failed = out.err
			}
		}
	}
	return s
}

// notStarted is how many items no outcome was had for: the caller's
// cancellation came first.
func (s outcomeSummary) notStarted() int { return s.listed - s.attempted }

// note says what a listing some of whose items resolved left out: how many
// the host refused, per reason, and how many could not be read. Items that
// held nothing are not mentioned: an empty post is the post as it is.
func (s outcomeSummary) note() string {
	return joinNotes(refusedNote(s.refusals), unreadNote(s.failures))
}

// sortOutcomes is the reasons the items refused were refused for, in the
// listing's order, and the first other failure.
func sortOutcomes[T, R any](outcomes []outcome[T, R]) (refusals []string, failed error) {
	s := summarise(len(outcomes), outcomes, func(R) bool { return true })
	return s.refusals, s.failed
}

// resolveFiles resolves each item into the files it holds, several at a
// time, keeping the files in the listing's order. Beside them come the
// reasons of the items refused, in order, and the first other failure, for
// the caller to report when nothing resolved (noneRead).
func resolveFiles[T any](ctx context.Context, items []T, resolve func(context.Context, T) ([]File, error)) (files []File, refusals []string, failed error) {
	files, s := resolveListing(ctx, items, resolve)
	return files, s.refusals, s.failed
}

// resolveListing is resolveFiles with the whole account of the items beside
// the files, for a caller that notes what was left out (outcomeSummary.note).
func resolveListing[T any](ctx context.Context, items []T, resolve func(context.Context, T) ([]File, error)) ([]File, outcomeSummary) {
	outcomes := resolveOutcomes(ctx, items, false, resolve)
	var files []File
	for _, out := range outcomes {
		if out.err == nil {
			files = append(files, out.got...)
		}
	}
	return files, summarise(len(items), outcomes, func(got []File) bool { return len(got) > 0 })
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
// listing's order, with the note counting the items refused, and those that
// could not be read, beside others that resolved (outcomeSummary.note). When
// none resolves, the error is the first reason why (noneRead). Once the
// caller has cancelled, the error is that, whatever had resolved
// (noneResolved).
func readListing[T any](ctx context.Context, site, noun, where string, items []T,
	resolve func(context.Context, T) ([]File, error)) ([]File, string, error) {
	files, s := resolveListing(ctx, items, resolve)
	if err := noneResolved(ctx, files, site, len(items), noun, where, s.refusals, s.failed); err != nil {
		return nil, "", err
	}
	return files, s.note(), nil
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
