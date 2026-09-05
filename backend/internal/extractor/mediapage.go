package extractor

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The generic media-page sniff: the last thing tried before an unrecognised
// URL is treated as a file.
//
// Pasting a video page no extractor knows used to write the HTML shell to
// disk under the page's own name and report a finished download — a few
// kilobytes that play as nothing. Nothing about that is recoverable
// afterwards, because the file looks complete and a later run skips it. The
// only place to catch it is here, by reading the page the way a browser would
// and looking for what its player is pointed at. That is what yt-dlp's
// generic extractor is for, and it is why it exists: no list of hosts ever
// catches up with the tail.
//
// Two guards keep it off pages that are not media pages, because the failure
// it must never introduce is fetching a promo clip in place of the file
// somebody asked for:
//
//   - The URL must name no file extension. A URL that names a file is a file,
//     and sniffing it would mean asking a server for a video in order to look
//     for a video inside it. A page-shaped extension (.html, .php) is left
//     out for the same reason: the value of this guard is that it never
//     touches a URL that names something.
//   - The response must be served as HTML, and that is decided on the
//     response headers before a byte of the body is read. This runs on every
//     extensionless URL, signed links to multi-gigabyte files included, so
//     reading first and deciding afterwards would pull megabytes of video
//     into memory to learn it was never a page.
//
// What no guard can rule out is a page that carries a video which is not the
// one that was wanted: a news article whose og:video is a promo clip resolves
// to the promo clip. That is accepted deliberately — the alternative is the
// page shell on disk, named after the article and marked complete, which is
// worse in every direction. The ordering below is the mitigation that costs
// nothing: what the page's own player is pointed at outranks what its sharing
// metadata advertises, so a real player is never outvoted by a share card.
//
// Failure is silent throughout, exactly as kvsSniff's is. Every step that
// cannot answer returns false and the caller carries on to the handling that
// would have run anyway, so a page this cannot read is no worse off than
// before.

// mediaPageSniff resolves a URL that is shaped like a page rather than a
// file, when the page turns out to carry media. The bool reports whether it
// found anything; there is no error, because an error here means only "not
// this, then".
func mediaPageSniff(ctx context.Context, client *httpx.Client, u *url.URL) (*Result, bool) {
	if path.Ext(u.Path) != "" {
		return nil, false // a URL that names a file is a file
	}

	doc, ok := mediaPageFetch(ctx, client, u)
	if !ok {
		return nil, false
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, false
	}

	found, ok := mediaPageFind(ctx, client, root, doc, u)
	if !ok {
		return nil, false
	}

	title := util.FirstNonEmpty(
		mediaPageTitle(root),
		found.Title,
		strings.Trim(u.Path, "/"),
		u.Hostname(),
	)
	// The page is the referer for whatever it pointed at, which is what a
	// browser would have sent and what hotlink checks look for.
	headers := httpx.Referer(u.String())
	name := mediaPageName(title, found)

	if found.IsHLS {
		segments, _, err := resolvePlaylist(ctx, client, found.URL, headers)
		if err != nil || len(segments) == 0 {
			return nil, false
		}
		return &Result{Title: title, Files: []File{{
			Name: name, Size: -1, Headers: headers, Segments: segments,
		}}}, true
	}
	return &Result{Title: title, Files: []File{{
		Name: name, URL: found.URL, Size: found.Size, Headers: headers,
	}}}, true
}

// mediaFinding is what one step found: the link, how it ranks, and whatever
// the step learned on the way that the URL alone does not say.
type mediaFinding struct {
	mediaCandidate
	// Title is the media's own name, when the step read one. The page's own
	// title is preferred over it; this is what is left when the page has none.
	Title string
	// Type is the MIME type the page or the host declared for this link, or
	// "" when nothing declared one. It names the extension for a link whose
	// path carries none.
	Type string
	// Size is an exact byte length when the source stated one, else -1.
	// Nothing here is ever a rounded listing figure, so nothing here is ever
	// SizeApprox.
	Size int64
}

