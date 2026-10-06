// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

// Bandcamp resolves albums, tracks and a band's whole discography natively,
// filing each track as <band>/<album>/<NN> - <title>.mp3.
//
// It used to be a yt-dlp handoff, because every endpoint answered with a
// client challenge — but the challenge is aimed at browsers, not at
// programs: a request that says it is a browser gets the three-kilobyte
// challenge page, while one that says what it is gets the real page. So
// every request here states bandcampAgent instead of the browser string the
// client sends everywhere else.
//
// The page carries everything in its data-tralbum attribute: the band, the
// release title, and per track its number, title and a stream link. That
// link is the 128 kbps MP3 anyone may play, and it is the same file yt-dlp
// fetched, byte for byte — nothing better is served without a purchase. It
// is signed for a day, so Resolve re-reads the page for a fresh one when the
// track's turn comes; a long queue would otherwise outlive it.
//
// A band's front page answers 403 whatever the client says, but /music is
// the same listing and is served, so a discography is read from there and
// every release on it resolved in turn.
type Bandcamp struct {
	client *httpx.Client
}

// bandcampAgent is the User-Agent every bandcamp request states. Anything
// that does not claim to be a browser is served; see Bandcamp.
const bandcampAgent = "HeapLeach"

// bandcampStream is the one format served without a purchase.
const bandcampStream = "mp3-128"

// NewBandcamp builds the bandcamp extractor.
func NewBandcamp(client *httpx.Client) *Bandcamp { return &Bandcamp{client: client} }

func (b *Bandcamp) Name() string { return "bandcamp" }

// Match accepts a band's subdomain: a release, or the band itself.
func (b *Bandcamp) Match(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(host, ".bandcamp.com") || host == "www.bandcamp.com" {
		return false
	}
	segs := util.PathSegments(u)
	switch {
	case len(segs) == 0:
		return true
	case len(segs) == 1:
		return segs[0] == "music"
	default:
		return segs[0] == "album" || segs[0] == "track"
	}
}

// Extract resolves a release, or every release a band lists.
func (b *Bandcamp) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) >= 2 {
		release, err := b.release(ctx, u.String())
		if err != nil {
			return nil, err
		}
		return &Result{Title: release.title(), Files: release.files(b)}, nil
	}
	return b.discography(ctx, u)
}

// discography resolves every release on a band's /music page.
func (b *Bandcamp) discography(ctx context.Context, u *url.URL) (*Result, error) {
	music := util.Origin(u) + "/music"
	root, err := b.page(ctx, music)
	if err != nil {
		return nil, err
	}
	// A band with one release has /music send it straight to that release.
	// The listing carries release data too, but empty and of no kind, so
	// the kind is what tells the two apart.
	if release, err := bandcampRelease(root, music); err == nil && (release.kind == "album" || release.kind == "track") {
		return &Result{Title: release.title(), Files: release.files(b)}, nil
	}

	links := bandcampReleaseLinks(root, u)
	if len(links) == 0 {
		return nil, fmt.Errorf("bandcamp: %s lists no releases", u.Redacted())
	}
	files := FanOut(ctx, links, func(ctx context.Context, link string) ([]File, error) {
		release, err := b.release(ctx, link)
		if err != nil {
			return nil, err
		}
		return release.files(b), nil
	})
	if len(files) == 0 {
		return nil, fmt.Errorf("bandcamp: none of the %d releases on %s could be read", len(links), u.Redacted())
	}
	band := util.FirstNonEmpty(metaContent(root, "og:site_name"), metaContent(root, "og:title"), u.Hostname())
	return &Result{Title: band, Files: files}, nil
}

// release reads one album or track page.
func (b *Bandcamp) release(ctx context.Context, link string) (*bandcampPage, error) {
	root, err := b.page(ctx, link)
	if err != nil {
		return nil, err
	}
	return bandcampRelease(root, link)
}

