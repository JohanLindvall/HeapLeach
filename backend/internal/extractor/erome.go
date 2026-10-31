// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Erome resolves https://www.erome.com/a/<id> albums.
//
// The album page carries every media URL inline; the only trick is that the
// CDN checks Referer, and that each item is rendered twice (once for the
// inline player, once for the lightbox), so the list needs de-duplicating.
type Erome struct {
	about
	client *httpx.Client
}

// eromeReferer must be the bare origin — the CDN rejects a deeper path.
const eromeReferer = "https://www.erome.com"

// NewErome builds the erome extractor.
func NewErome(client *httpx.Client) *Erome {
	return &Erome{client: client, about: about{name: "erome", shape: eromeAlbumShape,
		recognises: "albums (`/a/<id>`) and profiles (`/<user>`) whose page carries the site's album markup " +
			"(the album title heading and media group, or a profile's album links)"}}
}

// eromeAlbumShape is an album link, /a/<id>. A profile is a bare /<user>,
// which is every site's navigation too, so it is no shape: a profile is
// recognised by its page alone.
var eromeAlbumShape = pathShape(`^/a/[A-Za-z0-9]+/?$`)

// Recognise takes an album page carrying the markup album reads — the
// album-title heading over the media group — or a profile page listing
// album links in the site's own markup, under the user-info block.
func (e *Erome) Recognise(_ context.Context, p *Page) bool {
	segs := p.Segments()
	switch {
	case eromeAlbumShape(p.Final):
		return p.Has(`class="album-title-page"`, `class="media-group"`)
	case len(segs) == 1 && !eromeSections[strings.ToLower(segs[0])]:
		return p.Has(`user-info`, `class="album-link"`) && p.Named("EroMe", "Erome")
	}
	return false
}

// eromeSections are the paths that are not usernames, so a profile is never
// confused with one of the site's own pages.
var eromeSections = map[string]bool{
	"a": true, "search": true, "login": true, "register": true, "signup": true,
	"tags": true, "categories": true, "explore": true, "popular": true,
	"latest": true, "users": true, "terms": true, "privacy": true,
	"dmca": true, "faq": true, "contact": true, "settings": true, "upload": true,
}

// Extract handles an album (/a/<id>) or a whole profile (/<user>).
func (e *Erome) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	switch {
	case len(segs) >= 2 && segs[0] == "a":
		return e.album(ctx, u)
	case len(segs) == 1 && !eromeSections[strings.ToLower(segs[0])]:
		return e.profile(ctx, u, opts)
	default:
		return nil, fmt.Errorf("erome: %s is neither an album nor a profile", u.Redacted())
	}
}

// profile walks a user's pages and expands every album they list, filing
// each album's media into its own folder.
func (e *Erome) profile(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	albums, title, note, err := e.profileAlbums(ctx, u, opts)
	if err != nil {
		return nil, err
	}

	if len(albums) == 0 {
		return nil, fmt.Errorf("erome: no albums on profile %s", u.Redacted())
	}

	// Every album needs its own page read for the media it holds, and a
	// profile runs to hundreds; one after another leaves the job resolving
	// for minutes before a byte moves. The expansion keeps the profile's
	// own order however the requests interleave, stops at the file cap, and
	// skips an unavailable album rather than sinking the rest, its reason
	// kept for when no album held anything.
	expanded := expandSourcesNamed(ctx, albums, opts, opening(e.album), eromeFolders)
	if err := expanded.unresolved(ctx, "erome", "albums", "profile "+u.Redacted()); err != nil {
		return nil, err
	}
	return &Result{Title: title, Files: expanded.files, Note: expanded.note(note, sourceWords{noun: "albums"})}, nil
}

// profileAlbums walks a profile's pages for the albums they list, up to the
// listing cap, since every album costs a request of its own, and reports
// what the walk learned along the way: the profile's own name, and the note
// for a listing cut short rather than ended.
//
// Kept apart from the expansion so a fixture can stand in for the pages —
// the expansion fetches an album apiece, which a test of the walk has no
// business doing.
func (e *Erome) profileAlbums(ctx context.Context, u *url.URL, opts Options) (albums []string, title, note string, err error) {
	headers := httpx.Referer(eromeReferer + "/")
	first := eromeProfilePage(u, 1)
	root, _, err := fetchRoot(ctx, e.client, &first, headers)
	if err != nil {
		return nil, "", "", fmt.Errorf("erome: fetch profile: %w", err)
	}

	// The pages are numbered rather than linked, and the walk asks for one
	// more until a page adds nothing or will not load.
	seen := make(map[string]bool)
	page := 1
	limit, limitNote := listingLimit(opts)
	found, title, note, err := walkListing(ctx, e.client, &first, listingWalk{
		name: "erome", title: strings.Join(util.PathSegments(u), "-"), quiet: true, root: root,
		limit: limit, limitNote: limitNote,
		// A page that will not load is ordinarily the end of the listing:
		// asking past the last one is how the walk learns where to stop. A
		// rate limit is not that. The host has said come back, the client
		// has already waited it out as far as it will go, and reading it as
		// the end would hand back part of a profile as though it were the
		// whole of it. It is a note rather than part of the title because
		// the title names the folder: a partial run filed under "(partial —
		// rate limited)" and the complete run after it under the plain name
		// was every file downloaded twice.
		failed: func(err error) (string, bool) {
			if httpx.HasStatus(err, http.StatusTooManyRequests) {
				return "partial — rate limited", true
			}
			return "", true
		},
		fetch: fetchAsAsked(e.client, headers),
		read: func(root *html.Node, base, _ *url.URL) listingPage {
			p := listingPage{title: cmp.Or(firstText(root, atom.H1), trimSiteSuffix(firstText(root, atomTitle)))}
			for _, link := range eromeAlbumLinks(root, base, seen) {
				p.links = append(p.links, listingLink{url: link})
			}
			page++
			next := eromeProfilePage(u, page)
			p.next = &next
			return p
		},
	})
	if err != nil {
		return nil, "", "", err
	}
	return listingURLs(found), title, note, nil
}

