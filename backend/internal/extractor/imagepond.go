// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// imagepond resolves its viewer pages (/i/<code>).
//
// The media link is on the page, and the player element is what carries the
// truth: its data-src is the file and its data-type is the real content
// type. The page's own metadata is close but not reliable — for a video,
// og:image is the poster frame rather than the file, and og:video:type
// claimed video/mp4 for a QuickTime file — so the element is read first and
// the metadata is only the fallback.
//
// Two smaller things this host does its own way. The viewer accepts both the
// short code and a slug of the title, and answers identically to either. And
// its og:title is form-encoded, spaces written as "+", so the filename has to
// be decoded rather than used as it stands.
//
// A video page has since grown a second shape, and the two are handled
// together rather than one replacing the other, because both are still
// served: where the older one embeds a <video data-src> naming the stored
// file, the newer one renders its player client-side and the delivered HTML
// carries no player element at all. What it does carry is og:video naming
// the site's own /i/<code>/direct route, which answers with a redirect to
// that same stored file. See imagePondDirect.
//
// A link to the media host itself — media.imagepond.net/… — is the stored
// file, and is taken as given when it reaches Extract; through the registry
// such a link is a plain file and the direct fallback downloads it.
//
// Albums (/a/<code>) are served whole and server-side, so one fetch names
// every item. What that page does *not* carry is the media: each card links
// only to the item's own viewer, so the file behind it is resolved per item,
// at download time. See imagePondAlbum.
//
// Profile pages are still not handled: those render their listing
// client-side, so the delivered HTML names none of the items.

const imagePondRoot = "https://www.imagepond.net"

// imagePondMedia is the host the files themselves come from. It is also what
// separates a media element from the page's own furniture.
const imagePondMedia = "media.imagepond.net"

const (
	// imagePondDomain covers the site and its media host alike, which is
	// what a link has to be under to be worth following at all.
	imagePondDomain = "imagepond.net"

	// imagePondDirectSegment ends the site's own download route,
	// /i/<code>/direct.
	imagePondDirectSegment = "direct"
)

// NewImagePond builds the imagepond extractor.
func NewImagePond(client *httpx.Client) *Share { return newShare(client, &imagePondSite) }

// imagePondItemShape is a viewer page, /i/<code>; imagePondAlbumShape an
// album, /a/<code>.
var (
	imagePondItemShape  = pathShape(`^/i/[^/]+/?$`)
	imagePondAlbumShape = pathShape(`^/a/[^/]+/?$`)

	// imagePondMediaPath is a stored file's path on the media host, or the
	// site's /i/<code>/direct route to one.
	imagePondMediaPath = regexp.MustCompile(`^/media/(images|videos)/[^/]+$|^/i/[^/]+/direct$`)
)

// imagePondSite reads the places extract takes the file from — the player
// element, then og:video and og:image — for a link of the stored file's
// form, and an album's cards for a link to a viewer page. The site's name is
// required beside them, since a viewer path and a /media/ path are common.
var imagePondSite = shareSite{
	name: "imagepond",
	recognises: "viewer pages (`/i/<code>`) naming their stored file (`/media/…`, `/i/<code>/direct`) in the player " +
		"or og:video/og:image, and albums (`/a/<code>`) of cards linking to viewer pages, on pages named ImagePond",
	forms: []shareForm{
		{imagePondItemShape, func(p *Page) bool {
			media := func(raw string) bool {
				u, err := url.Parse(resolveRef(p.Final, raw))
				return err == nil && imagePondMediaPath.MatchString(u.Path)
			}
			if !p.Named("ImagePond") {
				return false
			}
			return media(p.Meta("og:video")) || media(p.Meta("og:image")) || p.hasElement(func(n *html.Node) bool {
				return (isElem(n, atom.Video) || isElem(n, atom.Source)) &&
					(media(attr(n, "data-src")) || media(attr(n, "src")))
			})
		}},
		{imagePondAlbumShape, func(p *Page) bool {
			return p.Named("ImagePond") && p.hasElement(func(a *html.Node) bool {
				return isElem(a, atom.A) && imagePondCardLink(a, p.Final, func(link string) bool {
					u, err := url.Parse(link)
					return err == nil && imagePondItemShape(u)
				}) != ""
			})
		}},
	},
	extract: imagePondExtract,
}

