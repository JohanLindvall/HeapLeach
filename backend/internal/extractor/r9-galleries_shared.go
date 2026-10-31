// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// about is what an extractor says of itself to the registry and the
// inventory: its name, the forms of link worth a probe, and the
// inventory's description of what it recognises. An extractor embeds it
// rather than spelling out the three methods, so that its constructor
// reads as its row.
type about struct {
	name string
	// shape is the forms Shape reports; none when nil.
	shape      func(*url.URL) bool
	recognises string
}

func (a about) Name() string { return a.name }

// Recognises is the inventory's description.
func (a about) Recognises() string { return a.recognises }

// Shape reports the forms of link worth a probe.
func (a about) Shape(u *url.URL) bool { return a.shape != nil && a.shape(u) }

// scriptTexts is the text of each script element under root, its spacing
// kept as written: the JSON a page assigns there may hold names whose
// spacing is their own.
func scriptTexts(root *html.Node) []string {
	var texts []string
	for _, script := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.Script) }) {
		var text strings.Builder
		for node := script.FirstChild; node != nil; node = node.NextSibling {
			if node.Type == html.TextNode {
				text.WriteString(node.Data)
			}
		}
		texts = append(texts, text.String())
	}
	return texts
}

// creatorAPI is where the API on origin keeps a creator's records — their
// /profile, /posts and each /post/<id> — on the creator-archive software
// kemono runs and on the API cum.st modelled on it.
func creatorAPI(origin, service, creator string) string {
	return origin + "/api/v1/" + url.PathEscape(service) + "/user/" + url.PathEscape(creator)
}

// linkFile is the file at link, named as the link names it, its length
// left to the response, fetched with headers.
func linkFile(link string, headers httpx.Header) File {
	return File{Name: util.NameFromURL(link), URL: link, Size: -1, Headers: headers}
}

// eitherShape is a shape matching what any of shapes match.
func eitherShape(shapes ...func(*url.URL) bool) func(*url.URL) bool {
	return func(u *url.URL) bool {
		for _, shape := range shapes {
			if shape(u) {
				return true
			}
		}
		return false
	}
}

// opening is an expansion's extract for a host reading the pages its own
// listing links: each link parsed and handed to open.
func opening(open func(context.Context, *url.URL) (*Result, error)) func(context.Context, string) (*Result, error) {
	return func(ctx context.Context, link string) (*Result, error) {
		u, err := ParseURL(link)
		if err != nil {
			return nil, err
		}
		return open(ctx, u)
	}
}

// noneResolved is the error for a listing of n items on where none of
// which yielded files — noneRead's, or the cancellation that cut it short —
// and nil when files holds any.
func noneResolved(ctx context.Context, files []File, site string, n int, noun, where string, refusals []string, failed error) error {
	if len(files) > 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return noneRead(site, n, noun, where, refusals, failed)
}

// limitNote is fileLimitNote when the file cap ended the expansion.
func (e expansion) limitNote() string {
	if e.full {
		return fileLimitNote
	}
	return ""
}
