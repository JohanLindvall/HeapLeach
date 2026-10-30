// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Helpers the public broadcasters share. Their APIs have nothing in common,
// so each keeps its own extractor; what they do share is the shape of what
// comes back — a programme refused for a reason, a series whose episodes are
// resolved when their transfers start, and a note on what a listing left
// out.

// broadcastRefusal is a broadcaster declining a programme: gone, live,
// protected, or shown only somewhere else. It is a type so that a listing
// can leave such programmes out, saying why once, rather than queue them all
// to fail. site is the extractor's prefix and title the programme's name, or
// its id; a refusal that names neither leaves title empty.
type broadcastRefusal struct{ site, title, reason string }

func (e *broadcastRefusal) Error() string {
	if e.title == "" {
		return e.site + ": " + e.reason
	}
	return e.site + ": " + e.title + ": " + e.reason
}

// refusalReason is the reason a broadcaster gave, when err is its refusal.
func refusalReason(err error) (string, bool) {
	if refusal, ok := errors.AsType[*broadcastRefusal](err); ok {
		return refusal.reason, true
	}
	return "", false
}

// listedItem is a listed item with the file it resolved to.
type listedItem[T any] struct {
	item T
	file File
}

// resolveEach resolves a listing's items into one file each, several at a
// time or, inTurn, one after another — for a broadcaster whose resolving
// carries state from one episode to the next, or that is not to be asked
// several things at once — keeping each file beside its item in the
// listing's order. One item refused or failing must not sink the rest; but
// when nothing resolves, why is the whole answer, so the broadcaster's
// reasons are kept in order, and the first other failure beside them, for
// the caller to report (firstReason).
func resolveEach[T any](ctx context.Context, items []T, inTurn bool, resolve func(context.Context, T) (*File, error)) (resolved []listedItem[T], refusals []string, failed error) {
	outcomes := resolveOutcomes(ctx, items, inTurn, resolve)
	refusals, failed = sortOutcomes(outcomes)
	for _, out := range outcomes {
		if out.err == nil && out.got != nil {
			resolved = append(resolved, listedItem[T]{item: out.item, file: *out.got})
		}
	}
	return resolved, refusals, failed
}

// series is a broadcaster's listing of episodes, as expandSeries takes it.
type series[T any] struct {
	// site opens the error, noun counts the items ("episodes"), and link
	// names the listing in it.
	site, noun, link string
	items            []T
	// listed is how many the listing holds when items is already a cut of
	// it; more says the listing goes on past items by a count it did not
	// state, its walk having stopped at the cap.
	listed int
	more   bool
	// label is the season an item is filed under, read once the items are
	// resolved, so that a resolve may fill it in. split files labelled items
	// into folders even when they share one label: the listing had several
	// seasons, if only one of them listed anything.
	label func(T) string
	split bool
	// inTurn asks one item at a time. budget stands in for
	// config.MaxResolvedEpisodes where a host bears fewer requests.
	inTurn bool
	budget int
	// resolve names an item's file; its Dir is the season's, set here.
	resolve func(context.Context, T) (*File, error)
	// unplayable is the reason given when nothing was refused or failed.
	unplayable string
	// refused, when set, is given the reasons of the items left out as
	// refused, in the listing's order, for a broadcaster whose note says
	// what it left out. noteRefused says it in the note, as refusedNote.
	refused     *[]string
	noteRefused bool
}

// expandSeries is the sequence the public broadcasters repeat once a
// series is listed: cut the episodes to the request budget, resolve them,
// file them by season when there is more than one to tell apart, and, when
// none resolved, say why in the broadcaster's words. It returns each file
// beside its item, and the listing's note on what the cap left out.
//
// The budget is config.MaxResolvedEpisodes, or s.budget, and not the
// user's file cap: an episode refused or failing costs a request and yields
// no file, so episodes asked up to the file cap could all be refused where
// the ones after them play. The registry holds the files to that cap.
func expandSeries[T any](ctx context.Context, s series[T]) ([]listedItem[T], string, error) {
	items := s.items
	listed := max(s.listed, len(items))
	if limit := cmp.Or(s.budget, config.MaxResolvedEpisodes); len(items) > limit {
		items = items[:limit]
	}
	note := cappedNote(len(items), listed, s.noun)
	if s.more {
		note = fmt.Sprintf("first %d %s", len(items), s.noun)
	}

	resolved, refusals, failed := resolveEach(ctx, items, s.inTurn, s.resolve)
	if s.refused != nil {
		*s.refused = refusals
	}
	if len(resolved) == 0 {
		return nil, "", fmt.Errorf("%s: none of the %d %s of %s are playable: %s",
			s.site, len(items), s.noun, s.link, firstReason(s.site, refusals, failed, s.unplayable))
	}
	if s.label != nil && (s.split || nestByLabel(items, s.label)) {
		for i := range resolved {
			resolved[i].file.Dir = sourceFolder(s.label(resolved[i].item))
		}
	}
	if s.noteRefused {
		note = joinNotes(note, refusedNote(refusals))
	}
	return resolved, note, nil
}

// listSeries is expandSeries ending a listing: the job titled title, its
// note the cap's followed by notes. A listing whose context ended while it
// was resolved returns that rather than whatever had resolved by then.
func listSeries[T any](ctx context.Context, title string, s series[T], notes ...string) (*Result, error) {
	resolved, note, err := expandSeries(ctx, s)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, err
	}
	return &Result{Title: title, Files: listedFiles(resolved), Note: joinNotes(append([]string{note}, notes...)...)}, nil
}

