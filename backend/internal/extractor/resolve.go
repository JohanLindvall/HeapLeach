// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
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
// outcomeSummary.noneResolved, expandSeries).
//
// The concurrent mode runs no more workers than there are items or slots,
// each taking the next index in turn; outcomes are kept by index, so the
// order is the listing's however the requests interleave. A serial reader
// — one whose host refuses in a way that should stop the rest, or whose
// state is not safe to share — asks inTurn and never gets a goroutine.
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
	slots := make([]outcome[T, R], len(items))
	started := make([]bool, len(items))
	var wg sync.WaitGroup
	indices := make(chan int)
	for range min(len(items), config.PageFetchConcurrency) {
		wg.Go(func() {
			for i := range indices {
				if ctx.Err() != nil {
					return
				}
				got, err := resolve(ctx, items[i])
				slots[i], started[i] = outcome[T, R]{index: i, item: items[i], got: got, err: err}, true
			}
		})
	}
dispatch:
	for i := range items {
		select {
		case indices <- i:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(indices)
	wg.Wait()

	var outcomes []outcome[T, R]
	for i, ok := range started {
		if ok {
			outcomes = append(outcomes, slots[i])
		}
	}
	return outcomes
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

// resolveListing resolves each item into the files it holds, several at a
// time, keeping the files in the listing's order, with the whole account of
// the items beside them (outcomeSummary): the caller reports why when
// nothing resolved (noneRead) and notes what was left out when some did
// (outcomeSummary.note).
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
// could be read, giving the first reason (firstReason) there was. When
// that reason is an item's failure, the error wraps it, so that a caller
// classifying by type — a status, a challenge, a refusal — still finds it
// beneath the listing's sentence.
func noneRead(site string, n int, noun, where string, refusals []string, failed error) error {
	err := &noneReadError{msg: fmt.Sprintf("%s: none of the %d %s on %s could be read: %s",
		site, n, noun, where, firstReason(site, refusals, failed, "they hold nothing to download"))}
	if len(refusals) == 0 {
		err.cause = failed
	}
	return err
}

// noneReadError is noneRead's error: its message names the first failure
// less the site's prefix, which %w could not trim, and it unwraps to that
// failure itself.
type noneReadError struct {
	msg   string
	cause error
}

func (e *noneReadError) Error() string { return e.msg }
func (e *noneReadError) Unwrap() error { return e.cause }

// readListing resolves a listing's items into their files, in the
// listing's order, with the note counting the items refused, and those that
// could not be read, beside others that resolved (outcomeSummary.note). When
// none resolves, the error is the first reason why (noneRead). Once the
// caller has cancelled, the error is that, whatever had resolved
// (outcomeSummary.noneResolved).
func readListing[T any](ctx context.Context, site, noun, where string, items []T,
	resolve func(context.Context, T) ([]File, error)) ([]File, string, error) {
	files, s := resolveListing(ctx, items, resolve)
	if err := s.noneResolved(ctx, files, site, noun, where); err != nil {
		return nil, "", err
	}
	return files, s.note(), nil
}

// listingFiles resolves listing pages against one produced-file budget.
// The walk bounds inspected entries separately. Small ordered batches stop
// empty or unavailable entries spending file slots, without resolving the
// rest of a large listing after enough files have been found.
type listingFiles[T any] struct {
	limit    int
	files    []File
	outcomes []outcome[T, []File]
}

func (f *listingFiles[T]) add(ctx context.Context, items []T, resolve func(context.Context, T) ([]File, error)) (done, cut bool) {
	for start := 0; start < len(items) && ctx.Err() == nil; {
		remaining := f.limit - len(f.files)
		if remaining <= 0 {
			return true, true
		}
		end := min(len(items), start+min(config.PageFetchConcurrency, remaining))
		outs := resolveOutcomes(ctx, items[start:end], false, resolve)
		f.outcomes = append(f.outcomes, outs...)
		for i, out := range outs {
			if out.err != nil {
				continue
			}
			keep := min(len(out.got), f.limit-len(f.files))
			f.files = append(f.files, out.got[:keep]...)
			if len(f.files) == f.limit {
				return true, keep < len(out.got) || i+1 < len(outs) || end < len(items)
			}
		}
		start = end
	}
	return len(f.files) == f.limit, false
}

func (f *listingFiles[T]) result(ctx context.Context, site, noun, where string) ([]File, string, error) {
	s := summarise(len(f.outcomes), f.outcomes, func(files []File) bool { return len(files) > 0 })
	if err := s.noneResolved(ctx, f.files, site, noun, where); err != nil {
		return nil, "", err
	}
	return f.files, s.note(), nil
}

// noneResolved is how a listing of s.listed items on where, which came to
// files, finishes: the caller's cancellation, which outranks everything
// that resolved before it — a listing cut short and returned as a success
// would be saved as the whole listing; else noneRead's error when no item
// yielded files, from the summary's own refusals and failure; else nil.
func (s outcomeSummary) noneResolved(ctx context.Context, files []File, site, noun, where string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(files) > 0 {
		return nil
	}
	return noneRead(site, s.listed, noun, where, s.refusals, s.failed)
}
