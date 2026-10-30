// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Player pages: one page, one video, and the renditions its player is
// given, read out of the page or out of the endpoint the player asks. Every
// such site is one row of a table, and one engine does the rest: read the
// page, take the best rendition (bestCandidate), and either follow a
// playlist down to its segments or hand over the file. A row can also take
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
	client *httpx.Client
	site   *playerSite
}

// playerSite is one site's row.
type playerSite struct {
	name string
	// shape is the URL forms worth a probe, for the link scanner; recognise
	// takes a probed page by the player configuration read reads;
	// recognises describes them for the inventory.
	shape      func(u *url.URL) bool
	recognise  func(p *Page) bool
	recognises string
	// page is the URL read for a pasted link: the one form of the video's
	// page that serves its player. Nil reads the link as it is. An error
	// says the link names no video.
	page func(u *url.URL) (string, error)
	// referer is the Referer every media request carries, given the page,
	// unless a read names its own (playerVideo.referer). Nil, with none
	// named, sends no Referer.
	referer func(page string) string
	// read fetches the page and reports what its player is given.
	read func(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error)
	// signed reads the page again when the transfer starts.
	signed bool
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
	// own: its entries, its title and its note.
	links func(ctx context.Context, client *httpx.Client, u *url.URL) ([]listingLink, string, string, error)
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
	return &Player{client: client, site: site}
}

func (p *Player) Name() string { return p.site.name }

// Recognises is the inventory's description.
func (p *Player) Recognises() string { return p.site.recognises }

// Shape reports the site's URL forms worth a probe.
func (p *Player) Shape(u *url.URL) bool { return p.site.shape(u) }

// Recognise reports whether the probed page is the site's player.
func (p *Player) Recognise(_ context.Context, page *Page) bool { return p.site.recognise(page) }

// ProxyFallback reports whether the row opted into proxy recovery.
func (p *Player) ProxyFallback() bool { return p.site.proxyFallback }

// Extract resolves a video page to its best rendition, or queues a listing.
func (p *Player) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	if p.site.listing != nil && p.site.listing.shape(u) {
		return p.list(ctx, u, opts)
	}
	page := u.String()
	if p.site.page != nil {
		var err error
		if page, err = p.site.page(u); err != nil {
			return nil, fmt.Errorf("%s: %w", p.site.name, err)
		}
	}
	if p.site.peek != nil {
		title, err := p.site.peek(ctx, p.client, page)
		if err != nil {
			return nil, p.readError(page, err)
		}
		title = util.FirstNonEmpty(title, p.site.name)
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
	}
	file.ProxyFallback = p.site.proxyFallback
	return &Result{Title: title, Files: []File{file}}, nil
}

// deferred is a video queued under title and read in full when its
// transfer starts, and again on recovery rather than whatever listed it.
func (p *Player) deferred(page, title string) File {
	resolve := func(ctx context.Context) (*Target, error) {
		target, _, err := p.resolve(ctx, page)
		return target, err
	}
	return File{Name: title + util.FirstNonEmpty(p.site.ext, ".mp4"), Size: -1,
		Resolve: resolve, Refresh: resolve, ProxyFallback: p.site.proxyFallback}
}

