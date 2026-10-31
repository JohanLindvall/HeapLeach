// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// pageOf is one page of a paged listing, as its fetch hands it to walkPages.
// How the listing ends is the page's to say, from what the site states: a
// total, an end flag, a missing cursor, or a page asked for past the end.
// A page shorter than the page size asked for says nothing on its own.
type pageOf[T any] struct {
	items []T
	// next says another page follows.
	next bool
	// end says the listing has nothing after this page, as an answer rather
	// than a failure: its items are taken, and an empty one is no repeat.
	end bool
	// total is how many entries the listing states it holds, when it
	// states one: the walk ends once the pages have served that many.
	total int
	// pages is how many pages the listing states it runs to, when it
	// states that instead: the walk ends with page pages-1.
	pages int
	// note is why the walk ends here though the listing goes on, such as
	// a next page that cannot be followed. It stands in a quiet walk too.
	note string
	// stop marks a page that is not the listing's — missing, read twice,
	// redirected elsewhere: the walk ends without its items, with stop as
	// its note unless quiet (and note, when set, in its place).
	stop string
}

// pageWalk describes a paged listing to walkPages. Where the listing is —
// its cursor, offset or page number — is the caller's, kept in fetch's
// closure.
type pageWalk[T any] struct {
	// fetch reads page n, counted from 0. A failure of the first page is
	// returned as it stands, so it carries the caller's own words.
	fetch func(ctx context.Context, n int) (pageOf[T], error)
	// key names an entry for spotting one listed twice.
	key func(T) string
	// limit caps the entries, and limitNote is the note for a walk it cut
	// short; a negative limit caps nothing.
	limit     int
	limitNote string
	// weigh, when set, counts the limit in what the entries hold rather
	// than in entries: the files of posts, of which one may carry several
	// and another none. It is handed each page's new entries, which are
	// kept whole; the walk ends at the page that reaches the limit, noted
	// when that page passes it or the listing goes on, and what is over
	// the limit is the caller's to cut.
	weigh func([]T) int
	// pages caps the pages read, config.MaxAlbumPages when zero.
	pages int
	// quiet is a listing whose walk stops where the listing does: a page
	// that adds nothing, a stop page, the page cap, a later page that
	// fails. It is no partial listing, so the walk adds no note of its
	// own; the pages' notes and limitNote still stand.
	quiet bool
	// failed decides the note for a later page that could not be
	// fetched: the note it returns, "" being none, in place of the walk's
	// own, or of none when quiet. A hook that singles out one failure
	// returns ok false for the rest, which leaves their note to the walk,
	// as if there were no hook.
	failed func(error) (note string, ok bool)
}

// walkStop is why a walk of a paged listing ended. It is the walk's state;
// the note that says it is rendered from it (pageWalk.render), so a caller
// that needs the reason — a series that says in its own words that the cap
// cut it — reads the reason rather than the wording.
type walkStop int

const (
	// walkEnded is the listing's own end: an end flag, the stated total or
	// page count, or no next page.
	walkEnded walkStop = iota
	// walkLimit is the walk's limit, in entries or in their weight.
	walkLimit
	// walkPageLimit is the page cap, with the listing going on.
	walkPageLimit
	// walkRepeated is a page that added nothing new.
	walkRepeated
	// walkStopped is a page that is not the listing's (pageOf.stop).
	walkStopped
	// walkFetchFailed is a later page that could not be fetched.
	walkFetchFailed
)

// walkEnd is why a walk ended, apart from its entries: the stop, the notes
// the pages and hooks gave for it, and the walk's own wording policy (quiet,
// limitNote). It is what a caller that does not need the entries' type —
// the tree a folder's walk belongs to — takes, and renders itself, so no
// caller reads a walk's reason back out of its note.
type walkEnd struct {
	stop walkStop
	// note is the ending page's own note (pageOf.note), and stopped its
	// stop reason, when it was a stop page.
	note, stopped string
	// failure is the failed hook's note for a page that could not be
	// fetched, and decided whether the hook gave one.
	failure string
	decided bool
	// quiet and limitNote are the walk's (pageWalk).
	quiet     bool
	limitNote string
}

// walked is what a walk came to: its entries and why it ended.
type walked[T any] struct {
	items []T
	walkEnd
}

