package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The other listings on a Kernel Video Sharing install: categories, models,
// tags, channels, playlists, and the site's own latest and top-rated lists.
//
// These are not recognised by path. The platform lets an install rename its
// sections — one names its models section /onlyfans-models/ — so a list of
// section names would trail the installs the way a host list does. They are
// recognised by shape instead: on a registered host, a URL that is neither a
// profile nor a search is fetched once, and what comes back decides. A page
// with a player is a video. A page with no player but a videos block is the
// listing it looks like, and is walked from the document already in hand. A
// page with neither is reported as it always was. The front page is refused
// outright: it carries a videos block like any listing, and what it lists is
// whatever the site chose to promote, which nobody means by pasting a domain.
//
// A pasted later page — /categories/<name>/3/ — means the whole listing, the
// way a member's section means the member, so the trailing number is dropped
// and the walk starts from the first page. That is decided only once the
// page has been fetched and found to be a listing: a video linked as
// /watch/<id>/ ends in a number too, and on an install where /watch/ is
// itself a listing, canonicalising first would have handed back the whole
// site for one pasted video. And a section whose own name is a number is
// caught by the rule, so a first page that does not exist or lists nothing
// means the number was the name, and the page is walked as pasted.

// kvsBrowse resolves a page on a registered install that is neither a member
// profile nor a search: a video, or a listing of them.
func kvsBrowse(ctx context.Context, client *httpx.Client, u *url.URL, label string) (*Result, error) {
	if len(util.PathSegments(u)) == 0 {
		return nil, fmt.Errorf("%s: %s is the front page; paste a video, a category, "+
			"a model, a tag or a search instead", label, u.Redacted())
	}
	doc, err := client.GetString(ctx, u.String(), httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return nil, fmt.Errorf("%s: fetch %s: %w", label, u.Redacted(), err)
	}
	res, err := kvsResult(doc, u, label)
	if err == nil {
		return res, nil
	}
	// Whatever is wrong with a video page — removed, private, a player this
	// does not handle — is what the error already says. A video page also
	// carries a block of related videos, which must not be walked in its
	// place, so the page's own layout decides before the block does.
	if kvsVideoPath(u) || flashvarsStart.MatchString(doc) {
		return nil, err
	}
	root, perr := parseHTML(doc)
	if perr != nil || !kvsHasListing(root) {
		return nil, err
	}

	if listing, ok := kvsPagedListing(u); ok {
		l := kvsListingForPage(listing)
		pages, title, err := kvsListingPages(ctx, client, l, label)
		switch {
		case err == nil && len(pages) > 0:
			return kvsResolvePages(ctx, client, l, pages, title, label)
		case err == nil, httpx.HasStatus(err, http.StatusNotFound, http.StatusGone):
			// Nothing at the listing's first page, or no such page: the
			// number was the section's own name.
		default:
			return nil, err
		}
	}
	l := kvsListingForPage(u.String())
	l.first = doc
	return kvsListingResult(ctx, client, l, label)
}

// kvsPagedListing reports whether a URL is a later page of a listing, and
// returns the listing's first page. A video linked by its bare id —
// /videos/<id> — ends in a number too, and is not a page of anything.
func kvsPagedListing(u *url.URL) (string, bool) {
	if kvsVideoPath(u) {
		return "", false
	}
	segs := util.PathSegments(u)
	if len(segs) < 2 {
		return "", false
	}
	last := segs[len(segs)-1]
	if n, err := strconv.Atoi(last); err != nil || n < 1 {
		return "", false
	}
	path := strings.TrimSuffix(strings.TrimSuffix(u.EscapedPath(), "/"), "/"+last)
	listing := util.Origin(u) + path + "/"
	if u.RawQuery != "" {
		listing += "?" + u.RawQuery
	}
	return listing, true
}

// kvsHasListing reports whether a page carries a videos block, which is what
// every listing on the platform renders its tiles into and names — the id
// on the container or the block id on its controls both begin the same way.
func kvsHasListing(root *html.Node) bool {
	return findFirst(root, func(n *html.Node) bool {
		return strings.HasPrefix(attr(n, "data-block-id"), "list_videos") ||
			strings.HasPrefix(attr(n, "id"), "list_videos")
	}) != nil
}

// kvsListingForPage describes any other listing to the shared walk.
func kvsListingForPage(listing string) kvsListing {
	return kvsListing{
		url:   listing,
		title: func(root *html.Node) string { return kvsListingTitle(root, listing) },
		empty: fmt.Sprintf("no videos listed at %s", listing),
	}
}

// kvsListingSuffix matches the tail of a listing heading: the sort the page
// is in, and the word "Videos" itself.
var kvsListingSuffix = regexp.MustCompile(`(?i)\s*(?:new|latest|most viewed|most popular|most commented|most favou?rited|top rated|longest|best|top)?\s*videos\s*$`)

// kvsListingTitle names a listing after what it lists. The heading reads
// "Asian New Videos" on a category, "<name>'s New Videos" on a model and
// "New Videos" on the site's own latest list: the possessive is the name,
// otherwise the sort and the word "Videos" come off, and a heading that was
// nothing but those falls back to the listing's own path.
func kvsListingTitle(root *html.Node, listing string) string {
	heading := strings.TrimSpace(firstText(root, atom.H1))
	for _, possessive := range []string{"'s ", "’s "} {
		if name, _, ok := strings.Cut(heading, possessive); ok && name != "" {
			return name
		}
	}
	if name := strings.TrimSpace(kvsListingSuffix.ReplaceAllString(heading, "")); name != "" {
		return name
	}
	if u, err := url.Parse(listing); err == nil {
		if segs := util.PathSegments(u); len(segs) > 0 {
			return segs[len(segs)-1]
		}
	}
	return heading
}
