// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"golang.org/x/net/html"
)

// listingLink is one entry of a paged listing: the page it links to, its
// title when the card names one, and the key that tells repeats apart — its
// URL when empty.
type listingLink struct {
	key, url, title string
}

// listingPage is what one page of a listing offers its walk.
type listingPage struct {
	links []listingLink
	// title is the listing's own, read off its first page.
	title string
	// next is the following page. badNext marks a pager that offers one
	// which cannot be followed — off the listing, or not the page after
	// this one — which ends the walk as "partial — invalid next page".
	next    *url.URL
	badNext bool
	// note is why a missing page is missing, in place of the walk's own
	// words.
	note string
	// missing marks a page without its listing; end one that says the
	// listing has nothing more, which is no failure.
	missing, end bool
	// skip marks a page with nothing usable on it that still says the
	// listing goes on (pageOf.skip).
	skip bool
}

// listingWalk describes a paged listing to walkListing.
type listingWalk struct {
	name  string // the extractor, prefixing errors
	title string // the title when the first page names none
	// none is the error when the listing yields nothing; without one, an
	// empty listing is no error and the caller decides.
	none string
	// limit caps the entries; a negative one caps nothing.
	limit int
	// limitNote is the note for a walk cut short by limit.
	limitNote string
	// consume opens each page's admitted links with a separate file
	// budget; see pageWalk.consume.
	consume     func([]listingLink) (done, cut bool)
	consumeNote string
	// pages caps the pages read, as pageWalk.pages does.
	pages int
	// read takes one parsed page apart; page is where it was served from
	// and first the listing's first page, as served. A listing paged by
	// number rather than by a link names its next page here too.
	read func(root *html.Node, page, first *url.URL) listingPage

	// The rest is for listings the defaults do not fit.

	// root is the first page, already fetched and parsed by the caller,
	// which has then said for itself what failing to read it means.
	root *html.Node
	// fetch reads a page in place of fetchRoot with the first page as
	// referer, and reports no progress for it. What it cannot read or
	// parse is a page that could not be fetched.
	fetch func(ctx context.Context, page, first *url.URL) (*html.Node, *url.URL, error)
	// key names a page for spotting one read twice; its URL when nil.
	key func(*url.URL) string
	// anyRedirect leaves where a page was served from to read, which marks
	// one that left the listing missing. Without it the first page must
	// keep its path, and a later one its site, path and query.
	anyRedirect bool
	// missing is the error for a first page without its listing, in place
	// of "no video listing on this page".
	missing string
	// ends and failed are walkPages' own: the ends the listing takes as
	// its own, and the note for a later page that fails.
	ends   walkAccepts
	failed func(error) (string, bool)
}

// listingLimit is how many entries a listing may queue as files: the
// lower of the source and file caps, with the note naming which one.
func listingLimit(opts Options) (int, string) {
	if opts.maxFiles() < opts.maxSources() {
		return opts.maxFiles(), fileLimitNote
	}
	return opts.maxSources(), sourceLimitNote
}

// entryLimit bounds entries queued as files, including deferred videos and
// image-host rows. Readers opening children while listing use a separate
// inspection budget and count produced files through pageWalk.consume.
// A host's lower cap (zero for none) is noted as the listing limit.
func entryLimit(opts Options, hostCap int) (int, string) {
	if hostCap > 0 && hostCap < opts.maxFiles() {
		return hostCap, listingLimitNote
	}
	return opts.maxFiles(), fileLimitNote
}

