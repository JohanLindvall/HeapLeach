// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
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
	about
	client *httpx.Client
}

// bandcampAgent is the User-Agent every bandcamp request states. Anything
// that does not claim to be a browser is served; see Bandcamp.
const bandcampAgent = "HeapLeach"

// bandcampStream is the one format served without a purchase.
const bandcampStream = "mp3-128"

// bandcampDomain is where the bands without a domain of their own live.
const bandcampDomain = "bandcamp.com"

// NewBandcamp builds the bandcamp extractor.
func NewBandcamp(client *httpx.Client) *Bandcamp {
	return &Bandcamp{client: client, about: about{name: "bandcamp", shape: bandcampShape,
		recognises: "releases (`/album/<slug>`, `/track/<slug>`), a band's `/music` and its front page, " +
			"where the page is built from Bandcamp's client bundle and carries `data-tralbum` release data or the music grid"}}
}

// bandcampShape is a release, /album/<slug> or /track/<slug>, or a band's
// /music listing. A band's own front page is its site root, which is no
// shape at all, so it is recognised by its page alone.
var bandcampShape = pathShape(`^/(?:album|track)/[^/.]+/?$`, `^/music/?$`)

// Recognise takes a page of the right form, or a band's front page, built
// from the platform's client bundle and carrying what Extract reads: the
// data-tralbum release data, or the /music grid of releases. The platform's
// own front page has the bundle but neither, so it is left alone.
//
// A probe says it is a browser, and so may be handed the platform's client
// challenge instead of the page (see Bandcamp). Then, and only for a page of
// the right form or a site root, the page is asked for once more as this
// extractor asks for it, and that answer is judged the same way.
func (b *Bandcamp) Recognise(ctx context.Context, p *Page) bool {
	if !bandcampShape(p.Final) && len(p.Segments()) != 0 {
		return false
	}
	if bandcampMarked(p) {
		return true
	}
	if b.client == nil || p.Title() != bandcampChallengeTitle || !p.Has(bandcampChallengeAssets) {
		return false
	}
	doc := memo(p, bandcampAgentKey, func() string {
		ctx, cancel := context.WithTimeout(ctx, config.PlatformProbeTimeout)
		defer cancel()
		doc, err := b.client.GetString(ctx, p.Final.String(), bandcampHeaders())
		if err != nil {
			return ""
		}
		return doc
	})
	return doc != "" && bandcampMarked(&Page{URL: p.Final, Final: p.Final, Body: doc})
}

// bandcampAgentKey memoises, on a challenged probe, the page as fetched
// under the extractor's own agent, so that Extract reads that document
// rather than the challenge or a third fetch.
const bandcampAgentKey = "bandcamp-agent"

// The client challenge a browser is shown in place of a page: its title,
// and the path its assets are served under.
const (
	bandcampChallengeTitle  = "Client Challenge"
	bandcampChallengeAssets = "/_fs-ch-"
)

// bandcampMarked reports a page built from the platform's client bundle
// that carries release data or a band's release grid.
func bandcampMarked(p *Page) bool {
	return p.Script("/client-bundle/") &&
		p.HasAny(`data-tralbum="`, `id="music-grid"`, `data-client-items="`)
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
	// Each release is a page to read, so a catalogue is read as far as
	// config.MaxResolvedReleases of them; the registry holds the files to
	// the user's cap.
	listed := len(links)
	links = links[:min(len(links), config.MaxResolvedReleases)]
	files, outcomes := resolveListing(ctx, links, func(ctx context.Context, link string) ([]File, error) {
		release, err := b.release(ctx, link)
		if err != nil {
			return nil, err
		}
		return release.files(b), nil
	})
	if err := noneResolved(ctx, files, "bandcamp", len(links), "releases", u.Redacted(), outcomes.refusals, outcomes.failed); err != nil {
		return nil, err
	}
	band := cmp.Or(metaContent(root, "og:site_name"), metaContent(root, "og:title"), u.Hostname())
	return &Result{Title: band, Files: files, Note: joinNotes(cappedNote(len(links), listed, "releases"), outcomes.note())}, nil
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
	u, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("bandcamp: %w", err)
	}
	if p := probed(ctx, u); p != nil && p.Title() == bandcampChallengeTitle {
		if doc := memo(p, bandcampAgentKey, func() string { return "" }); doc != "" {
			return parseHTML(doc)
		}
	}
	// A page the probe was served, rather than the challenge, is the page
	// whichever agent asks: this host's representation does not depend on
	// the User-Agent once it serves one, so it is read as it is. That is a
	// decision about this host, made here; fetchRoot would not take the
	// probe for a request carrying an agent of its own (Page.answers).
	if p := probed(ctx, u); p.answers(b.client, nil) && p.Title() != bandcampChallengeTitle {
		if doc, ok := p.Document(); ok {
			if root := p.HTML(); root != nil {
				return root, nil
			}
			return parseHTML(doc)
		}
	}
	root, _, err := fetchRoot(ctx, b.client, u, bandcampHeaders())
	if err != nil {
		return nil, fmt.Errorf("bandcamp: fetch %s: %w", link, err)
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

// bandcampData returns the JSON document the page carries in its first
// element with the attribute name — data-tralbum, data-embed,
// data-client-items — or "".
func bandcampData(root *html.Node, name string) string {
	n := findFirst(root, func(n *html.Node) bool { return n.Type == html.ElementNode && hasAttr(n, name) })
	if n == nil {
		return ""
	}
	return attr(n, name)
}

// bandcampRelease reads a release out of its page.
func bandcampRelease(root *html.Node, link string) (*bandcampPage, error) {
	data := bandcampData(root, "data-tralbum")
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
		var e struct {
			AlbumTitle string `json:"album_title"`
		}
		if json.Unmarshal([]byte(bandcampData(root, "data-embed")), &e) == nil {
			p.album = e.AlbumTitle
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
	// A slash in a title is part of the title, not a folder.
	return path.Join(sourceFolder(p.artist), sourceFolder(album))
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

// bandcampReleaseLinks collects the releases a /music page lists, in its
// order: the grid's own links, and the items the page renders from script
// once the grid grows past what it draws up front.
func bandcampReleaseLinks(root *html.Node, u *url.URL) []string {
	seen := make(map[string]bool)
	var links []string
	add := func(ref string) {
		link := resolveRef(u, ref)
		lu, err := url.Parse(link)
		// A band on a domain of its own lists releases on that domain.
		if err != nil || !(strings.EqualFold(lu.Host, u.Host) || util.HostMatches(lu.Host, bandcampDomain)) {
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
	var items []struct {
		PageURL string `json:"page_url"`
	}
	if json.Unmarshal([]byte(bandcampData(root, "data-client-items")), &items) == nil {
		for _, it := range items {
			add(it.PageURL)
		}
	}
	return links
}
