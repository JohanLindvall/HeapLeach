// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Player pages: one page, one video, and the renditions its player is
// given, read out of the page or out of the endpoint the player asks. Every
// such site is one row of a table (playersites.go), and one engine does the
// rest: read the page, take the best rendition (bestCandidate), and either
// follow a playlist down to its segments or hand over the file. A row can also take
// a listing of the site's videos, each queued by its card and read the same
// way when its transfer starts.
//
// Most of these sites sign their links for minutes or hours and tie them to
// the visit that produced them, so a row marked signed is read again when
// the item's transfer starts: a link signed while it waited in a queue
// would be spent before its turn came. The page is read again rather than
// the link reused, segments and key included. A row that peeks goes further
// and reads nothing signed until the transfer starts.
//
// The rows that read a player's script are the most fragile extractors in
// the program: each depends on the exact shape of a script the host can
// change at any time, which is why each reads the numbers it needs (the
// substring offset, the packer's parameters) from the page rather than
// hard-coding what they were on the day it was written.
type Player struct {
	about
	client *httpx.Client
	site   *playerSite
}

// playerSite is one site's row.
type playerSite struct {
	about
	// recognise takes a probed page by the player configuration read reads.
	recognise func(p *Page) bool
	// page is the URL read for a pasted link: the one form of the video's
	// page that serves its player. Nil reads the link as it is. An error
	// says the link names no video.
	page func(u *url.URL) (string, error)
	// probeCanonical validates the canonical player's markup when an alias
	// answers with an unrelated page. It is opt-in and only follows a URL
	// whose submitted shape matches, with one bounded identification request.
	probeCanonical bool
	// referer is the Referer every media request carries, given the page,
	// unless a read names its own (playerVideo.referer). Nil, with none
	// named, sends no Referer.
	referer func(page string) string
	// read fetches the page and reports what its player is given.
	read func(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error)
	// parse, in read's place, is a row whose page is all it reads: the
	// engine fetches the page (readDocument, so the probe's own page and
	// tree when it holds them), with asks's Referer, and parse reads what
	// the player is given off it. A row has read or parse, never both; an
	// API, a handshake or another request keeps a read of its own.
	parse func(page *playerPage) (*playerVideo, error)
	// asks is the Referer the page itself is fetched with, given it; nil
	// sends none. It is the page's, apart from referer, the media's.
	asks func(page string) string
	// signed reads the page again when the transfer starts. withheld
	// leaves the link off the queued file as well, which carries only its
	// name and size until then.
	signed, withheld bool
	// peek, when set, puts the whole read off until the transfer starts,
	// and reads it again on recovery: Extract learns only the video's
	// title, and that it plays. For a site whose player spends its
	// signature by being read at all.
	peek func(ctx context.Context, client *httpx.Client, page string) (string, error)
	// ext names a video queued before it is read, a peeked one or a
	// listing's: ".mp4" when empty.
	ext string
	// listing, when set, takes a listing of the site's videos.
	listing *playerListing
	// finished refuses a playlist still being written, for a site whose
	// videos are published while they are encoded: joined then, it would
	// finish as a fragment named as the whole. A site that streams live, or
	// does not mark its playlists' end, leaves it unset.
	finished bool
	// proxyFallback opts the site's sources and files into proxy recovery
	// after a temporary failure on the normal connection.
	proxyFallback bool
}

// playerListing is a row's listing of videos. Each entry is queued under
// its card's title and read, as a peeked video is, when its transfer
// starts.
type playerListing struct {
	// shape tells a listing from a video.
	shape func(u *url.URL) bool
	// walk names a paged listing's first page and how walkListing reads
	// it, the caps aside.
	walk func(u *url.URL) (*url.URL, listingWalk)
	// links, in walk's place, reads a listing that pages in a way of its
	// own, up to the listing cap (listingLimit): its entries, its title and
	// its note.
	links func(ctx context.Context, client *httpx.Client, u *url.URL, opts Options) ([]listingLink, string, string, error)
}

// playerVideo is what one reading of a page found.
type playerVideo struct {
	title      string
	candidates []mediaCandidate
	// ext names a plain file, ".mp4" when empty. A playlist is named after
	// the container its segments are.
	ext string
	// referer, when set, is the Referer of this read's media requests in
	// place of the row's: the address of a player only this read found.
	referer string
}

// NewPlayer builds the extractor for one row.
func NewPlayer(client *httpx.Client, site *playerSite) *Player {
	return &Player{about: site.about, client: client, site: site}
}

// Recognise reports whether the probed page is the site's player.
func (p *Player) Recognise(ctx context.Context, page *Page) bool {
	return p.site.recognise(page) || p.canonicalPage(ctx, page) != nil
}