// list walks a listing, queueing each video it names unread.
func (p *Player) list(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	var links []listingLink
	var title, note string
	if read := p.site.listing.links; read != nil {
		var err error
		if links, title, note, err = read(ctx, p.client, u); err != nil {
			return nil, fmt.Errorf("%s: %w", p.site.name, err)
		}
	} else {
		first, walk := p.site.listing.walk(u)
		walk.name = p.site.name
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
	name := p.site.name
	video, err := p.site.read(ctx, p.client, page)
	if err != nil {
		return nil, "", p.readError(page, err)
	}
	best, ok := bestCandidate(video.candidates)
	if !ok {
		return nil, "", fmt.Errorf("%s: no playable rendition on %s (the video may have been removed)", name, page)
	}
	title := util.FirstNonEmpty(video.title, name)
	referer := video.referer
	if referer == "" && p.site.referer != nil {
		referer = p.site.referer(page)
	}
	var headers httpx.Header
	if referer != "" {
		headers = httpx.Referer(referer)
	}
	if !best.IsHLS {
		return &Target{URL: best.URL, Name: title + util.FirstNonEmpty(video.ext, ".mp4"), Size: -1,
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

// readError is a failed read in the row's name. A 404 on the page is the
// video being gone, for every row, and still carries its status for the
// downloader to read.
func (p *Player) readError(page string, err error) error {
	if httpx.HasStatus(err, http.StatusNotFound) {
		return &playerGone{fmt.Sprintf("%s: %s is not there — the site says it was not found or has been removed",
			p.site.name, page), err}
	}
	return fmt.Errorf("%s: %w", p.site.name, err)
}

// playerGone says a video is gone in words, keeping the reply that said so.
type playerGone struct {
	msg string
	err error
}

func (e *playerGone) Error() string { return e.msg }
func (e *playerGone) Unwrap() error { return e.err }

// playerFetch reads a page, sending referer as its Referer.
func playerFetch(ctx context.Context, client *httpx.Client, page, referer string) (string, error) {
	doc, err := fetchPage(ctx, client, page, httpx.Referer(referer))
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", page, err)
	}
	return doc, nil
}

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

// The player sites, in the order NewRegistry registers them.
var playerSites = []*playerSite{
	&pornhubPlayer, &xhamsterPlayer, &tnaflixPlayer, &drtuberPlayer, &pornonePlayer, &epornerPlayer,
	&vidmolyPlayer, &okruPlayer, &streamtapePlayer, &doodPlayer, &mixdropPlayer, &luluPlayer, &vidsPlayer,
	&sendvidPlayer, &knuthingPlayer, &noodlePlayer,
}

// ---------------------------------------------------------------- pornhub

const pornhubRoot = "https://www.pornhub.com"

var (
	// pornhubFlashvars finds the player configuration object.
	pornhubFlashvars = regexp.MustCompile(`var\s+flashvars_\d+\s*=\s*\{`)
	pornhubViewKey   = regexp.MustCompile(`^[0-9a-z]{6,20}$`)
)

// pornhubShape is a video page, /view_video.php?viewkey=<key>.
func pornhubShape(u *url.URL) bool {
	return u.EscapedPath() == "/view_video.php" && pornhubViewKey.MatchString(u.Query().Get("viewkey"))
}

var pornhubPlayer = playerSite{
	name:  "pornhub",
	shape: pornhubShape,
	recognise: func(p *Page) bool {
		return pornhubShape(p.Final) && p.Has("flashvars_", "mediaDefinitions") &&
			pornhubFlashvars.MatchString(p.Body) && p.Named("Pornhub")
	},
	recognises: "video pages (`/view_video.php?viewkey=<key>`) named Pornhub whose player configuration " +
		"(`flashvars_<n>` with its `mediaDefinitions`) is in the page",
	referer: siteRoot(pornhubRoot),
	read:    pornhubRead,
}

// pornhubRead reads the player configuration embedded in a video page.
func pornhubRead(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
	doc, err := playerFetch(ctx, client, page, pornhubRoot+"/")
	if err != nil {
		return nil, err
	}
	loc := pornhubFlashvars.FindStringIndex(doc)
	if loc == nil {
		return nil, fmt.Errorf("no player configuration found (the video may be private or removed)")
	}
	var flashvars struct {
		VideoTitle       string `json:"video_title"`
		MediaDefinitions []struct {
			Format   string          `json:"format"`
			Quality  json.RawMessage `json:"quality"`
			VideoURL string          `json:"videoUrl"`
			Remote   bool            `json:"remote"`
		} `json:"mediaDefinitions"`
	}
	// The object is followed by the rest of the script; Decode reads the
	// one value and stops.
	if err := json.NewDecoder(strings.NewReader(doc[loc[1]-1:])).Decode(&flashvars); err != nil {
		return nil, fmt.Errorf("read player configuration: %w", err)
	}
	video := &playerVideo{title: util.FirstNonEmpty(flashvars.VideoTitle, pageTitle(doc))}
	for _, def := range flashvars.MediaDefinitions {
		if def.Remote || def.VideoURL == "" {
			continue // a pointer to another list, not a stream
		}
		// The quality is a bare "1080", which qualityOf's "<n>p" pattern
		// does not read, so a plain number is taken as it stands.
		label := strings.Trim(string(def.Quality), `"[]`)
		quality, err := strconv.Atoi(label)
		if err != nil {
			quality = qualityOf(label)
		}
		video.candidates = append(video.candidates, mediaCandidate{
			URL:     def.VideoURL,
			Quality: quality,
			IsHLS:   strings.EqualFold(def.Format, "hls") || isPlaylistLink(def.VideoURL),
		})
	}
	return video, nil
}

// ------------------------------------------------- xhamster, tnaflix

const (
	xhamsterRoot = "https://xhamster.com"
	tnaflixRoot  = "https://www.tnaflix.com"
)

var (
	// xhamsterShape is a video page, /videos/<slug>-<id>, or its embed.
	xhamsterShape = pathShape(`^/videos/[^/]+-[0-9A-Za-z]+/?$`, `^/(?:xembed\.php|embed/[0-9A-Za-z]+/?)$`)
	// tnaflixShape is a video page, /<category>/<slug>/video<id>.
	tnaflixShape = pathShape(`^/[^/]+/[^/]+/video[0-9]+/?$`)
)

// The player config stores its links encoded, but the page also carries
// the plain CDN links, which are what is read. The same pages are served on
// every one of the site's domains.
var xhamsterPlayer = playerSite{
	name:  "xhamster",
	shape: xhamsterShape,
	recognise: func(p *Page) bool {
		return xhamsterShape(p.Final) && p.Has("window.initials", "xplayerSettings") && p.Named("xHamster")
	},
	recognises: "video pages (`/videos/<slug>-<id>`) named xHamster that carry the site's page state " +
		"(`window.initials`) and player settings",
	referer: siteRoot(xhamsterRoot),
	read:    mediaLinksRead(xhamsterRoot, "xhcdn.com"),
	signed:  true,
}

// The renditions are plain signed links in the page, listed by the
// player's own <source> elements.
var tnaflixPlayer = playerSite{
	name:  "tnaflix",
	shape: tnaflixShape,
	recognise: func(p *Page) bool {
		return tnaflixShape(p.Final) && p.hasElement(videoSource) && p.namedPrefix("TNAFlix")
	},
	recognises: "video pages (`/<category>/<slug>/video<id>`) named TNAFlix whose player lists its " +
		"renditions as `<source>` elements",
	referer: siteRoot(tnaflixRoot),
	read:    mediaLinksRead(tnaflixRoot, "tnaflix.com"),
	signed:  true,
}

// mediaLinksRead reads the media links a page carries on the site's own
// media hosts, those whose address contains hint.
func mediaLinksRead(root, hint string) func(context.Context, *httpx.Client, string) (*playerVideo, error) {
	return func(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
		doc, err := playerFetch(ctx, client, page, root+"/")
		return &playerVideo{title: pageTitle(doc), candidates: mediaLinksIn(doc, hint)}, err
	}
}

// ---------------------------------------------------------------- drtuber

const drtuberRoot = "https://www.drtuber.com"

var (
	// drtuberID reads the id out of a watch page, /video/<id>/<slug>, or
	// the player the embeds point at, /embed/<id>.
	drtuberID    = regexp.MustCompile(`^/(?:video|embed)/(\d+)`)
	drtuberShape = pathShape(`^/video/[0-9]+(?:/[^/]*)?/?$`, `^/embed/[0-9]+/?$`)
	// drtuberQualities maps the configuration's rendition names to the
	// heights the player shows for them. Sorted as text "lq" would beat
	// "4k", and qualityOf cannot read a name.
	drtuberQualities = map[string]int{"lq": 320, "hq": 720, "4k": 2160}
)

// drtuberConfigPath is the player configuration endpoint, which both the
// watch page and the player name.
const drtuberConfigPath = "player_config_json"

// The page carries no media at all: its <video> has a poster and nothing
// else, and title and every rendition arrive from the player's
// configuration endpoint, which is all that is read. Its links expire a few
// minutes out.
var drtuberPlayer = playerSite{
	name:      "drtuber",
	shape:     drtuberShape,
	recognise: func(p *Page) bool { return drtuberShape(p.Final) && p.Has(drtuberConfigPath) },
	recognises: "watch pages and players (`/video/<id>/<slug>`, `/embed/<id>`) whose player is configured " +
		"from the DrTuber `player_config_json` endpoint",
	page: func(u *url.URL) (string, error) {
		m := drtuberID.FindStringSubmatch(u.Path)
		if m == nil {
			return "", fmt.Errorf("%s names no video", u.Redacted())
		}
		return drtuberRoot + "/" + drtuberConfigPath + "/?vid=" + url.QueryEscape(m[1]), nil
	},
	referer: siteRoot(drtuberRoot),
	read:    drtuberRead,
	signed:  true,
}

// drtuberRead reads the player configuration.
//
// The endpoint answers only a request whose Accept names JSON: asked with
// the "*/*" a plain fetch sends, it returns an empty array under a 200,
// which reads like a missing video. GetJSON sends the Accept that works.
func drtuberRead(ctx context.Context, client *httpx.Client, endpoint string) (*playerVideo, error) {
	id := ""
	if u, err := url.Parse(endpoint); err == nil {
		id = u.Query().Get("vid")
	}
	var raw json.RawMessage
	if err := client.GetJSON(ctx, endpoint, nil, &raw); err != nil {
		return nil, fmt.Errorf("read the player configuration for video %s: %w", id, err)
	}
	// A video that is gone, and a request the endpoint declines, both come
	// back as an empty array under a 200, which only the reply's shape
	// tells from a real answer.
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("no player configuration for video %s "+
			"(it has been removed, or is not served to this request)", id)
	}
	var config struct {
		Title string            `json:"title"`
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("read the player configuration for video %s: %w", id, err)
	}
	return &playerVideo{
		title:      util.FirstNonEmpty(strings.TrimSpace(config.Title), "drtuber-"+id),
		candidates: drtuberRenditions(config.Files),
	}, nil
}

// drtuberRenditions turns the configuration's file list into candidates.
// The list names every rendition the site knows of, so an absent one (an
// empty string, or a null, which decodes the same) is dropped. The names
// are walked sorted, so equal renditions are chosen alike on every read:
// at queue time and at download time included.
func drtuberRenditions(files map[string]string) []mediaCandidate {
	var out []mediaCandidate
	for _, name := range slices.Sorted(maps.Keys(files)) {
		link := files[name]
		if link == "" {
			continue
		}
		quality := drtuberQualities[name]
		if quality == 0 {
			quality = qualityOf(name + " " + link)
		}
		out = append(out, mediaCandidate{URL: link, Quality: quality, IsHLS: isPlaylistLink(link)})
	}
	return out
}

// ---------------------------------------------------------------- pornone

const pornoneRoot = "https://pornone.com"

var (
	// pornoneShape is a video page, /<category>/<slug>/<id>/.
	pornoneShape = pathShape(`^/[^/]+/[^/]+/[0-9]{5,}/?$`)
	// pornoneDimensions finds the frame size the encoder wrote into a
	// filename, as in "<id>_1920x1080_4000k.mp4".
	pornoneDimensions = regexp.MustCompile(`_(\d{2,5})x(\d{2,5})_`)
)

var pornonePlayer = playerSite{
	name:  "pornone",
	shape: pornoneShape,
	recognise: func(p *Page) bool {
		return pornoneShape(p.Final) && p.namedPrefix("PornOne") && p.hasElement(func(n *html.Node) bool {
			return isElem(n, atom.Source) && pornoneDimensions.MatchString(attr(n, "src"))
		})
	},
	recognises: "video pages (`/<category>/<slug>/<id>/`) named PornOne whose player lists renditions " +
		"as `<source>` elements named by their frame size",
	referer: siteRoot(pornoneRoot),
	read: func(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
		root, err := fetchPageRoot(ctx, client, page, httpx.Referer(pornoneRoot+"/"))
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", page, err)
		}
		return &playerVideo{
			title:      util.FirstNonEmpty(firstText(root, atom.H1), trimSiteSuffix(firstText(root, atomTitle))),
			candidates: pornoneRenditions(root),
		}, nil
	},
	signed: true,
}

// pornoneRenditions reads the <source> elements the player offers.
//
// Their res and label attributes are not to be trusted: the largest file on
// a page can be marked 720p while its own name says 1920x1080. The name is
// what the encoder wrote, so it decides, and the attribute is only a
// fallback for a rendition whose name carries no dimensions.
func pornoneRenditions(root *html.Node) []mediaCandidate {
	var out []mediaCandidate
	for _, n := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.Source) }) {
		link := attr(n, "src")
		if link == "" {
			continue
		}
		quality := 0
		if m := pornoneDimensions.FindStringSubmatch(link); m != nil {
			quality, _ = strconv.Atoi(m[2])
		}
		if quality == 0 {
			quality, _ = strconv.Atoi(attr(n, "res"))
		}
		if quality == 0 {
			quality = qualityOf(attr(n, "label") + " " + link)
		}
		out = append(out, mediaCandidate{URL: link, Quality: quality, IsHLS: isPlaylistLink(link)})
	}
	return out
}