// mediaPageFind runs the steps in confidence order and takes the first that
// answers. They are not ranked against each other: a page's own player is
// better evidence than its metadata whatever resolution either claims, and
// mixing them would let a share card outrank a rendition list.
func mediaPageFind(ctx context.Context, client *httpx.Client, root *html.Node, doc string, u *url.URL) (mediaFinding, bool) {
	if best, ok := bestCandidate(mediaPageElements(root, u)); ok {
		return mediaFinding{mediaCandidate: best, Size: -1}, true
	}
	// JW Player before the metadata, and not after it. A page built on that
	// player either advertises the player's own embed page in og:video — an
	// HTML document that would be saved under the film's name — or, as on the
	// player's own preview pages, one fixed middling rendition, while the
	// media endpoint below lists every rendition there is. Reading the
	// metadata first gives up the film, or the best copy of it, for nothing.
	if found, ok := jwPlayerFind(ctx, client, doc); ok {
		return found, true
	}
	if found, ok := mediaPageOpenGraph(root, u); ok {
		return found, true
	}
	if found, ok := mediaPageTwitterStream(root, u); ok {
		return found, true
	}
	return mediaPageLinkedData(root, u)
}

// mediaPageFetch reads the page, and only once the host has agreed it is one.
//
// The content type is taken from the response headers and the body is read
// only afterwards. That ordering is the whole point: an extensionless URL is
// as likely to be a signed media link as a page, and one that answers
// video/mp4 costs an aborted request here rather than a download.
func mediaPageFetch(ctx context.Context, client *httpx.Client, u *url.URL) (string, bool) {
	req, err := client.NewRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", false
	}
	// Asking as a browser navigating to the page, which is what the sniff is
	// pretending to be and what some hosts serve their real markup to.
	req.Header.Set(httpx.HeaderAccept, httpx.AcceptHTML)
	req.Header.Set(httpx.HeaderSecFetchDest, "document")
	req.Header.Set(httpx.HeaderSecFetchMode, "navigate")
	req.Header.Set(httpx.HeaderReferer, util.Origin(u)+"/")

	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false
	}
	// A host that will not say what it sent is treated as not having sent a
	// page. That is at worst today's behaviour, which is the whole bargain
	// this sniff makes.
	kind, _, _ := mime.ParseMediaType(resp.Header.Get(httpx.HeaderContentType))
	if !mediaPageIsHTMLType(kind) {
		return "", false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, config.MaxResponseBytes))
	if err != nil {
		return "", false
	}
	return string(body), true
}

// ------------------------------------------------------- the page's player