// given is the note a caller gave for the walk's end — a page's, a stop
// page's, the failed hook's or the limit's — when that is what says it.
// own is the walk's own reason, bare, when it says it instead; a quiet
// walk has none.
func (e walkEnd) given() (note string, own string) {
	mine := func(reason string) string {
		if e.quiet {
			return ""
		}
		return reason
	}
	switch e.stop {
	case walkLimit:
		return e.limitNote, ""
	case walkPageLimit:
		return "", mine("page limit")
	case walkRepeated:
		return "", mine("repeated or empty page")
	case walkStopped:
		if e.note != "" {
			return e.note, ""
		}
		if e.quiet {
			return "", ""
		}
		return e.stopped, ""
	case walkFetchFailed:
		if e.decided {
			return e.failure, ""
		}
		return "", mine(strings.TrimPrefix(couldNotFetchNote, partialPrefix))
	}
	return e.note, ""
}

// render is the note a walk that ended so says, exactly as walkPages has
// always worded it: the page's or the hook's note where they give one, the
// limit's note, and the walk's own, which a quiet listing does not take.
func (e walkEnd) render() string {
	note, own := e.given()
	return cmp.Or(note, partial(own))
}

// reason is the walk's end as a bare reason, without the "partial — " a
// note opens with, for a caller that marks its result partial itself: a
// tree, whose note says "partial — " once for every folder it read.
func (e walkEnd) reason() string {
	note, own := e.given()
	return cmp.Or(strings.TrimPrefix(note, partialPrefix), own)
}

// walkPages follows a paged listing from its first page and returns its
// entries in order without repeats, and the note for a walk that stopped
// short of the listing's end. A first page that fails is an error; a later
// one ends the walk with what was already read. The walk ends at a page that
// says so, at the stated total or page count, at a page that adds nothing
// new — a host that ignores its paging parameter serves page one again — at
// the limit, or at the page cap. A walk that ended while the caller cancelled is a
// cancellation, not a partial listing: the page that ended it may have been
// answered before its request saw the cancellation.
func walkPages[T any](ctx context.Context, w pageWalk[T]) (items []T, note string, err error) {
	out, err := walkPagesFor(ctx, w)
	if err != nil {
		return nil, "", err
	}
	return out.items, out.render(), nil
}

// walkPagesFor is walkPages with why the walk ended (walked) in place of its
// note.
func walkPagesFor[T any](ctx context.Context, w pageWalk[T]) (out walked[T], err error) {
	out.quiet, out.limitNote = w.quiet, w.limitNote
	pages := w.pages
	if pages <= 0 {
		pages = config.MaxAlbumPages
	}
	seen := make(map[string]bool)
	// served counts the entries the pages carried, and count what the
	// limit is counted in: entries, or their weight.
	served, count := 0, 0
	for n := range pages {
		if err := ctx.Err(); err != nil {
			return walked[T]{}, err
		}
		p, err := w.fetch(ctx, n)
		if err != nil {
			if ctx.Err() != nil {
				return walked[T]{}, ctx.Err()
			}
			if n == 0 {
				return walked[T]{}, err
			}
			out.stop = walkFetchFailed
			if w.failed != nil {
				out.failure, out.decided = w.failed(err)
			}
			break
		}
		if p.stop != "" {
			out.stop, out.note, out.stopped = walkStopped, p.note, p.stop
			break
		}
		before := len(out.items)
		for _, item := range p.items {
			k := w.key(item)
			if seen[k] {
				continue
			}
			seen[k] = true
			if w.weigh == nil && len(out.items) == w.limit {
				// The cap ends the walk early, but not past a
				// cancellation, which outranks a partial listing.
				if err := ctx.Err(); err != nil {
					return walked[T]{}, err
				}
				out.stop = walkLimit
				return out, nil
			}
			out.items = append(out.items, item)
		}
		served += len(p.items)
		if w.weigh == nil {
			count = len(out.items)
		} else if count += w.weigh(out.items[before:]); w.limit >= 0 && count > w.limit {
			out.stop = walkLimit
			break
		}
		if p.end || (p.total > 0 && served >= p.total) || (p.pages > 0 && n+1 >= p.pages) {
			out.stop, out.note = walkEnded, p.note
			break
		}
		if len(out.items) == before {
			out.stop = walkRepeated
			break
		}
		if !p.next {
			out.stop, out.note = walkEnded, p.note
			break
		}
		if count == w.limit {
			out.stop = walkLimit
			break
		}
		if n == pages-1 {
			out.stop = walkPageLimit
		}
	}
	if err := ctx.Err(); err != nil {
		return walked[T]{}, err
	}
	return out, nil
}
