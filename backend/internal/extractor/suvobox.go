// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
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
func NewSuvobox(client *httpx.Client) *Share { return newShare(client, &suvoboxSite) }

// suvoboxAlbumShape is an album share, /a/<id>; suvoboxFileShape a file,
// /f/<id>.
var (
	suvoboxAlbumShape = pathShape(`^/a/[A-Za-z0-9]+/?$`)
	suvoboxFileShape  = pathShape(`^/f/[A-Za-z0-9]+/?$`)
)

// suvoboxSite reads what extract does: an album's tiles, each naming a file;
// or, on a file page, the site's own download button for that file, the
// ?dl=1 form of its path, which is what says the id is a stored file here.
var suvoboxSite = shareSite{
	name: "suvobox",
	recognises: "albums (`/a/<id>`) listing `thumb-item` tiles, and file pages (`/f/<id>`) " +
		"whose download button is the file's own `/f/<id>?dl=1` link",
	forms: []shareForm{
		{suvoboxAlbumShape, func(p *Page) bool {
			return p.hasElement(func(n *html.Node) bool {
				return isElem(n, atom.A) && hasClass(n, suvoboxTileClass) &&
					linkPathID(resolveRef(p.Final, attr(n, "href")), "f") != ""
			})
		}},
		{suvoboxFileShape, func(p *Page) bool {
			// A trailing slash on either is the same file page.
			want := strings.TrimRight(p.Final.EscapedPath(), "/")
			return p.hasElement(func(n *html.Node) bool {
				if !isElem(n, atom.A) {
					return false
				}
				link, err := p.Final.Parse(attr(n, "href"))
				return err == nil && strings.TrimRight(link.EscapedPath(), "/") == want && link.Query().Get("dl") == "1"
			})
		}},
	},
	extract: func(ctx context.Context, s *Share, u *url.URL, _ Options) (*Result, error) {
		segs := util.PathSegments(u)
		if len(segs) < 2 || (segs[0] != "a" && segs[0] != "f") {
			return nil, fmt.Errorf("suvobox: %s is neither an album (/a/<id>) nor a file (/f/<id>)",
				u.Redacted())
		}
		root, err := s.root(ctx, u, suvoboxRoot+"/")
		if err != nil {
			return nil, err
		}
		if segs[0] == "a" {
			return suvoboxAlbum(root, u)
		}
		return suvoboxFile(root, segs[1], u)
	},
}

// suvoboxAlbum reads every tile of an album listing.
func suvoboxAlbum(root *html.Node, u *url.URL) (*Result, error) {
	seen := make(map[string]bool)
	var files []File

	for _, tile := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.A) && hasClass(n, suvoboxTileClass)
	}) {
		// The listing appends ?album= to keep its own breadcrumb.
		id := linkPathID(resolveRef(u, attr(tile, "href")), "f")
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
			Name: cmp.Or(name, id),
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
	// The page's own heading is truncated with an ellipsis to fit its box,
	// so the metadata is read first — it carries the name in full.
	return &Result{Title: albumTitle(root, u), Files: files}, nil
}

// suvoboxFile builds the single-file result for a /f/<id> page.
func suvoboxFile(root *html.Node, id string, u *url.URL) (*Result, error) {
	name := cmp.Or(metaContent(root, "og:title"), id)
	return &Result{Title: name, Files: []File{{
		Name:    name,
		URL:     suvoboxRawURL(id),
		Size:    -1,
		Headers: httpx.Referer(suvoboxRoot + "/"),
	}}}, nil
}

// suvoboxRawURL is the media link for a file id.
func suvoboxRawURL(id string) string {
	return suvoboxMedia + "/f/" + url.PathEscape(id) + "?raw=1"
}

// suvoboxText reads the text of the first descendant carrying a class.
func suvoboxText(n *html.Node, class string) string {
	if el := findFirst(n, func(c *html.Node) bool { return hasClass(c, class) }); el != nil {
		return strings.TrimSpace(textOf(el))
	}
	return ""
}
