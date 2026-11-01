// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
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
	// skip says the page carries nothing the walk takes — cards it could
	// not use, a pager alone, a continuation that only names others — yet
	// is no repeat: the walk goes on to the next page rather than reading
	// it as the host serving the same page again. At most
	// config.MaxSkippedListingPages in a row are taken so.
	skip bool
	// total is how many entries the listing states it holds, when it
	// states one: the walk ends once the pages have served that many.
	total int
	// pages is how many pages the listing states it runs to, when it
	// states that instead: the walk ends with page pages-1.
	pages int
	// note is why the walk ends here though the listing goes on, such as
	// a next page that cannot be followed. It stands whatever the walk
	// accepts.
	note string
	// stop marks a page that is not the listing's — missing, read twice,
	// redirected elsewhere: the walk ends without its items, with stop as
	// its note unless the walk accepts that end (and note, when set, in its
	// place). stopped says which of those it was, for the walk's policy to
	// read (walkAccepts) rather than the wording.
	stop    string
	stopped pageStop
}

// pageStop is what a stop page was, as its reader saw it.
type pageStop int

const (
	// stopOther is a page not the listing's for a reason of the reader's
	// own, which no policy accepts.
	stopOther pageStop = iota
	// stopRepeatedPage is a page already read, served again.
	stopRepeatedPage
	// stopMissing is a page without the listing on it.
	stopMissing
	// stopRedirected is a page that moved to another listing.
	stopRedirected
)

// walkAccepts is how a listing ends when it ends normally, as its caller
// knows the host: each field an end taken as the listing's own, with no
// note, where by default it is a cut and says so. Anything not accepted —
// a timeout, a challenge, a rate limit, an exhausted page budget — keeps
// its note.
type walkAccepts struct {
	// repeated is a page that adds nothing new, or a page read before
	// served again: the host's way of saying there is no more.
	repeated bool
	// missing is a later page without the listing on it.
	missing bool
	// redirected is a later page that moved to another listing.
	redirected bool
	// statuses are the HTTP statuses a later page answers with past the
	// end: a 404 asked for page n+1, say.
	statuses []int
	// undecodable is a later page whose answer does not decode: a host
	// that answers past its end with an empty or broken body rather than
	// an empty list.
	undecodable bool
}

