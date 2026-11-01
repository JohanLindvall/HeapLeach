// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/html"
)

// Odnoklassniki video pages.
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
// asked for them, so the row is signed. Live streams, and some ordinary
// videos, come only as a playlist, which is taken when no file is offered;
// a live one has no end, and is let through.
const (
	okruRoot = "https://ok.ru"
	// okruEmbed is the page carrying a filled-in rendition list.
	okruEmbed = okruRoot + "/videoembed/"
	// okruOptionsAttr holds the player configuration, as JSON in an
	// attribute. The HTML parser unescapes it, so it needs no unescaping
	// of its own.
	okruOptionsAttr = "data-options"
	// okruPlaylistRank ranks the playlist below every file, an unknown name
	// included (okruRank).
	okruPlaylistRank = -2
)

// okruQualities are the rendition names, worst first. The site names them
// rather than measuring them, and lists them in no reliable order, so this
// is what "best" means here.
var okruQualities = []string{
	"mobile", "lowest", "low", "sd", "hd", "full", "quad", "ultra",
}

// okruShape is a video page, its player, or a live stream, by itself or
// in a group's or a profile's videos.
var okruShape = pathShape(`^/(?:(?:group|profile)/[^/]+/)?(?:video|videoembed|live)/[0-9]+/?$`)

// okruLegacy is the old desktop link, /dk?st.cmd=…&st.mvId=<id>, whose page
// is never the player.
var okruLegacy = pathQueryShape(`^/dk\?(?:.*&)?st\.mvId=[0-9]+(?:&|$)`)

// okruCanonicalGate admits a link whose own page may not mount the player
// to the check of the embed it names: a group's or a profile's video, or
// the legacy desktop link.
func okruCanonicalGate(p *Page) bool {
	return okruLegacy(p.URL) || okruListedVideo(p.URL)
}

// okruListedVideo is a video as a group or a profile lists it.
var okruListedVideo = pathShape(`^/(?:group|profile)/[^/]+/video/[0-9]+/?$`)

// okruPlayerModule is the player component the pages mount, configured by
// the data-options attribute read reads.
const okruPlayerModule = `data-module="OKVideo"`

var okruPlayer = playerSite{
	name:      "ok.ru",
	shape:     okruShape,
	recognise: func(p *Page) bool { return okruShape(p.Final) && p.Has(okruPlayerModule, okruOptionsAttr) },
	recognises: "videos (`/video/<id>`, `/videoembed/<id>`, `/live/<id>`, a group's or a profile's) whose page " +
		"mounts the OK video player (`OKVideo`) with its `data-options` configuration; group, profile and " +
		"legacy `/dk?st.mvId=<id>` links by the embed they name",
	probeCanonical: true,
	canonicalGate:  okruCanonicalGate,
	page: func(u *url.URL) (string, error) {
		id := okruVideoID(u)
		if id == "" {
			return "", fmt.Errorf("no video id in %s", u.Redacted())
		}
		return okruEmbed + url.PathEscape(id), nil
	},
	referer: siteRoot(okruRoot),
	read:    okruRead,
	signed:  true,
}

// okruRead reads the embed page's renditions, asking again when the page
// arrives without any.
//
// The site intermittently serves its ordinary page with the rendition list
// empty while still reporting the video as fine. It comes and goes in
// windows of minutes and follows neither the pacing of the requests nor
// anything about the client, so it looks like a bad backend rather than
// anything asked of us. Reported as it stands it reads as "this video offers
// nothing", which is wrong and leaves the user nothing to act on, so a page
// that calls the movie OK and then lists nothing is asked for again. A page
// that says the video is gone is believed at once.
func okruRead(ctx context.Context, client *httpx.Client, page string) (*playerVideo, error) {
	id, _ := url.PathUnescape(path.Base(page))
	meta, err := withExtractRetries(ctx,
		func(ctx context.Context) (*okruMetadata, error) { return okruFetch(ctx, client, page) },
		func(meta *okruMetadata, err error) bool {
			return err == nil && !okruPlayable(meta) && strings.EqualFold(meta.Movie.Status, "OK")
		})
	if err != nil {
		return nil, err
	}
	if !okruPlayable(meta) {
		return nil, fmt.Errorf("%s offers no playable rendition%s", id, okruWhyNot(meta))
	}
	return &playerVideo{title: cmp.Or(meta.Movie.Title, "ok.ru-"+id), candidates: okruCandidates(meta)}, nil
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

// okruPlayable reports whether the metadata carries anything to fetch.
func okruPlayable(meta *okruMetadata) bool {
	_, ok := bestCandidate(okruCandidates(meta))
	return ok
}

// okruCandidates are the files the page offers, ranked by name, and the
// playlist below them all.
func okruCandidates(meta *okruMetadata) []mediaCandidate {
	var out []mediaCandidate
	for _, v := range meta.Videos {
		out = append(out, mediaCandidate{URL: v.URL, Quality: okruRank(v.Name)})
	}
	if meta.HLSManifestURL != "" {
		out = append(out, mediaCandidate{URL: meta.HLSManifestURL, Quality: okruPlaylistRank, IsHLS: true})
	}
	return out
}

// okruFetch reads the embed page once.
func okruFetch(ctx context.Context, client *httpx.Client, page string) (*okruMetadata, error) {
	// The probe's own tree on the first attempt; withExtractRetries hands a
	// retry a context without it, so an empty but OK player is asked again.
	root, err := fetchPageRoot(ctx, client, page, httpx.Referer(okruRoot+"/"))
	if err != nil {
		return nil, videoPageError{fmt.Errorf("fetch %s: %w", page, err)}
	}
	meta, err := okruMetadataRead(root)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", page, err)
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
	return okruMetadataRead(root)
}

// okruMetadataRead is okruMetadataFrom for a parsed page, which may be the
// shared probe's: it is only read.
func okruMetadataRead(root *html.Node) (*okruMetadata, error) {

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

// okruRank orders a rendition by name. An unknown name ranks below every
// known one but above nothing at all, so a site that invents a new label
// still yields a download.
func okruRank(name string) int {
	// An unknown name is usable, but never preferred over one recognised.
	return rankIn(okruQualities, name, -1)
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
