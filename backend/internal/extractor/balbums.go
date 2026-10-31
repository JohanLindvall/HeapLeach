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
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Balbums searches an index of somebody else's albums.
//
// The site hosts nothing. It is a catalogue of bunkr albums — six hundred
// thousand of them — and a search over it answers with a grid of cards, each
// linking to an album on bunkr itself. So there is no media here to fetch,
// and the work is to walk the result pages and hand every album to the
// extractor that does host it, which is the same composition the harvester
// uses and shares its machinery (sources.go). Each album keeps its own
// extractor's files untouched, resolver included, and lands in a folder of
// its own.
//
// Only a search is accepted. Every other page on the site is either rendered
// in script — the top-album and top-file charts carry no links in the markup
// an anonymous fetch receives — or is the front page, which is the same grid
// over the whole index, twenty thousand pages of it, and nobody pasting a
// bare domain is asking for that.
type Balbums struct {
	about
	client   *httpx.Client
	registry *Registry
}

const balbumsRoot = "https://balbums.st"

// balbumsPerPage is how many results one page is asked for.
//
// Twenty is the site's own default, and a hundred is the most it honours.
// Asking for more is not refused: five hundred is ignored and served as
// twenty, under a page count for twenty, so a walk that trusted the
// parameter would collect a fifth of what it thought. Nothing here trusts
// it — the stated page count and a page that adds nothing are what stop the
// walk — but a hundred is what makes a search of three hundred albums four
// requests instead of eighteen.
const balbumsPerPage = 100

// balbumsPages reads "Page 1 of 18", which is how a result page states how
// far the search goes. A search with no matches states nought pages.
var balbumsPages = regexp.MustCompile(`(?i)page\s+\d+\s+of\s+(\d+)`)

// NewBalbums builds the balbums extractor. Like the harvester it resolves
// what it finds through the registry it is part of, so it is handed that
// registry rather than building one.
func NewBalbums(client *httpx.Client, registry *Registry) *Balbums {
	return &Balbums{client: client, registry: registry, about: about{name: "balbums", shape: balbumsShape,
		recognises: "searches of a Bunkr album index (`/?search=<query>`) whose page carries the index's search form " +
			"and its \"Page n of m\" result count"}}
}

// balbumsShape is a search of the index: its root with a search query.
func balbumsShape(u *url.URL) bool {
	_, _, ok := balbumsSearch(u)
	return ok && (u.EscapedPath() == "" || u.EscapedPath() == "/")
}

// Recognise takes a search whose page has the index's own search form and
// states its page count, which is what walk pages by.
func (b *Balbums) Recognise(_ context.Context, p *Page) bool {
	return balbumsShape(p.Final) && p.Has(`id="searchForm"`, `name="search"`) && balbumsPages.MatchString(p.Body)
}

// Extract walks a search and resolves every album it lists.
func (b *Balbums) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	query, first, ok := balbumsSearch(u)
	if !ok {
		return nil, fmt.Errorf("balbums: %s names no search — this site indexes albums "+
			"hosted elsewhere, and its other pages are either built in the browser or "+
			"list the whole catalogue; paste a /?search=<query> URL", u.Redacted())
	}

	albums, found, err := b.walk(ctx, first, query, opts.maxSources())
	if err != nil {
		return nil, err
	}
	if len(albums) == 0 {
		return nil, fmt.Errorf("balbums: nothing matches %q", query)
	}

	e := expandSources(ctx, b.registry, albums, opts)
	if err := noneResolved(ctx, e.files, "balbums", len(albums), "albums", fmt.Sprintf("the search for %q", query), e.refusals, e.failed); err != nil {
		return nil, err
	}
	return &Result{
		Title: query,
		Note:  partialNote("albums", e, found, len(albums) < found),
		Files: e.files,
	}, nil
}

// walk reads the result pages and collects the albums they link to, with the
// number the search claims to have.
//
// It stops at the cap rather than collecting everything and truncating
// afterwards, because a two-word search over six hundred thousand albums
// states twenty thousand pages and walking them to throw all but five away
// would be a lot of requests to reach the same answer.
func (b *Balbums) walk(ctx context.Context, first *url.URL, query string, limit int) (albums []string, found int, err error) {
	headers := httpx.Referer(balbumsRoot + "/")
	doc, err := b.client.GetString(ctx, balbumsPage(first, 1), headers)
	if err != nil {
		return nil, 0, fmt.Errorf("balbums: search for %q: %w", query, err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, 0, nil
	}
	total := 0
	if m := balbumsPages.FindStringSubmatch(doc); m != nil {
		total, _ = strconv.Atoi(m[1])
	}

	// The pages are numbered, and the search states how many there are.
	// The walk ends at the last of them, at a page with nothing new on it
	// (the end of the results), or at the cap, where found is what the
	// search has in all: what was left is declared in the job's note
	// rather than passed over quietly.
	seen := make(map[string]bool)
	page, capped := 0, false
	start, _ := url.Parse(balbumsPage(first, 1))
	links, _, _, err := walkListing(ctx, b.client, start, listingWalk{
		name: "balbums", limit: -1, quiet: true, root: root,
		fetch: fetchAsAsked(b.client, headers),
		read: func(root *html.Node, _, _ *url.URL) listingPage {
			page++
			var p listingPage
			for _, link := range supportedSources(b.registry, balbumsLinks(root, first), b) {
				p.links = append(p.links, listingLink{url: link})
				seen[link] = true
			}
			switch {
			case len(seen) >= limit:
				capped, found = true, len(seen)
				if total > 0 {
					found = total * len(seen) / page
				}
			case total > 0 && page >= total:
			default:
				p.next, _ = url.Parse(balbumsPage(first, page+1))
			}
			return p
		},
	})
	if err != nil {
		return nil, 0, err
	}
	albums = listingURLs(links)
	if capped {
		return albums[:limit], found, nil
	}
	return albums, len(albums), nil
}

// balbumsSearch reads the query out of a URL and returns the first page of
// its results.
//
// Everything the URL carries is kept but the paging: the site takes a mode
// and a sort, and somebody who narrowed their search before pasting it meant
// the narrowing. A pasted later page means the whole search, the way a
// member's section means the member, so the page number is dropped rather
// than started from.
func balbumsSearch(u *url.URL) (query string, first *url.URL, ok bool) {
	query = strings.TrimSpace(u.Query().Get("search"))
	if query == "" {
		return "", nil, false
	}
	page := *u
	q := u.Query()
	q.Del("page")
	q.Set("per", strconv.Itoa(balbumsPerPage))
	page.RawQuery = q.Encode()
	return query, &page, true
}

// balbumsPage names one page of the results.
func balbumsPage(first *url.URL, page int) string {
	target := *first
	q := first.Query()
	q.Set("page", strconv.Itoa(page))
	target.RawQuery = q.Encode()
	return target.String()
}

// balbumsLinks collects what the result cards point at.
//
// Anchors only, and no filter of its own beyond that: a card is an anchor
// wrapping a thumbnail, and which of a page's anchors are albums is a
// question the registry answers better than a selector would — the site's
// own navigation is on its own host and drops out as unsupported, while
// anything it starts indexing besides bunkr is picked up for free. Reading
// the page's text as the harvester does would be wrong here, since a
// catalogue prints album names and counts that are nobody's link.
func balbumsLinks(root *html.Node, base *url.URL) []string {
	var out []string
	for _, a := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		if link := resolveRef(base, attr(a, "href")); link != "" {
			out = append(out, link)
		}
	}
	return util.Dedupe(out)
}
