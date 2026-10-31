// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Helpers the public broadcasters share. Their APIs have nothing in common,
// so each keeps its own extractor; what they do share is the shape of what
// comes back — a programme refused for a reason, a lone programme named
// after its series, a listing of episodes resolved one by one or when
// their transfers start, and a note on what a listing left out.

// The reasons several broadcasters give in the same words.
const (
	refusedDRM           = "it is DRM-protected"
	refusedLiveBroadcast = "it is a live broadcast, which has no end to download to"
	refusedLiveStream    = "it is a live stream, which has no end to download to"
)

// refusalBody decodes the body of a failed request as JSON, for a
// broadcaster that says why it refused there rather than in a document of
// its own; codes, when given, are the statuses that carry such a body.
func refusalBody[T any](err error, codes ...int) (body T, ok bool) {
	status, isStatus := errors.AsType[*httpx.StatusError](err)
	if !isStatus || len(codes) > 0 && !slices.Contains(codes, status.Code) {
		return body, false
	}
	return body, json.Unmarshal([]byte(status.Body), &body) == nil
}

// broadcastMedia resolves a broadcaster's playlist, a failure reading
// "<site>: <name>: <cause>".
func broadcastMedia(ctx context.Context, client *httpx.Client, site, name, link string, headers httpx.Header) (*hlsMedia, error) {
	media, err := resolveMediaPlaylist(ctx, client, link, headers)
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", site, name, err)
	}
	return media, nil
}

// progressiveFile is a programme's plain file, named only by its extension
// ext. Its length is left for the response to state: the broadcasters that
// publish one print no byte count of it, or one of another rendition.
func progressiveFile(link, ext string, headers httpx.Header) *File {
	return &File{Name: ext, URL: link, Size: -1, Headers: headers}
}

// broadcastEpisode is one entry of a broadcaster's listing: the id its
// player is asked for, the name the listing gave it, the season it is
// filed under, and its programme where the listing names that per entry.
type broadcastEpisode struct{ id, name, season, programme string }

// seasonOf is the season an episode is filed under, as series.label takes
// it.
func (e broadcastEpisode) seasonOf() string { return e.season }

// listedAs resolves listed episodes through play, which answers with a file
// named only by its extension and the name the player gives the episode.
// The file is named by the listing's name, else the player's, else fallback
// of the id (the id itself when fallback is nil): a listing read again
// finds what was saved under the same name. A play answering with neither
// a file nor an error passes the episode over.
func listedAs(play func(context.Context, string) (*File, string, error), fallback func(string) string) func(context.Context, broadcastEpisode) (*File, error) {
	return func(ctx context.Context, ep broadcastEpisode) (*File, error) {
		file, name, err := play(ctx, ep.id)
		if err != nil || file == nil {
			return nil, err
		}
		id := ep.id
		if fallback != nil {
			id = fallback(id)
		}
		file.Name = cmp.Or(ep.name, name, id) + file.Name
		return file, nil
	}
}

