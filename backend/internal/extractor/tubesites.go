package extractor

import (
	"context"
	"encoding/json"
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

// ---------------------------------------------------------------- pornhub

// PornHub reads the player configuration embedded in a video page.
type PornHub struct{ client *httpx.Client }

const pornhubRoot = "https://www.pornhub.com"

// flashvarsPattern finds the player configuration object.
var pornhubFlashvars = regexp.MustCompile(`var\s+flashvars_\d+\s*=\s*\{`)

// NewPornHub builds the pornhub extractor.
func NewPornHub(client *httpx.Client) *PornHub { return &PornHub{client: client} }

func (p *PornHub) Name() string { return "pornhub" }

func (p *PornHub) Match(u *url.URL) bool { return util.HostMatches(u.Host, "pornhub.com") }

// Extract resolves a video page to its best rendition.
func (p *PornHub) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	doc, err := p.client.GetString(ctx, u.String(), httpx.Referer(pornhubRoot+"/"))
	if err != nil {
		return nil, fmt.Errorf("pornhub: fetch %s: %w", u.Redacted(), err)
	}

	config, err := jsonObjectAfter(doc, pornhubFlashvars)
	if err != nil {
		return nil, fmt.Errorf("pornhub: %w (the video may be private or removed)", err)
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
	if err := json.Unmarshal([]byte(config), &flashvars); err != nil {
		return nil, fmt.Errorf("pornhub: read player configuration: %w", err)
	}

	var candidates []mediaCandidate
	for _, def := range flashvars.MediaDefinitions {
		if def.Remote || def.VideoURL == "" {
			continue // a pointer to another list, not a stream
		}
		candidates = append(candidates, mediaCandidate{
			URL:     def.VideoURL,
			Quality: qualityOf(strings.Trim(string(def.Quality), `"[]`)),
			IsHLS:   strings.EqualFold(def.Format, "hls") || strings.Contains(def.VideoURL, ".m3u8"),
		})
	}
	best, ok := bestCandidate(candidates)
	if !ok {
		return nil, fmt.Errorf("pornhub: no playable rendition for %s", u.Redacted())
	}

	title := util.FirstNonEmpty(flashvars.VideoTitle, pageTitle(doc), "pornhub")
	headers := httpx.Referer(pornhubRoot + "/")

	if best.IsHLS {
		segments, _, err := resolvePlaylist(ctx, p.client, best.URL, headers)
		if err != nil {
			return nil, fmt.Errorf("pornhub: %w", err)
		}
		return &Result{Title: title, Files: []File{{
			Name: title + ".ts", Size: -1, Headers: headers, Segments: segments,
		}}}, nil
	}
	return &Result{Title: title, Files: []File{{
		Name: title + ".mp4", URL: best.URL, Size: -1, Headers: headers,
	}}}, nil
}

// --------------------------------------------------------------- xhamster

// XHamster reads the media links a video page carries. Its player config
// stores them encoded, but the page also contains the plain CDN links.
type XHamster struct{ client *httpx.Client }

const xhamsterRoot = "https://xhamster.com"

// NewXHamster builds the xhamster extractor.
func NewXHamster(client *httpx.Client) *XHamster { return &XHamster{client: client} }

func (x *XHamster) Name() string { return "xhamster" }

// Match accepts any host with an "xhamster" label, the way bunkr covers its
// domain rotation: xhamster.com, xhamster.desi, m.xhamster.com. A bare
// substring test would also claim unrelated hosts that merely contain the
// name ("notxhamster.example").
func (x *XHamster) Match(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	for label := range strings.SplitSeq(host, ".") {
		if strings.HasPrefix(label, "xhamster") {
			return true
		}
	}
	return false
}

// Extract picks the best rendition, re-reading the page at download time
// because the links are signed and expire.
func (x *XHamster) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	page := u.String()
	return refetchedVideo(ctx, ".mp4", httpx.Referer(xhamsterRoot+"/"), func(ctx context.Context) (string, string, error) {
		title, best, err := x.best(ctx, page)
		return title, best.URL, err
	})
}

func (x *XHamster) best(ctx context.Context, page string) (string, mediaCandidate, error) {
	doc, err := x.client.GetString(ctx, page, httpx.Referer(xhamsterRoot+"/"))
	if err != nil {
		return "", mediaCandidate{}, fmt.Errorf("xhamster: fetch %s: %w", page, err)
	}
	best, ok := bestCandidate(mediaLinksIn(doc, "xhcdn.com"))
	if !ok {
		return "", mediaCandidate{}, fmt.Errorf("xhamster: no playable rendition on %s", page)
	}
	return util.FirstNonEmpty(pageTitle(doc), "xhamster"), best, nil
}

// ---------------------------------------------------------------- tnaflix

// TNAFlix lists its renditions as plain signed links in the page.
type TNAFlix struct{ client *httpx.Client }