// canonicalPage keeps the validated player on the original probe so Extract
// can read it without another request. The submitted form matters: a redirect
// to advertising can have discarded both the player's path and its markers.
func (p *Player) canonicalPage(ctx context.Context, page *Page) *Page {
	if !p.site.probeCanonical || p.site.page == nil || p.client == nil || page == nil || !p.Shape(page.URL) {
		return nil
	}
	endpoint, err := p.site.page(page.URL)
	if err != nil || endpoint == page.Final.String() {
		return nil
	}
	return memo(page, "player "+endpoint, func() *Page {
		ctx, cancel := context.WithTimeout(ctx, config.PlatformProbeTimeout)
		defer cancel()
		req, err := p.client.NewRequest(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil
		}
		httpx.Navigate.Apply(req)
		resp, err := p.client.DoOnce(req)
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		canonical, canonicalErr := p.site.page(resp.Request.URL)
		if canonicalErr != nil || canonical != endpoint {
			return nil
		}
		if resp.StatusCode != http.StatusOK || !mediaPageIsHTMLType(mediaPageMediaType(resp.Header.Get(httpx.HeaderContentType))) {
			return nil
		}
		body, err := httpx.ReadBody(resp.Body)
		if err != nil {
			return nil
		}
		candidate := &Page{URL: req.URL, Final: resp.Request.URL, Status: resp.StatusCode, Header: resp.Header, Body: string(body)}
		if !p.site.recognise(candidate) {
			return nil
		}
		return candidate
	})
}

// ProxyFallback reports whether the row opted into proxy recovery.
func (p *Player) ProxyFallback() bool { return p.site.proxyFallback }

// Extract resolves a video page to its best rendition, or queues a listing.
func (p *Player) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	if source := probed(ctx, u); source != nil {
		if canonical := p.canonicalPage(ctx, source); canonical != nil {
			ctx = withPage(ctx, canonical)
		}
	}
	if p.site.listing != nil && p.site.listing.shape(u) {
		return p.list(ctx, u, opts)
	}
	page := u.String()
	if p.site.page != nil {
		var err error
		if page, err = p.site.page(u); err != nil {
			return nil, fmt.Errorf("%s: %w", p.name, err)
		}
	}
	if p.site.peek != nil {
		title, err := p.site.peek(ctx, p.client, page)
		if err != nil {
			return nil, p.readError(page, err)
		}
		title = cmp.Or(title, p.name)
		return &Result{Title: title, Files: []File{p.deferred(page, title)}}, nil
	}
	first, title, err := p.resolve(ctx, page)
	if err != nil {
		return nil, err
	}
	file := first.file("")
	if p.site.signed {
		file = refreshing(first, func(ctx context.Context) (*Target, error) {
			fresh, _, err := p.resolve(ctx, page)
			return fresh, err
		})
		if p.site.withheld {
			file.URL = ""
		}
	}
	file.ProxyFallback = p.site.proxyFallback
	return &Result{Title: title, Files: []File{file}}, nil
}

// deferred is a video queued under title and read in full when its
// transfer starts, and again on recovery rather than whatever listed it.
func (p *Player) deferred(page, title string) File {
	resolve := func(ctx context.Context) (*Target, error) {
		target, _, err := p.resolve(httpx.WithoutAdmission(freshRead(ctx)), page)
		return target, err
	}
	return File{Name: title + cmp.Or(p.site.ext, ".mp4"), Size: -1,
		Resolve: resolve, Refresh: resolve, ProxyFallback: p.site.proxyFallback}
}

// list walks a listing, queueing each video it names unread.
func (p *Player) list(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	var links []listingLink
	var title, note string
	if read := p.site.listing.links; read != nil {
		var err error
		if links, title, note, err = read(ctx, p.client, u, opts); err != nil {
			return nil, fmt.Errorf("%s: %w", p.name, err)
		}
	} else {
		first, walk := p.site.listing.walk(u)
		walk.name = p.name
		walk.limit, walk.limitNote = listingLimit(opts)
		var err error
		if links, title, note, err = walkListing(ctx, p.client, first, walk); err != nil {
			return nil, err
		}
	}
	res := &Result{Title: title, Note: note}
	for _, link := range links {
		res.Files = append(res.Files, p.deferred(link.url, link.title))
	}
	return res, nil
}

// resolve reads the page once and turns its best rendition into a Target.
func (p *Player) resolve(ctx context.Context, page string) (*Target, string, error) {
	name := p.name
	video, err := p.read(ctx, page)
	if err != nil {
		return nil, "", p.readError(page, err)
	}
	best, ok := bestCandidate(video.candidates)
	if !ok {
		return nil, "", fmt.Errorf("%s: no playable rendition on %s (the video may have been removed)", name, page)
	}
	title := cmp.Or(video.title, name)
	referer := video.referer
	if referer == "" && p.site.referer != nil {
		referer = p.site.referer(page)
	}
	var headers httpx.Header
	if referer != "" {
		headers = httpx.Referer(referer)
	}
	if !best.IsHLS {
		return &Target{URL: best.URL, Name: title + cmp.Or(video.ext, ".mp4"), Size: knownSize(best.Size),
			Headers: headers}, title, nil
	}
	ResolveNote(ctx, "Reading video playlist")
	media, err := resolveMediaPlaylist(ctx, p.client, best.URL, headers)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %s: %w", name, page, err)
	}
	if live, why := hlsLiveEdge(media.Doc); live && p.site.finished {
		return nil, "", fmt.Errorf("%s: %s: the video's playlist is still being written: %s", name, page, why)
	}
	return media.target(best.URL, title+media.extension(), headers), title, nil
}

