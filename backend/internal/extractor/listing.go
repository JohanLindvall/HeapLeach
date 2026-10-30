// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

// listingLink is one entry of a paged listing: the page it links to, its
// title when the card names one, and the key that tells repeats apart.
type listingLink struct {
	key, url, title string
}

// listingPage is what one page of a listing offers its walk.
type listingPage struct {
	links []listingLink
	// title is the listing's own, read off its first page.
	title string
	// next is the following page, and note why there is none when the
	// pager offered one that cannot be followed.
	next *url.URL
	note string
	// missing marks a page without its listing; end one that says the
	// listing has nothing more, which is no failure.
	missing, end bool
}

// listingWalk describes a paged listing to walkListing.
type listingWalk struct {
	name  string // the extractor, prefixing errors
	title string // the title when the first page names none
	none  string // the error when the listing yields nothing
	limit int
	// limitNote is the note for a walk cut short by limit.
	limitNote string
	// read takes one parsed page apart; page is where it was served from
	// and first the listing's first page, as served.
	read func(root *html.Node, page, first *url.URL) listingPage
}

// listingLimit is how many entries a listing may queue as files: the
// lower of the source and file caps, with the note naming which one.
func listingLimit(opts Options) (int, string) {
	if opts.maxFiles() < opts.maxSources() {
		return opts.maxFiles(), "partial — file limit"
	}
	return opts.maxSources(), "partial — source limit"
}

// walkListing follows a listing from first through the pages its pager
// links, up to config.MaxAlbumPages, and returns its entries in order
// without repeats, its title and the note for a walk that stopped short.
// A first page that fails, redirects elsewhere or has no listing is an
// error; a later one ends the walk with what was already read. A walk that
// ended while the caller cancelled is a cancellation, not a partial
// listing: the page that ended it may have been answered before its
// request saw the cancellation.
func walkListing(ctx context.Context, client *httpx.Client, first *url.URL, w listingWalk) (links []listingLink, title, note string, err error) {
	title, page := w.title, first
	seen, pages := make(map[string]bool), make(map[string]bool)
	for n := range config.MaxAlbumPages {
		if err := ctx.Err(); err != nil {
			return nil, "", "", err
		}
		if pages[page.String()] {
			note = "partial — repeated page"
			break
		}
		pages[page.String()] = true
		ResolveNote(ctx, fmt.Sprintf("Reading listing page %d", n+1))
		doc, final, err := fetchDocument(ctx, client, page, httpx.Referer(first.String()))
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", "", ctx.Err()
			}
			if n == 0 {
				return nil, "", "", fmt.Errorf("%s: fetch listing: %w", w.name, err)
			}
			note = "partial — could not fetch next page"
			break
		}
		if final.String() != page.String() && pages[final.String()] {
			note = "partial — repeated page"
			break
		}
		pages[final.String()] = true
		if n == 0 {
			if !samePath(final, first) {
				return nil, "", "", fmt.Errorf("%s: listing redirected to another page", w.name)
			}
			first = final
		} else if !sameSite(final, page) || !samePath(final, page) || final.RawQuery != page.RawQuery {
			note = "partial — next page redirected to another listing"
			break
		}
		root, err := parseHTML(doc)
		if err != nil {
			return nil, "", "", err
		}
		p := w.read(root, final, first)
		if p.end {
			break
		}
		if p.missing {
			if n == 0 {
				return nil, "", "", fmt.Errorf("%s: no video listing on this page", w.name)
			}
			note = "partial — listing is missing on next page"
			break
		}
		if n == 0 {
			title = util.FirstNonEmpty(p.title, title)
		}
		before := len(links)
		for _, link := range p.links {
			if seen[link.key] {
				continue
			}
			seen[link.key] = true
			if len(links) == w.limit {
				return links, title, w.limitNote, nil
			}
			links = append(links, link)
		}
		if len(links) == before {
			note = "partial — repeated or empty page"
			break
		}
		if p.next == nil {
			note = p.note
			break
		}
		if len(links) == w.limit {
			note = w.limitNote
			break
		}
		if n == config.MaxAlbumPages-1 {
			note = "partial — page limit"
		}
		page = p.next
	}
	if err := ctx.Err(); err != nil {
		return nil, "", "", err
	}
	if len(links) == 0 {
		return nil, "", "", fmt.Errorf("%s: %s", w.name, w.none)
	}
	return links, title, note, nil
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
	u.Fragment = ""
	return u
}

// sameSite reports whether a and b share their scheme and host.
func sameSite(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

// samePath reports whether a and b name one path, trailing slash aside.
func samePath(a, b *url.URL) bool {
	return strings.TrimRight(a.Path, "/") == strings.TrimRight(b.Path, "/")
}