// ---------------------------------------------------------------- vidmoly

// Only one of the four domains vidmoly answers on still serves anything.
// The .to domain answers a video path with a redirect to an ad landing
// page; .net redirects the bare /<code> to .biz, which refuses it; and .me
// prefixes the path with "embed-", so /embed-<code>.html redirects to
// /embed-embed-<code>.html, which 404s. Following the site's own redirect
// can never reach the video, so every shape is reduced to its file code and
// re-issued on the one domain that serves the player to anyone.
const (
	vidmolyHost = "vidmoly.biz"
	vidmolyRoot = "https://" + vidmolyHost
	// vidmolyMinCodeLength rejects a segment that cannot be a file code (a
	// language prefix, "faq", "tos") before it costs a request; the site's
	// codes are twelve characters.
	vidmolyMinCodeLength = 5
)

var (
	// vidmolyWatchShape is a watch page, /w/<code> or /v/<code>.
	vidmolyWatchShape = pathShape(`^/[wv]/[A-Za-z0-9]{5,}/?$`)
	// vidmolyEmbedShape is the player, /embed-<code>.html, doubled prefix
	// included.
	vidmolyEmbedShape  = pathShape(`^/(?:embed-)+[A-Za-z0-9]{5,}\.html$`)
	vidmolyCodePattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)
)

