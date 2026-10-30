// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Page is what a URL answered when the registry asked it what it is.
//
// The registry fetches every submitted http(s) URL once, before choosing an
// extractor, and each extractor's Recognise reads this answer rather than the
// URL's host. A page that could not be read at all is still a Page — Err says
// why — so that an extractor whose URL form alone is conclusive can claim it,
// and the error it then meets is its own rather than the probe's.
type Page struct {
	// URL is the URL as submitted; Final is where redirects ended.
	URL, Final *url.URL
	// Status is the HTTP status, zero when nothing answered.
	Status int
	// Header is the response's header, nil when nothing answered.
	Header http.Header
	// Body is the response, read when it is text of some kind (markup, a
	// script, JSON, a playlist) and bounded by config.MaxResponseBytes.
	// Empty for media, which the probe never reads.
	Body string
	// Challenged is set when a WAF challenge stood in for the page: there is
	// nothing of the site's own to read, only the URL.
	Challenged bool
	// Err is the fetch's failure, if it failed.
	Err error

	once sync.Once
	root *html.Node

	memoMu sync.Mutex
	memo   map[string]*memoEntry
}

type memoEntry struct {
	once  sync.Once
	value any
}

// Memo runs compute once per page and key, however many recognisers ask:
// a bounded probe of the page's origin (platformJSON) that several
// extractors would make is made once, and its answer shared. Concurrent
// callers wait for the first.
func (p *Page) Memo(key string, compute func() any) any {
	p.memoMu.Lock()
	if p.memo == nil {
		p.memo = map[string]*memoEntry{}
	}
	e := p.memo[key]
	if e == nil {
		e = &memoEntry{}
		p.memo[key] = e
	}
	p.memoMu.Unlock()
	e.once.Do(func() { e.value = compute() })
	return e.value
}

// pageCache keeps recently probed pages so that one URL is fetched once:
// identification, the extractor that then reads the document, a refresh
// at transfer time and a link several sources share all ask the same
// question. Entries are keyed by route as well as URL, since a page fetched
// through one egress address says nothing about what another is served,
// and a probe that failed or met a challenge is never kept.
type pageCache struct {
	mu      sync.Mutex
	entries map[string]*pageCacheEntry
	order   []string
}

type pageCacheEntry struct {
	ready   chan struct{}
	page    *Page
	fetched time.Time
}

// get returns the page for u through ctx's route, probing it at most once
// at a time and reusing a good answer for config.PageCacheTTL.
func (c *pageCache) get(ctx context.Context, client *httpx.Client, u *url.URL) *Page {
	key := httpx.RouteID(ctx) + " " + u.String()
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*pageCacheEntry{}
	}
	if e := c.entries[key]; e != nil {
		c.mu.Unlock()
		select {
		case <-e.ready:
		case <-ctx.Done():
			return &Page{URL: u, Final: u, Err: ctx.Err()}
		}
		if e.page != nil && time.Since(e.fetched) < config.PageCacheTTL {
			return e.page
		}
		c.mu.Lock()
		if c.entries[key] == e {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		return c.get(ctx, client, u)
	}
	e := &pageCacheEntry{ready: make(chan struct{})}
	c.entries[key] = e
	c.order = append(c.order, key)
	for len(c.order) > config.PageCacheEntries {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	c.mu.Unlock()

	p := probePage(ctx, client, u)
	if p.Err == nil && !p.Challenged {
		e.page, e.fetched = p, time.Now()
	}
	close(e.ready)
	return p
}

// probePage fetches a URL as a browser navigating to it would, reading the
// body only when it is text. One attempt per transient failure budget, like
// any page fetch: a page that would not load is reported in the Page rather
// than as an error, since a recogniser may still know the URL's form.
func probePage(ctx context.Context, client *httpx.Client, u *url.URL) *Page {
	p := &Page{URL: u, Final: u}
	if u.Scheme != "http" && u.Scheme != "https" {
		return p
	}
	req, err := client.NewRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		p.Err = err
		return p
	}
	req.Header.Set(httpx.HeaderAccept, httpx.AcceptHTML)
	req.Header.Set(httpx.HeaderSecFetchDest, "document")
	req.Header.Set(httpx.HeaderSecFetchMode, "navigate")
	resp, err := client.Do(req)
	if err != nil {
		p.Err = err
		_, p.Challenged = errors.AsType[*httpx.ChallengeError](err)
		return p
	}
	defer resp.Body.Close()
	p.Status, p.Header, p.Final = resp.StatusCode, resp.Header, resp.Request.URL
	if !probeReadsType(p.MediaType()) {
		return p
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, config.MaxResponseBytes))
	if err != nil && len(body) == 0 {
		p.Err = err
	}
	p.Body = string(body)
	return p
}

// probeReadsType reports whether a response of this type is worth reading
// for markers: anything textual. Media is never read, which is what keeps a
// probe of a direct link to a large file down to its headers.
func probeReadsType(kind string) bool {
	switch {
	case kind == "", strings.HasPrefix(kind, "text/"),
		strings.HasSuffix(kind, "json"), strings.HasSuffix(kind, "xml"),
		strings.HasSuffix(kind, "javascript"), strings.Contains(kind, "mpegurl"):
		return true
	}
	return false
}

// pageKey carries the probed page to the chosen extractor, so one that reads
// the same document need not fetch it twice.
type pageKey struct{}