// walkListing follows a listing from first through the pages its pager
// links, as walkPages walks it, and returns its entries, its title and the
// note for a walk that stopped short. A first page that fails, redirects
// elsewhere or has no listing is an error; a later one ends the walk with
// what was already read.
func walkListing(ctx context.Context, client *httpx.Client, first *url.URL, w listingWalk) (links []listingLink, title, note string, err error) {
	title, page := w.title, first
	key := w.key
	if key == nil {
		key = (*url.URL).String
	}
	pages := make(map[string]bool)
	links, note, err = walkPages(ctx, pageWalk[listingLink]{
		key:   func(link listingLink) string { return cmp.Or(link.key, link.url) },
		limit: w.limit, limitNote: w.limitNote, ends: w.ends, failed: w.failed, pages: w.pages,
		consume: w.consume, consumeNote: w.consumeNote,
		fetch: func(ctx context.Context, n int) (pageOf[listingLink], error) {
			const repeated = "partial — repeated page"
			if pages[key(page)] {
				return pageOf[listingLink]{stop: repeated, stopped: stopRepeatedPage}, nil
			}
			pages[key(page)] = true
			var (
				root  *html.Node
				final *url.URL
				err   error
			)
			switch {
			case n == 0 && w.root != nil:
				root, final = w.root, page
			case w.fetch != nil:
				root, final, err = w.fetch(ctx, page, first)
			default:
				ResolveNote(ctx, fmt.Sprintf("Reading listing page %d", n+1))
				root, final, err = fetchRoot(ctx, client, page, httpx.Referer(first.String()))
			}
			if err != nil {
				if n == 0 {
					return pageOf[listingLink]{}, fmt.Errorf("%s: fetch listing: %w", w.name, err)
				}
				return pageOf[listingLink]{}, err
			}
			if key(final) != key(page) && pages[key(final)] {
				return pageOf[listingLink]{stop: repeated, stopped: stopRepeatedPage}, nil
			}
			pages[key(final)] = true
			if n == 0 {
				if !w.anyRedirect && !samePath(final, first) {
					return pageOf[listingLink]{}, fmt.Errorf("%s: listing redirected to another page", w.name)
				}
				first = final
			} else if !w.anyRedirect && !sameListingPage(final, page) {
				return pageOf[listingLink]{stop: "partial — next page redirected to another listing", stopped: stopRedirected}, nil
			}
			p := w.read(root, final, first)
			switch {
			case p.end:
				return pageOf[listingLink]{end: true}, nil
			case p.missing && n == 0:
				return pageOf[listingLink]{}, fmt.Errorf("%s: %s", w.name, cmp.Or(w.missing, "no video listing on this page"))
			case p.missing:
				return pageOf[listingLink]{stop: "partial — listing is missing on next page", stopped: stopMissing, note: p.note}, nil
			}
			if n == 0 {
				title = cmp.Or(p.title, title)
			}
			if p.badNext {
				p.next, p.note = nil, invalidNextNote
			}
			page = p.next
			return pageOf[listingLink]{items: p.links, next: p.next != nil, note: p.note, skip: p.skip}, nil
		},
	})
	if err != nil {
		return nil, "", "", err
	}
	if len(links) == 0 && w.none != "" {
		return nil, "", "", fmt.Errorf("%s: %s", w.name, w.none)
	}
	return links, title, note, nil
}

// listingURLs is the pages a listing's links point to, in order.
func listingURLs(links []listingLink) []string {
	urls := make([]string, 0, len(links))
	for _, link := range links {
		urls = append(urls, link.url)
	}
	return urls
}

// fetchAsAsked is a listingWalk fetch for a listing that reads each page
// with headers as the page it asked for, wherever it was served from.
func fetchAsAsked(client *httpx.Client, headers httpx.Header) func(context.Context, *url.URL, *url.URL) (*html.Node, *url.URL, error) {
	return func(ctx context.Context, page, _ *url.URL) (*html.Node, *url.URL, error) {
		doc, _, err := client.GetPage(ctx, page.String(), headers, nil)
		if err != nil {
			return nil, nil, err
		}
		root, err := parseHTML(doc)
		return root, page, err
	}
}

// webLink resolves ref against base as an http or https link with a host
// and no credentials, dropping its fragment; nil for anything else.
func webLink(base *url.URL, ref string) *url.URL {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	u, err := base.Parse(ref)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil
	}
	u.Fragment, u.RawFragment = "", ""
	return u
}

// sameSite reports whether a and b share their scheme and host.
func sameSite(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

// sameListingPage reports whether a later page was served from the page
// asked for, allowing for what a server's canonical redirect changes
// without changing the page: a scheme upgraded to https, the host's case, a
// trailing slash, and the order or escaping of the query's parameters. Only
// a redirect that changes what is asked for ends the walk.
func sameListingPage(final, page *url.URL) bool {
	if !strings.EqualFold(final.Host, page.Host) {
		return false
	}
	if final.Scheme != page.Scheme && (page.Scheme != "http" || final.Scheme != "https") {
		return false
	}
	return samePath(final, page) && final.Query().Encode() == page.Query().Encode()
}

// samePath reports whether a and b name one path, trailing slash aside.
func samePath(a, b *url.URL) bool {
	return strings.TrimRight(a.Path, "/") == strings.TrimRight(b.Path, "/")
}
