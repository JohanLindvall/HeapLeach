// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Paged video listings on a Kernel Video Sharing install.
//
// A member's videos and a search's results are one shape: a page of tiles
// linking to video pages, a few dozen at a time, under a pagination control.
// The walk follows the listing's own "next" anchor where the theme renders
// one, which is both simpler and the only version that cannot be wrong about
// where the pages stop: the last page carries no next link at all.
//
// Not every listing renders one. Search results, categories, and the member
// listing on some installs, page entirely in script: the control is there
// but its anchor points at "#", and the site's own script asks for the block
// directly — ?mode=async&function=get_block&block_id=<block>&<parameters> —
// so the walk does the same, with the block and the parameters read off
// that very control. Those listings say where they end in the control too.
// The "next" item is still rendered on the last page, holding a span where
// the anchor was, the way "Back" is rendered on the first, and that is read
// as the end rather than asking for a page past it.
//
// The parameters are read rather than guessed because guessing looks right
// and is not. Which parameter pages a block is the block's own business —
// "from" on a category, "from_videos" on a member's videos, "from_videos"
// and "from_albums" together on a search — and the platform does not refuse
// a wrong one: it ignores it and serves page one again, byte for byte, so a
// walk built on a guess stops at the first repeat having quietly collected
// only the first page of however many there are. Pages are deduplicated and
// the walk stops on one that adds nothing, so a control that carries no
// parameters at all ends the walk rather than looping; and the stated total
// — "Showing 49 - 64 of 64" — is read as a second stop where the listing
// states one.

// kvsListing is one paged list of videos: where it starts, what to name the
// job after, and how to say it turned out empty.
type kvsListing struct {
	// url is the first page, and the base every later one is asked for from.
	url string
	// title names the job, given the first page.
	title func(root *html.Node) string
	// empty explains a listing with no videos on it.
	empty string
	// first is the first page when the caller has already read it, so
	// that it is neither fetched nor parsed twice: its text for the stated
	// total, its tree for the walk.
	first *document
	// limit is the listing cap in force; zero means the default.
	limit int
}

// fileCap is how many videos the walk may collect.
func (l kvsListing) fileCap() int {
	if l.limit > 0 {
		return l.limit
	}
	return config.MaxListingFiles
}

// kvsShowingTotal reads the "Showing 49 - 64 of 64 videos" line, which is how
// a listing states how far it goes when it states it at all.
var kvsShowingTotal = regexp.MustCompile(`Showing\s+\d+\s*-\s*\d+\s+of\s+(\d+)`)

// kvsListingResult resolves a listing into one file per video, with the
// note for a listing the cap cut short and for videos that refused.
//
// Every video needs its own page read for a media URL, so they are fetched
// several at a time and a video that will not load is skipped rather than
// failing a listing of hundreds; when none loads, the first reason says why.
// A listing with no videos at all is a kvsEmpty.
func kvsListingResult(ctx context.Context, client *httpx.Client, l kvsListing) (*Result, error) {
	pages, title, note, err := kvsListingWalk(ctx, client, l)
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, kvsEmpty{name: kvsNameOf(l.url), why: l.empty}
	}
	files, refused, err := readListing(ctx, kvsNameOf(l.url), "videos", l.url, pages, func(ctx context.Context, page string) ([]File, error) {
		u, err := ParseURL(page)
		if err != nil {
			return nil, err
		}
		res, err := kvsExtract(ctx, client, u)
		if err != nil {
			return nil, err
		}
		return res.Files, nil
	})
	if err != nil {
		return nil, err
	}
	return &Result{Title: title, Files: files, Note: joinNotes(note, refused)}, nil
}

// kvsEmpty is the error for a listing with no videos on it, saying why in
// the listing's own words.
type kvsEmpty struct{ name, why string }

func (e kvsEmpty) Error() string { return e.name + ": " + e.why }

