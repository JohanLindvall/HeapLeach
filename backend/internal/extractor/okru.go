package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

// OKru resolves Odnoklassniki video pages.
//
// The watch page is no use on its own. Served to a logged-out client it
// carries the player's options with an empty rendition list, and asking the
// player's own metadata endpoint for them answers the same way — the site
// keeps playback links back from anonymous callers there. The embed page,
// which exists to be framed by other sites, carries the identical structure
// filled in. So that is what is read, and nothing else is needed: no
// session, no token, no player JavaScript.
//
// The links it hands out are signed with both an expiry and the address that
// asked for them, so they are resolved when the item starts rather than when
// the page is read.
type OKru struct {
	client *httpx.Client
}

const (
	okruRoot = "https://ok.ru"
	// okruEmbed is the page carrying a filled-in rendition list.
	okruEmbed = okruRoot + "/videoembed/"
	// okruOptionsAttr holds the player configuration, as JSON in an
	// attribute. The HTML parser unescapes it, so it needs no unescaping
	// of its own.
	okruOptionsAttr = "data-options"
)

// okruQualities are the rendition names, worst first. The site names them
// rather than measuring them, and lists them in no reliable order, so this
// is what "best" means here.
var okruQualities = []string{
	"mobile", "lowest", "low", "sd", "hd", "full", "quad", "ultra",
}

// NewOKru builds the odnoklassniki extractor.
func NewOKru(client *httpx.Client) *OKru { return &OKru{client: client} }

func (o *OKru) Name() string { return "ok.ru" }

func (o *OKru) Match(u *url.URL) bool {
	return util.HostMatches(u.Host, "ok.ru") || util.HostMatches(u.Host, "odnoklassniki.ru")
}

// Extract resolves a video page to its best rendition.
func (o *OKru) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id := okruVideoID(u)
	if id == "" {
		return nil, fmt.Errorf("ok.ru: no video id in %s", u.Redacted())
	}

	meta, err := o.metadata(ctx, id)
	if err != nil {
		return nil, err
	}
	title := util.FirstNonEmpty(meta.Movie.Title, "ok.ru-"+id)
	headers := httpx.Referer(okruRoot + "/")

	if best, ok := okruBest(meta.Videos); ok {
		return &Result{Title: title, Files: []File{{
			Name:    title + ".mp4",
			URL:     best,
			Size:    -1,
			Headers: headers,
			Resolve: func(ctx context.Context) (*Target, error) {
				fresh, err := o.metadata(ctx, id)
				if err != nil {
					return nil, err
				}
				link, ok := okruBest(fresh.Videos)
				if !ok {
					return nil, fmt.Errorf("ok.ru: %s no longer offers a downloadable rendition", id)
				}
				return &Target{URL: link, Name: title + ".mp4", Size: -1, Headers: headers}, nil
			},
		}}}, nil
	}

	// Live streams, and some ordinary videos, come only as a playlist.
	if meta.HLSManifestURL != "" {
		segments, _, err := resolvePlaylist(ctx, o.client, meta.HLSManifestURL, headers)
		if err != nil {
			return nil, fmt.Errorf("ok.ru: %w", err)
		}
		return &Result{Title: title, Files: []File{{
			Name: title + ".ts", Size: -1, Headers: headers, Segments: segments,
		}}}, nil
	}

	return nil, fmt.Errorf("ok.ru: %s offers no playable rendition%s", id, okruWhyNot(meta))
}

// okruWhyNot adds the site's own explanation when it gave one. A video that
// is merely gone reports it here rather than as a parse failure.
func okruWhyNot(meta *okruMetadata) string {
	status := strings.TrimSpace(meta.Movie.StatusText)
	if status == "" || strings.EqualFold(status, "OK") {
		return " (it may be private, deleted, or restricted to signed-in viewers)"
	}
	return " (" + status + ")"
}