// page fetches and parses a page as something other than a browser.
func (b *Bandcamp) page(ctx context.Context, link string) (*html.Node, error) {
	doc, err := b.client.GetString(ctx, link, bandcampHeaders())
	if err != nil {
		return nil, fmt.Errorf("bandcamp: fetch %s: %w", link, err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("bandcamp: parse %s: %w", link, err)
	}
	return root, nil
}

func bandcampHeaders() httpx.Header { return httpx.Header{httpx.HeaderUserAgent: bandcampAgent} }

// bandcampPage is what an album or track page says about itself.
type bandcampPage struct {
	link   string
	artist string
	kind   string // "album" or "track"
	name   string // the release's own title
	album  string // the album a track page belongs to, if any
	tracks []bandcampTrack
}

type bandcampTrack struct {
	Num    int               `json:"track_num"`
	Title  string            `json:"title"`
	Artist string            `json:"artist"`
	ID     int64             `json:"track_id"`
	File   map[string]string `json:"file"`
}

// bandcampData returns the page's data-tralbum document, or "".
func bandcampData(root *html.Node) string {
	n := findFirst(root, func(n *html.Node) bool { return n.Type == html.ElementNode && hasAttr(n, "data-tralbum") })
	if n == nil {
		return ""
	}
	return attr(n, "data-tralbum")
}

// bandcampRelease reads a release out of its page.
func bandcampRelease(root *html.Node, link string) (*bandcampPage, error) {
	data := bandcampData(root)
	if data == "" {
		return nil, fmt.Errorf("bandcamp: %s carries no release data (the page may be a purchase-only or removed item)", link)
	}
	var t struct {
		Artist   string `json:"artist"`
		ItemType string `json:"item_type"`
		Current  struct {
			Title string `json:"title"`
		} `json:"current"`
		TrackInfo []bandcampTrack `json:"trackinfo"`
	}
	if err := json.Unmarshal([]byte(data), &t); err != nil {
		return nil, fmt.Errorf("bandcamp: read %s: %w", link, err)
	}
	p := &bandcampPage{link: link, artist: t.Artist, kind: t.ItemType, name: t.Current.Title, tracks: t.TrackInfo}

	// A track page names its album only in the embed data beside it.
	if p.kind == "track" {
		if n := findFirst(root, func(n *html.Node) bool { return n.Type == html.ElementNode && hasAttr(n, "data-embed") }); n != nil {
			var e struct {
				AlbumTitle string `json:"album_title"`
			}
			if json.Unmarshal([]byte(attr(n, "data-embed")), &e) == nil {
				p.album = e.AlbumTitle
			}
		}
	}
	return p, nil
}

// title names the job: the band and the release.
func (p *bandcampPage) title() string {
	if p.artist == "" {
		return p.name
	}
	return p.artist + " - " + p.name
}

// folder is where the release's tracks go: the band, then the album. A
// single that belongs to no album goes straight into the band's folder.
func (p *bandcampPage) folder() string {
	album := p.name
	if p.kind == "track" {
		album = p.album
	}
	return path.Join(bandcampSegment(p.artist), bandcampSegment(album))
}

// files lists every track the page will stream.
func (p *bandcampPage) files(b *Bandcamp) []File {
	width := len(strconv.Itoa(len(p.tracks)))
	if width < 2 {
		width = 2
	}
	var out []File
	for _, tr := range p.tracks {
		stream := tr.File[bandcampStream]
		if stream == "" {
			continue // not streamable without a purchase
		}
		name := tr.Title
		// A compilation credits each track to its own artist.
		if tr.Artist != "" && tr.Artist != p.artist {
			name = tr.Artist + " - " + name
		}
		// A lone single has no place in an album to number.
		if tr.Num > 0 && (p.kind == "album" || p.album != "") {
			name = fmt.Sprintf("%0*d - %s", width, tr.Num, name)
		}

		link, id := p.link, tr.ID
		out = append(out, File{
			Name:    name + ".mp3",
			URL:     stream,
			Size:    -1,
			Dir:     p.folder(),
			Headers: bandcampHeaders(),
			Resolve: func(ctx context.Context) (*Target, error) { return b.stream(ctx, link, id) },
		})
	}
	return out
}

// stream re-reads a release page for a fresh link to one of its tracks.
func (b *Bandcamp) stream(ctx context.Context, link string, id int64) (*Target, error) {
	p, err := b.release(ctx, link)
	if err != nil {
		return nil, err
	}
	for _, tr := range p.tracks {
		if tr.ID == id && tr.File[bandcampStream] != "" {
			return &Target{URL: tr.File[bandcampStream], Size: -1, Headers: bandcampHeaders()}, nil
		}
	}
	return nil, fmt.Errorf("bandcamp: track %d is no longer streamable from %s", id, link)
}

// bandcampSegment keeps a name from being read as a path: a slash in a title
// is part of the title, not a folder.
func bandcampSegment(s string) string {
	return strings.NewReplacer("/", "_", `\`, "_").Replace(strings.TrimSpace(s))
}

// bandcampReleaseLinks collects the releases a /music page lists, in its
// order: the grid's own links, and the items the page renders from script
// once the grid grows past what it draws up front.
func bandcampReleaseLinks(root *html.Node, u *url.URL) []string {
	seen := make(map[string]bool)
	var links []string
	add := func(ref string) {
		link := resolveRef(u, ref)
		lu, err := url.Parse(link)
		if err != nil || !strings.HasSuffix(strings.ToLower(lu.Hostname()), ".bandcamp.com") {
			return
		}
		segs := util.PathSegments(lu)
		if len(segs) < 2 || (segs[0] != "album" && segs[0] != "track") {
			return
		}
		lu.RawQuery, lu.Fragment = "", ""
		if key := lu.String(); !seen[key] {
			seen[key] = true
			links = append(links, key)
		}
	}

	grid := findFirst(root, func(n *html.Node) bool { return n.Type == html.ElementNode && attr(n, "id") == "music-grid" })
	if grid == nil {
		grid = root
	}
	for _, a := range findAll(grid, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "a" }) {
		add(attr(a, "href"))
	}
	if n := findFirst(root, func(n *html.Node) bool { return n.Type == html.ElementNode && hasAttr(n, "data-client-items") }); n != nil {
		var items []struct {
			PageURL string `json:"page_url"`
		}
		if json.Unmarshal([]byte(attr(n, "data-client-items")), &items) == nil {
			for _, it := range items {
				add(it.PageURL)
			}
		}
	}
	return links
}