// mediaPageElements reads the renditions the page's own player carries.
//
// Only elements inside a <video> or an <audio> count, and that restriction is
// what makes this safe to run on an arbitrary page: <source> is also how
// <picture> spells a responsive image, so collecting them document-wide — as
// a single-host extractor such as pornone's safely does — would resolve half
// the web's articles to their header photograph.
//
// The first player that offers anything is the answer, and the rest of the
// page is not looked at. Several <video> elements on a page are several
// different films — the article's own and the previews of three more below
// it — so ranking across them would let a related clip win for being longer
// or larger. Renditions of one film live inside one player, which is exactly
// the set that is ranked. A player offering nothing fetchable (an MSE one,
// whose src is a blob: URL) is stepped over rather than ending the search.
//
// Ranking within a player follows pornone's, down to sharing its pattern: the
// dimensions the encoder wrote into the filename outrank the player's own res
// and label attributes, because on real pages the two disagree and the
// filename is the one that is right.
func mediaPageElements(root *html.Node, base *url.URL) []mediaCandidate {
	for _, player := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.Video) || isElem(n, atom.Audio)
	}) {
		var out []mediaCandidate
		add := func(n *html.Node, ref string) {
			link := mediaPageLink(base, ref)
			if link == "" {
				return
			}
			// A declared type that is not media is believed — a player whose
			// <source> is an image or a DASH manifest is saying so.
			declared := mediaPageMediaType(attr(n, "type"))
			if declared != "" && !mediaPageIsMediaType(declared) {
				return
			}
			out = append(out, mediaCandidate{
				URL:     link,
				Quality: mediaPageQuality(n, link),
				IsHLS:   mediaPageIsHLS(link, declared),
			})
		}

		add(player, attr(player, "src"))
		for _, source := range findAll(player, func(n *html.Node) bool { return isElem(n, atom.Source) }) {
			add(source, attr(source, "src"))
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// mediaPageQuality ranks one rendition, filename first for the reason
// pornone documents.
func mediaPageQuality(n *html.Node, link string) int {
	if m := pornoneDimensions.FindStringSubmatch(link); m != nil {
		if height, err := strconv.Atoi(m[2]); err == nil {
			return height
		}
	}
	// Hosts spell the same number both ways, as a bare "720" and as "720p",
	// and there is no agreement on which attribute carries it.
	for _, key := range []string{"res", "height", "data-res", "data-height", "size", "label"} {
		value := strings.TrimSpace(attr(n, key))
		if value == "" {
			continue
		}
		if height, err := strconv.Atoi(value); err == nil && height > 0 {
			return height
		}
		if height := qualityOf(value); height > 0 {
			return height
		}
	}
	return qualityOf(link)
}

// ------------------------------------------------------------- jw player

// JW Player is a hosted player rather than a site, which is why one
// implementation reaches so many hosts: the site embeds the player, the
// player asks jwplayer.com which files a media has, and that answer is the
// same JSON for whoever asks — no key, no session, no referer. yt-dlp calls
// its equivalent from dozens of its extractors for exactly this reason.
//
// Every collection under the two CDN hosts is keyed on the media id, so one
// pattern over the page text covers all three embed shapes at once: the
// <script src> a site drops in, the iframe form, and the inline
// jwplayer(...).setup() whose playlist names the media endpoint directly. It
// also means a page carrying nothing but the player's poster image still says
// which media it is showing.
//
// The second id in players/<media>-<player>.js names the player configuration
// rather than the media and is deliberately not captured. Ids are eight
// characters exactly, so a longer run of them is something else and is left
// alone rather than truncated into a plausible-looking id.
var jwPlayerEmbed = regexp.MustCompile(
	`(?:content\.jwplatform|cdn\.jwplayer)\.com/(?:players|v2/media|videos|manifests|previews|thumbs|strips)/([A-Za-z0-9]{8})\b`)

// jwPlayerMediaAPI answers with a media's rendition list. It is a var only so
// a test can point it at a stub.
var jwPlayerMediaAPI = "https://cdn.jwplayer.com/v2/media/"

// jwPlayerFeed is the part of the media document this reads.
type jwPlayerFeed struct {
	Title    string `json:"title"`
	Playlist []struct {
		Title   string           `json:"title"`
		Sources []jwPlayerSource `json:"sources"`
	} `json:"playlist"`
}

// jwPlayerSource is one rendition. A single media offers several kinds at
// once — a DASH manifest, an HLS manifest, a handful of MP4s and an
// audio-only track beside them — so the type is what has to be read first,
// not the order.
type jwPlayerSource struct {
	File   string `json:"file"`
	Type   string `json:"type"`
	Height int    `json:"height"`
	Label  string `json:"label"`
	// Filesize is the file's own byte count, not a listing's rounded figure,
	// so it is passed through as exact. That is what lets a later run see it
	// already has the file without asking the host first.
	Filesize int64 `json:"filesize"`
}

// jwPlayerFind resolves a page that embeds the player.
func jwPlayerFind(ctx context.Context, client *httpx.Client, doc string) (mediaFinding, bool) {
	m := jwPlayerEmbed.FindStringSubmatch(doc)
	if m == nil {
		return mediaFinding{}, false
	}
	var feed jwPlayerFeed
	if err := client.GetJSON(ctx, jwPlayerMediaAPI+m[1], nil, &feed); err != nil {
		return mediaFinding{}, false
	}
	return jwPlayerBest(&feed)
}

// jwPlayerBest picks the rendition to fetch out of a media document.
//
// Only the first playlist entry is read: a media feed holds exactly one, and
// what the page embedded is that one.
//
// Video and audio are ranked in separate groups rather than together, and the
// audio-only track is considered only when the media has no video at all.
// Every media carries one — the live feed lists an .m4a beside its MP4s, with
// no height on it — so ranking the two together would let a film whose
// renditions omit their heights resolve to its soundtrack. DASH is dropped
// outright: nothing here can assemble an .mpd, and saving the manifest would
// leave a few kilobytes named after the film.
func jwPlayerBest(feed *jwPlayerFeed) (mediaFinding, bool) {
	if len(feed.Playlist) == 0 {
		return mediaFinding{}, false
	}
	item := feed.Playlist[0]

	var video, audio []mediaCandidate
	stated := make(map[string]jwPlayerSource, len(item.Sources))
	for _, source := range item.Sources {
		if source.File == "" {
			continue
		}
		kind := mediaPageMediaType(source.Type)
		quality := source.Height
		if quality == 0 {
			quality = qualityOf(source.Label + " " + source.File)
		}
		candidate := mediaCandidate{
			URL:     source.File,
			Quality: quality,
			IsHLS:   mediaPageIsHLS(source.File, kind),
		}
		switch {
		case strings.HasPrefix(kind, "video/"), candidate.IsHLS:
			video = append(video, candidate)
		case strings.HasPrefix(kind, "audio/"):
			audio = append(audio, candidate)
		default:
			continue // DASH, and anything else this cannot turn into a file
		}
		stated[source.File] = source
	}

	best, ok := bestCandidate(video)
	if !ok {
		if best, ok = bestCandidate(audio); !ok {
			return mediaFinding{}, false
		}
	}
	source := stated[best.URL]
	return mediaFinding{
		mediaCandidate: best,
		Title:          util.FirstNonEmpty(item.Title, feed.Title),
		Type:           mediaPageMediaType(source.Type),
		Size:           mediaPageSize(source.Filesize),
	}, true
}

// ---------------------------------------------------------------- metadata

// mediaPageOpenGraph reads the sharing metadata, in the order the standard
// gives it: the https spelling first, then either spelling of the plain one.
//
// og:video is frequently not media at all. A great many players advertise
// their own embed page there — an HTML document that would land on disk under
// the film's name — and a page that does so says as much in og:video:type. A
// declared type of text/html is believed and ends the step, rather than being
// argued with.
//
// One that names a player page without declaring it is still handed on. The
// downloader recognises a host answering a file request with a web page and
// refuses it (rejectWebPage), so the outcome is a bounded failure with a
// message naming the URL, not a page shell recorded as a finished film.
func mediaPageOpenGraph(root *html.Node, base *url.URL) (mediaFinding, bool) {
	declared := mediaPageMediaType(metaContent(root, "og:video:type"))
	if mediaPageIsHTMLType(declared) {
		return mediaFinding{}, false
	}
	for _, key := range []string{"og:video:secure_url", "og:video:url", "og:video"} {
		link := mediaPageLink(base, metaContent(root, key))
		if link == "" {
			continue
		}
		quality := mediaPageMetaInt(root, "og:video:height")
		if quality == 0 {
			quality = qualityOf(link)
		}
		return mediaFinding{
			URL:     link,
			Quality: quality,
			IsHLS:   mediaPageIsHLS(link, declared),
			Type:    declared,
			Size:    -1,
		}, true
	}
	return mediaFinding{}, false
}

// mediaPageTwitterStream reads the card metadata's media link.
//
// Only twitter:player:stream is read. twitter:player beside it is the player
// *page*, which the card frames in an iframe — following that one would save
// the frame.
func mediaPageTwitterStream(root *html.Node, base *url.URL) (mediaFinding, bool) {
	link := mediaPageLink(base, metaContent(root, "twitter:player:stream"))
	if link == "" {
		return mediaFinding{}, false
	}
	declared := mediaPageMediaType(metaContent(root, "twitter:player:stream:content_type"))
	if mediaPageIsHTMLType(declared) {
		return mediaFinding{}, false
	}
	return mediaFinding{
		URL:     link,
		Quality: qualityOf(link),
		IsHLS:   mediaPageIsHLS(link, declared),
		Type:    declared,
		Size:    -1,
	}, true
}

// mediaPageLinkedData reads the schema.org description a page publishes for
// search engines.
//
// Three shapes occur and all three have to be handled: a bare VideoObject, an
// array of objects, and the @graph wrapper every CMS emits. So the decoded
// document is walked rather than matched, which covers a VideoObject nested
// under an article's own "video" property as a side effect.
//
// The type is what makes walking safe. contentUrl is not a video's property
// but a MediaObject's, and every page with a photograph on it carries an
// ImageObject holding one, usually before the video — walking for the first
// contentUrl would resolve half the web to a JPEG. Only VideoObject,
// AudioObject and the bare MediaObject are accepted.
func mediaPageLinkedData(root *html.Node, base *url.URL) (mediaFinding, bool) {
	for _, script := range findAll(root, func(n *html.Node) bool {
		return isElem(n, atom.Script) && strings.Contains(strings.ToLower(attr(n, "type")), "ld+json")
	}) {
		var doc any
		if err := json.Unmarshal([]byte(textOf(script)), &doc); err != nil {
			continue
		}
		if found, ok := linkedDataMedia(doc, base); ok {
			return found, true
		}
	}
	return mediaFinding{}, false
}

// linkedDataMedia walks a decoded JSON-LD document for a media object.
func linkedDataMedia(node any, base *url.URL) (mediaFinding, bool) {
	switch v := node.(type) {
	case []any:
		for _, item := range v {
			if found, ok := linkedDataMedia(item, base); ok {
				return found, true
			}
		}
	case map[string]any:
		if found, ok := linkedDataObject(v, base); ok {
			return found, true
		}
		// Keys are visited in sorted order because Go's map order is
		// randomised on purpose: without this, a document holding two media
		// objects under different properties would resolve to a different one
		// on different runs.
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if found, ok := linkedDataMedia(v[k], base); ok {
				return found, true
			}
		}
	}
	return mediaFinding{}, false
}

// linkedDataObject reads one object, if it is a media object with a link.
func linkedDataObject(obj map[string]any, base *url.URL) (mediaFinding, bool) {
	if !linkedDataIsMedia(obj["@type"]) {
		return mediaFinding{}, false
	}
	link := mediaPageLink(base, linkedDataString(obj["contentUrl"]))
	if link == "" {
		return mediaFinding{}, false
	}
	declared := mediaPageMediaType(linkedDataString(obj["encodingFormat"]))
	if mediaPageIsHTMLType(declared) {
		return mediaFinding{}, false
	}
	quality := linkedDataInt(obj["height"])
	if quality == 0 {
		quality = qualityOf(link)
	}
	return mediaFinding{
		URL:     link,
		Quality: quality,
		IsHLS:   mediaPageIsHLS(link, declared),
		Title:   linkedDataString(obj["name"]),
		Type:    declared,
		Size:    -1,
	}, true
}

// linkedDataMediaTypes are the schema.org types whose contentUrl is worth
// fetching. ImageObject is a MediaObject too and is pointedly not here.
var linkedDataMediaTypes = map[string]bool{
	"videoobject": true,
	"audioobject": true,
	"mediaobject": true,
}

// linkedDataIsMedia reads an @type, which may be one name or a list of them.
func linkedDataIsMedia(value any) bool {
	switch v := value.(type) {
	case string:
		return linkedDataMediaTypes[strings.ToLower(strings.TrimSpace(v))]
	case []any:
		if slices.ContainsFunc(v, linkedDataIsMedia) {
			return true
		}
	}
	return false
}

// linkedDataString reads a string field, tolerating the number a publisher
// writes where the vocabulary asks for text.
func linkedDataString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// linkedDataInt reads a numeric field, which arrives as a number or as the
// string spelling of one depending on the publisher.
func linkedDataInt(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return qualityOf(v)
		}
		return n
	}
	return 0
}

