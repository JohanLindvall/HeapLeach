// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// ORF reads ORF ON's own API (api-tvthek.orf.at), with the credentials the
// site hands every visitor: its runtime configuration (window.__NUXT__.config)
// names the API's base, user and password, and the site's own player sends
// them as Basic authorisation. They are read off the page rather than written
// down here, and read again when the API stops accepting them.
//
// An episode names its streams (one adaptive HLS master among fixed
// qualities), its DRM flag, the rights it is shown under, and its subtitles
// as files of their own, WebVTT among them. The master keeps its audio in
// renditions of their own, the audio description one of them, which the
// downloader joins to the video with the subtitles (download/mux.go). It used
// to go to yt-dlp for the muxing.
//
// What is shown in Austria alone is not refused by the API but by the
// stream: asked from elsewhere, the master redirects to a placeholder that
// says so on screen (insert_geoprotection), which would download as a short
// clip of something else. That is recognised and refused in words, and a
// series of such episodes left out after one has shown it.
//
// Anything else on orf.at — its radio, its news pages — is still handed to
// yt-dlp, as all of it was.
type ORF struct {
	client *httpx.Client
	// site is where the runtime configuration is read from, a field so that
	// tests can stand in for it.
	site    string
	handoff *Handoff

	mu  sync.Mutex
	api *orfAPI
}

// orfAPI is the API's base and the authorisation the site sends it.
type orfAPI struct{ base, authorization string }

// orfRest is the part of orf.at still handed to yt-dlp.
var orfRest = handoffSite{
	name: "orf",
	why:  "only ORF ON's videos and series are read natively, not a page like this one",
}

const orfSite = "https://on.orf.at/"

// NewORF builds the ORF extractor.
func NewORF(client *httpx.Client) *ORF {
	return &ORF{
		client: client, site: orfSite,
		handoff: &Handoff{client: client, site: orfRest},
	}
}

func (o *ORF) Name() string { return "orf" }

// Recognises is the inventory's description.
func (o *ORF) Recognises() string {
	return "episodes and series (`/video/<id>`, `/sendereihe/<id>`, `/profile/…`) of ORF ON, whose page carries the runtime configuration (`__NUXT__`) the API credentials are read from"
}

// Shape reports an episode or series link as ORF ON writes them, with the
// slug after the id: a bare /video/<number> is every other video site's
// form too, and only a probed page can say it is this one.
func (o *ORF) Shape(u *url.URL) bool {
	kind, _ := orfLink(u)
	segs := util.PathSegments(u)
	return kind != 0 && (segs[0] == "profile" || len(segs) >= 3)
}

// Recognise takes a page of ORF ON: it carries the runtime configuration
// (window.__NUXT__) that names the API and the credentials this reads with.
// A page of it that is no episode or series is handed to yt-dlp, as before.
func (o *ORF) Recognise(_ context.Context, p *Page) bool {
	return len(p.Segments()) > 0 && p.Has("__NUXT__") && p.Named("ORF ON")
}

// The kinds of page read natively.
const (
	orfEpisode = iota + 1
	orfSeries
)

var orfNumber = regexp.MustCompile(`^[0-9]+$`)

// orfLink tells what a link names: on.orf.at's /video/<id>[/<segment>][/slug]
// and /sendereihe/<id>[/slug], and the older tvthek.orf.at's
// /profile/<name>/<series>[/<title>/<episode>].
func orfLink(u *url.URL) (kind int, id string) {
	segs := util.PathSegments(u)
	switch {
	case len(segs) >= 2 && segs[0] == "video" && orfNumber.MatchString(segs[1]):
		return orfEpisode, segs[1]
	case len(segs) >= 2 && segs[0] == "sendereihe" && orfNumber.MatchString(segs[1]):
		return orfSeries, segs[1]
	case len(segs) >= 5 && segs[0] == "profile" && orfNumber.MatchString(segs[4]):
		return orfEpisode, segs[4]
	case len(segs) >= 3 && segs[0] == "profile" && orfNumber.MatchString(segs[2]):
		return orfSeries, segs[2]
	}
	return 0, ""
}

// Extract reads an episode or a series, and hands anything else on orf.at
// to yt-dlp.
func (o *ORF) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	switch kind, id := orfLink(u); kind {
	case orfEpisode:
		ep, err := o.episode(ctx, id)
		if err != nil {
			return nil, err
		}
		target, err := o.stream(ctx, ep)
		if err != nil {
			return nil, err
		}
		return &Result{Title: ep.name(), Files: []File{target.file("")}}, nil
	case orfSeries:
		return o.series(ctx, id, opts)
	}
	return o.handoff.Extract(ctx, u, opts)
}

