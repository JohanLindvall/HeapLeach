// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// pageSniff recognises WordPress categories, then gives an ordinary HTML
// page's file links precedence over an embedded preview. Every platform has
// already been offered the probed page by its own recogniser, so what
// reaches this is a page none of them claimed. Outside a recognised
// category, only registered extractors are expanded: ordinary HTML
// navigation cannot recurse through this fallback. A page whose supported
// links held nothing, or none of which could be read, keeps the media
// fallback: a side link that failed says nothing about the page's own
// clip. An empty successful probe gets one fetch with the origin Referer,
// unless the probe already asked that way. A successful file response to
// that retry keeps its request headers and becomes a direct file. read
// reports whether HTML was read, so an unsuccessful retry cannot erase the
// earlier page classification and queue its URL as a file.
func (d *Direct) pageSniff(ctx context.Context, u *url.URL, opts Options) (res *Result, read bool, err error) {
	ext := strings.ToLower(path.Ext(u.Path))
	switch ext {
	case "", ".html", ".htm", ".php", ".asp", ".aspx", ".jsp":
	default:
		return nil, false, nil
	}
	doc, page, probe, ok := d.document(ctx, u)
	if !ok {
		return nil, false, nil
	}
	res, read, err = d.pageContents(ctx, u, page, doc, opts)
	if res != nil || err != nil || !read {
		return res, read, err
	}
	if _, whole := probe.Document(); whole && !probe.originReferer {
		if err := ctx.Err(); err != nil {
			return nil, true, err
		}
		// A 200 page can be a soft Referer gate. Read it once under the
		// fallback's own request policy, without reusing the same probe.
		fresh := freshRead(ctx)
		if reply, ok := mediaPageFetch(fresh, d.client, u); ok {
			if reply.file {
				if err := ctx.Err(); err != nil {
					return nil, true, err
				}
				return directResult(u, reply.headers), true, nil
			}
			if reply.html {
				res, _, err = d.pageContents(fresh, u, reply.final, reply.body, opts)
			}
		}
		if err == nil {
			err = ctx.Err()
		}
	}
	return res, true, err
}

// pageContents reads one HTML response in the fallback's usual order.
// Keeping acquisition separate lets the Referer retry use the same parsing
// and link expansion without retrying recursively or losing the first
// response's evidence that this URL was HTML.
func (d *Direct) pageContents(ctx context.Context, u, page *url.URL, doc string, opts Options) (res *Result, read bool, err error) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, false, nil
	}
	if d.registry != nil {
		if wordPressCategory(root, page) {
			res, err := wordPressExtract(ctx, d.client, d.registry, page, root, opts)
			return res, true, err
		}
	}
	album, albumErr := darkGramResult(d.client, page, root, opts)
	var linksErr error
	if d.registry != nil {
		if sources := fileLinkSources(d.registry, linksCandidates(root, page, u), page); len(sources) > 0 {
			res, err := expandPageLinks(ctx, d.registry, page, root, sources, opts)
			if res != nil {
				return res, true, nil
			}
			if err := ctx.Err(); err != nil {
				return nil, true, err
			}
			linksErr = err
		}
	}
	if album != nil || albumErr != nil {
		return album, true, albumErr
	}
	if path.Ext(u.Path) == "" {
		if res, _ := mediaPageResult(ctx, d.client, u, root, doc); res != nil {
			return res, true, nil
		}
	}
	return nil, true, linksErr
}

// document is the page as HTML: the registry's probe when it read one, or
// else a fetch of its own. Either way it is header-gated and bounded, since
// a binary response must never be read as a page, even when its signed URL
// has no extension. The returned probe is the one actually reused, nil for
// a fetch of its own; only a reused probe may need a Referer retry.
//
// A probe that answered is the answer: a status other than success, or a
// type other than a page, is not asked again. A page too large to read
// whole is sniffed by the prefix the probe kept, rather than fetched again
// only to be refused for the same size.
func (d *Direct) document(ctx context.Context, u *url.URL) (string, *url.URL, *Page, bool) {
	if p := probedFor(ctx, d.client, u, nil); p != nil {
		if p.Status == 0 && p.Err != nil && !errors.Is(p.Err, errNoProbe) {
			// Nothing answered the probe, which already asked twice: the
			// URL is queued as the file, and the transfer's retries wait
			// out the outage rather than another page fetch.
			return "", nil, nil, false
		}
		if doc, ok := p.Document(); ok || p.Status != 0 {
			if !ok && httpx.IsTooLarge(p.Err) && p.Status == http.StatusOK && p.Body != "" {
				doc, ok = p.Body, true
			}
			if !ok || !mediaPageIsHTMLType(p.MediaType()) {
				return "", nil, nil, false
			}
			return doc, cloneURL(p.Final), p, true
		}
	}
	doc, final, ok := mediaPageFetchURL(ctx, d.client, u)
	return doc, final, nil, ok
}

// fileLinkSources keeps links claimed by registered extractors. The generic
// Direct fallback is never followed, even if it could discover more links on
// that page. Other hosts keep their complete URLs (including Mega's key).
// Keep2Share file links additionally deduplicate by file ID, ignoring
// filenames and tracking parameters, within each service: across that
// service's own domains, but never across the two services, whose file
// stores are their own (Keep2Share.fileKey). A link of that form on any
// other host is keyed by its host as well, so it cannot shadow theirs.
//
// from is the page the candidates were found on, as for supportedSources.
func fileLinkSources(registry *Registry, candidates []string, from *url.URL) []string {
	var sources []string
	seen := make(map[string]bool)
	for _, link := range candidates {
		u, err := ParseURL(link)
		if err != nil {
			continue
		}
		ex, known := registry.Follows(u, from)
		if !known || ex.Name() == linksScheme {
			continue
		}
		key := link
		if k, ok := ex.(*Keep2Share); ok {
			// The services' other pages (premium, folders, an id left
			// out) are followed as their hosts' links but hold no file.
			if !keep2ShareShape(u) {
				continue
			}
			key = k.fileKey(u)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		sources = append(sources, link)
	}
	return sources
}