// eromeFolders names the folder each album's files are filed under.
//
// The album's own title is what a person recognises, so that is the name.
// But a title is not unique within a profile — a creator posting a series
// gives every part the same one, and reposts carry whatever title they were
// given elsewhere — and two albums sharing a folder interleave their files
// and report the profile as holding one album fewer than it does. So where a
// title repeats, every album carrying it takes its own id alongside: the site
// mints those, they are unique, and they never change.
//
// Only the repeats are qualified. An album whose title is its own keeps
// exactly the folder it has always had, so a re-run skips what it already
// holds rather than fetching it again into a renamed one. These names
// predate sourceFolders, whose "[id]" form and case-blind count would
// rename the repeats, so the host keeps its own.
func eromeFolders(albums []sourceResult) []string {
	seen := make(map[string]int, len(albums))
	for _, album := range albums {
		seen[album.res.Title]++
	}

	names := make([]string, len(albums))
	for i, album := range albums {
		// isEromeAlbum has already established the /a/<id> shape, and the
		// link parsed to be resolved at all, so the id is there to be read.
		u, _ := ParseURL(album.link)
		title, id := album.res.Title, util.PathSegments(u)[1]
		switch {
		case title == "":
			// Nothing to disambiguate against, and the id is already unique.
			names[i] = id
		case seen[title] > 1:
			names[i] = sourceFolder(title + " " + id)
		default:
			names[i] = sourceFolder(title)
		}
	}
	return names
}

// eromeProfilePage is the URL of one page of a profile.
//
// Whatever the profile URL already carried is kept, and on this host that is
// load-bearing rather than tidiness: a profile lists its own albums under
// ?t=posts and the ones it has reposted under ?t=reposts, which are
// different sets. A page number appended without the tab would walk the
// default listing from page two onward — collecting the wrong albums, and
// silently, since they are albums either way.
func eromeProfilePage(u *url.URL, page int) url.URL {
	next := *u
	if page > 1 {
		query := next.Query()
		query.Set("page", strconv.Itoa(page))
		next.RawQuery = query.Encode()
	}
	return next
}

// eromeAlbumLinks reads the album links one page of a profile lists, in the
// order the page gives them and skipping any already collected.
//
// Kept apart from the fetch so a fixture can stand in for the page. The
// profile's own furniture links out to the site's sections and to other
// creators, so what makes a link an album is its shape rather than where it
// sits in the markup.
func eromeAlbumLinks(root *html.Node, base *url.URL, seen map[string]bool) []string {
	var out []string
	for _, anchor := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		href := resolveRef(base, attr(anchor, "href"))
		if !isEromeAlbum(href) || seen[href] {
			continue
		}
		seen[href] = true
		out = append(out, href)
	}
	return out
}

// isEromeAlbum reports whether a link points at an album page.
func isEromeAlbum(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !util.HostMatches(u.Host, "erome.com") {
		return false
	}
	segs := util.PathSegments(u)
	return len(segs) == 2 && segs[0] == "a"
}

// album lists every video and image in one album.
func (e *Erome) album(ctx context.Context, u *url.URL) (*Result, error) {
	doc, _, err := fetchDocument(ctx, e.client, u, httpx.Referer(eromeReferer+"/"))
	if err != nil {
		// A deleted album is answered with 410 and a full page of the
		// site's own furniture, which the status error would quote 200
		// characters of. The code already says everything there is to
		// know, so it is said instead of shown.
		if httpx.HasStatus(err, http.StatusGone) {
			return nil, fmt.Errorf("erome: the album %s has been deleted", u.Redacted())
		}
		return nil, fmt.Errorf("erome: fetch album: %w", err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("erome: %w", err)
	}

	title := cmp.Or(
		textOfFirst(root, func(n *html.Node) bool {
			return isElem(n, atom.H1) && hasClass(n, "album-title-page")
		}),
		metaContent(root, "og:title"),
		trimSiteSuffix(firstText(root, atomTitle)),
	)

	var urls []string
	walk(root, func(n *html.Node) {
		switch {
		case isElem(n, atom.Source):
			urls = append(urls, resolveRef(u, attr(n, "src")))
		case isElem(n, atom.Video):
			// Some albums put the URL on the element itself.
			urls = append(urls, resolveRef(u, attr(n, "src")))
		case isElem(n, atom.Img):
			// Full-resolution images are lazy-loaded via data-src; the
			// eager src is a thumbnail, so prefer data-src when present.
			if v := attr(n, "data-src"); v != "" {
				urls = append(urls, resolveRef(u, v))
			} else if hasAnyClass(n, "img-front", "img-back") {
				urls = append(urls, resolveRef(u, attr(n, "src")))
			}
		}
	})

	files := make([]File, 0, len(urls))
	for _, raw := range util.Dedupe(urls) {
		if !isEromeMedia(raw) {
			continue
		}
		files = append(files, linkFile(raw, httpx.Referer(eromeReferer)))
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("erome: no media found in %s (album may be private or removed)", u.Redacted())
	}
	return &Result{Title: title, Files: files}, nil
}

// isEromeMedia filters out sprites, icons and poster thumbnails.
func isEromeMedia(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	name := strings.ToLower(util.NameFromURL(raw))
	if name == "" || strings.HasPrefix(name, "thumb") {
		return false
	}
	for _, ext := range []string{".mp4", ".m4v", ".webm", ".mov", ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}