// ----------------------------------------------------------------- shared

// mediaPageTitle names the job after the page: what it says it is about,
// falling back to what it calls itself.
//
// Both are trimmed of the site's name, and og:title is not exempt from that
// however much the property is meant to name the item rather than the
// document — Wikipedia writes "Big Buck Bunny - Wikipedia" into og:title
// itself, and a great many sites simply copy their <title> into it. The cost
// is a title whose own dash is meaningful losing what follows it, which is a
// name on disk, not a wrong download.
func mediaPageTitle(root *html.Node) string {
	return trimSiteSuffix(util.FirstNonEmpty(
		metaContent(root, "og:title"),
		firstText(root, atomTitle),
	))
}

// mediaPageName is the filename to save under: the page's title, plus an
// extension that says what the bytes are.
func mediaPageName(title string, found mediaFinding) string {
	if found.IsHLS {
		// Segments are concatenated as they arrive, which yields a transport
		// stream; remuxing to MP4 happens afterwards, if it can.
		return title + ".ts"
	}
	if ext := strings.ToLower(path.Ext(util.NameFromURL(found.URL))); mediaPageExtensions[ext] {
		return title + ext
	}
	if ext := mediaPageTypeExtensions[found.Type]; ext != "" {
		return title + ext
	}
	// A signed link often carries no extension at all, and the host is not
	// going to be asked ahead of time just to name the file. MP4 is what the
	// overwhelming majority of these turn out to be, and a wrong extension
	// costs a rename where a wrong URL costs the download.
	return title + ".mp4"
}