// imagePondExtract resolves a viewer page (/i/<code>), an album (/a/<code>),
// or a link to the stored file itself.
func imagePondExtract(ctx context.Context, s *Share, u *url.URL, _ Options) (*Result, error) {
	// The media host serves the files the pages point at, and Match claims
	// it along with the site — one is a subdomain of the other. A link to it
	// is already the answer the other two routes go looking for, so it is
	// taken as it stands rather than rejected for not being a page.
	if link := imagePondMediaFile(u); link != "" {
		name := cmp.Or(util.NameFromURL(link), "imagepond-file")
		return &Result{Title: name, Files: []File{{
			Name: name,
			URL:  link,
			Size: -1,
			// Not required — the host serves these to anyone — but sent for
			// the same reason every other file here carries it, so one host
			// tightening up later does not become a puzzle.
			Headers: httpx.Referer(imagePondRoot + "/"),
		}}}, nil
	}

	segs := util.PathSegments(u)
	switch {
	case len(segs) >= 2 && segs[0] == "a":
		return imagePondAlbum(ctx, s, u, segs[1])
	case len(segs) >= 2 && segs[0] == "i":
		target, err := imagePondItem(s, u.String(), segs[1])(ctx)
		if err != nil {
			return nil, err
		}
		return &Result{Title: target.Name, Files: []File{target.file("")}}, nil
	}
	return nil, fmt.Errorf("imagepond: %s is not an item page (/i/<code>), an album "+
		"(/a/<code>), or a file on %s — profile pages list their items client-side, "+
		"so there is nothing in them to read", u.Redacted(), imagePondMedia)
}

// imagePondMediaFile keeps a link that already addresses a stored file.
//
// Unlike imagePondHosted, which reads links out of a page and has to tell the
// file from the poster frame generated beside it, this one is given the link
// by the user: a thumbnail asked for by name is a thumbnail wanted, so
// nothing is filtered but the host and the absence of a filename.
func imagePondMediaFile(u *url.URL) string {
	if !util.HostMatches(u.Host, imagePondMedia) {
		return ""
	}
	if util.NameFromURL(u.String()) == "" {
		return ""
	}
	return u.String()
}

// imagePondMedia fetches one viewer page and reports the file it shows, with
// the name the page states.
func imagePondShown(ctx context.Context, s *Share, u *url.URL, code string) (link, name string, err error) {
	root, err := s.root(ctx, u, imagePondRoot+"/")
	if err != nil {
		return "", "", err
	}

	link = imagePondMediaLink(root, u)
	if link == "" {
		// An item the host has aged out still answers 200, with a page
		// carrying no media at all — indistinguishable from a layout change
		// unless the notice on it is read. Saying which it is matters most
		// in an album, where a dozen expired items would otherwise all
		// report that the parser might be broken.
		if imagePondExpired(root) {
			return "", "", fmt.Errorf("imagepond: %s has expired — the host says "+
				"this item is no longer available", u.Redacted())
		}
		return "", "", fmt.Errorf("imagepond: no media on %s "+
			"(the item may have been removed, or the page layout changed)", u.Redacted())
	}

	// The link's own last segment names the file for a media-host URL and
	// says nothing for the /direct route, which would otherwise save every
	// video as "direct". og:title is what names those, and it is read first
	// regardless.
	fromLink := util.NameFromURL(link)
	if fromLink == imagePondDirectSegment {
		fromLink = ""
	}
	name = cmp.Or(
		imagePondName(metaContent(root, "og:title")),
		fromLink,
		code,
	)
	return link, name, nil
}

