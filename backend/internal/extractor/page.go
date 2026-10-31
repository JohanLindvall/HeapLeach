// SPDX-License-Identifier: MIT

package extractor

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
//
// A Page is shared: the page cache hands one to every concurrent caller of
// its URL and route, and every recogniser and the chosen extractor read it.
// Everything it holds — its URLs, header, body, parsed document and memoised
// values — is borrowed and must not be changed. A reader that needs to edit
// a URL, a header or a subtree copies it first; probedDocument and fetchRoot
// already hand back a final URL the caller owns.
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

	// via and route are who asked: the client, whose User-Agent, language
	// and cookie jar the answer was for, and the route it went out on. A
	// page built without them (a fixture) answers any reader.
	via   *httpx.Client
	route string

	// parsed is filled once, on first use: the document and what every
	// recogniser asks of its head, read in one pass, since each unrecognised
	// page is offered to every recogniser in turn.
	parseOnce sync.Once
	parsed    pageIndex
	// indexed is set once parsed is filled, for a reader that takes the
	// tree only when recognition already built it (parsedRoot).
	indexed atomic.Bool

	memoMu sync.Mutex
	memo   map[string]*memoEntry
}

// pageIndex is what one pass over the document collects.
type pageIndex struct {
	root *html.Node
	// meta maps each meta name and property to its first element's content.
	meta  map[string]string
	title string
	// scripts are the paths of the scripts the document loads.
	scripts []string
	names   []string
}

type memoEntry struct {
	once  sync.Once
	value any
	// dropped marks an answer memoAsked did not keep: one computed after
	// its asker's context had ended.
	dropped bool
}

// memo runs compute once per page and key, however many recognisers ask:
// a bounded probe of the page's origin that several extractors would make
// is made once, and its answer shared. Concurrent callers wait for the
// first. Keys are namespaced by what is asked ("json <endpoint>"), and a key
// is only ever asked for one type.
func memo[T any](p *Page, key string, compute func() T) T {
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
	v, _ := e.value.(T)
	return v
}

// memoAsked is memo for a question compute puts to the network with ctx.
// An answer that comes back after ctx has ended is that caller's
// cancellation, not the page's answer: it is handed back to that caller
// alone and not kept, so the page — shared through the cache with every
// job reading the same URL — goes on as if nobody had asked, and a caller
// that was waiting on it asks again with its own context. Any other
// answer, failures and the probe's own timeouts included, is kept as memo
// keeps it.
func memoAsked[T any](ctx context.Context, p *Page, key string, compute func() T) T {
	for {
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
		asked := false
		e.once.Do(func() {
			asked = true
			e.value = compute()
			if ctx.Err() != nil {
				e.dropped = true
				p.memoMu.Lock()
				if p.memo[key] == e {
					delete(p.memo, key)
				}
				p.memoMu.Unlock()
			}
		})
		if asked || !e.dropped {
			v, _ := e.value.(T)
			return v
		}
	}
}

// pageCache keeps recently probed pages so that one URL is fetched once:
// identification, the extractor that then reads the document, a refresh
// at transfer time and a link several sources share all ask the same
// question. Entries are keyed by route as well as URL, since a page fetched
// through one egress address says nothing about what another is served,
// and a probe that failed or met a challenge is never kept. It is bounded by
// the bytes of the bodies it holds as well as by age, oldest dropped first.
type pageCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List // of *pageCacheEntry, oldest first
	bytes   int
}

type pageCacheEntry struct {
	key     string
	ready   chan struct{}
	page    *Page
	fetched time.Time
}

// get returns the page for u through ctx's route, probing it at most once
// at a time and reusing a good answer for config.PageCacheTTL.
func (c *pageCache) get(ctx context.Context, client *httpx.Client, u *url.URL) *Page {
	key := httpx.RouteID(ctx) + " " + u.String()
	for {
		c.mu.Lock()
		if c.entries == nil {
			c.entries = map[string]*list.Element{}
		}
		c.expireLocked()
		el := c.entries[key]
		if el == nil {
			break
		}
		e := el.Value.(*pageCacheEntry)
		c.mu.Unlock()
		select {
		case <-e.ready:
		case <-ctx.Done():
			return &Page{URL: u, Final: u, Err: ctx.Err()}
		}
		if e.page != nil {
			return e.page
		}
		// The probe this caller waited for failed and was dropped: ask
		// again rather than share a failure.
	}
	e := &pageCacheEntry{key: key, ready: make(chan struct{})}
	c.entries[key] = c.order.PushBack(e)
	c.mu.Unlock()

	// The page keeps a URL of its own: the caller's may be changed once
	// the page is shared with others.
	p := probePage(ctx, client, cloneURL(u))
	c.mu.Lock()
	if p.Err == nil && !p.Challenged {
		e.page, e.fetched = p, time.Now()
		c.bytes += len(p.Body)
		for c.bytes > config.PageCacheBytes && c.order.Len() > 1 {
			c.removeLocked(c.order.Front())
		}
	} else if el := c.entries[key]; el != nil && el.Value == e {
		c.removeLocked(el)
	}
	c.mu.Unlock()
	close(e.ready)
	return p
}