var vidmolyPlayer = playerSite{
	name:  "vidmoly",
	shape: func(u *url.URL) bool { return vidmolyWatchShape(u) || vidmolyEmbedShape(u) },
	recognise: func(p *Page) bool {
		code := vidmolyCode(p.Final)
		switch {
		case code == "":
			return false
		case vidmolyWatchShape(p.Final):
			// The site's app, linking the player for its own code.
			return p.Has("__NUXT", "embed-"+code)
		case vidmolyEmbedShape(p.Final):
			return jwSources.MatchString(p.Body) && p.HasAny("VidMoly", "Vidmoly")
		}
		return false
	},
	recognises: "watch pages (`/w/<code>`, `/v/<code>`) of the VidMoly app naming the file's player, and " +
		"players (`/embed-<code>.html`) whose JWPlayer source list names VidMoly",
	page: func(u *url.URL) (string, error) {
		code := vidmolyCode(u)
		if code == "" {
			return "", fmt.Errorf("no file code in %s (links look like %s/embed-<code>.html)",
				u.Redacted(), vidmolyRoot)
		}
		return vidmolyEmbed(code).String(), nil
	},
	referer: samePage,
	read: func(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
		doc, err := playerFetch(ctx, client, page, vidmolyRoot+"/")
		if err != nil {
			return nil, err
		}
		base, _ := url.Parse(page)
		link, err := jwSource(doc, base)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", page, err)
		}
		return &playerVideo{
			title:      util.FirstNonEmpty(pageTitle(doc), vidmolyCode(base)),
			candidates: playerLink(link),
			ext:        vidmolyExtension(link),
		}, nil
	},
}