func withPage(ctx context.Context, p *Page) context.Context {
	return context.WithValue(ctx, pageKey{}, p)
}

// probedDocument returns the page the registry already fetched for u, when it
// is a successful document. ok is false whenever the extractor must fetch it
// itself: another URL, a failure, or a body the probe did not read.
func probedDocument(ctx context.Context, u *url.URL) (doc string, final *url.URL, ok bool) {
	p, _ := ctx.Value(pageKey{}).(*Page)
	if p == nil || p.URL.String() != u.String() || p.Err != nil || p.Status != http.StatusOK || p.Body == "" {
		return "", nil, false
	}
	return p.Body, p.Final, true
}

// fetchDocument is GetPage for the submitted URL that reuses the registry's
// probe when it already holds the document, so a page an extractor reads
// whole is fetched once rather than once to recognise and again to read.
// Any other URL, or a probe that did not get the page, is fetched as usual.
func fetchDocument(ctx context.Context, client *httpx.Client, u *url.URL, headers httpx.Header) (string, *url.URL, error) {
	if doc, final, ok := probedDocument(ctx, u); ok {
		return doc, final, nil
	}
	return client.GetPage(ctx, u.String(), headers, nil)
}

// OK reports whether the page answered with content of its own.
func (p *Page) OK() bool { return p.Err == nil && p.Status >= 200 && p.Status < 300 }

// MediaType is the response's declared type, lower case and without
// parameters.
func (p *Page) MediaType() string {
	if p.Header == nil {
		return ""
	}
	kind, _, _ := mime.ParseMediaType(p.Header.Get(httpx.HeaderContentType))
	return strings.ToLower(kind)
}

// IsHTML reports whether the body is a document worth parsing.
func (p *Page) IsHTML() bool {
	kind := p.MediaType()
	return p.Body != "" && (mediaPageIsHTMLType(kind) || kind == "" && strings.Contains(p.Body[:min(len(p.Body), 1024)], "<"))
}

// HTML is the parsed document, or nil when the body is not one.
func (p *Page) HTML() *html.Node {
	p.once.Do(func() {
		if p.IsHTML() {
			p.root, _ = parseHTML(p.Body)
		}
	})
	return p.root
}

// Has reports whether the body contains every one of the fragments.
func (p *Page) Has(fragments ...string) bool {
	if p.Body == "" {
		return false
	}
	for _, f := range fragments {
		if !strings.Contains(p.Body, f) {
			return false
		}
	}
	return true
}

// HasAny reports whether the body contains at least one of the fragments.
func (p *Page) HasAny(fragments ...string) bool {
	for _, f := range fragments {
		if p.Has(f) {
			return true
		}
	}
	return false
}

// Meta is a meta element's content by name or property.
func (p *Page) Meta(key string) string {
	if root := p.HTML(); root != nil {
		return metaContent(root, key)
	}
	return ""
}

// Generator is the generator meta tag, lower case: what software made the
// page, by its own account.
func (p *Page) Generator() string { return strings.ToLower(p.Meta("generator")) }

// Title is the document's title, trimmed.
func (p *Page) Title() string {
	if root := p.HTML(); root != nil {
		return strings.TrimSpace(firstText(root, atom.Title))
	}
	return ""
}

// titleSeparators split a title into the page's own name and the site's.
var titleSeparators = regexp.MustCompile(`\s+[-|—–·:»]{1,2}\s+`)

// Names are what the page says the site is called: its og:site_name,
// application-name and web-app title, and the first and last parts of its
// title. A site's own name is the steadiest thing its pages carry — bundles
// are renamed with every deploy, while the name is on the root, on every
// content page and on its error pages alike.
func (p *Page) Names() []string {
	var names []string
	for _, key := range []string{"og:site_name", "application-name", "apple-mobile-web-app-title"} {
		if v := p.Meta(key); v != "" {
			names = append(names, v)
		}
	}
	if title := p.Title(); title != "" {
		parts := titleSeparators.Split(title, -1)
		names = append(names, parts[0])
		if len(parts) > 1 {
			names = append(names, parts[len(parts)-1])
		}
	}
	return names
}

// Named reports whether the page calls its site any of the names, ignoring
// case and surrounding space.
func (p *Page) Named(names ...string) bool {
	for _, have := range p.Names() {
		for _, want := range names {
			if strings.EqualFold(strings.TrimSpace(have), want) {
				return true
			}
		}
	}
	return false
}

// Script reports whether the document loads a script whose path contains
// the fragment. Only the path is compared, never the host it is served from.
func (p *Page) Script(fragment string) bool {
	root := p.HTML()
	if root == nil {
		return false
	}
	return findFirst(root, func(n *html.Node) bool {
		if !isElem(n, atom.Script) {
			return false
		}
		src, err := url.Parse(attr(n, "src"))
		return err == nil && strings.Contains(src.Path, fragment)
	}) != nil
}

// Segments are the final URL's path segments.
func (p *Page) Segments() []string { return util.PathSegments(p.Final) }

// pathShape builds a URL-form test from patterns matched against the path
// alone. A form is the half of recognition that needs no fetch, and so the
// half the link scanner can use, which is why it never looks at the host.
func pathShape(patterns ...string) func(*url.URL) bool {
	res := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		res[i] = regexp.MustCompile(p)
	}
	return func(u *url.URL) bool {
		for _, re := range res {
			if re.MatchString(u.EscapedPath()) {
				return true
			}
		}
		return false
	}
}