// mediaPageExtensions are the extensions worth keeping off a URL's own path.
// Anything outside this set is a path that happens to have a dot in it.
var mediaPageExtensions = map[string]bool{
	".mp4": true, ".m4v": true, ".webm": true, ".mov": true, ".mkv": true,
	".ts": true, ".flv": true, ".avi": true, ".ogv": true, ".m4s": true,
	".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".opus": true,
	".wav": true, ".flac": true,
}

// mediaPageTypeExtensions name a file whose URL will not.
var mediaPageTypeExtensions = map[string]string{
	"video/mp4":             ".mp4",
	"video/webm":            ".webm",
	"video/quicktime":       ".mov",
	"video/x-matroska":      ".mkv",
	"video/x-flv":           ".flv",
	"video/ogg":             ".ogv",
	"video/mp2t":            ".ts",
	"audio/mpeg":            ".mp3",
	"audio/mp4":             ".m4a",
	"audio/aac":             ".aac",
	"audio/ogg":             ".ogg",
	"audio/wav":             ".wav",
	"audio/x-wav":           ".wav",
	"application/x-mpegurl": ".ts",
}

// mediaPageLink resolves a reference against the page and keeps it only if it
// is something that can be fetched.
//
// The scheme check is not a formality: a player driven by Media Source
// Extensions sets its <video> src to a blob: URL, which names an object
// inside the browser and nothing a downloader can reach. Taking one would
// turn a page that cannot be downloaded at all into a download that fails
// with a confusing error.
func mediaPageLink(base *url.URL, ref string) string {
	link := resolveRef(base, ref)
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return link
}

