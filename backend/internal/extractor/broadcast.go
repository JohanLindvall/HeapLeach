// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
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

// resolveListed resolves a listing's items several at a time, keeping the
// files in the listing's order. One item refused or failing must not sink
// the rest; but when nothing resolves, why is the whole answer, and a whole
// series refused is refused for one reason. So the broadcaster's reasons are
// kept in order, and the first other failure beside them, for the caller to
// report: a refusal outranks a failure, being the one the caller can act on.
func resolveListed[T any](ctx context.Context, items []T, resolve func(context.Context, T) (*File, error)) (files []File, refusals []string, failed error) {
	resolved, refusals, failed := resolveEach(ctx, items, false, resolve)
	return listedFiles(resolved), refusals, failed
}

// resolveInTurn is resolveListed one item at a time, for a broadcaster
// whose resolving carries state from one episode to the next or that is
// not to be asked several things at once.
func resolveInTurn[T any](ctx context.Context, items []T, resolve func(context.Context, T) (*File, error)) (files []File, refusals []string, failed error) {
	resolved, refusals, failed := resolveEach(ctx, items, true, resolve)
	return listedFiles(resolved), refusals, failed
}

// listedItem is a listed item with the file it resolved to.
type listedItem[T any] struct {
	item T
	file File
}

// resolveEach is resolveListed keeping each file beside its item, for a
// caller that still needs the item afterwards; inTurn resolves one at a
// time.
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
	// what it left out.
	refused *[]string
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
	return resolved, note, nil
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

// listingNote joins what a listing says of itself, leaving out the parts
// that are empty.
func listingNote(parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "; ")
}

// cappedNote says a listing of shown items was cut to kept, or nothing when
// it was not.
func cappedNote(kept, shown int, unit string) string {
	if shown <= kept {
		return ""
	}
	return fmt.Sprintf("%d of %d %s", kept, shown, unit)
}

// leftOutNote says n items were left out, and why, or nothing when none
// were. why reads on from "left out": ": <reason>" or " that ...".
func leftOutNote(n int, why string) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d left out%s", n, why)
}

// refusedNote says how many were left out for the reasons given, naming
// the first, or nothing when none were.
func refusedNote(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return fmt.Sprintf("%d left out: %s", len(reasons), reasons[0])
}

// brokenNote says a listing's walk broke off partway, when it did.
func brokenNote(err error) string {
	if err == nil {
		return ""
	}
	return "the listing broke off partway"
}
