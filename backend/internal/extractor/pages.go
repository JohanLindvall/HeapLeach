// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"

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
	// failed is the note for a later page that could not be fetched, in
	// place of the walk's own, or of none when quiet; "" is no note, and
	// pageFailed the walk's own after all.
	failed func(error) string
}

// pageFailed, returned by a pageWalk's failed hook, leaves the note to the
// walk, as if there were no hook: a hook that singles out one failure need
// not spell out the note for the rest.
const pageFailed = "\x00page failed"

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
	pages := w.pages
	if pages <= 0 {
		pages = config.MaxAlbumPages
	}
	// own is the walk's own note for ending early, which a quiet listing
	// does not take.
	own := func(note string) string {
		if w.quiet {
			return ""
		}
		return note
	}
	seen := make(map[string]bool)
	// served counts the entries the pages carried, and count what the
	// limit is counted in: entries, or their weight.
	served, count := 0, 0
	for n := range pages {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		p, err := w.fetch(ctx, n)
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			if n == 0 {
				return nil, "", err
			}
			note = pageFailed
			if w.failed != nil {
				note = w.failed(err)
			}
			if note == pageFailed {
				note = own(couldNotFetchNote)
			}
			break
		}
		if p.stop != "" {
			note = cmp.Or(p.note, own(p.stop))
			break
		}
		before := len(items)
		for _, item := range p.items {
			k := w.key(item)
			if seen[k] {
				continue
			}
			seen[k] = true
			if w.weigh == nil && len(items) == w.limit {
				return items, w.limitNote, nil
			}
			items = append(items, item)
		}
		served += len(p.items)
		if w.weigh == nil {
			count = len(items)
		} else if count += w.weigh(items[before:]); w.limit >= 0 && count > w.limit {
			note = w.limitNote
			break
		}
		if p.end || (p.total > 0 && served >= p.total) || (p.pages > 0 && n+1 >= p.pages) {
			note = p.note
			break
		}
		if len(items) == before {
			note = own("partial — repeated or empty page")
			break
		}
		if !p.next {
			note = p.note
			break
		}
		if count == w.limit {
			note = w.limitNote
			break
		}
		if n == pages-1 {
			note = own("partial — page limit")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return items, note, nil
}
