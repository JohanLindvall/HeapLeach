// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/url"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// pageSniff recognises WordPress categories, then gives an ordinary HTML
// page's file links precedence over an embedded preview. Named hosts and
// earlier platform sniffs have already had their turn; remaining platforms
// identify themselves in the fetched page or through a bounded API probe.
// Outside a recognised category, only registered extractors are expanded:
// ordinary HTML navigation cannot recurse through this fallback. A page
// without supported links keeps the existing media fallback.
func (d *Direct) pageSniff(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	ext := strings.ToLower(path.Ext(u.Path))
	switch ext {
	case "", ".html", ".htm", ".php", ".asp", ".aspx", ".jsp":
	default:
		return nil, nil
	}
	// Header-gated and bounded: a binary response must never be read as a
	// page, even when its signed URL has no extension.
	doc, page, ok := mediaPageFetchURL(ctx, d.client, u)
	if !ok {
		return platformAPIs(ctx, d.client, u, opts)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, nil
	}
	if res, err := platformPage(ctx, d.client, page, root, doc, opts); res != nil || err != nil {
		return res, err
	}
	if d.registry != nil {
		if wordPressCategory(root, page) {
			return wordPressExtract(ctx, d.client, d.registry, page, root, opts)
		}
	}
	album, albumErr := darkGramResult(d.client, page, root, opts)
	if album == nil && albumErr == nil {
		if res, err := platformAPIs(ctx, d.client, page, opts); res != nil || err != nil {
			return res, err
		}
	}
	if d.registry != nil {
		if sources := fileLinkSources(d.registry, linksCandidates(root, u)); len(sources) > 0 {
			return expandPageLinks(ctx, d.registry, u, root, sources, opts)
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

// fileLinkSources keeps links claimed by registered extractors. The generic
// Direct fallback is never followed, even if it could discover more links on
// that page. Other hosts keep their complete URLs (including Mega's key).
// K2S and FileBoom additionally deduplicate by service and file ID, ignoring
// aliases, filenames and tracking parameters, and exclude non-file pages.
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
		if ex.Name() == "keep2share" || ex.Name() == "fileboom" {
			parts := util.PathSegments(u)
			if len(parts) < 2 || parts[0] != "file" {
				continue
			}
			key = ex.Name() + "/" + parts[1]
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		sources = append(sources, link)
	}
	return sources
}