// vidmolyCode reads the file code out of whichever shape the link arrived
// in. The "embed-" prefix is stripped in a loop because the site itself
// mints the doubled form, and a link pasted from where its redirect landed
// is repaired rather than refused.
func vidmolyCode(u *url.URL) string {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return ""
	}
	code := strings.TrimSuffix(segs[len(segs)-1], ".html")
	for strings.HasPrefix(code, "embed-") {
		code = strings.TrimPrefix(code, "embed-")
	}
	if len(code) < vidmolyMinCodeLength || !vidmolyCodePattern.MatchString(code) {
		return ""
	}
	return code
}

// vidmolyEmbed is the player for a code, on the one host that serves it.
func vidmolyEmbed(code string) *url.URL {
	return &url.URL{Scheme: "https", Host: vidmolyHost, Path: "/embed-" + code + ".html"}
}

// vidmolyExtension names the container of a progressive file, for the rare
// page that offers one instead of a playlist.
func vidmolyExtension(link string) string {
	if ext := path.Ext(util.NameFromURL(link)); ext != "" {
		return ext
	}
	return ".mp4"
}

// ------------------------------------------------------------- streamtape

// streamtapeShape is a watch page or a player, /v/<id> or /e/<id>.
var streamtapeShape = pathShape(`^/[ve]/[0-9A-Za-z]{6,}(?:/[^/]*)?/?$`)

// streamtapeLink matches the assignment that joins the link's two halves:
// an incomplete one in a hidden element, and the remainder in a script that
// appends it minus a few leading characters, an offset read from the page.
var streamtapeLink = regexp.MustCompile(
	`(?s)ById\(\s*['"](?:no)?robotlink['"]\s*\)\.innerHTML\s*=\s*['"]([^'"]*)['"]\s*\+\s*` +
		`\(?\s*['"]([^'"]*)['"]\s*\)?\s*\.\s*substring\(\s*(\d+)\s*\)`)

var streamtapePlayer = playerSite{
	name:       "streamtape",
	shape:      streamtapeShape,
	recognise:  func(p *Page) bool { return streamtapeShape(p.Final) && streamtapeLink.MatchString(p.Body) },
	recognises: "videos (`/v/<id>`, `/e/<id>`) whose page assembles the Streamtape `robotlink` media link",
	referer:    samePage,
	read: func(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
		// These hosts reject a referer from a sibling domain.
		doc, err := playerFetch(ctx, client, page, strings.TrimSuffix(pageOrigin(page), "/"))
		if err != nil {
			return nil, err
		}
		link, err := streamtapeAssemble(doc, page)
		if err != nil {
			return nil, err
		}
		return &playerVideo{title: pageTitle(doc), candidates: playerLink(link)}, nil
	},
	signed: true,
}

// streamtapeAssemble joins the two halves of the link the page carries.
// Kept apart from the fetch because this is the part that breaks, and what
// a fixture can check.
func streamtapeAssemble(doc, page string) (string, error) {
	m := streamtapeLink.FindStringSubmatch(doc)
	if m == nil {
		return "", fmt.Errorf("no media link on %s "+
			"(the video may have been removed, or the page layout changed)", page)
	}
	head, tail := m[1], m[2]
	offset, err := strconv.Atoi(m[3])
	if err != nil || offset > len(tail) {
		return "", fmt.Errorf("media link on %s is assembled in a way this does not follow", page)
	}
	link := head + tail[offset:]
	switch {
	case strings.HasPrefix(link, "//"):
		link = "https:" + link
	case strings.HasPrefix(link, "/"):
		link = strings.TrimSuffix(pageOrigin(page), "/") + link
	}
	// The player asks for the stream itself; without this the host answers
	// with a redirect back to the page.
	if !strings.Contains(link, "stream=") {
		sep := "&"
		if !strings.Contains(link, "?") {
			sep = "?"
		}
		link += sep + "stream=1"
	}
	return link, nil
}

// ---------------------------------------------------------------- mixdrop

var (
	// mixdropShape is a player or its landing page, /e/<code> or /f/<code>.
	mixdropShape   = pathShape(`^/[ef]/[0-9a-z]{6,}(?:/[^/]*)?/?$`)
	mixdropLanding = pathShape(`^/f/[0-9a-z]{6,}(?:/[^/]*)?/?$`)
	// mixdropURL matches the assignment the unpacked script leaves behind.
	mixdropURL = regexp.MustCompile(`MDCore\.(?:wurl|vsrc)\s*=\s*["']([^"']+)["']`)
)