// imagePondAlbum lists what one album page links to, resolving each item lazily.
//
// The whole album arrives in one document: there are no paging controls, and
// asking for ?page=2 serves a listing with nothing in it, so this fetches
// once. If a very large album ever turns out to be cut short, that is the
// assumption to revisit.
//
// An album may list items the host has since aged out; those resolve to a
// stated expiry rather than to a file, and fail one by one with the rest of
// the album downloading around them. Filtering them out here instead would
// mean fetching every item page before the first byte moved, to save nothing.
//
// The cards carry names but no media, so each file gets a resolver rather
// than a URL. That is not the signed-link case bunkr has — these links do not
// expire — it is simply that the media address is one fetch further on, and
// doing that fetch per item at download time beats doing every one of them
// before the first byte moves.
func imagePondAlbum(ctx context.Context, s *Share, u *url.URL, code string) (*Result, error) {
	root, err := s.root(ctx, u, imagePondRoot+"/")
	if err != nil {
		return nil, err
	}

	files := imagePondAlbumFiles(root, u, func(page, code string) func(context.Context) (*Target, error) {
		return imagePondItem(s, page, code)
	})
	if len(files) == 0 {
		return nil, fmt.Errorf("imagepond: no items on album %s "+
			"(it may be empty or private, or the page layout changed)", u.Redacted())
	}

	title := cmp.Or(
		strings.TrimSpace(firstText(root, atom.H1)),
		trimSiteSuffix(firstText(root, atom.Title)),
		code,
	)
	return &Result{Title: title, Files: files}, nil
}

// imagePondItem reads one viewer page into the file behind it: the
// single-item route, and an album entry's resolver at download time, which
// need exactly the same hop.
func imagePondItem(s *Share, page, code string) func(context.Context) (*Target, error) {
	return func(ctx context.Context) (*Target, error) {
		u, err := ParseURL(page)
		if err != nil {
			return nil, err
		}
		link, name, err := imagePondShown(ctx, s, u, code)
		if err != nil {
			return nil, err
		}
		return &Target{
			URL: link,
			// The viewer page states the stored filename, which is the
			// authority; an album card's copy of it is what the queue shows
			// until then. For a video reached by the /direct route it is the
			// only name there is, since that URL ends in "direct".
			Name:    name,
			Size:    -1,
			Headers: httpx.Referer(imagePondRoot + "/"),
		}, nil
	}
}

// imagePondAlbumFiles reads one album listing.
//
// Each card is an anchor wrapping a thumbnail and the stored filename, and
// the filename is taken from the anchor's own text rather than from the
// element carrying it: that element is identified only by a run of utility
// classes, which are the most churn-prone thing on the page, while the
// anchor holds no other text.
func imagePondAlbumFiles(root *html.Node, base *url.URL,
	resolver func(page, code string) func(context.Context) (*Target, error)) []File {

	var files []File
	seen := make(map[string]bool)
	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		page := imagePondCardLink(a, base, imagePondItemPage)
		if page == "" || seen[page] {
			continue
		}
		seen[page] = true

		code := linkPathID(page, "i")
		files = append(files, File{
			Name:    cmp.Or(collapseSpace(textOf(a)), code),
			Size:    -1,
			Resolve: resolver(page, code),
		})
	}
	return files
}

// imagePondCardLink pulls an item page out of an album card's link.
//
// The href is an Alpine expression rather than an address —
// `manageMode ? 'javascript:void(0)' : 'https://…/i/<code>'` — because the
// same card becomes a checkbox in the album owner's manage mode. So the
// quoted operands are examined and the one naming an item page is taken,
// which also ignores the `javascript:void(0)` beside it. A plain href is
// still honoured, for the day the template stops doing this. keep says
// which resolved candidate is an item page.
func imagePondCardLink(a *html.Node, base *url.URL, keep func(link string) bool) string {
	for _, raw := range []string{attr(a, ":href"), attr(a, "href")} {
		candidates := []string{raw}
		for _, m := range imagePondQuoted.FindAllStringSubmatch(raw, -1) {
			candidates = append(candidates, m[1])
		}
		for _, c := range candidates {
			if link := resolveRef(base, c); link != "" && keep(link) {
				return link
			}
		}
	}
	return ""
}

