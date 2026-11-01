// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// filejump serves public file shares. The download button is a stable
// redirector that signs the storage URL on every request, so keep the button
// URL itself: saving its redirect target would expire while queued.

// NewFileJump builds the filejump extractor.
func NewFileJump(client *httpx.Client) *Share { return newShare(client, &fileJumpSite) }

// fileJumpShape is a public share, /s/<id>, or its /download and /content
// routes.
var fileJumpShape = pathShape(`^/s/[A-Za-z0-9_-]+(/(download|content))?/?$`)

// fileJumpSite takes a share page carrying what extract keeps: an enabled
// link to the share's own download route, beside it on the same site.
var fileJumpSite = shareSite{
	name:       "filejump",
	recognises: "public shares (`/s/<id>`) whose page offers the share's own `/s/<id>/download` button",
	forms:      []shareForm{{fileJumpShape, fileJumpOffers}},
	left:       fileJumpLeft,
	extract:    fileJumpExtract,
}

// fileJumpOffers reports a share page offering its own download button.
func fileJumpOffers(p *Page) bool {
	want := "/s/" + p.Segments()[1] + "/download"
	return p.hasElement(func(a *html.Node) bool {
		if !isElem(a, atom.A) || !enabledControl(a) {
			return false
		}
		// The link resolved against the page: its path is compared, and
		// its origin only with the page's own.
		link, err := p.Final.Parse(strings.TrimSpace(attr(a, "href")))
		return err == nil && link.Path == want && util.Origin(link) == util.Origin(p.Final)
	})
}

// fileJumpLeft takes a share's /download or /content route, which redirects
// to signed storage, by the share page it belongs to: fetched once, and
// judged as a probe of it would be. Extract reads that page for the share's
// name and size and keeps the stable route.
func fileJumpLeft(ctx context.Context, s *Share, p *Page) bool {
	segs := util.PathSegments(p.URL)
	if len(segs) != 3 {
		return false
	}
	share := *p.URL
	share.Path, share.RawPath, share.Fragment = "/s/"+segs[1], "", ""
	return memoAsked(ctx, p, "filejump share "+share.String(), func() bool {
		ctx, done, ok := probeContext(ctx)
		if !ok {
			return false
		}
		defer done()
		doc, err := s.client.GetString(ctx, share.String(), httpx.Referer(util.Origin(&share)+"/"))
		return err == nil && fileJumpOffers(&Page{URL: &share, Final: &share, Status: http.StatusOK, Body: doc})
	})
}

func fileJumpExtract(ctx context.Context, s *Share, u *url.URL, _ Options) (*Result, error) {
	parts := util.PathSegments(u)
	if len(parts) < 2 || parts[0] != "s" || len(parts) > 3 ||
		(len(parts) == 3 && parts[2] != "download" && parts[2] != "content") {
		return nil, fmt.Errorf("filejump: expected a public share (/s/<id>)")
	}
	page := *u
	page.Path, page.RawPath, page.Fragment = "/s/"+parts[1], "", ""
	root, _, err := fetchRoot(ctx, s.client, &page, httpx.Referer(util.Origin(&page)+"/"))
	if err != nil {
		return nil, fmt.Errorf("filejump: fetch share: %w", err)
	}
	var download string
	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		if !enabledControl(a) {
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
	name := cmp.Or(textOfFirst(root, func(n *html.Node) bool { return hasClass(n, "title") }),
		trimSiteSuffix(firstText(root, atom.Title)), parts[1])
	meta := textOfFirst(root, func(n *html.Node) bool { return hasClass(n, "meta") })
	size := parseHumanSize(strings.ReplaceAll(meta, ",", ""))
	return &Result{Title: name, Files: []File{{
		Name: name, URL: download, Size: size, SizeApprox: size >= 0,
		Headers: httpx.Referer(page.String()),
	}}}, nil
}