// expireLocked drops entries older than the cache's time to live. A probe
// still in flight has no fetch time and is left alone.
func (c *pageCache) expireLocked() {
	for el := c.order.Front(); el != nil; {
		next := el.Next()
		if e := el.Value.(*pageCacheEntry); e.page != nil && time.Since(e.fetched) >= config.PageCacheTTL {
			c.removeLocked(el)
		}
		el = next
	}
}

func (c *pageCache) removeLocked(el *list.Element) {
	e := c.order.Remove(el).(*pageCacheEntry)
	if c.entries[e.key] == el {
		delete(c.entries, e.key)
	}
	if e.page != nil {
		c.bytes -= len(e.page.Body)
	}
}

// probePage fetches a URL as a browser navigating to it would, reading the
// body only when it is text. One attempt per transient failure budget, like
// any page fetch: a page that would not load is reported in the Page rather
// than as an error, since a recogniser may still know the URL's form.
func probePage(ctx context.Context, client *httpx.Client, u *url.URL) *Page {
	p := &Page{URL: u, Final: u, via: client, route: httpx.RouteID(ctx)}
	if u.Scheme != "http" && u.Scheme != "https" {
		return p
	}
	req, err := client.NewRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		p.Err = err
		return p
	}
	httpx.Navigate.Apply(req)
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
	// A body cut short — interrupted, or past the bound — is kept for the
	// recognisers to sniff, but the page is a failure: neither the cache
	// nor Document may offer a prefix as the document, which a fetch of the
	// same URL would refuse.
	body, err := httpx.ReadBody(resp.Body)
	if httpx.IsTooLarge(err) {
		err = fmt.Errorf("%s: %w", p.Final.Redacted(), err)
	}
	p.Err = err
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

// freshRead is ctx for a read that must ask the site again rather than take
// the page the registry probed: a retry of an answer that was received but
// wrong, and every read at transfer time, whose signed link or metadata the
// probe's would have made stale. Everything else ctx carries — cancellation,
// route, cookie jar, logger, challenge capture — stays as it was; only the
// probed page is withheld, so fetchDocument, fetchRoot and probedDocument
// fetch as if there had been none.
//
// Freshness is decided here, once, rather than by each reader: a reader
// takes the ctx it is handed and never one it captured earlier.
func freshRead(ctx context.Context) context.Context {
	if attachedPage(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, pageKey{}, (*Page)(nil))
}

// attachedPage is the page carried in ctx, whatever URL it answers.
func attachedPage(ctx context.Context) *Page {
	p, _ := ctx.Value(pageKey{}).(*Page)
	return p
}

// probed is the page the registry probed for u, carried in ctx, or nil when
// there is none for this URL. Either end of the redirect chain will do: an
// extractor that rebuilds the canonical address the submitted link
// redirected to is asking for the same page.
func probed(ctx context.Context, u *url.URL) *Page {
	p := attachedPage(ctx)
	if p == nil || u == nil {
		return nil
	}
	// A page probed through another egress address says nothing of what
	// this one is served (route recovery can move a read to another).
	if p.via != nil && p.route != httpx.RouteID(ctx) {
		return nil
	}
	if want := u.String(); p.URL.String() != want && p.Final.String() != want {
		return nil
	}
	return p
}

// Document is the page's body when it is a successful document of its own,
// the only kind an extractor may read instead of fetching.
func (p *Page) Document() (string, bool) {
	if p == nil || p.Err != nil || p.Status != http.StatusOK || p.Body == "" {
		return "", false
	}
	return p.Body, true
}