// soleFile is a lone programme's job: file, named only by its extension,
// named title. A lone file lands with no folder above it to say what it
// belongs to, so title carries the programme as well (joinTitle).
func soleFile(title string, file *File) *Result {
	file.Name = title + file.Name
	return &Result{Title: title, Files: []File{*file}}
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
// the caller to report (firstReason), in the account of the items
// (outcomeSummary) that also counts the failures for the note.
func resolveEach[T any](ctx context.Context, items []T, inTurn bool, resolve func(context.Context, T) (*File, error)) ([]listedItem[T], outcomeSummary) {
	outcomes := resolveOutcomes(ctx, items, inTurn, resolve)
	var resolved []listedItem[T]
	for _, out := range outcomes {
		if out.err == nil && out.got != nil {
			resolved = append(resolved, listedItem[T]{item: out.item, file: *out.got})
		}
	}
	return resolved, summarise(len(items), outcomes, func(got *File) bool { return got != nil })
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

	resolved, outcomes := resolveEach(ctx, items, s.inTurn, s.resolve)
	refusals, failed := outcomes.refusals, outcomes.failed
	if s.refused != nil {
		*s.refused = refusals
	}
	// The caller's cancellation outranks what resolved before it, as in
	// noneResolved.
	if err := ctx.Err(); err != nil {
		return nil, "", err
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
	// A refusal is the broadcaster's to word, or to keep quiet about; an
	// episode that could not be read is always said to be missing.
	note = joinNotes(note, unreadNote(outcomes.failures))
	return resolved, note, nil
}

// listSeries is expandSeries ending a listing: the job titled title, its
// note the cap's followed by notes. A listing whose context ended while it
// was resolved returns that rather than whatever had resolved by then
// (expandSeries).
func listSeries[T any](ctx context.Context, title string, s series[T], notes ...string) (*Result, error) {
	resolved, note, err := expandSeries(ctx, s)
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
	unread := false
	for _, out := range resolveOutcomes(ctx, seasons, inTurn, read) {
		if out.err != nil {
			unread = true
			if out.index == 0 {
				first = out.err
			}
			continue
		}
		list.items = append(list.items, out.got.items...)
		list.more = list.more || out.got.more
		list.note = cmp.Or(list.note, out.got.note)
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

// Steps several broadcasters repeat around expandSeries: a listing read
// off the rendered page where the catalogue has none, a GraphQL request,
// a Next.js page's island, and the probes that tell a region-only
// programme refused here from one that plays.

// pageIDs reads the episodes a rendered page links to, by the id in link's
// first group, for a broadcaster whose catalogue does not describe the odd
// listing. site opens its errors; doc is the page, for its title.
func pageIDs(ctx context.Context, client *httpx.Client, u *url.URL, headers httpx.Header, site string, link *regexp.Regexp) (doc string, episodes []broadcastEpisode, err error) {
	doc, _, err = fetchDocument(ctx, client, u, headers)
	if err != nil {
		return "", nil, fmt.Errorf("%s: fetch %s: %w", site, u.Redacted(), err)
	}
	for _, id := range matchedIDs(doc, link) {
		episodes = append(episodes, broadcastEpisode{id: id})
	}
	if len(episodes) == 0 {
		return "", nil, fmt.Errorf("%s: no episodes listed on %s", site, u.Redacted())
	}
	return doc, episodes, nil
}

// postGraphQL asks a GraphQL endpoint query, with its variables, into out.
func postGraphQL(ctx context.Context, client *httpx.Client, endpoint string, headers httpx.Header, query string, variables, out any) error {
	return client.PostJSON(ctx, endpoint, headers, map[string]any{"query": query, "variables": variables}, out)
}

// regionGate leaves out a listing's items shown in one region alone (only)
// once one of them is refused here. refusedHere asks of them in the
// listing's order, at most probes of them, until one can tell (known), and
// its answer holds for the rest: a refusal says nothing of the programme
// and everything of the address asking. It returns the items kept, in
// order, and how many were left out.
func regionGate[T any](ctx context.Context, items []T, only func(T) bool, probes int, refusedHere func(context.Context, T) (refused, known bool)) ([]T, int) {
	blocked := false
	for _, item := range items {
		if probes == 0 {
			break
		}
		if !only(item) {
			continue
		}
		probes--
		if refused, known := refusedHere(ctx, item); known {
			blocked = refused
			break
		}
	}
	if !blocked {
		return items, 0
	}
	kept := make([]T, 0, len(items))
	for _, item := range items {
		if !only(item) {
			kept = append(kept, item)
		}
	}
	return kept, len(items) - len(kept)
}

// refusesByte asks for a file's first byte, to learn whether its host
// serves it to this address at all: a 403 is a refusal, and a probe that
// could not be made is none.
func refusesByte(ctx context.Context, client *httpx.Client, link string, headers httpx.Header) bool {
	status, err := client.StatusOnce(ctx, http.MethodGet, link,
		headers.Merge(httpx.Header{httpx.HeaderRange: "bytes=0-0"}))
	return err == nil && status == http.StatusForbidden
}

// nextDataScript is the JSON island a Next.js page renders from.
var nextDataScript = regexp.MustCompile(`(?s)<script[^>]*\bid="__NEXT_DATA__"[^>]*>(.*?)</script>`)

// nextData is the JSON island of a page rendered by Next.js
// (<script id="__NEXT_DATA__">), as written, or false when it has none.
func nextData(doc string) (string, bool) {
	if m := nextDataScript.FindStringSubmatch(doc); m != nil {
		return m[1], true
	}
	return "", false
}

// nextDataIn is nextData for a parsed page: the island's text as written
// (rawText), for a reader that already holds the tree and would otherwise
// scan the body again.
func nextDataIn(root *html.Node) (string, bool) {
	island := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.Script) && attr(n, "id") == "__NEXT_DATA__"
	})
	if island == nil {
		return "", false
	}
	return rawText(island), true
}

// allDigits reports a non-empty run of decimal digits: an id where a slug
// could stand instead.
func allDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
