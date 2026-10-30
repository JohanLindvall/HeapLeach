// SPDX-License-Identifier: MIT

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

// FileJump serves public file shares. The download button is a stable
// redirector that signs the storage URL on every request, so keep the button
// URL itself: saving its redirect target would expire while queued.
type FileJump struct {
	client *httpx.Client
}

func NewFileJump(client *httpx.Client) *FileJump {
	return &FileJump{client: client}
}

func (f *FileJump) Name() string { return "filejump" }

// fileJumpShape is a public share, /s/<id>, or its /download and /content
// routes.
var fileJumpShape = pathShape(`^/s/[A-Za-z0-9_-]+(/(download|content))?/?$`)

// Recognises is the inventory's description.
func (f *FileJump) Recognises() string {
	return "public shares (`/s/<id>`) whose page offers the share's own `/s/<id>/download` button"
}

// Shape reports a share or one of its routes.
func (f *FileJump) Shape(u *url.URL) bool { return fileJumpShape(u) }

// Recognise takes a share page carrying what Extract keeps: an enabled
// link to the share's own download route, beside it on the same site.
func (f *FileJump) Recognise(_ context.Context, p *Page) bool {
	parts := util.PathSegments(p.Final)
	if !fileJumpShape(p.Final) || len(parts) < 2 {
		return false
	}
	root := p.HTML()
	if root == nil {
		return false
	}
	want := "/s/" + parts[1] + "/download"
	return findFirst(root, func(a *html.Node) bool {
		if !isElem(a, atom.A) || hasAttr(a, "disabled") || attr(a, "aria-disabled") == "true" {
			return false
		}
		// The link resolved against the page: its path is compared, and its
		// origin only with the page's own.
		link, err := p.Final.Parse(strings.TrimSpace(attr(a, "href")))
		return err == nil && link.Path == want && util.Origin(link) == util.Origin(p.Final)
	}) != nil
}

func (f *FileJump) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	parts := util.PathSegments(u)
	if len(parts) < 2 || parts[0] != "s" || len(parts) > 3 ||
		(len(parts) == 3 && parts[2] != "download" && parts[2] != "content") {
		return nil, fmt.Errorf("filejump: expected a public share (/s/<id>)")
	}
	page := *u
	page.Path, page.RawPath, page.Fragment = "/s/"+parts[1], "", ""
	doc, _, err := fetchDocument(ctx, f.client, &page, httpx.Referer(util.Origin(&page)+"/"))
	if err != nil {
		return nil, fmt.Errorf("filejump: fetch share: %w", err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}
	var download string
	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		if hasAttr(a, "disabled") || attr(a, "aria-disabled") == "true" {
			continue
		}
		link, err := page.Parse(strings.TrimSpace(attr(a, "href")))
		if err != nil || link.User != nil || link.Scheme != page.Scheme ||
			!strings.EqualFold(link.Host, page.Host) || link.Path != page.Path+"/download" {
			continue
		}
		link.Fragment = ""
		download = link.String()
		break
	}
	if download == "" {
		return nil, fmt.Errorf("filejump: no public download link (the share may be restricted, expired or password protected)")
	}
	name := util.FirstNonEmpty(textOfFirst(root, func(n *html.Node) bool { return hasClass(n, "title") }),
		trimSiteSuffix(firstText(root, atom.Title)), parts[1])
	meta := textOfFirst(root, func(n *html.Node) bool { return hasClass(n, "meta") })
	size := parseHumanSize(strings.ReplaceAll(meta, ",", ""))
	return &Result{Title: name, Files: []File{{
		Name: name, URL: download, Size: size, SizeApprox: size >= 0,
		Headers: httpx.Referer(page.String()),
	}}}, nil
}
