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
// Profile pages are not handled: they render their listing client-side, so
// the delivered HTML names none of the items.
type ImagePond struct {
	client *httpx.Client
}

const imagePondRoot = "https://www.imagepond.net"

// imagePondMedia is the host the files themselves come from. It is also what
// separates a media element from the page's own furniture.
const imagePondMedia = "media.imagepond.net"

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

	name := util.FirstNonEmpty(
		imagePondName(metaContent(root, "og:title")),
		util.NameFromURL(link),
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
func imagePondMediaLink(root *html.Node, base *url.URL) string {
	for _, n := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.Video) || isElem(n, atom.Source)
	}) {
		for _, key := range []string{"data-src", "src"} {
			if link := imagePondHosted(resolveRef(base, attr(n, key))); link != "" {
				return link
			}
		}
	}
	return util.FirstNonEmpty(
		imagePondHosted(resolveRef(base, metaContent(root, "og:video"))),
		imagePondHosted(resolveRef(base, metaContent(root, "og:image"))),
	)
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