// accepts reports whether the walk takes this end as the listing's own.
func (a walkAccepts) accepts(end walkEnd) bool {
	switch end.stop {
	case walkRepeated:
		return a.repeated
	case walkStopped:
		switch end.stopKind {
		case stopRepeatedPage:
			return a.repeated
		case stopMissing:
			return a.missing
		case stopRedirected:
			return a.redirected
		}
	case walkFetchFailed:
		if a.undecodable && undecodable(end.err) {
			return true
		}
		for _, status := range a.statuses {
			if httpx.HasStatus(end.err, status) {
				return true
			}
		}
	}
	return false
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
	// consume opens each page's new entries after the entry limit and
	// deduplication. It returns done when its separate output budget is
	// full, and cut when it left something out of those entries. The walk
	// adds consumeNote only for a cut or when more pages remain.
	consume     func([]T) (done, cut bool)
	consumeNote string
	// pages caps the pages read, config.MaxAlbumPages when zero.
	pages int
	// ends are the ends this listing takes as its own (walkAccepts); the
	// pages' notes and limitNote stand whatever it accepts.
	ends walkAccepts
	// failed decides the note for a later page that could not be
	// fetched: the note it returns, "" being none, in place of the walk's
	// own, or of none when the walk accepts it. A hook that singles out one failure
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
// the pages and hooks gave for it, and the walk's own wording policy (ends,
// limitNote). It is what a caller that does not need the entries' type —
// the tree a folder's walk belongs to — takes, and renders itself, so no
// caller reads a walk's reason back out of its note.
type walkEnd struct {
	stop walkStop
	// note is the ending page's own note (pageOf.note), and stopped its
	// stop reason and stopKind what it was, when it was a stop page.
	note, stopped string
	stopKind      pageStop
	// err is why a later page could not be fetched, kept whole so a caller
	// reads the status or the challenge rather than the note.
	err error
	// failure is the failed hook's note for a page that could not be
	// fetched, and decided whether the hook gave one.
	failure string
	decided bool
	// ends and limitNote are the walk's (pageWalk).
	ends      walkAccepts
	limitNote string
}

// walked is what a walk came to: its entries and why it ended.
type walked[T any] struct {
	items []T
	walkEnd
}

// given is the note a caller gave for the walk's end — a page's, a stop
// page's, the failed hook's or the limit's — when that is what says it.
// own is the walk's own reason, bare, when it says it instead; an accepted
// walk has none.
func (e walkEnd) given() (note string, own string) {
	accepted := e.ends.accepts(e)
	mine := func(reason string) string {
		if accepted {
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
		if accepted {
			return "", ""
		}
		return e.stopped, ""
	case walkFetchFailed:
		if e.decided {
			return e.failure, ""
		}
		// A rate limit the client waited out as far as it would is said
		// as such: the host asked to come back, not that the listing ended.
		if httpx.HasStatus(e.err, http.StatusTooManyRequests) {
			return "", mine(rateLimited)
		}
		return "", mine(strings.TrimPrefix(couldNotFetchNote, partialPrefix))
	}
	return e.note, ""
}

// render is the note a walk that ended so says, exactly as walkPages has
// always worded it: the page's or the hook's note where they give one, the
// limit's note, and the walk's own, which an end the listing accepts does
// not take.
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

// undecodable reports whether err is an answer that did not decode as the
// structured data asked for.
func undecodable(err error) bool {
	if _, ok := errors.AsType[*json.SyntaxError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF)
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
	out.ends, out.limitNote = w.ends, w.limitNote
	pages := w.pages
	if pages <= 0 {
		pages = config.MaxAlbumPages
	}
	seen := make(map[string]bool)
	skipped := 0
	// first is the first page's entries by key: a later page served as
	// exactly that is the host ignoring its paging parameter.
	var first string
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
			out.stop, out.err = walkFetchFailed, err
			if w.failed != nil {
				out.failure, out.decided = w.failed(err)
			}
			break
		}
		if p.stop != "" {
			out.stop, out.note, out.stopped, out.stopKind = walkStopped, p.note, p.stop, p.stopped
			break
		}
		before := len(out.items)
		keys := make([]string, 0, len(p.items))
		for _, item := range p.items {
			keys = append(keys, w.key(item))
		}
		pageKeys := strings.Join(keys, "\x00")
		if n == 0 {
			first = pageKeys
		}
		limited := false
		for _, item := range p.items {
			k := w.key(item)
			if seen[k] {
				continue
			}
			seen[k] = true
			if w.weigh == nil && len(out.items) == w.limit {
				limited = true
				break
			}
			out.items = append(out.items, item)
		}
		served += len(p.items)
		ended := p.end || (p.total > 0 && served >= p.total) || (p.pages > 0 && n+1 >= p.pages)
		if w.consume != nil {
			if done, cut := w.consume(out.items[before:]); done {
				out.stop, out.note = walkEnded, p.note
				if cut || limited || (!ended && p.next) {
					out.stop, out.limitNote = walkLimit, w.consumeNote
				}
				break
			}
		}
		if limited {
			out.stop = walkLimit
			break
		}
		if w.weigh == nil {
			count = len(out.items)
		} else if count += w.weigh(out.items[before:]); w.limit >= 0 && count > w.limit {
			out.stop = walkLimit
			break
		}
		if ended {
			out.stop, out.note = walkEnded, p.note
			break
		}
		if len(out.items) == before && (p.next || p.note != "" || pageKeys == first) {
			// A last page (no next, nothing to say of why, and not the
			// first page served again) that adds nothing is still the
			// listing's own end, as below, not a repeat: a newest-first
			// feed whose window shifted during the walk ends this way.
			//
			// A page that only repeats is the host serving one page
			// again; one that says it has nothing usable but goes on
			// (skip) is passed over, a few in a row at most.
			if !p.skip || skipped >= config.MaxSkippedListingPages {
				out.stop = walkRepeated
				break
			}
			skipped++
		} else {
			skipped = 0
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