// metadata reads the embed page's player configuration, asking again when
// the page arrives without one.
//
// The site intermittently serves its ordinary page with the rendition list
// empty while still reporting the video as fine. It comes and goes in
// windows of minutes and follows neither the pacing of the requests nor
// anything about the client, so it looks like a bad backend rather than
// anything asked of us. Reported as it stands it reads as "this video offers
// nothing", which is wrong and leaves the user nothing to act on, so a page
// that calls the movie OK and then lists nothing is asked for again. A page
// that says the video is gone is believed at once.
func (o *OKru) metadata(ctx context.Context, id string) (*okruMetadata, error) {
	var last *okruMetadata
	for attempt := 0; attempt < config.ExtractRetries; attempt++ {
		if attempt > 0 {
			wait := util.Backoff(attempt-1, config.RequestRetryBase, config.RequestRetryMax)
			if err := util.SleepCtx(ctx, wait); err != nil {
				return nil, err
			}
		}
		meta, err := o.fetchMetadata(ctx, id)
		if err != nil {
			return nil, err
		}
		last = meta
		if okruPlayable(meta) || !strings.EqualFold(meta.Movie.Status, "OK") {
			return meta, nil
		}
	}
	return last, nil
}

// okruPlayable reports whether the metadata carries anything to fetch.
func okruPlayable(meta *okruMetadata) bool {
	if meta.HLSManifestURL != "" {
		return true
	}
	_, ok := okruBest(meta.Videos)
	return ok
}

// fetchMetadata reads the embed page once.
func (o *OKru) fetchMetadata(ctx context.Context, id string) (*okruMetadata, error) {
	page := okruEmbed + url.PathEscape(id)
	doc, err := o.client.GetString(ctx, page, httpx.Referer(okruRoot+"/"))
	if err != nil {
		return nil, fmt.Errorf("ok.ru: fetch %s: %w", page, err)
	}
	meta, err := okruMetadataFrom(doc)
	if err != nil {
		return nil, fmt.Errorf("ok.ru: %s: %w", page, err)
	}
	return meta, nil
}

// okruMetadataFrom pulls the metadata out of a fetched embed page.
//
// It is nested twice on purpose by the site: the element's attribute is JSON,
// and one of its fields is another JSON document as a string. Kept apart from
// the fetch so a fixture can stand in for the page.
func okruMetadataFrom(doc string) (*okruMetadata, error) {
	root, err := parseHTML(doc)
	if err != nil {
		return nil, err
	}

	// Several elements can carry player options; the one that matters is
	// whichever holds a metadata document.
	for _, n := range findAll(root, func(n *html.Node) bool { return attr(n, okruOptionsAttr) != "" }) {
		var opts struct {
			Flashvars struct {
				Metadata json.RawMessage `json:"metadata"`
			} `json:"flashvars"`
		}
		if err := json.Unmarshal([]byte(attr(n, okruOptionsAttr)), &opts); err != nil {
			continue
		}
		raw := opts.Flashvars.Metadata
		if len(raw) == 0 {
			continue
		}
		// The field is usually a JSON string holding a JSON document, and
		// occasionally the document itself.
		var nested string
		if err := json.Unmarshal(raw, &nested); err == nil {
			raw = json.RawMessage(nested)
		}
		var meta okruMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue
		}
		return &meta, nil
	}
	return nil, fmt.Errorf("no player configuration on the page")
}

// okruBest picks the largest rendition the page offered.
func okruBest(videos []okruVideo) (string, bool) {
	// Starts below the rank of an unknown name, so a rendition the site
	// labelled in a way this does not recognise is still taken when it is
	// all there is.
	best, rank := "", -2
	for _, v := range videos {
		if v.URL == "" {
			continue
		}
		if r := okruRank(v.Name); r > rank {
			best, rank = v.URL, r
		}
	}
	return best, best != ""
}

// okruRank orders a rendition by name. An unknown name ranks below every
// known one but above nothing at all, so a site that invents a new label
// still yields a download.
func okruRank(name string) int {
	for i, known := range okruQualities {
		if strings.EqualFold(name, known) {
			return i
		}
	}
	return -1 // usable, but never preferred over a name that is recognised
}

// okruVideoID reads the numeric id out of the link shapes the site uses.
func okruVideoID(u *url.URL) string {
	if id := u.Query().Get("st.mvId"); id != "" {
		return id
	}
	segs := util.PathSegments(u)
	for i, seg := range segs {
		switch seg {
		case "video", "videoembed", "live":
			if i+1 < len(segs) {
				return segs[i+1]
			}
		}
	}
	return ""
}

// okruMetadata is the part of the player configuration this needs.
type okruMetadata struct {
	Movie struct {
		Title      string `json:"title"`
		Status     string `json:"status"`
		StatusText string `json:"statusText"`
		IsLive     bool   `json:"isLive"`
	} `json:"movie"`
	Videos         []okruVideo `json:"videos"`
	HLSManifestURL string      `json:"hlsManifestUrl"`
}

// okruVideo is one rendition.
type okruVideo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}