// probedDocument returns the page the registry already fetched for u, when it
// is a successful document. ok is false whenever the extractor must fetch it
// itself: another URL, a failure, or a body the probe did not read. The final
// URL is a copy the caller may change, as it may one GetPage returned.
func probedDocument(ctx context.Context, u *url.URL) (doc string, final *url.URL, ok bool) {
	p := probed(ctx, u)
	if doc, ok = p.Document(); !ok {
		return "", nil, false
	}
	return doc, cloneURL(p.Final), true
}

// cloneURL is a copy of u its holder may change without changing u. The
// user information is immutable, so it may be shared.
func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	c := *u
	return &c
}

// probedFor is the probed page for u when it is the response a GET of u
// with headers, through client on ctx's route, would get (Page.answers);
// nil when the reader must ask for its own.
func probedFor(ctx context.Context, client *httpx.Client, u *url.URL, headers httpx.Header) *Page {
	if p := probed(ctx, u); p.answers(client, headers) {
		return p
	}
	return nil
}

// answers reports whether p stands for a GET of its URL with headers
// through client: the one place a reader's request is held to the probe's.
// The probe navigated as a browser does, with no referer, through the
// registry's client on a route probed checks. A client of another identity —
// another User-Agent, language or session (httpx.Client.SameIdentity) — is
// served its own representation, and so is a request carrying any header
// but a referer: an Accept, a cookie, a credential or an agent of its own.
// A referer is the one header taken not to change a page, which every
// caller passing httpx.Referer to fetchDocument or fetchRoot relies on;
// a host whose page does depend on it fetches with GetPage instead. Other
// methods and request bodies never reach the probe: these readers GET.
func (p *Page) answers(client *httpx.Client, headers httpx.Header) bool {
	if p == nil {
		return false
	}
	if p.via != nil && client != nil && !p.via.SameIdentity(client) {
		return false
	}
	for name := range headers {
		if !strings.EqualFold(name, httpx.HeaderReferer) {
			return false
		}
	}
	return true
}

// acquire is the one place a reader's page is obtained: the registry's
// probe when it is the response this request would get (probedFor) and a
// successful document of its own (Page.Document), otherwise a GetPage of
// u. A probe that failed, answered another status, or was read only in
// part is never offered; the reader fetches for itself. The document's
// final URL is the caller's own either way, and the probe's tree, when
// recognition built one, is borrowed rather than parsed again.
//
// fetchDocument, readDocument and fetchRoot are this with the result in the
// form each reader wants it. probe is the page reused, nil after a fetch.
func acquire(ctx context.Context, client *httpx.Client, u *url.URL, headers httpx.Header) (body string, final *url.URL, probe *Page, err error) {
	if p := probedFor(ctx, client, u, headers); p != nil {
		if body, ok := p.Document(); ok {
			return body, cloneURL(p.Final), p, nil
		}
	}
	body, final, err = client.GetPage(ctx, u.String(), headers, nil)
	return body, final, nil, err
}

// fetchDocument is GetPage for the submitted URL that reuses the registry's
// probe when it already holds the document, so a page an extractor reads
// whole is fetched once rather than once to recognise and again to read.
// Any other URL, a probe that did not get the page, or a request the probe
// does not answer (Page.answers) is fetched as usual.
func fetchDocument(ctx context.Context, client *httpx.Client, u *url.URL, headers httpx.Header) (string, *url.URL, error) {
	body, final, _, err := acquire(ctx, client, u, headers)
	if err != nil {
		return "", nil, err
	}
	return body, final, nil
}

// document is a page read whole, as a reader may want it: its body, where
// it was served from, and its tree, parsed on first use — the probe's own
// when recognition parsed the page, which is shared and only read.
type document struct {
	body  string
	final *url.URL
	probe *Page

	once    sync.Once
	root    *html.Node
	rootErr error
}

// parse is the document's tree, built once: the probe's when recognition
// already parsed it, otherwise the body parsed here, with the error of a
// body that will not parse. Taking the probe's tree never indexes a page
// that recognition did not, so a reader that wants only a tree costs no
// more than its own parse.
func (d *document) parse() (*html.Node, error) {
	d.once.Do(func() { d.root, d.rootErr = parseDocument(d.body, d.probe) })
	return d.root, d.rootErr
}