// mediaPageMetaInt reads a numeric meta value, or 0 when there is none.
func mediaPageMetaInt(root *html.Node, key string) int {
	n, err := strconv.Atoi(strings.TrimSpace(metaContent(root, key)))
	if err != nil {
		return 0
	}
	return n
}

// mediaPageMediaType normalises a declared type: lower case, and without the
// codecs parameter a <source> element carries.
func mediaPageMediaType(declared string) string {
	kind, _, _ := strings.Cut(declared, ";")
	return strings.ToLower(strings.TrimSpace(kind))
}

// mediaPageIsHTMLType reports whether a type names a web page.
func mediaPageIsHTMLType(kind string) bool {
	return kind == "text/html" || kind == "application/xhtml+xml"
}

// mediaPageIsHLSType reports whether a type names a playlist, consulting the
// one table of playlist spellings so this and the manifest sniff cannot
// drift apart.
func mediaPageIsHLSType(kind string) bool {
	return hlsManifestMediaTypes[kind]
}

// mediaPageIsMediaType reports whether a declared type is something this can
// turn into a file. DASH is not: it names an .mpd nothing here assembles.
func mediaPageIsMediaType(kind string) bool {
	return strings.HasPrefix(kind, "video/") || strings.HasPrefix(kind, "audio/") ||
		mediaPageIsHLSType(kind)
}

// mediaPageIsHLS reports whether a link is an adaptive playlist, by its path
// or by what the page declared it to be.
func mediaPageIsHLS(link, declared string) bool {
	return strings.Contains(link, ".m3u8") || mediaPageIsHLSType(declared)
}

// mediaPageSize normalises a stated byte count to the File convention, where
// an unknown length is -1 rather than 0.
func mediaPageSize(stated int64) int64 {
	if stated > 0 {
		return stated
	}
	return -1
}