// orfEpisodeDoc is the part of an episode read here.
type orfEpisodeDoc struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	ProfileTitle   string `json:"profile_title"`
	Right          string `json:"right"`
	IsDRMProtected bool   `json:"is_drm_protected"`
	Sources        struct {
		HLS []struct {
			Src              string `json:"src"`
			QualityKey       string `json:"quality_key"`
			IsAdaptiveStream bool   `json:"is_adaptive_stream"`
			IsDRMProtected   bool   `json:"is_drm_protected"`
		} `json:"hls"`
	} `json:"sources"`
	Embedded struct {
		Subtitle *struct {
			VTTURL string `json:"vtt_url"`
		} `json:"subtitle"`
	} `json:"_embedded"`
}

// name is what an episode is filed as: its series and its title, the
// series left out where the title already says it.
func (e *orfEpisodeDoc) name() string {
	return cmp.Or(ardJoin(strings.TrimSpace(e.ProfileTitle), strings.TrimSpace(e.Title)), strconv.FormatInt(e.ID, 10))
}

// errORFAustriaOnly is an episode the stream refuses outside Austria.
var errORFAustriaOnly = errors.New("ORF shows it in Austria only, and its stream shows a notice saying so to anywhere else")

// episode reads one episode.
func (o *ORF) episode(ctx context.Context, id string) (*orfEpisodeDoc, error) {
	var ep orfEpisodeDoc
	if err := o.get(ctx, "episode/"+url.PathEscape(id), &ep); err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("orf: episode %s does not exist, or is no longer online", id)
		}
		return nil, fmt.Errorf("orf: episode %s: %w", id, err)
	}
	return &ep, nil
}

// stream resolves an episode's adaptive master into its file: the video,
// its audio renditions, and the subtitles the master or the episode names.
func (o *ORF) stream(ctx context.Context, ep *orfEpisodeDoc) (*Target, error) {
	name := ep.name()
	if ep.IsDRMProtected {
		return nil, fmt.Errorf("orf: %s is DRM-protected", name)
	}
	var master string
	for _, s := range ep.Sources.HLS {
		if s.IsAdaptiveStream && !s.IsDRMProtected && s.Src != "" {
			master = s.Src
			break
		}
	}
	if master == "" {
		return nil, fmt.Errorf("orf: %s offers no stream to download", name)
	}
	headers := httpx.RefererOrigin(orfSite, strings.TrimSuffix(orfSite, "/"))
	media, err := resolveMediaPlaylist(ctx, o.client, master, headers)
	if err != nil {
		return nil, fmt.Errorf("orf: %s: %w", name, err)
	}
	if orfGeoProtected(media) {
		return nil, fmt.Errorf("orf: %s: %w", name, errORFAustriaOnly)
	}
	target := media.target(master, name+media.extension(), headers)
	if sub := ep.Embedded.Subtitle; len(target.Subtitles) == 0 && sub != nil && sub.VTTURL != "" {
		target.Subtitles = []Track{{Segments: []string{sub.VTTURL}, Language: "de", Name: "Deutsch"}}
	}
	return target, nil
}

// orfGeoProtected reports a stream that is the notice ORF shows outside
// Austria in place of the programme.
func orfGeoProtected(media *hlsMedia) bool {
	return strings.Contains(media.Variant.URL, "geoprotection") ||
		slices.ContainsFunc(media.Segments, func(s string) bool { return strings.Contains(s, "geoprotection") })
}

// orfEpisodesPage is one page of a series' episodes, newest first.
type orfEpisodesPage struct {
	Pages    int `json:"pages"`
	Embedded struct {
		Items []orfEpisodeDoc `json:"items"`
	} `json:"_embedded"`
}

