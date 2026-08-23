package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ImagePond resolves its viewer pages (/i/<code>).
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
// Profile pages are not handled: they render their listing client-side, so
// the delivered HTML names none of the items.
type ImagePond struct {
	client *httpx.Client
}

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
func NewImagePond(client *httpx.Client) *ImagePond { return &ImagePond{client: client} }

func (i *ImagePond) Name() string { return "imagepond" }

func (i *ImagePond) Match(u *url.URL) bool { return util.HostMatches(u.Host, "imagepond.net") }

// Extract resolves one viewer page to the file behind it.
func (i *ImagePond) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 2 || segs[0] != "i" {
		return nil, fmt.Errorf("imagepond: %s is not an item page (/i/<code>) — "+
			"profile pages list their items client-side, so there is nothing in them to read",
			u.Redacted())
	}

	doc, err := i.client.GetString(ctx, u.String(), httpx.Referer(imagePondRoot+"/"))
	if err != nil {
		return nil, fmt.Errorf("imagepond: fetch %s: %w", u.Redacted(), err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("imagepond: parse %s: %w", u.Redacted(), err)
	}

	link := imagePondMediaLink(root, u)
	if link == "" {
		return nil, fmt.Errorf("imagepond: no media on %s "+
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
	name := util.FirstNonEmpty(
		imagePondName(metaContent(root, "og:title")),
		fromLink,
		segs[1],
	)
	return &Result{Title: name, Files: []File{{
		Name:    name,
		URL:     link,
		Size:    -1,
		Headers: httpx.Referer(imagePondRoot + "/"),
	}}}, nil
}

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
	return util.FirstNonEmpty(
		imagePondUsable(resolveRef(base, metaContent(root, "og:video"))),
		imagePondUsable(resolveRef(base, metaContent(root, "og:image"))),
	)
}

// imagePondUsable keeps a link the downloader can fetch: the stored file on
// the media host, or the site's own route to it.
func imagePondUsable(link string) string {
	return util.FirstNonEmpty(imagePondHosted(link), imagePondDirect(link))
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
