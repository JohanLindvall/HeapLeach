package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Vidmoly is a video embed host, and only one of the four domains it answers
// on still serves anything.
//
// That is the whole of the difficulty here, so it is worth stating plainly
// what each of them does. The .to domain answers a video path with a redirect
// to an ad landing page. The .net domain redirects to .biz but only for the
// bare /<code> shape, which .biz then refuses. And the .me domain rewrites
// the request by prefixing the path with "embed-" — so /embed-<code>.html is
// answered with a redirect to /embed-embed-<code>.html, which 404s. That last
// one is why this extractor takes a URL apart and builds a new one rather
// than fetching what it was given: following the site's own redirect can
// never reach the video, and no amount of retrying changes it.
//
// So every recognised shape is reduced to its file code and re-issued as
// https://vidmoly.biz/embed-<code>.html, which is served to anyone. The
// response carries x-frame-options: ALLOWALL, which is the same reason ok.ru's
// embed page works where its watch page does not — a page whose purpose is to
// be framed by strangers cannot afford to demand a session. Nothing else is
// required either: no user agent, no cookie, no fetch-metadata headers. A
// referer is sent all the same, because it costs nothing and the media CDN,
// unlike the page, has not been proven indifferent to one.
//
// The media arrives as an HLS master playlist whose variants carry audio and
// video together, so the segments join into something playable and the
// external downloader is not needed.
type Vidmoly struct {
	hostSet
	client *httpx.Client
}

const (
	// vidmolyHost is the only domain that serves the embed page. The others
	// are matched, so a pasted link of any shape works, but never fetched.
	vidmolyHost = "vidmoly.biz"
	vidmolyRoot = "https://" + vidmolyHost

	// vidmolyMinCodeLength rejects a path segment that cannot be a file code
	// — a language prefix, "faq", "tos" — before it costs a request. The
	// site's own codes are twelve characters; the bound sits well below that
	// so a shorter one it may once have issued still resolves.
	vidmolyMinCodeLength = 5
)

// vidmolyHosts are the domains a link may arrive on.
var vidmolyHosts = hostSet{"vidmoly.me", "vidmoly.to", "vidmoly.biz", "vidmoly.net"}

// vidmolyCodePattern is what a file code may consist of.
var vidmolyCodePattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// vidmolySources isolates the player's source list, and the anchor is the
// entire point of it.
//
// The page configures JWPlayer's seek-bar thumbnails several lines above the
// media, as file: "/api/v1/slides?...jpg". A pattern that merely looks for a
// file: key therefore comes away with a sprite sheet, and — since that URL is
// perfectly valid and answers with a real body — the failure is silent: a
// JPEG lands on disk under the video's name and the job reports success. Only
// what is inside sources: is media.
var vidmolySources = regexp.MustCompile(`(?s)\bsources\s*:\s*\[(.*?)]`)

// vidmolyFile reads one entry's link out of that list, tolerating the quoted
// key some builds of the player emit.
var vidmolyFile = regexp.MustCompile(`["']?file["']?\s*:\s*["']([^"']+)["']`)

// NewVidmoly builds the vidmoly extractor.
func NewVidmoly(client *httpx.Client) *Vidmoly {
	return &Vidmoly{hostSet: vidmolyHosts, client: client}
}

func (v *Vidmoly) Name() string { return "vidmoly" }

// Extract resolves any of the site's link shapes to one playable file.
func (v *Vidmoly) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	code := vidmolyCode(u)
	if code == "" {
		return nil, fmt.Errorf("vidmoly: no file code in %s "+
			"(links look like %s/embed-<code>.html)", u.Redacted(), vidmolyRoot)
	}
	base := vidmolyEmbed(code)
	page := base.String()

	doc, err := v.client.GetString(ctx, page, httpx.Referer(vidmolyRoot+"/"))
	if err != nil {
		return nil, fmt.Errorf("vidmoly: fetch %s: %w "+
			"(the file may have been removed)", page, err)
	}
	link, err := vidmolyMedia(doc, base)
	if err != nil {
		return nil, fmt.Errorf("vidmoly: %s: %w", page, err)
	}

	title := util.FirstNonEmpty(trimSiteSuffix(firstTitleOf(doc)), code)
	headers := httpx.Referer(page)

	// Guard the playlist path on the link actually being a playlist. Handing
	// a progressive file to the resolver would read an MP4 body as text and
	// take every line that does not start with a # for a segment URL, which
	// produces a job full of nonsense instead of an error.
	if !strings.Contains(strings.ToLower(link), ".m3u8") {
		return &Result{Title: title, Files: []File{{
			Name: title + vidmolyExtension(link), URL: link, Size: -1, Headers: headers,
		}}}, nil
	}

	segments, variant, err := resolvePlaylist(ctx, v.client, link, headers)
	if err != nil {
		return nil, fmt.Errorf("vidmoly: %s: %w", page, err)
	}
	return &Result{Title: title, Files: []File{{
		Name:     title + playlistExtension(variant),
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}}}, nil
}

// vidmolyMedia picks the media link out of a fetched embed page, resolved
// against the page it came from because the player writes some of its links
// relative.
//
// Kept apart from the fetch so a fixture can stand in for the page: what is
// under test is which of the several file: keys on it is the video.
func vidmolyMedia(doc string, base *url.URL) (string, error) {
	var sawDASH bool
	for _, list := range vidmolySources.FindAllStringSubmatch(doc, -1) {
		for _, entry := range vidmolyFile.FindAllStringSubmatch(list[1], -1) {
			link := resolveRef(base, entry[1])
			switch {
			case link == "":
				continue
			case strings.Contains(strings.ToLower(link), ".mpd"):
				// DASH, which nothing here can join into a file. The page
				// usually offers HLS beside it, so this is noted and the
				// list read on rather than failing at the first entry.
				sawDASH = true
			default:
				return link, nil
			}
		}
	}
	if sawDASH {
		return "", fmt.Errorf("this file is offered only as a DASH manifest, " +
			"which this does not handle")
	}
	return "", fmt.Errorf("no source list in the player configuration " +
		"(the file may have been removed, or the player may have changed)")
}

// vidmolyCode reads the file code out of whichever shape the link arrived in.
func vidmolyCode(u *url.URL) string {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return ""
	}
	code := strings.TrimSuffix(segs[len(segs)-1], ".html")

	// Stripped in a loop rather than once, because the site itself mints the
	// doubled form: its .me domain answers /embed-<code>.html with a redirect
	// to /embed-embed-<code>.html. Somebody who followed that and pasted
	// where they landed gets it repaired here rather than told their link is
	// malformed, which it was not when they clicked it.
	for strings.HasPrefix(code, "embed-") {
		code = strings.TrimPrefix(code, "embed-")
	}

	if len(code) < vidmolyMinCodeLength || !vidmolyCodePattern.MatchString(code) {
		return ""
	}
	return code
}

// vidmolyEmbed builds the page URL for a code. Assembled as a URL rather than
// concatenated so the result is escaped, and always on the one host that
// serves it whatever host the link came in on.
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