// series reads a series into one file an episode, oldest first, each
// resolved when its transfer starts. An episode that is DRM-protected is
// left out; and once one shown in Austria alone turns out refused here,
// the rest of those are left out with it, the note saying so once.
func (o *ORF) series(ctx context.Context, id string, opts Options) (*Result, error) {
	var episodes []orfEpisodeDoc
	for page := 1; ; page++ {
		var doc orfEpisodesPage
		if err := o.get(ctx, fmt.Sprintf("profile/%s/episodes?limit=%d&page=%d", url.PathEscape(id), orfPageSize, page), &doc); err != nil {
			if page == 1 {
				if httpx.HasStatus(err, http.StatusNotFound) {
					return nil, fmt.Errorf("orf: series %s does not exist", id)
				}
				return nil, fmt.Errorf("orf: series %s: %w", id, err)
			}
			break
		}
		episodes = append(episodes, doc.Embedded.Items...)
		if page >= doc.Pages || len(doc.Embedded.Items) == 0 {
			break
		}
	}
	slices.Reverse(episodes)

	var (
		files        []File
		drm, austria int
		blocked      *bool // whether Austria-only episodes are refused here, once one has said
		title        string
	)
	limit := opts.maxFiles()
	for i := range episodes {
		ep := &episodes[i]
		title = cmp.Or(title, strings.TrimSpace(ep.ProfileTitle))
		switch {
		case ep.IsDRMProtected:
			drm++
			continue
		case ep.Right != "worldwide" && blocked == nil:
			refused := false
			if full, err := o.episode(ctx, strconv.FormatInt(ep.ID, 10)); err == nil {
				_, err := o.stream(ctx, full)
				refused = errors.Is(err, errORFAustriaOnly)
			}
			blocked = &refused
		}
		if ep.Right != "worldwide" && *blocked {
			austria++
			continue
		}
		if len(files) < limit {
			files = append(files, o.lazy(ep))
		}
	}
	if len(files) == 0 {
		switch {
		case austria > 0:
			return nil, fmt.Errorf("orf: %s: %w", cmp.Or(title, "series "+id), errORFAustriaOnly)
		case drm > 0:
			return nil, fmt.Errorf("orf: %s: every episode of it is DRM-protected", cmp.Or(title, "series "+id))
		}
		return nil, fmt.Errorf("orf: series %s lists no episodes", id)
	}
	note := listingNote(cappedNote(len(files), len(episodes)-drm-austria, "episodes"),
		leftOutNote(austria, " that ORF shows in Austria only"), leftOutNote(drm, " that are DRM-protected"))
	return &Result{Title: cmp.Or(title, "series "+id), Files: files, Note: note}, nil
}

// orfPageSize is how many episodes a page of a series asks for.
const orfPageSize = 100

// lazy is a series' episode, read again and resolved when its transfer
// starts.
func (o *ORF) lazy(ep *orfEpisodeDoc) File {
	id := strconv.FormatInt(ep.ID, 10)
	return lazyEpisode(ep.name()+".mp4", "", nil, func(ctx context.Context) (*Target, error) {
		full, err := o.episode(ctx, id)
		if err != nil {
			return nil, err
		}
		return o.stream(ctx, full)
	})
}

// get fetches one API document, with the site's credentials. Credentials
// the API no longer accepts are read off the site again, once.
func (o *ORF) get(ctx context.Context, path string, out any) error {
	for attempt := 0; ; attempt++ {
		api, err := o.credentials(ctx, attempt > 0)
		if err != nil {
			return err
		}
		headers := httpx.Header{"Authorization": api.authorization, httpx.HeaderReferer: orfSite}
		err = o.client.GetJSON(ctx, api.base+path, headers, out)
		if attempt == 0 && httpx.HasStatus(err, http.StatusUnauthorized, http.StatusForbidden) {
			continue
		}
		return err
	}
}

// orfConfigAPI finds the API's part of the site's runtime configuration,
// and orfConfigField each of its fields: a JavaScript object, not JSON, so
// its keys are bare and their order the build's business.
var (
	orfConfigAPI   = regexp.MustCompile(`\bapi:\{([^{}]*)\}`)
	orfConfigField = regexp.MustCompile(`\b(baseUrl|user|password):"([^"]*)"`)
)

// credentials reads the API's base and authorisation off the site, kept
// until fresh asks for them again.
func (o *ORF) credentials(ctx context.Context, fresh bool) (*orfAPI, error) {
	o.mu.Lock()
	api := o.api
	o.mu.Unlock()
	if api != nil && !fresh {
		return api, nil
	}
	page, err := o.client.GetString(ctx, o.site, nil)
	if err != nil {
		return nil, fmt.Errorf("orf: read the site's configuration: %w", err)
	}
	fields := make(map[string]string)
	for _, block := range orfConfigAPI.FindAllStringSubmatch(page, -1) {
		for _, f := range orfConfigField.FindAllStringSubmatch(block[1], -1) {
			fields[f[1]] = f[2]
		}
		if fields["baseUrl"] != "" && fields["user"] != "" {
			break
		}
		clear(fields)
	}
	if fields["baseUrl"] == "" || fields["user"] == "" {
		return nil, errors.New("orf: the site's configuration names no API to ask")
	}
	base := strings.TrimSuffix(fields["baseUrl"], "/") + "/"
	api = &orfAPI{base: base, authorization: "Basic " +
		base64.StdEncoding.EncodeToString([]byte(fields["user"]+":"+fields["password"]))}
	o.mu.Lock()
	o.api = api
	o.mu.Unlock()
	return api, nil
}