// kvsListingWalk walks the listing and collects every video page it names,
// up to the cap, each being a page to read, with the note for a walk the cap
// cut short.
//
// The listing's own end is the walk's: a page that adds nothing, one asked
// for twice, a later page that will not load or parse. The stated total
// ends it as well, ahead of the cap.
func kvsListingWalk(ctx context.Context, client *httpx.Client, l kvsListing) ([]string, string, string, error) {
	headers := httpx.Referer(l.url)
	first := l.first
	if first == nil {
		doc, err := client.GetString(ctx, l.url, headers)
		if err != nil {
			return nil, "", "", fmt.Errorf("%s: fetch %s: %w", kvsNameOf(l.url), l.url, err)
		}
		first = &document{body: doc}
	}
	doc := first.body
	root, err := first.parse()
	if err != nil {
		return nil, "", "", nil
	}
	start, err := url.Parse(l.url)
	if err != nil {
		return nil, "", "", nil
	}
	total := 0
	if m := kvsShowingTotal.FindStringSubmatch(doc); m != nil {
		total, _ = strconv.Atoi(m[1])
	}

	var (
		seen  = make(map[string]bool)
		page  = 0
		block string
	)
	links, title, note, err := walkListing(ctx, client, start, listingWalk{
		name: kvsNameOf(l.url), title: l.title(root), root: root,
		// The stated total, a page adding nothing, one without the
		// listing, or the 404 an async page request past the end answers
		// is where it ends; a page that fails otherwise is a cut.
		ends:  walkAccepts{repeated: true, missing: true, statuses: []int{http.StatusNotFound}},
		limit: l.fileCap(), limitNote: fileLimitNote,
		fetch: fetchAsAsked(client, headers),
		read: func(root *html.Node, _, _ *url.URL) listingPage {
			page++
			var p listingPage
			for _, link := range kvsListingVideos(root, l.url) {
				p.links = append(p.links, listingLink{url: link})
				seen[link] = true
			}
			switch {
			case total > 0 && len(seen) >= total:
				return p
			case kvsListingEnds(root):
				// The listing says where the next page is, and on the last
				// one says that there is none. That is the stop condition;
				// everything above is a guard against a listing that
				// misreports itself.
				return p
			}
			target := kvsNextPage(root, l.url)
			if target == "" {
				target = kvsAsyncNext(root, l.url)
			}
			if target == "" {
				if page == 1 {
					block = kvsListingBlock(root)
				}
				target = kvsAsyncPage(l.url, block, page+1)
			}
			if target != "" {
				p.next, _ = url.Parse(target)
			}
			return p
		},
	})
	if err != nil {
		return nil, "", "", err
	}
	return listingURLs(links), title, note, nil
}

