// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Fapello resolves a model's page into their media.
//
// Listing pages carry the model's own thumbnails alongside recommendations
// for other models, so entries are kept only when the path names the model
// being fetched. The thumbnails differ from the full-size file by a "_300px"
// suffix, which is simply dropped.
type Fapello struct {
	client *httpx.Client
}

// fapelloThumbSuffix marks a listing thumbnail.
var fapelloThumbSuffix = regexp.MustCompile(`_\d+px(\.[A-Za-z0-9]+)$`)

// NewFapello builds the fapello extractor.
func NewFapello(client *httpx.Client) *Fapello {
	return &Fapello{client: client}
}

func (f *Fapello) Name() string { return "fapello" }

// fapelloShape is one of a model's posts, /<model>/<n>/. The model's own
// page is a bare /<model>/, which is no shape, so it is recognised by its
// page alone.
var fapelloShape = pathShape(`^/[A-Za-z0-9_.-]+/\d+/$`)

// Recognises is the inventory's description.
func (f *Fapello) Recognises() string {
	return "model pages and their posts (`/<model>/`, `/<model>/<n>/`) whose page links the model's own " +
		"media under `/content/…/<model>/`"
}

// Shape reports a model's post.
func (f *Fapello) Shape(u *url.URL) bool { return fapelloShape(u) }

// Recognise takes a model page, or one of its posts, carrying what Extract
// collects: media under the content tree in the model's own folder, on a
// page that names the site.
func (f *Fapello) Recognise(_ context.Context, p *Page) bool {
	segs := p.Segments()
	if len(segs) != 1 && !fapelloShape(p.Final) {
		return false
	}
	return p.Named("Fapello") && fapelloLinksMedia(p.Body, segs[0])
}

// fapelloContentLink is a link into the content tree, capturing what follows
// it.
var fapelloContentLink = regexp.MustCompile(`https?://[a-z0-9.-]+/content/([^"'\\ <>]+)`)

// fapelloLinksMedia reports what fapelloMedia matches without compiling it
// per page: a link into the content tree with the model's folder inside it,
// and something after the folder.
func fapelloLinksMedia(body, model string) bool {
	folder := "/" + model + "/"
	for _, m := range fapelloContentLink.FindAllStringSubmatch(body, -1) {
		if rest := m[1]; strings.Contains(rest[:len(rest)-1], folder) {
			return true
		}
	}
	return false
}

// fapelloMedia matches a link to the model's own media: the content tree
// files every model's media under a folder of their name.
func fapelloMedia(model string) *regexp.Regexp {
	return regexp.MustCompile(`https?://[a-z0-9.-]+/content/[^"'\\ <>]*/` +
		regexp.QuoteMeta(model) + `/[^"'\\ <>]+`)
}

// Extract walks a model's pages and collects everything of theirs.
func (f *Fapello) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return nil, fmt.Errorf("fapello: %s names no model", u.Redacted())
	}
	model := segs[0]

	// Keep whichever regional host was given: they serve the same content
	// and the site is happier being asked on the one it advertised.
	base := util.Origin(u)
	headers := httpx.Referer(base + "/")

	// Media lives on the shared content host and always names the model.
	ownMedia := fapelloMedia(model)

	// Paging past the last page wraps back to the first rather than
	// returning nothing, so the end is a page that adds nothing new, and
	// the walk is a quiet one.
	files, note, err := walkPages(ctx, pageWalk[File]{
		key:   func(f File) string { return f.URL },
		limit: opts.maxFiles(), limitNote: "partial — file limit", quiet: true,
		fetch: func(ctx context.Context, n int) (pageOf[File], error) {
			target := base + "/" + url.PathEscape(model) + "/"
			if n > 0 {
				target += "page-" + strconv.Itoa(n+1) + "/"
			}
			targetURL, err := url.Parse(target)
			if err != nil {
				return pageOf[File]{}, fmt.Errorf("fapello: %w", err)
			}
			doc, _, err := fetchDocument(ctx, f.client, targetURL, headers)
			if err != nil {
				return pageOf[File]{}, fmt.Errorf("fapello: fetch %s: %w", target, err)
			}
			var files []File
			for _, link := range ownMedia.FindAllString(doc, -1) {
				full := fapelloFullSize(link)
				files = append(files, File{
					Name:    util.NameFromURL(full),
					URL:     full,
					Size:    -1,
					Headers: headers,
				})
			}
			return pageOf[File]{items: files, next: true}, nil
		},
	})
	if err != nil {
		return nil, err
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("fapello: found no media for %q", model)
	}
	return &Result{Title: model, Files: files, Note: note}, nil
}

// fapelloFullSize turns a listing thumbnail into the full-size file.
func fapelloFullSize(link string) string {
	if strings.Contains(link, "px.") {
		return fapelloThumbSuffix.ReplaceAllString(link, "$1")
	}
	return link
}
