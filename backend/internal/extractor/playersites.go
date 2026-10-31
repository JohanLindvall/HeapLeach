// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
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

// The player sites: one row of the Player engine's table (players.go) each,
// with what is particular to reading that site's page.

// The player sites, in the order NewRegistry registers them.
var playerSites = []*playerSite{
	&pornhubPlayer, &xhamsterPlayer, &tnaflixPlayer, &drtuberPlayer, &pornonePlayer, &epornerPlayer,
	&streamablePlayer, &vidmolyPlayer, &okruPlayer, &streamtapePlayer, &doodPlayer, &mixdropPlayer, &luluPlayer, &vidsPlayer,
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
	video := &playerVideo{title: cmp.Or(flashvars.VideoTitle, pageTitle(doc))}
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
		title:      cmp.Or(strings.TrimSpace(config.Title), "drtuber-"+id),
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
			title:      cmp.Or(firstText(root, atom.H1), trimSiteSuffix(firstText(root, atomTitle))),
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
			title:      cmp.Or(pageTitle(doc), vidmolyCode(base)),
			candidates: playerLink(link),
			ext:        linkExtAsIs(link, ".mp4"), // the rare page offering a progressive file
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
		return &playerVideo{title: cmp.Or(pageTitle(doc), util.NameFromURL(page)),
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
		return &playerVideo{title: cmp.Or(firstHeading(doc), pageTitle(doc), util.NameFromURL(page)),
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
	name := cmp.Or(strings.TrimSpace(cfg.VideoName), util.NameFromURL(link), "vids.st-"+id)
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
			title:      cmp.Or(metaContent(root, "og:title"), metaContent(root, "twitter:title"), id),
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