// joinTitle names something by its series and its episode together, coping
// with either half missing. says reports whether the episode's title
// already says the series, so prefixing it would say it twice; each
// broadcaster keeps its own, since their titles repeat the series in
// different ways (titleEqual, titlePrefixed, titleContains).
func joinTitle(series, episode string, says func(episode, series string) bool) string {
	switch {
	case episode == "":
		return series
	case series == "" || says(episode, series):
		return episode
	}
	return series + " - " + episode
}

// writtenTitle is a page's <title> as written, its entities decoded, or ""
// when it has none. Unlike firstTitleOf it keeps the title's own spacing —
// the no-break space French typography puts before a colon, a doubled
// space — which a page's job and file names already carry.
func writtenTitle(doc string) string {
	if m := titleElement.FindStringSubmatch(doc); m != nil {
		return util.Unescape(m[1])
	}
	return ""
}

var titleElement = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// titleEqual is a one-off programme, its episode named after it.
func titleEqual(episode, series string) bool { return strings.EqualFold(series, episode) }

// titlePrefixed is an episode whose title opens with the series' name: a
// daily strand's "10 vor 10 vom 21.08.2026", a drama's "A Drama S1E1 - …".
func titlePrefixed(episode, series string) bool {
	return strings.HasPrefix(strings.ToLower(episode), strings.ToLower(series))
}

// titleContains is an episode whose title spells the series out anywhere,
// as in "Folge 12 · Staffel 4 | A Drama (S04/E12)".
func titleContains(episode, series string) bool {
	return strings.Contains(strings.ToLower(episode), strings.ToLower(series))
}

// seasonList is one season's listing as read: its entries, whether it went
// on past the limit it was read to, and the note for a walk of it cut short
// some other way.
type seasonList[T any] struct {
	items []T
	more  bool
	note  string
}

// unreadableSeasons is the note on a programme some of whose seasons could
// not be read.
const unreadableSeasons = "partial — unreadable seasons"

// readSeasons reads a programme's seasons, several at a time or, inTurn,
// one after another, into one listing in the seasons' order: their entries,
// whether any went on past its limit, and the first walk's note. A season
// that cannot be read is left out rather than losing the others, and the
// note says seasons were (unreadableSeasons). first is the first season's
// failure, when it could not be read, for a broadcaster that takes that as
// the programme itself failing; the rest take what the others gave.
func readSeasons[S, T any](ctx context.Context, seasons []S, inTurn bool, read func(context.Context, S) (seasonList[T], error)) (list seasonList[T], first error) {
	type outcome struct {
		index int
		list  seasonList[T]
		err   error
	}
	readOne := func(ctx context.Context, i int) ([]outcome, error) {
		got, err := read(ctx, seasons[i])
		return []outcome{{i, got, err}}, nil
	}
	indices := make([]int, len(seasons))
	for i := range indices {
		indices[i] = i
	}
	var outcomes []outcome
	if inTurn {
		for _, i := range indices {
			if ctx.Err() != nil {
				break
			}
			got, _ := readOne(ctx, i)
			outcomes = append(outcomes, got...)
		}
	} else {
		outcomes = FanOut(ctx, indices, readOne)
	}

	unread := false
	for _, out := range outcomes {
		if out.err != nil {
			unread = true
			if out.index == 0 {
				first = out.err
			}
			continue
		}
		list.items = append(list.items, out.list.items...)
		list.more = list.more || out.list.more
		list.note = cmp.Or(list.note, out.list.note)
	}
	if unread {
		list.note = joinNotes(list.note, unreadableSeasons)
	}
	return list, first
}

// walkCut is the limit note walkEpisodes hands walkPages, to learn that
// the walk stopped at its limit with the listing going on.
const walkCut = "\x00cut"

// walkEpisodes is walkPages for a broadcaster's listing taken up to limit
// entries (none when negative), saying whether the listing went on past
// them (more) rather than noting it, so that expandSeries or the caller says
// what the cap left out in its own words. note is the walk's own, for a
// listing cut short some other way.
func walkEpisodes[T any](ctx context.Context, limit int, w pageWalk[T]) (items []T, more bool, note string, err error) {
	w.limit, w.limitNote = limit, walkCut
	items, note, err = walkPages(ctx, w)
	if note == walkCut {
		return items, true, "", err
	}
	return items, false, note, err
}

// nestByLabel reports whether a listing spans more than one distinct
// non-empty label, which is when filing its files into per-label folders
// earns its keep. With one label the folder would only restate the job's own
// name, and with none there is nothing to tell apart.
func nestByLabel[T any](items []T, label func(T) string) bool {
	distinct := make(map[string]bool)
	for _, item := range items {
		if l := label(item); l != "" {
			distinct[l] = true
		}
	}
	return len(distinct) > 1
}

// listedFiles is the files of resolved items, in order.
func listedFiles[T any](resolved []listedItem[T]) []File {
	var files []File
	for _, r := range resolved {
		files = append(files, r.file)
	}
	return files
}

// lazyEpisode is a listed episode resolved when its transfer starts: a
// series can run to hundreds, each several requests, and a signed stream
// read at listing time can expire in the queue. It keeps the name it is
// listed under, so that a listing read again finds what was saved under it.
func lazyEpisode(name, dir string, headers httpx.Header, resolve func(context.Context) (*Target, error)) File {
	return File{Name: name, Size: -1, Dir: dir, Headers: headers, Resolve: resolve}
}