// parseDocument is body's tree: probe's when recognition already parsed it,
// otherwise body parsed now, with the error of one that will not parse.
func parseDocument(body string, probe *Page) (*html.Node, error) {
	if probe != nil {
		if root, _ := probe.parsedRoot(); root != nil {
			return root, nil
		}
	}
	return parseHTML(body)
}

// tree is the parsed document, or nil for a body that will not parse; a
// reader that needs it says so.
func (d *document) tree() *html.Node {
	root, _ := d.parse()
	return root
}

// readDocument is fetchDocument with the page's tree to hand: a reader that
// scans the body never parses it, and one that reads the tree parses it once
// or takes the probe's.
func readDocument(ctx context.Context, client *httpx.Client, u *url.URL, headers httpx.Header) (*document, error) {
	body, final, probe, err := acquire(ctx, client, u, headers)
	if err != nil {
		return nil, err
	}
	return &document{body: body, final: final, probe: probe}, nil
}

// fetchPage is fetchDocument for a link held as a string.
func fetchPage(ctx context.Context, client *httpx.Client, link string, headers httpx.Header) (string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return "", err
	}
	doc, _, err := fetchDocument(ctx, client, u, headers)
	return doc, err
}

// fetchRoot is fetchDocument parsed: the probe's own tree when it is reused,
// so a document read to recognise it is not parsed a second time. That tree
// is the page's and is shared: a reader that removes or rewrites nodes parses
// its own copy instead. A document recognition did not parse — its
// recognisers only scanned the body, or the probe did not take it for HTML
// for want of a declared type — is still the one asked for, and is parsed
// here rather than fetched again. The final URL is the caller's own.
func fetchRoot(ctx context.Context, client *httpx.Client, u *url.URL, headers httpx.Header) (*html.Node, *url.URL, error) {
	body, final, probe, err := acquire(ctx, client, u, headers)
	if err != nil {
		return nil, nil, err
	}
	root, err := parseDocument(body, probe)
	return root, final, err
}

// fetchPageRoot is fetchRoot for a link held as a string.
func fetchPageRoot(ctx context.Context, client *httpx.Client, link string, headers httpx.Header) (*html.Node, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, err
	}
	root, _, err := fetchRoot(ctx, client, u, headers)
	return root, err
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

// index parses the document once and reads its head in the same pass.
func (p *Page) index() *pageIndex {
	p.parseOnce.Do(func() {
		if !p.IsHTML() {
			return
		}
		root, err := parseHTML(p.Body)
		if err != nil {
			return
		}
		x := &p.parsed
		x.root, x.meta = root, map[string]string{}
		walk(root, func(n *html.Node) {
			switch {
			case isElem(n, atom.Meta):
				content := strings.TrimSpace(attr(n, "content"))
				for _, key := range []string{attr(n, "property"), attr(n, "name")} {
					if _, seen := x.meta[key]; key != "" && !seen {
						x.meta[key] = content
					}
				}
			case isElem(n, atom.Title):
				if x.title == "" {
					x.title = strings.TrimSpace(textOf(n))
				}
			case isElem(n, atom.Script):
				if src, err := url.Parse(attr(n, "src")); err == nil && src.Path != "" {
					x.scripts = append(x.scripts, src.Path)
				}
			}
		})
		for _, key := range []string{"og:site_name", "application-name", "apple-mobile-web-app-title"} {
			if v := x.meta[key]; v != "" {
				x.names = append(x.names, v)
			}
		}
		if x.title != "" {
			parts := titleSeparators.Split(x.title, -1)
			x.names = append(x.names, parts[0])
			if len(parts) > 1 {
				x.names = append(x.names, parts[len(parts)-1])
			}
		}
	})
	p.indexed.Store(true)
	return &p.parsed
}

// parsedRoot is the page's tree when recognition has already parsed it,
// and false when nothing has yet. A reader that only wants a tree, not the
// index Page.HTML builds beside it, parses the body itself in that case:
// that costs less than the index, and a page whose recognisers only scan
// its body is never indexed for nothing.
func (p *Page) parsedRoot() (*html.Node, bool) {
	if !p.indexed.Load() {
		return nil, false
	}
	return p.parsed.root, true
}

// HTML is the parsed document, or nil when the body is not one.
func (p *Page) HTML() *html.Node { return p.index().root }

// hasElement reports whether the document has an element the test accepts.
func (p *Page) hasElement(test func(n *html.Node) bool) bool {
	root := p.HTML()
	return root != nil && findFirst(root, test) != nil
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
	return slices.ContainsFunc(fragments, func(f string) bool { return p.Has(f) })
}