// The link is inside a packed script, which unpacks to an assignment to the
// player object naming the delivery host.
var mixdropPlayer = playerSite{
	name:  "mixdrop",
	shape: mixdropShape,
	recognise: func(p *Page) bool {
		if !mixdropShape(p.Final) {
			return false
		}
		if p.Has("MDCore.") {
			return true
		}
		// A landing page frames the player rather than being it.
		return mixdropLanding(p.Final) && p.Named("MixDrop") && p.frames(func(src *url.URL) bool {
			return strings.HasPrefix(src.EscapedPath(), "/e/")
		})
	},
	recognises: "players (`/e/<code>`) configuring the MixDrop player object (`MDCore`), and landing pages " +
		"(`/f/<code>`) named MixDrop that frame or configure one",
	// The embed page carries the player; /f/ is a landing page around it.
	page:    func(u *url.URL) (string, error) { return strings.Replace(u.String(), "/f/", "/e/", 1), nil },
	referer: samePage,
	read: func(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
		doc, err := playerFetch(ctx, client, page, pageOrigin(page))
		if err != nil {
			return nil, err
		}
		source := doc
		if unpacked, ok := unpackJS(doc); ok {
			source = unpacked + doc
		}
		found := mixdropURL.FindStringSubmatch(source)
		if found == nil {
			return nil, fmt.Errorf("no media link on %s (the video may have been removed)", page)
		}
		link := found[1]
		if strings.HasPrefix(link, "//") {
			link = "https:" + link
		}
		return &playerVideo{title: util.FirstNonEmpty(pageTitle(doc), util.NameFromURL(page)),
			candidates: playerLink(link)}, nil
	},
	signed: true,
}

// ------------------------------------------------------------- lulustream

var (
	// luluShape is a player or a download page.
	luluShape = pathShape(`^/[ed]/[0-9a-z]{12}/?$`)
	// luluPage is either, or the bare watch page, /<id>, too common a form
	// for the link scanner.
	luluPage = pathShape(`^/(?:[ed]/)?[0-9a-z]{12}/?$`)
)

// The page's JW Player setup is packed, and names an HLS master playlist
// signed for eight hours and tied to the address that asked: one muxed
// rendition, in AES-128 encrypted segments the downloader decrypts.
var luluPlayer = playerSite{
	name:  "lulustream",
	shape: luluShape,
	recognise: func(p *Page) bool {
		if !luluPage(p.Final) || !p.HasAny("LuluStream", "LuluVdo", "Luluvdo") {
			return false
		}
		link, err := luluSource(p.Body, p.Final)
		return err == nil && isPlaylistLink(link)
	},
	recognises: "videos (`/<id>`, `/e/<id>`, `/d/<id>`) whose page names LuluStream and carries its " +
		"player's HLS source",
	// The plain page, whichever shape was pasted: it is served on every
	// domain, where the embed and download pages redirect on some of them.
	page:    idPage(luluID, "/", "/<id>"),
	referer: pageOrigin,
	read: func(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
		doc, err := playerFetch(ctx, client, page, pageOrigin(page))
		if err != nil {
			return nil, err
		}
		base, _ := url.Parse(page)
		link, err := luluSource(doc, base)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", page, err)
		}
		// The heading names the video; the document title adds the site's.
		return &playerVideo{title: util.FirstNonEmpty(firstHeading(doc), pageTitle(doc), util.NameFromURL(page)),
			candidates: playerLink(link)}, nil
	},
	signed: true,
}

// luluSource reads the player's source, from its setup as the page carries
// it or once unpacked.
func luluSource(doc string, base *url.URL) (string, error) {
	link, err := jwSource(doc, base)
	if err != nil {
		if setup, ok := unpackJS(doc); ok {
			return jwSource(setup, base)
		}
	}
	return link, err
}

// luluID reads the video id out of /<id>, /e/<id> or /d/<id>.
func luluID(u *url.URL) string { return pathID(u, true, "e", "d") }

// ---------------------------------------------------------------- vids.st

var (
	// vidsShape is a watch page or a player by the numeric id the site
	// issues, /v/<id> or /e/<id>; vidsPage takes any id of that form.
	vidsShape = pathShape(`^/[ve]/[0-9]+/?$`)
	vidsPage  = pathShape(`^/[ve]/[A-Za-z0-9]+/?$`)
)

// vidsConfigMarker opens the page's player configuration.
const vidsConfigMarker = "playerConfig = "

// One object, playerConfig, states the file's address and its original
// name. The address is usually the uploaded file itself, served with no
// token and ranges honoured; the player also plays HLS, and so does this.
var vidsPlayer = playerSite{
	name:       "vids.st",
	shape:      vidsShape,
	recognise:  func(p *Page) bool { return vidsPage(p.Final) && p.Has(vidsConfigMarker, `"videoUrl"`) },
	recognises: "videos (`/v/<id>`, `/e/<id>`) whose page carries the player's `playerConfig` with its `videoUrl`",
	page:       idPage(vidsID, "/v/", "/v/<id>"),
	referer:    samePage,
	read:       vidsRead,
}

