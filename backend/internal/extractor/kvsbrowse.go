// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"errors"
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
// recognised by shape instead: on a recognised install, a URL that is neither a
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
func kvsBrowse(ctx context.Context, client *httpx.Client, u *url.URL, limit int) (*Result, error) {
	if len(util.PathSegments(u)) == 0 {
		return nil, fmt.Errorf("%s: %s is the front page; paste a video, a category, "+
			"a model, a tag or a search instead", kvsName(u), u.Redacted())
	}
	d, err := readDocument(ctx, client, u, httpx.Referer(util.Origin(u)+"/"))
	if err != nil {
		return nil, fmt.Errorf("%s: fetch %s: %w", kvsName(u), u.Redacted(), err)
	}
	doc := d.body
	res, err := kvsResult(doc, u)
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
	root, perr := d.parse()
	if perr != nil || !kvsHasListing(root) {
		return nil, err
	}

	if listing, ok := kvsPagedListing(u); ok {
		l := kvsListingForPage(listing)
		l.limit = limit
		res, err := kvsListingResult(ctx, client, l)
		switch {
		case err == nil:
			return res, nil
		case errors.As(err, new(kvsEmpty)), httpx.HasStatus(err, http.StatusNotFound, http.StatusGone):
			// Nothing at the listing's first page, or no such page: the
			// number was the section's own name.
		default:
			return nil, err
		}
	}
	// The page in hand is the listing's first page, so the walk starts from
	// it, its text and its tree both, rather than asking or parsing again.
	// A canonical first page found above was a request of its own.
	l := kvsListingForPage(u.String())
	l.first = d
	l.limit = limit
	return kvsListingResult(ctx, client, l)
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

// A member's public videos on a Kernel Video Sharing install.
//
// The platform lists them at /members/<id>/public_videos/, forty-eight at a
// time, and pages them at /public_videos/<n>/. The walk is the one every
// listing on the platform shares, in kvslisting.go; what is here is what
// makes a profile a profile — which URLs name one, which section of it is
// the member's own work, and what to call the job.
//
// The page is recognised as KVS by its player or its listing blocks before
// any of this runs. A /members/ path by itself is too ordinary to establish
// which platform, or whose listing, it addresses.

// kvsMemberSection is the listing this falls back to. A profile has several
// — favourites, albums, friends — and only a member's own videos are
// unambiguously theirs and reachable without an account.
const kvsMemberSection = "public_videos"

// kvsMemberSections are the names installs give that same listing. Which one
// exists varies, and guessing wrong is not a 404: this platform answers an
// unknown member section with a generic block of the site's newest videos,
// served under the member's own URL. That reads as a perfectly good listing
// and quietly downloads somebody else's work, so a section the caller
// actually named is kept rather than rewritten.
var kvsMemberSections = map[string]bool{
	"public_videos":   true,
	"videos":          true,
	"uploaded_videos": true,
}

// kvsMemberPath reports whether a URL names a member profile, and returns the
// canonical public-videos listing for it.
//
// Any section is accepted and redirected to the public videos: someone who
// pastes a profile, or its wall, or its favourites, means "this person's
// videos", and the sections that are not that are either unreachable without
// an account or not the member's own work.
func kvsMemberPath(u *url.URL) (listing string, ok bool) {
	segs := util.PathSegments(u)
	if len(segs) < 2 || segs[0] != "members" {
		return "", false
	}
	if _, err := strconv.Atoi(segs[1]); err != nil {
		return "", false
	}
	section := kvsMemberSection
	if len(segs) >= 3 && kvsMemberSections[strings.ToLower(segs[2])] {
		section = strings.ToLower(segs[2])
	}
	return fmt.Sprintf("%s/members/%s/%s/", util.Origin(u), segs[1], section), true
}

// kvsListingForMember describes a profile's videos to the shared walk.
func kvsListingForMember(listing string) kvsListing {
	return kvsListing{
		url:   listing,
		title: kvsMemberTitle,
		empty: fmt.Sprintf("no public videos on %s (the member may have none, or may keep them private)", listing),
	}
}

// kvsMemberTitle names the job after the member, which is what a folder full
// of their videos should be called.
func kvsMemberTitle(root *html.Node) string {
	heading := strings.TrimSpace(firstText(root, atom.H2))
	// The heading reads "<name>'s Public Videos"; the possessive is the
	// name, and the rest is a section label nobody wants in a folder name.
	if name, _, ok := strings.Cut(heading, "'s "); ok && name != "" {
		return name
	}
	return cmp.Or(heading, trimSiteSuffix(firstText(root, atomTitle)))
}

// Search results on a Kernel Video Sharing install.
//
// The platform lays a search out as /search/<query>/, with the query kept in
// the path, and pages the results in script: there is no /search/<query>/2/
// to fetch, that path is a 404, so the walk in kvslisting.go asks for the
// block the way the site's own script does. Past the last page the block
// request is a 404 as well, where a member listing serves page one again;
// the walk survives either, but the last page announces itself in its pager
// and the walk reads that rather than finding out. Nothing about the results
// is particular to searching. The tiles link to ordinary video pages, which
// are resolved exactly as a pasted one would be — and a page also lists the
// albums matching the query, in a block with a pager of its own, which is
// why both are picked by name in the walk rather than by coming first.
//
// Like a member's profile this is only offered on a page recognised as
// KVS: /search/ is every site's URL shape, not this platform's.

// kvsSearchPath reports whether a URL is a search on the install, and returns
// the query and the canonical first page of its results.
//
// A pasted later page, on a theme that pages by path, means the whole search
// the way a member's section means the member, so anything after the query
// is dropped. The query keeps the escaping it arrived with: the site's own
// links write a plus as %2B and a space as a hyphen, and re-encoding a
// decoded segment would hand the platform a literal plus, which its routing
// may well read as a space. The form-style /search/?q=<query> is accepted
// too, since that is what a theme's search box submits before the redirect.
func kvsSearchPath(u *url.URL) (query, listing string, ok bool) {
	var segs []string
	for s := range strings.SplitSeq(u.EscapedPath(), "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 || !strings.EqualFold(segs[0], "search") {
		return "", "", false
	}
	var raw string
	switch {
	case len(segs) >= 2:
		raw = segs[1]
	case u.Query().Get("q") != "":
		raw = url.PathEscape(strings.TrimSpace(u.Query().Get("q")))
	}
	query, err := url.PathUnescape(raw)
	if err != nil || strings.TrimSpace(query) == "" {
		return "", "", false
	}
	return strings.TrimSpace(query), util.Origin(u) + "/search/" + raw + "/", true
}

// kvsListingForSearch describes a search's results to the shared walk. The
// job is named for the query, which is what a folder of its results is
// about; the page's own heading only restates it inside the theme's wording.
func kvsListingForSearch(query, listing string) kvsListing {
	return kvsListing{
		url:   listing,
		title: func(*html.Node) string { return query },
		empty: fmt.Sprintf("no videos match the search at %s", listing),
	}
}