// Meta is a meta element's content by name or property.
func (p *Page) Meta(key string) string { return p.index().meta[key] }

// Generator is the generator meta tag, lower case: what software made the
// page, by its own account.
func (p *Page) Generator() string { return strings.ToLower(p.Meta("generator")) }

// Title is the document's title, trimmed.
func (p *Page) Title() string { return p.index().title }

// titleSeparators split a title into the page's own name and the site's.
var titleSeparators = regexp.MustCompile(`\s+[-|—–·:»]{1,2}\s+`)

// Names are what the page says the site is called: its og:site_name,
// application-name and web-app title, and the first and last parts of its
// title. A site's own name is the steadiest thing its pages carry — bundles
// are renamed with every deploy, while the name is on the root, on every
// content page and on its error pages alike.
func (p *Page) Names() []string { return p.index().names }

// namedBy reports whether one of the page's names for its site, trimmed,
// passes the test.
func (p *Page) namedBy(test func(name string) bool) bool {
	return slices.ContainsFunc(p.Names(), func(name string) bool { return test(strings.TrimSpace(name)) })
}

// Named reports whether the page calls its site any of the names, ignoring
// case and surrounding space.
func (p *Page) Named(names ...string) bool {
	return p.namedBy(func(have string) bool {
		return slices.ContainsFunc(names, func(want string) bool { return strings.EqualFold(have, want) })
	})
}

// namedPrefix reports whether one of the page's names for its site begins
// with the prefix, ignoring case: for a site whose name carries a varying
// tail, as "<Name> ex <old name>" does.
func (p *Page) namedPrefix(prefix string) bool {
	return p.namedBy(func(name string) bool {
		return len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix)
	})
}

// Script reports whether the document loads a script whose path contains
// the fragment. Only the path is compared, never the host it is served from.
func (p *Page) Script(fragment string) bool {
	return slices.ContainsFunc(p.index().scripts, func(path string) bool { return strings.Contains(path, fragment) })
}

// playerMetaKeys are the meta tags a video page names its embeddable player
// in. A site's own player is the steadiest structural fact a watch page
// carries: it is what every other site embeds, so it survives redesigns of
// the page around it.
var playerMetaKeys = []string{"twitter:player", "og:video:url", "og:video:secure_url", "og:video"}

// playerMeta reports whether the page names a player whose URL has the
// form; only its path, query and fragment are read.
func (p *Page) playerMeta(form func(*url.URL) bool) bool {
	return slices.ContainsFunc(playerMetaKeys, func(key string) bool {
		u, err := url.Parse(p.Meta(key))
		return err == nil && u.Path != "" && form(u)
	})
}

// frames reports whether the document frames a page whose URL, resolved
// against the page's own, has the form. A landing page that wraps a player
// is recognised by the player it frames.
func (p *Page) frames(form func(*url.URL) bool) bool {
	return p.hasElement(func(n *html.Node) bool {
		if !isElem(n, atom.Iframe) {
			return false
		}
		src, err := p.Final.Parse(attr(n, "src"))
		return err == nil && form(src)
	})
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

// pathQueryShape is pathShape for a form that names its query too: the
// pattern is matched against the escaped path, "?" and the raw query.
func pathQueryShape(pattern string) func(*url.URL) bool {
	re := regexp.MustCompile(pattern)
	return func(u *url.URL) bool { return re.MatchString(u.EscapedPath() + "?" + u.RawQuery) }
}

// hostMemo remembers one answer per host for the extractor's lifetime: what
// a platform install established about itself — the software it runs, where
// it keeps its API — which no install changes between one paste and the next.
// The zero value is ready to use.
type hostMemo[T any] struct {
	mu sync.Mutex
	m  map[string]T
}

// lookup answers for host from memory, or with compute, remembering what it
// answered. A failure is not remembered: the next paste asks again.
func (h *hostMemo[T]) lookup(host string, compute func() (T, error)) (T, error) {
	host = strings.ToLower(host)
	h.mu.Lock()
	v, ok := h.m[host]
	h.mu.Unlock()
	if ok {
		return v, nil
	}
	v, err := compute()
	if err != nil {
		return v, err
	}
	h.mu.Lock()
	if h.m == nil {
		h.m = make(map[string]T)
	}
	h.m[host] = v
	h.mu.Unlock()
	return v, nil
}