func vidsRead(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
	base, _ := url.Parse(page)
	id := vidsID(base)
	doc, err := playerFetch(ctx, client, page, page)
	if err != nil {
		return nil, err
	}
	at := strings.Index(doc, vidsConfigMarker)
	if at < 0 {
		return nil, fmt.Errorf("%s: no player on the page (the video may have been removed)", page)
	}
	var cfg struct {
		VideoURL  string `json:"videoUrl"`
		VideoName string `json:"videoName"`
	}
	// The object is followed by the rest of the script; Decode reads the
	// one value and stops.
	if err := json.NewDecoder(strings.NewReader(doc[at+len(vidsConfigMarker):])).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: read the player configuration: %w", page, err)
	}
	if cfg.VideoURL == "" {
		return nil, fmt.Errorf("%s: the player configuration names no video", page)
	}
	link := resolveRef(base, cfg.VideoURL)
	name := util.FirstNonEmpty(strings.TrimSpace(cfg.VideoName), util.NameFromURL(link), "vids.st-"+id)
	ext := path.Ext(name)
	return &playerVideo{title: strings.TrimSuffix(name, ext), candidates: playerLink(link), ext: ext}, nil
}

// vidsID reads the video id out of /v/<id> or the embed's /e/<id>.
func vidsID(u *url.URL) string { return pathID(u, false, "v", "e") }

// ---------------------------------------------------------------- sendvid

var (
	// sendvidShape is the player, /embed/<id>.
	sendvidShape = pathShape(`^/embed/[A-Za-z0-9]{1,32}/?$`)
	// sendvidPage is the player or the bare watch page, /<id>, too common a
	// form for the link scanner.
	sendvidPage = pathShape(`^/(?:embed/)?[A-Za-z0-9]{1,32}(?:\.mp4)?/?$`)
)

// sendvidSourceElem is the player's own <source>.
func sendvidSourceElem(n *html.Node) bool {
	return isElem(n, atom.Source) && attr(n, "id") == "video_source"
}

// The player's one <source> is signed for a few hours and with a rate the
// media host holds each connection to. The signature covers both, but the
// rate is per connection, so a file split across several goes as many
// times faster. og:video only redirects back to the page, and the embed
// page's <title> is the site's name alone, so the title is og:title.
var sendvidPlayer = playerSite{
	name:  "sendvid",
	shape: sendvidShape,
	recognise: func(p *Page) bool {
		return sendvidPage(p.Final) && p.Named("Sendvid") && p.hasElement(sendvidSourceElem)
	},
	recognises: "videos (`/<id>`, `/embed/<id>`) whose page, named Sendvid, carries the player's " +
		"`video_source`",
	page:    idPage(sendvidID, "/", "/<id> or /embed/<id>"),
	referer: samePage,
	read: func(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
		base, _ := url.Parse(page)
		id := sendvidID(base)
		root, err := fetchPageRoot(ctx, client, page, nil)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", page, err)
		}
		link := resolveRef(base, sendvidSource(root))
		if link == "" {
			return nil, fmt.Errorf("no video on %s (it may have been removed, or the page layout changed)", page)
		}
		return &playerVideo{
			title:      util.FirstNonEmpty(metaContent(root, "og:title"), metaContent(root, "twitter:title"), id),
			candidates: playerLink(link),
		}, nil
	},
	signed: true,
}

// sendvidSource is the player's media address: its own <source>, any video
// <source>, or the <video>'s own src.
func sendvidSource(root *html.Node) string {
	for _, match := range []func(*html.Node) bool{
		sendvidSourceElem,
		func(n *html.Node) bool { return isElem(n, atom.Source) && strings.HasPrefix(attr(n, "type"), "video/") },
		func(n *html.Node) bool { return isElem(n, atom.Video) && attr(n, "src") != "" },
	} {
		if n := findFirst(root, match); n != nil {
			return attr(n, "src")
		}
	}
	return ""
}

// sendvidID reads the video id out of /<id>, /<id>.mp4 (the og:video link)
// or /embed/<id>: letters and digits. The site's own pages have more
// segments than that.
func sendvidID(u *url.URL) string {
	id := strings.TrimSuffix(pathID(u, true, "embed"), ".mp4")
	if len(id) > 32 || !sendvidIDPattern.MatchString(id) {
		return ""
	}
	return id
}

var sendvidIDPattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// ----------------------------------------------------------------- shared

// jwSources isolates a JW Player setup's source list, and the anchor is the
// point of it: a player can configure its seek-bar thumbnails above the
// media as file: "/api/v1/slides?...jpg", a valid URL answering a real
// body, so a pattern that merely looks for file: lands a JPEG on disk under
// the video's name and reports success. Only what is inside sources: is
// media.
var jwSources = regexp.MustCompile(`(?s)\bsources\s*:\s*\[(.*?)]`)

// jwSourceFile reads one entry's link, tolerating a quoted key.
var jwSourceFile = regexp.MustCompile(`["']?file["']?\s*:\s*["']([^"']+)["']`)