// imagePondQuoted picks the single-quoted operands out of that expression.
var imagePondQuoted = regexp.MustCompile(`'([^']*)'`)

// imagePondItemPage reports a link that addresses a viewer page on this
// host, which is what separates an album card from the site's own furniture
// and from the /i/<code>/direct route.
func imagePondItemPage(link string) bool {
	u, err := url.Parse(link)
	return err == nil && util.HostMatches(u.Host, imagePondDomain) && linkPathID(link, "i") != ""
}

// imagePondExpired recognises the notice the host serves for an item it has
// aged out. It answers 200 rather than 404 or 410, so the page's own words
// are the only thing that tells the two apart; the heading is checked as
// well as the title because a theme is likelier to restyle one than to
// reword both.
func imagePondExpired(root *html.Node) bool {
	for _, text := range []string{firstText(root, atom.H1), firstText(root, atom.Title)} {
		if strings.Contains(strings.ToLower(text), "expired") {
			return true
		}
	}
	return false
}

// collapseSpace folds the whitespace a template leaves around a value.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// imagePondMediaLink finds the file the page is showing.
//
// The metadata is what identifies the item — the page's own images, its logo
// and avatars among them, are served from the media host too, so scanning
// the markup for a hosted image finds furniture as readily as the file. What
// the markup is needed for is the video case, where the metadata names the
// poster frame under og:image and the player element carries the file.
//
// Hence: the player element, then og:video, then og:image. A page offering
// none of those is reported rather than guessed at.
//
// That order is unchanged by the newer page shape; only what counts as a
// usable link has widened, since a page rendering its player client-side
// names the /direct route under og:video where the older one named the
// stored file. An image page is unaffected either way: it carries no
// og:video, so its og:image on the media host still wins.
func imagePondMediaLink(root *html.Node, base *url.URL) string {
	for _, n := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.Video) || isElem(n, atom.Source)
	}) {
		for _, key := range []string{"data-src", "src"} {
			if link := imagePondUsable(resolveRef(base, attr(n, key))); link != "" {
				return link
			}
		}
	}
	return cmp.Or(
		imagePondUsable(resolveRef(base, metaContent(root, "og:video"))),
		imagePondUsable(resolveRef(base, metaContent(root, "og:image"))),
	)
}

// imagePondUsable keeps a link the downloader can fetch: the stored file on
// the media host, or the site's own route to it.
func imagePondUsable(link string) string {
	return cmp.Or(imagePondHosted(link), imagePondDirect(link))
}

// imagePondDirect keeps the site's own download route, /i/<code>/direct.
//
// It sits on the site host rather than the media host, so the check above
// will not take it — and for a video page that renders its player
// client-side it is the only usable thing on the page: there is no <video>
// element to read, and the og:image beside it is the poster frame. The route
// answers with a redirect to the stored file, which the client follows, and
// what it lands on honours ranges like any other media link.
//
// It carries no token and no expiry, so it goes on the File as a plain URL
// rather than behind a resolver — the same judgement suvobox's ?raw=1 gets,
// and for the same reason: a signed link would go stale while the item
// waited its turn, and this one cannot.
func imagePondDirect(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || !util.HostMatches(u.Host, imagePondDomain) {
		return ""
	}
	segs := util.PathSegments(u)
	if len(segs) != 3 || segs[0] != "i" || segs[2] != imagePondDirectSegment {
		return ""
	}
	return link
}

// imagePondHosted keeps a link only when the media host serves it, and only
// when it is the file rather than the poster frame generated beside it.
func imagePondHosted(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || !util.HostMatches(u.Host, imagePondMedia) {
		return ""
	}
	name := util.NameFromURL(link)
	if name == "" || strings.Contains(name, "_thumb.") {
		return ""
	}
	return link
}

// imagePondName decodes the filename the page states, which is written as a
// form-encoded value: spaces arrive as "+".
func imagePondName(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	if decoded, err := url.QueryUnescape(title); err == nil {
		return strings.TrimSpace(decoded)
	}
	return title
}
