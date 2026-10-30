// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// pageSniff recognises WordPress categories, then gives an ordinary HTML
// page's file links precedence over an embedded preview. Every platform has
// already been offered the probed page by its own recogniser, so what
// reaches this is a page none of them claimed. Outside a recognised
// category, only registered extractors are expanded: ordinary HTML
// navigation cannot recurse through this fallback. A page without supported
// links keeps the existing media fallback.
func (d *Direct) pageSniff(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	ext := strings.ToLower(path.Ext(u.Path))
	switch ext {
	case "", ".html", ".htm", ".php", ".asp", ".aspx", ".jsp":
	default:
		return nil, nil
	}
	doc, page, ok := d.document(ctx, u)
	if !ok {
		return nil, nil
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, nil
	}
	if d.registry != nil {
		if wordPressCategory(root, page) {
			return wordPressExtract(ctx, d.client, d.registry, page, root, opts)
		}
	}
	album, albumErr := darkGramResult(d.client, page, root, opts)
	if d.registry != nil {
		if sources := fileLinkSources(d.registry, linksCandidates(root, u)); len(sources) > 0 {
			if res, err := expandPageLinks(ctx, d.registry, u, root, sources, opts); res != nil || err != nil {
				return res, err
			}
		}
	}
	if album != nil || albumErr != nil {
		return album, albumErr
	}
	if ext == "" {
		res, _ := mediaPageResult(ctx, d.client, u, root, doc)
		return res, nil
	}
	return nil, nil
}

// document is the page as HTML: the registry's probe when it read one, or
// else a fetch of its own. Either way it is header-gated and bounded, since
// a binary response must never be read as a page, even when its signed URL
// has no extension.
func (d *Direct) document(ctx context.Context, u *url.URL) (string, *url.URL, bool) {
	if p := probedPage(ctx, u); p != nil && p.Err == nil && p.Status == http.StatusOK && p.Body != "" {
		if !mediaPageIsHTMLType(p.MediaType()) {
			return "", nil, false
		}
		return p.Body, p.Final, true
	}
	return mediaPageFetchURL(ctx, d.client, u)
}

// fileLinkSources keeps links claimed by registered extractors. The generic
// Direct fallback is never followed, even if it could discover more links on
// that page. Other hosts keep their complete URLs (including Mega's key).
// K2S and FileBoom links additionally deduplicate by file ID, ignoring the
// domain, filenames and tracking parameters, and exclude non-file pages: a
// link's form cannot say which of the two it is before its page is probed,
// and the two serve one file store, an ID naming the same file on either.
func fileLinkSources(registry *Registry, candidates []string) []string {
	var sources []string
	seen := make(map[string]bool)
	for _, link := range candidates {
		u, err := ParseURL(link)
		if err != nil {
			continue
		}
		ex, known := registry.Known(u)
		if !known || ex.Name() == "direct" || ex.Name() == linksScheme {
			continue
		}
		key := link
		if _, ok := ex.(*Keep2Share); ok {
			parts := util.PathSegments(u)
			if len(parts) < 2 || parts[0] != "file" {
				continue
			}
			key = "keep2share-file/" + parts[1]
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		sources = append(sources, link)
	}
	return sources
}
