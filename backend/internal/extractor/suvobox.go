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

// Suvobox resolves album shares (/a/<id>) and single files (/f/<id>).
//
// An album listing is unusually generous: every tile states the file's id,
// its full name with extension and its size, so a whole album resolves in
// one request with real names in the queue from the start — no per-file page
// fetch, and nothing left to discover when a transfer begins.
//
// The bytes come from the media host, which serves ?raw=1 to anyone: no
// token, no cookie, no referer, and ranges honoured. The site's own download
// button points at a ?dl=1 link carrying a signed token instead, which is
// worth knowing only to say why it is not used — a signed link expires while
// an item waits its turn, and this one buys nothing over the plain path.
type Suvobox struct {
	hostSet
	client *httpx.Client
}

const (
	suvoboxRoot = "https://www.suvobox.com"
	// suvoboxMedia serves the files themselves.
	suvoboxMedia = "https://media.suvobox.com"
)

// Markers on the album listing.
const (
	suvoboxTileClass = "thumb-item"
	suvoboxNameClass = "thumb-name"
	suvoboxSizeClass = "thumb-size"
)

// NewSuvobox builds the suvobox extractor.
func NewSuvobox(client *httpx.Client) *Suvobox {
	return &Suvobox{hostSet: hostSet{"suvobox.com"}, client: client}
}

func (s *Suvobox) Name() string { return "suvobox" }

// Extract resolves an album or a single file.
func (s *Suvobox) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) < 2 || (segs[0] != "a" && segs[0] != "f") {
		return nil, fmt.Errorf("suvobox: %s is neither an album (/a/<id>) nor a file (/f/<id>)",
			u.Redacted())
	}

	doc, err := s.client.GetString(ctx, u.String(), httpx.Referer(suvoboxRoot+"/"))
	if err != nil {
		return nil, fmt.Errorf("suvobox: fetch %s: %w", u.Redacted(), err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("suvobox: parse %s: %w", u.Redacted(), err)
	}

	if segs[0] == "a" {
		return suvoboxAlbum(root, u)
	}
	return suvoboxFile(root, segs[1], u)
}

// suvoboxAlbum reads every tile of an album listing.
func suvoboxAlbum(root *html.Node, u *url.URL) (*Result, error) {
	seen := make(map[string]bool)
	var files []File

	for _, tile := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.A) && hasClass(n, suvoboxTileClass)
	}) {
		id := suvoboxFileID(resolveRef(u, attr(tile, "href")))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		// The tile names the file in full, extension included; the image's
		// alt text is the same name with the extension cut off.
		name := suvoboxText(tile, suvoboxNameClass)
		if name == "" {
			if img := findFirst(tile, func(n *html.Node) bool { return isElem(n, atom.Img) }); img != nil {
				name = strings.TrimSpace(attr(img, "alt"))
			}
		}
		size := parseHumanSize(suvoboxText(tile, suvoboxSizeClass))

		files = append(files, File{
			Name: util.FirstNonEmpty(name, id),
			URL:  suvoboxRawURL(id),
			// Listings round these, so they total a job but never settle
			// whether a file on disk is already this one.
			Size:       size,
			SizeApprox: size > 0,
			Headers:    httpx.Referer(suvoboxRoot + "/"),
		})
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("suvobox: no files in %s (the album may have been removed)", u.Redacted())
	}
	return &Result{Title: suvoboxTitle(root, u), Files: files}, nil
}

// suvoboxFile builds the single-file result for a /f/<id> page.
func suvoboxFile(root *html.Node, id string, u *url.URL) (*Result, error) {
	name := util.FirstNonEmpty(metaContent(root, "og:title"), id)
	return &Result{Title: name, Files: []File{{
		Name:    name,
		URL:     suvoboxRawURL(id),
		Size:    -1,
		Headers: httpx.Referer(suvoboxRoot + "/"),
	}}}, nil
}

// suvoboxTitle names the job after the album.
//
// The page's own heading is truncated with an ellipsis to fit its box, so the
// metadata is read first — it carries the name in full.
func suvoboxTitle(root *html.Node, u *url.URL) string {
	return util.FirstNonEmpty(
		metaContent(root, "og:title"),
		trimSiteSuffix(firstText(root, atom.Title)),
		strings.Trim(u.Path, "/"),
	)
}

// suvoboxRawURL is the media link for a file id.
func suvoboxRawURL(id string) string {
	return suvoboxMedia + "/f/" + url.PathEscape(id) + "?raw=1"
}

// suvoboxFileID reads the id out of a /f/<id> link, ignoring the ?album=
// parameter the listing appends to keep its own breadcrumb.
func suvoboxFileID(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	segs := util.PathSegments(u)
	if len(segs) != 2 || segs[0] != "f" {
		return ""
	}
	return segs[1]
}

// suvoboxText reads the text of the first descendant carrying a class.
func suvoboxText(n *html.Node, class string) string {
	if el := findFirst(n, func(c *html.Node) bool { return hasClass(c, class) }); el != nil {
		return strings.TrimSpace(textOf(el))
	}
	return ""
}