// kvsAsyncNext builds the request the site's own script would send for the
// next page: the block the "next" control names, with the parameters it
// carries, on top of whatever query the listing already had.
//
// The parameters are written "key:value;key:value", and a key written as
// "a+b" gives each name the value — "from_videos+from_albums:2" is how a
// search pages its two blocks in step — exactly as the script reads them.
// A control that names no block or carries no parameters gives nothing,
// and the walk falls back to its older guess.
func kvsAsyncNext(root *html.Node, base string) string {
	a := kvsNextAnchor(kvsNextControl(root))
	if a == nil {
		return ""
	}
	block, params := attr(a, "data-block-id"), attr(a, "data-parameters")
	if block == "" || params == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("mode", "async")
	q.Set("function", "get_block")
	q.Set("block_id", block)
	for pair := range strings.SplitSeq(params, ";") {
		key, value, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		for name := range strings.SplitSeq(key, "+") {
			if name = strings.TrimSpace(name); name != "" {
				q.Set(name, value)
			}
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// kvsPager finds the pagination control that belongs to the videos, or nil
// when the page has none.
//
// A search page carries a second listing, of albums, with a pager of its
// own, and reading "next" off the wrong one would either end the walk after
// page one or send it on past the end. A pager's id names the block it
// pages, so the one naming the videos block wins; a theme that puts no id on
// its pager has only the one.
func kvsPager(root *html.Node) *html.Node {
	pagers := findAll(root, func(n *html.Node) bool { return hasClass(n, "pagination") })
	for _, p := range pagers {
		if strings.HasPrefix(attr(p, "id"), "list_videos") {
			return p
		}
	}
	if len(pagers) > 0 {
		return pagers[0]
	}
	return nil
}

// kvsNextControl finds the pager's "next" item. Themes class it either
// pagination-next or next, on the list item with the anchor inside it or on
// the anchor itself. The bare "next" is only looked for inside a pager: on a
// page without one, only the specific class is trusted, since "next" is
// also what a carousel calls its button.
func kvsNextControl(root *html.Node) *html.Node {
	if pager := kvsPager(root); pager != nil {
		return findFirst(pager, func(n *html.Node) bool {
			return hasClass(n, "pagination-next") || hasClass(n, "next")
		})
	}
	return findFirst(root, func(n *html.Node) bool { return hasClass(n, "pagination-next") })
}

// kvsNextAnchor reads the link out of a next control, or nil when it holds
// none.
func kvsNextAnchor(control *html.Node) *html.Node {
	if control == nil {
		return nil
	}
	if isElem(control, atom.A) {
		return control
	}
	return findFirst(control, func(n *html.Node) bool { return isElem(n, atom.A) })
}

// kvsNextPage follows the listing's own "next" control, and returns nothing
// when there is no page to follow it to: a last page, a theme without the
// control, or one whose anchor points at "#" because the listing pages in
// script. That last case matters — followed, "#" resolves straight back to
// the page it is on, and the walk would end there having read one page of
// many.
func kvsNextPage(root *html.Node, base string) string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return ""
	}
	a := kvsNextAnchor(kvsNextControl(root))
	if a == nil {
		return ""
	}
	href := strings.TrimSpace(attr(a, "href"))
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	return resolveRef(baseURL, href)
}

// kvsListingEnds reports whether a page says it is the last one: its pager
// still renders the "next" item but with nothing to follow, a span where
// the anchor was, the way the first page renders "Back". A page with no
// pager, or a pager with no next item at all, says nothing either way, and
// the walk asks for one more page to find out.
func kvsListingEnds(root *html.Node) bool {
	control := kvsNextControl(root)
	return control != nil && kvsNextAnchor(control) == nil
}

// kvsListingBlock reads the id of the videos block a page renders, which is
// what an async page request has to name. A search page renders an albums
// block beside it, with an id of its own, so the block is chosen by name
// rather than by coming first.
func kvsListingBlock(root *html.Node) string {
	var first string
	for _, n := range findAll(root, func(n *html.Node) bool { return attr(n, "data-block-id") != "" }) {
		id := attr(n, "data-block-id")
		if strings.HasPrefix(id, "list_videos") {
			return id
		}
		if first == "" {
			first = id
		}
	}
	return first
}

// kvsAsyncPage builds a member listing's async page request by guess, for
// a theme whose "next" control points at "#" but carries no parameters to
// read. It is the last resort after kvsAsyncNext: the parameter it names is
// the one a member's videos page by, and on any other block the platform
// ignores it and serves page one again, which the walk reads as the end.
func kvsAsyncPage(listing, block string, page int) string {
	if block == "" {
		return ""
	}
	// A listing pasted with a query of its own (a sort order) already has
	// its "?"; a second one would fold mode=async into that query's value.
	sep := "?"
	if strings.Contains(listing, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%smode=async&function=get_block&block_id=%s&from_videos=%d",
		listing, sep, url.QueryEscape(block), page)
}

// kvsListingVideos reads the video pages a listing links to.
func kvsListingVideos(root *html.Node, base string) []string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		link := resolveRef(baseURL, attr(a, "href"))
		if link == "" {
			continue
		}
		ref, err := url.Parse(link)
		if err != nil || !strings.EqualFold(ref.Host, baseURL.Host) {
			continue
		}
		// The listing sits among the site's own furniture, so the video
		// path shape is what separates a thumbnail from a menu item — the
		// same test the direct-link sniffer uses.
		if kvsVideoPath(ref) {
			out = append(out, link)
		}
	}
	return util.Dedupe(out)
}
