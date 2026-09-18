package extractor

import (
	"context"
	"fmt"
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
// Not every listing renders one. Search results, and the member listing on
// some installs, page entirely in script: the control is there but its
// anchor points at "#", and the site's own script asks for the block
// directly — ?mode=async&function=get_block&block_id=<block>&from_videos=<n>
// — so the walk does the same. Those listings say where they end in the
// control too. The "next" item is still rendered on the last page, holding
// a span where the anchor was, the way "Back" is rendered on the first, and
// that is read as the end rather than asking for a page past it.
//
// One thing about the async form deserves a note, because it looks right and
// is not. The platform also takes a plain ?from=<n>, and on a member listing
// ignores it: every page comes back as page one, byte for byte, and a walk
// built on it stops at the first repeat having quietly collected only the
// first forty-eight of however many there are. The parameter that actually
// pages is named for the block it pages. Pages are deduplicated and the walk
// stops on one that adds nothing, so an install that ignores that one too
// ends the walk rather than looping; and the stated total — "Showing 49 - 64
// of 64" — is read as a second stop where the listing states one.

// kvsListing is one paged list of videos: where it starts, what to name the
// job after, and how to say it turned out empty.
type kvsListing struct {
	// url is the first page, and the base every later one is asked for from.
	url string
	// title names the job, given the first page.
	title func(root *html.Node) string
	// empty explains a listing with no videos on it.
	empty string
}

// kvsShowingTotal reads the "Showing 49 - 64 of 64 videos" line, which is how
// a listing states how far it goes when it states it at all.
var kvsShowingTotal = regexp.MustCompile(`Showing\s+\d+\s*-\s*\d+\s+of\s+(\d+)`)

// kvsListingResult resolves a listing into one file per video.
func kvsListingResult(ctx context.Context, client *httpx.Client, l kvsListing, label string) (*Result, error) {
	pages, title, err := kvsListingPages(ctx, client, l, label)
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("%s: %s", label, l.empty)
	}

	// Every video needs its own page read for a media URL, so they are
	// fetched several at a time and a video that will not load is skipped
	// rather than failing a listing of hundreds.
	files := FanOut(ctx, pages, func(ctx context.Context, page string) ([]File, error) {
		u, err := ParseURL(page)
		if err != nil {
			return nil, err
		}
		res, err := kvsExtract(ctx, client, u, label)
		if err != nil {
			return nil, err
		}
		return res.Files, nil
	})
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: none of the %d videos on %s could be resolved",
			label, len(pages), l.url)
	}
	return &Result{Title: title, Files: files}, nil
}

// kvsListingPages walks the listing and collects every video page it names.
func kvsListingPages(ctx context.Context, client *httpx.Client, l kvsListing, label string) ([]string, string, error) {
	var (
		pages   []string
		title   string
		total   int
		seen    = make(map[string]bool)
		visited = make(map[string]bool)
		target  = l.url
		block   string
	)

	for page := 1; page <= config.MaxAlbumPages && target != ""; page++ {
		if visited[target] {
			break // the listing pointed back at a page already walked
		}
		visited[target] = true

		doc, err := client.GetString(ctx, target, httpx.Referer(l.url))
		if err != nil {
			if page == 1 {
				return nil, "", fmt.Errorf("%s: fetch %s: %w", label, l.url, err)
			}
			break // a later page failing still leaves the earlier ones
		}
		root, err := parseHTML(doc)
		if err != nil {
			break
		}
		if page == 1 {
			title = l.title(root)
			if m := kvsShowingTotal.FindStringSubmatch(doc); m != nil {
				total, _ = strconv.Atoi(m[1])
			}
		}

		added := 0
		for _, link := range kvsListingVideos(root, l.url) {
			if seen[link] {
				continue
			}
			seen[link] = true
			pages = append(pages, link)
			added++
		}
		if added == 0 {
			break
		}
		if total > 0 && len(pages) >= total {
			break
		}
		if len(pages) >= config.MaxListingFiles {
			break
		}
		// The listing says where the next page is, and on the last one says
		// that there is none. That is the stop condition; everything above
		// is a guard against a listing that misreports itself.
		if kvsListingEnds(root) {
			break
		}
		target = kvsNextPage(root, l.url)
		if target == "" {
			if page == 1 {
				block = kvsListingBlock(root)
			}
			target = kvsAsyncPage(l.url, block, page+1)
		}
	}
	return pages, title, nil
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

// kvsAsyncPage builds the listing's own async page request, for listings
// that page entirely in script and leave the "next" control pointing at "#"
// — there is no anchor to follow on those.
//
// Note what this is not. The same platform takes a ?from=<n> on a member
// listing and ignores it, handing back page one every time; a walk built on
// that collects the first page over and over and stops thinking it is done.
// The parameter that actually pages is named for the block it pages. The
// walk deduplicates and stops on a page that adds nothing either way, so an
// install that ignores this one as well ends the walk rather than looping.
func kvsAsyncPage(listing, block string, page int) string {
	if block == "" {
		return ""
	}
	return fmt.Sprintf("%s?mode=async&function=get_block&block_id=%s&from_videos=%d",
		listing, url.QueryEscape(block), page)
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