const tnaflixRoot = "https://www.tnaflix.com"

// NewTNAFlix builds the tnaflix extractor.
func NewTNAFlix(client *httpx.Client) *TNAFlix { return &TNAFlix{client: client} }

func (t *TNAFlix) Name() string { return "tnaflix" }

func (t *TNAFlix) Match(u *url.URL) bool { return util.HostMatches(u.Host, "tnaflix.com") }

// Extract picks the best rendition, re-reading the page at download time
// because the links carry an expiry.
func (t *TNAFlix) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	page := u.String()
	return refetchedVideo(ctx, ".mp4", httpx.Referer(tnaflixRoot+"/"), func(ctx context.Context) (string, string, error) {
		title, best, err := t.best(ctx, page)
		return title, best.URL, err
	})
}

func (t *TNAFlix) best(ctx context.Context, page string) (string, mediaCandidate, error) {
	doc, err := t.client.GetString(ctx, page, httpx.Referer(tnaflixRoot+"/"))
	if err != nil {
		return "", mediaCandidate{}, fmt.Errorf("tnaflix: fetch %s: %w", page, err)
	}
	best, ok := bestCandidate(mediaLinksIn(doc, "tnaflix.com"))
	if !ok {
		return "", mediaCandidate{}, fmt.Errorf("tnaflix: no playable rendition on %s", page)
	}
	return util.FirstNonEmpty(pageTitle(doc), "tnaflix"), best, nil
}

// ---------------------------------------------------------------- pornone

// PornOne lists its renditions as <source> elements on the page, signed with
// an expiry, so they are re-read when the item starts.
type PornOne struct{ client *httpx.Client }

const pornoneRoot = "https://pornone.com"

// pornoneDimensions finds the frame size the encoder wrote into a filename,
// as in "<id>_1920x1080_4000k.mp4".
var pornoneDimensions = regexp.MustCompile(`_(\d{2,5})x(\d{2,5})_`)

// NewPornOne builds the pornone extractor.
func NewPornOne(client *httpx.Client) *PornOne { return &PornOne{client: client} }

func (p *PornOne) Name() string { return "pornone" }

func (p *PornOne) Match(u *url.URL) bool { return util.HostMatches(u.Host, "pornone.com") }

// Extract picks the largest rendition, re-reading the page at download time
// because the links carry an expiry.
func (p *PornOne) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	page := u.String()
	return refetchedVideo(ctx, ".mp4", httpx.Referer(pornoneRoot+"/"), func(ctx context.Context) (string, string, error) {
		title, best, err := p.best(ctx, page)
		return title, best.URL, err
	})
}

func (p *PornOne) best(ctx context.Context, page string) (string, mediaCandidate, error) {
	doc, err := p.client.GetString(ctx, page, httpx.Referer(pornoneRoot+"/"))
	if err != nil {
		return "", mediaCandidate{}, fmt.Errorf("pornone: fetch %s: %w", page, err)
	}
	best, ok := bestCandidate(pornoneRenditions(doc))
	if !ok {
		return "", mediaCandidate{}, fmt.Errorf("pornone: no playable rendition on %s "+
			"(the video may have been removed)", page)
	}
	return util.FirstNonEmpty(firstHeading(doc), pageTitle(doc), "pornone"), best, nil
}

// pornoneRenditions reads the <source> elements the player offers.
//
// Their res and label attributes are not to be trusted: the largest file on
// a page can be marked 720p while its own name says 1920x1080. The name is
// what the encoder wrote, so it decides, and the attribute is only a
// fallback for a rendition whose name carries no dimensions.
func pornoneRenditions(doc string) []mediaCandidate {
	root, err := parseHTML(doc)
	if err != nil {
		return nil
	}
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
		out = append(out, mediaCandidate{
			URL:     link,
			Quality: quality,
			IsHLS:   strings.Contains(link, ".m3u8"),
		})
	}
	return out
}

// ----------------------------------------------------------------- shared

// jsonObjectAfter extracts the brace-matched object that follows a marker.
func jsonObjectAfter(doc string, marker *regexp.Regexp) (string, error) {
	loc := marker.FindStringIndex(doc)
	if loc == nil {
		return "", fmt.Errorf("no player configuration found")
	}
	start := strings.LastIndex(doc[:loc[1]], "{")
	if start < 0 {
		// Guards a marker that does not itself end in the opening brace,
		// which the current callers' do; without this a future marker would
		// index the document at -1.
		return "", fmt.Errorf("no object after the player configuration marker")
	}
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
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			depth++
		case c == '}':
			if depth--; depth == 0 {
				return doc[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("player configuration is truncated")
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
func pageTitle(doc string) string {
	root, err := parseHTML(doc)
	if err != nil {
		return ""
	}
	return trimSiteSuffix(firstText(root, atom.Title))
}