// jwSource is the first media link a JW Player setup lists, resolved
// against the page because players write some links relative. DASH is
// passed over, since nothing here joins it into a file; a page usually
// offers HLS beside it.
func jwSource(setup string, base *url.URL) (string, error) {
	var sawDASH bool
	for _, list := range jwSources.FindAllStringSubmatch(setup, -1) {
		for _, entry := range jwSourceFile.FindAllStringSubmatch(list[1], -1) {
			link := resolveRef(base, entry[1])
			switch {
			case link == "":
			case strings.Contains(strings.ToLower(link), ".mpd"):
				sawDASH = true
			default:
				return link, nil
			}
		}
	}
	if sawDASH {
		return "", fmt.Errorf("this file is offered only as a DASH manifest, which this does not handle")
	}
	return "", fmt.Errorf("no source list in the player configuration " +
		"(the file may have been removed, or the player may have changed)")
}

// videoSource is a <source> element naming a video file.
func videoSource(n *html.Node) bool {
	return isElem(n, atom.Source) && attr(n, "src") != "" &&
		(strings.HasPrefix(attr(n, "type"), "video/") || strings.Contains(attr(n, "src"), ".mp4"))
}

// scanObject finds the end of the brace-delimited object that opens at
// doc[start], and reports the index just past its closing brace.
//
// String literals delimited by quote are stepped over whole, backslash
// escapes included, so a brace inside a value cannot end the object early.
// The quote character is a parameter because the two configurations read
// here disagree about it: a JSON object quotes with '"', a KVS flashvars
// block with '\”. A document that ends before the object closes reports
// false.
func scanObject(doc string, start int, quote byte) (int, bool) {
	depth, inString, escaped := 0, false, false
	for i := start; i < len(doc); i++ {
		c := doc[i]
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == quote:
				inString = false
			}
		case c == quote:
			inString = true
		case c == '{':
			depth++
		case c == '}':
			if depth--; depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// firstHeading reads the page's own <h1>, which names a video where the
// document title adds the site's furniture around it.
func firstHeading(doc string) string {
	root, err := parseHTML(doc)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(firstText(root, atom.H1))
}

// pageTitle reads a document title, trimmed of the site's own suffix.
func pageTitle(doc string) string { return trimSiteSuffix(firstTitleOf(doc)) }

// packedScript matches a script compressed by the p,a,c,k,e,d packer, which
// is what these players ship instead of readable source.
var packedScript = regexp.MustCompile(
	`(?s)\}\s*\(\s*'(.*?)'\s*,\s*(\d+)\s*,\s*(\d+)\s*,\s*'(.*?)'\s*\.\s*split\(\s*'\|'\s*\)`)

// unpackJS reverses that packer.
//
// The scheme is a dictionary substitution: every identifier in the payload
// was replaced by its index in a word list, written in base a. Undoing it is
// walking the payload word by word and putting the words back — no
// evaluation, and nothing host-specific.
func unpackJS(script string) (string, bool) {
	m := packedScript.FindStringSubmatch(script)
	if m == nil {
		return "", false
	}
	payload := unescapeJSString(m[1])
	base, err := strconv.Atoi(m[2])
	if err != nil || base < 2 || base > 62 {
		return "", false
	}
	count, err := strconv.Atoi(m[3])
	if err != nil || count < 0 {
		return "", false
	}
	words := strings.Split(unescapeJSString(m[4]), "|")
	count = min(count, len(words))

	var out strings.Builder
	out.Grow(len(payload))
	for i := 0; i < len(payload); {
		if !isWordByte(payload[i]) {
			out.WriteByte(payload[i])
			i++
			continue
		}
		start := i
		for i < len(payload) && isWordByte(payload[i]) {
			i++
		}
		word := payload[start:i]
		if index, ok := packerIndex(word, base); ok && index < count && words[index] != "" {
			out.WriteString(words[index])
			continue
		}
		out.WriteString(word)
	}
	return out.String(), true
}

// isWordByte reports whether a byte can be part of a packed token.
func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// packerIndexMax bounds the value a token may decode to. A packed index is
// small, while the payload also carries ordinary words whose characters all
// happen to be digits of the base (a long lowercase run, a hex token in a
// URL). Unbounded, those overflow the accumulator, and a wrapped-negative
// "index" would pass the caller's range check and panic the word lookup.
const packerIndexMax = 1 << 30

// packerIndex reads a token as a number in the packer's alphabet: digits,
// then lowercase, then uppercase, the last only once the word list outgrows
// base 36.
func packerIndex(word string, base int) (int, bool) {
	value := 0
	for i := 0; i < len(word); i++ {
		var digit int
		switch c := word[i]; {
		case c >= '0' && c <= '9':
			digit = int(c - '0')
		case c >= 'a' && c <= 'z':
			digit = int(c-'a') + 10
		case c >= 'A' && c <= 'Z':
			digit = int(c-'A') + 36
		default:
			return 0, false
		}
		if digit >= base || value > (packerIndexMax-digit)/base {
			return 0, false
		}
		value = value*base + digit
	}
	return value, len(word) > 0
}
