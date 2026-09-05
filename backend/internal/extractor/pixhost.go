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

// Pixhost resolves galleries and single images.
//
// A gallery page carries a thumbnail per image, and a thumbnail's link says
// where the full image is: the two differ only by host prefix and one path
// segment. Deriving the images from the thumbnails resolves a gallery of
// fifty in one request, where following each viewer page would take fifty —
// and if the site ever changes that mapping the downloads fail visibly with
// a 404 rather than quietly fetching the wrong thing.
//
// Single viewer pages take the other route and read the image straight off
// the page, because there is no saving to be had from one request either way.
type Pixhost struct {
	hostSet
	client *httpx.Client
}

const pixhostRoot = "https://pixhost.to"

// pixhostImageID marks the full-size image on a viewer page.
const pixhostImageID = "image"

// NewPixhost builds the pixhost extractor.
func NewPixhost(client *httpx.Client) *Pixhost {
	return &Pixhost{hostSet: hostSet{"pixhost.to"}, client: client}
}

func (p *Pixhost) Name() string { return "pixhost" }

// Extract resolves a gallery or a single image.
func (p *Pixhost) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return nil, fmt.Errorf("pixhost: %s names no gallery or image", u.Redacted())
	}

	doc, err := p.client.GetString(ctx, u.String(), httpx.Referer(pixhostRoot+"/"))
	if err != nil {
		return nil, fmt.Errorf("pixhost: fetch %s: %w", u.Redacted(), err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("pixhost: parse %s: %w", u.Redacted(), err)
	}

	switch segs[0] {
	case "gallery":
		return pixhostGallery(root, u)
	case "show":
		return pixhostImage(root, u)
	}
	return nil, fmt.Errorf("pixhost: %s is neither a gallery (/gallery/<code>) "+
		"nor an image (/show/<group>/<file>)", u.Redacted())
}

// pixhostGallery reads every image out of a gallery listing.
func pixhostGallery(root *html.Node, u *url.URL) (*Result, error) {
	seen := make(map[string]bool)
	var files []File
	for _, img := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.Img) }) {
		full := pixhostFullImage(resolveRef(u, attr(img, "src")))
		if full == "" || seen[full] {
			continue
		}
		seen[full] = true
		files = append(files, File{
			// The name keeps the site's own id prefix rather than the
			// prettier alt text: two images in one gallery can share an
			// original filename, and the id is what makes them distinct.
			Name:    util.NameFromURL(full),
			URL:     full,
			Size:    -1,
			Headers: httpx.Referer(pixhostRoot + "/"),
		})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("pixhost: no images in %s (the gallery may have been removed)", u.Redacted())
	}
	return &Result{Title: pixhostTitle(root, u), Files: files}, nil
}

// pixhostImage reads the one image a viewer page shows.
func pixhostImage(root *html.Node, u *url.URL) (*Result, error) {
	img := findFirst(root, func(n *html.Node) bool {
		return isElem(n, atom.Img) && attr(n, "id") == pixhostImageID
	})
	if img == nil {
		return nil, fmt.Errorf("pixhost: no image on %s (it may have been removed)", u.Redacted())
	}
	link := resolveRef(u, attr(img, "src"))
	if link == "" {
		return nil, fmt.Errorf("pixhost: the image on %s has no source", u.Redacted())
	}

	name := util.NameFromURL(link)
	return &Result{Title: name, Files: []File{{
		Name:    name,
		URL:     link,
		Size:    -1,
		Headers: httpx.Referer(pixhostRoot + "/"),
	}}}, nil
}

// pixhostTitle names the job after the gallery.
//
// The document title carries it; the first heading on the page does not,
// being a content warning rather than the gallery's name.
func pixhostTitle(root *html.Node, u *url.URL) string {
	return util.FirstNonEmpty(
		trimSiteSuffix(firstText(root, atomTitle)),
		strings.Trim(u.Path, "/"),
	)
}

// pixhostFullImage turns a thumbnail link into the image it stands for,
// which differs only in the host's prefix and one path segment:
//
//	https://t2.pixhost.to/thumbs/<group>/<file>
//	https://img2.pixhost.to/images/<group>/<file>
//
// Anything that is not a thumbnail — the site's own furniture, an advert —
// yields "" and is skipped.
func pixhostFullImage(thumb string) string {
	u, err := url.Parse(thumb)
	if err != nil || u.Host == "" {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Path, "/thumbs/")
	if !ok {
		return ""
	}
	server, ok := strings.CutPrefix(u.Host, "t")
	if !ok || !util.HostMatches(server, "pixhost.to") {
		return ""
	}
	u.Host = "img" + server
	u.Path = "/images/" + rest
	return u.String()
}