// read is the row's one acquisition: its own read, or the page fetched and
// handed to its parse.
func (p *Player) read(ctx context.Context, page string) (*playerVideo, error) {
	if p.site.parse == nil {
		return p.site.read(ctx, p.client, page)
	}
	return readPlayerPage(ctx, p.client, page, p.site.asks, p.site.parse)
}

// readPlayerPage fetches a player's page with asks's Referer and hands it
// to parse.
func readPlayerPage(ctx context.Context, client *httpx.Client, page string, asks func(string) string,
	parse func(*playerPage) (*playerVideo, error)) (*playerVideo, error) {
	u, err := url.Parse(page)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", page, err)
	}
	var headers httpx.Header
	if asks != nil {
		headers = httpx.Referer(asks(page))
	}
	doc, err := readDocument(ctx, client, u, headers)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", page, err)
	}
	return parse(&playerPage{document: doc, page: page, base: u})
}

// playerPage is a player's page as a row's parse reads it: the document,
// the page as asked for, and that page parsed, for resolving its links.
type playerPage struct {
	*document
	page string
	base *url.URL
}

// title is the document's title less the site's suffix (pageTitle), read
// off the tree.
func (d *playerPage) title() string { return trimSiteSuffix(d.rawTitle()) }

// rawTitle is the document's first title as written.
func (d *playerPage) rawTitle() string {
	if root := d.tree(); root != nil {
		return firstText(root, atomTitle)
	}
	return ""
}

// heading is the document's first <h1>, trimmed (firstHeading).
func (d *playerPage) heading() string {
	if root := d.tree(); root != nil {
		return strings.TrimSpace(firstText(root, atom.H1))
	}
	return ""
}

// readError is a failed read in the row's name. A 404 on the page is the
// video being gone, for every row, and still carries its status for the
// downloader to read.
func (p *Player) readError(page string, err error) error {
	if httpx.HasStatus(err, http.StatusNotFound) {
		return &playerGone{fmt.Sprintf("%s: %s is not there — the site says it was not found or has been removed",
			p.name, page), err}
	}
	return fmt.Errorf("%s: %w", p.name, err)
}

// playerGone says a video is gone in words, keeping the reply that said so.
type playerGone struct {
	msg string
	err error
}

func (e *playerGone) Error() string { return e.msg }
func (e *playerGone) Unwrap() error { return e.err }

// idPage is a row's page for a link from which id reads the video: the
// site's origin, then prefix and the id.
func idPage(id func(*url.URL) string, prefix, expect string) func(*url.URL) (string, error) {
	return func(u *url.URL) (string, error) {
		if v := id(u); v != "" {
			return util.Origin(u) + prefix + v, nil
		}
		return "", fmt.Errorf("%s names no video (expected %s)", u.Redacted(), expect)
	}
}

// pathID reads a video id out of /<id>, when bare allows it, or out of
// /<prefix>/<id> for any of the prefixes.
func pathID(u *url.URL, bare bool, prefixes ...string) string {
	segs := util.PathSegments(u)
	switch {
	case len(segs) >= 2 && slices.Contains(prefixes, segs[0]):
		return segs[1]
	case len(segs) == 1 && bare:
		return segs[0]
	}
	return ""
}

// playerLink is a page's one media link as a candidate.
func playerLink(link string) []mediaCandidate {
	return []mediaCandidate{{URL: link, IsHLS: isPlaylistLink(link)}}
}

// isPlaylistLink reports whether a media link names an HLS playlist. A
// progressive file handed to the playlist resolver would be read as text,
// every line of it a segment.
func isPlaylistLink(link string) bool { return strings.Contains(strings.ToLower(link), ".m3u8") }

// samePage refers media requests to the page itself.
func samePage(page string) string { return page }

// siteRoot refers media requests to one fixed site root.
func siteRoot(root string) func(string) string {
	return func(string) string { return root + "/" }
}

// pageOrigin refers media requests to the root of the page's own domain,
// for hosts that answer on several and reject a sibling's referer.
func pageOrigin(page string) string {
	if u, err := url.Parse(page); err == nil && u.Host != "" {
		return util.Origin(u) + "/"
	}
	return page
}

// ----------------------------------------------------------------- shared

// videoSource is a <source> element naming a video file.
func videoSource(n *html.Node) bool {
	return isElem(n, atom.Source) && attr(n, "src") != "" &&
		(strings.HasPrefix(attr(n, "type"), "video/") || strings.Contains(attr(n, "src"), ".mp4"))
}

// pageTitle reads a document title, trimmed of the site's own suffix.
func pageTitle(doc string) string { return trimSiteSuffix(firstTitleOf(doc)) }
