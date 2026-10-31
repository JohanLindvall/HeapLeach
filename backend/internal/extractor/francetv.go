// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// FranceTV reads france.tv through the API its player calls
// (k7.ftven.fr/videos/<id>), asked as Safari. Asked as Chrome, that API
// answers with DASH, which is where "most of the catalogue is DASH" came
// from and why france.tv went to yt-dlp; asked as Safari, it answers with
// HLS of the same asset, without DRM for nearly all of it. What France TV
// keeps under DRM it refuses to a player that does not declare DRM (2019),
// and that refusal is the signal: a player that declares it is handed
// FairPlay instead.
//
// The master is signed by a token service the answer names
// (hdfauth.ftven.fr), for six hours in the path and ten minutes in the
// query, so it is minted when the transfer starts. Its video is MPEG-TS
// under AES-128, one static key, and its audio is packed AAC in renditions
// of its own, timed by an ID3 tag that the join reads (download/
// packedaudio.go): ffmpeg alone puts that audio ten seconds off the
// picture. Subtitles are taken from the answer's whole WebVTT files rather
// than the master's segmented ones, whose packager splits a cue at every
// segment boundary into two.
//
// The asset's name says where it is shown: "_monde_" everywhere,
// "_france-domtom_" in France and its overseas departments, which the CDN
// refuses from elsewhere with a 403. The API answers wherever it is asked
// from unless told a country, which is not done here.
//
// A video page names its id in its flight data, as the player's options.
// A programme's page and its "toutes-les-videos" pages list cards, each with
// the video's id and whether it is a whole programme ("integrale") or a
// clip, so a listing costs a request a page; the clips are left out where
// whole programmes are listed beside them. Anything else on france.tv,
// such as a live channel's page, is still handed to yt-dlp.
type FranceTV struct {
	about
	client *httpx.Client
	// api is the player API's base, a field so that tests can stand in for
	// it.
	api     string
	handoff *Handoff
}

const francetvAPIRoot = "https://k7.ftven.fr/videos/"

// francetvRest is the part of france.tv still handed to yt-dlp.
var francetvRest = handoffSite{
	name: "francetv",
	why:  "only france.tv's programmes and their listings are read natively, not a page like this one",
}

// NewFranceTV builds the france.tv extractor.
func NewFranceTV(client *httpx.Client) *FranceTV {
	return &FranceTV{
		client: client, api: francetvAPIRoot,
		handoff: newHandoff(client, francetvRest),
		about: about{name: "francetv", shape: francetvShape,
			recognises: "video pages (`/…/<id>-<slug>.html`) and programme listings of France TV whose flight data names `video_factory_id`s"},
	}
}

// francetvShape is a video page (/…/<id>-<slug>.html) or a programme's
// every video (/…/toutes-les-videos/).
var francetvShape = pathShape(`/[0-9]{6,}-[^/]+\.html$`, `/toutes-les-videos/?$`)

// Recognise takes a page whose flight data names the player's video ids
// (video_factory_id), which is what this resolves: a video page, known by
// its form when its title names only the video, or a programme's page.
func (f *FranceTV) Recognise(_ context.Context, p *Page) bool {
	return len(p.Segments()) > 0 && p.Has("video_factory_id") &&
		(p.Named("France TV") || francetvContentID.MatchString(p.Final.Path))
}

var (
	francetvContentID = regexp.MustCompile(`/([0-9]+)-[^/]*\.html$`)
	francetvOptionsID = regexp.MustCompile(`"options":\{"id":"([0-9a-f-]{36})"`)
	francetvCard      = regexp.MustCompile(`\{"content":\{"id":([0-9]+),"title":"((?:[^"\\]|\\.)*)"(?s:.*?)"tracking":\{([^{}]*)\}`)
	francetvFactoryID = regexp.MustCompile(`"video_factory_id":"([0-9a-f-]{36})"`)
	francetvFormat    = regexp.MustCompile(`"content_format":"([a-z]+)"`)
)

// Extract reads a video page into its file, a programme's page into one
// file a programme, and hands anything else on france.tv to yt-dlp.
func (f *FranceTV) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	page, _, err := fetchDocument(ctx, f.client, u, nil)
	if err != nil {
		return nil, fmt.Errorf("francetv: %s: %w", u.Redacted(), err)
	}
	// The flight data is JSON written into script as strings.
	flight := strings.ReplaceAll(page, `\"`, `"`)
	pageTitle := francetvPageTitle(page)
	if id := francetvVideoID(flight, u); id != "" {
		// The page's title names the video where the API refuses to.
		label := cmp.Or(pageTitle, id)
		target, title, err := f.video(ctx, id, "", label)
		if err != nil {
			return nil, err
		}
		return &Result{Title: title, Files: []File{refreshing(target, func(ctx context.Context) (*Target, error) {
			target, _, err := f.video(ctx, id, "", label)
			return target, err
		})}}, nil
	}
	if strings.Contains(flight, `"video_factory_id"`) {
		return f.programme(ctx, u, cmp.Or(pageTitle, u.Redacted()), flight, opts)
	}
	return f.handoff.Extract(ctx, u, opts)
}

// francetvPageTitle is what a page is called: its og:title, less the "en
// replay" the site adds, or its title, less the site's name and, for a
// programme's every video, the "Toutes les vidéos" that would file them
// apart from the programme's own page. A video page's title is the site's
// name alone, and a listing's og:title the site's description.
func francetvPageTitle(page string) string {
	root, err := parseHTML(page)
	if err != nil {
		return ""
	}
	if og := strings.TrimSpace(strings.TrimSuffix(metaContent(root, "og:title"), " en replay")); og != "" && !strings.HasPrefix(og, "France TV") {
		return og
	}
	title := strings.TrimSuffix(trimSiteSuffix(strings.TrimSpace(firstText(root, atomTitle))), " - Toutes les vidéos")
	if title == "France TV" {
		return ""
	}
	return title
}

// francetvVideoID reads the id a video page plays: its player's options,
// or the tracking whose content id is the one the page's link ends in.
func francetvVideoID(flight string, u *url.URL) string {
	if m := francetvOptionsID.FindStringSubmatch(flight); m != nil {
		return m[1]
	}
	m := francetvContentID.FindStringSubmatch(u.Path)
	if m == nil {
		return ""
	}
	tracking := regexp.MustCompile(`"content_id":` + m[1] + `[,}][^{}]*?"video_factory_id":"([0-9a-f-]{36})"`)
	if m := tracking.FindStringSubmatch(flight); m != nil {
		return m[1]
	}
	return ""
}

// francetvAnswer is the part of the player API's answer read here, or of
// its refusal.
type francetvAnswer struct {
	Video struct {
		Format string `json:"format"`
		URL    string `json:"url"`
		Token  struct {
			Akamai string `json:"akamai"`
		} `json:"token"`
		DRM    json.RawMessage `json:"drm"`
		IsLive bool            `json:"is_live"`
	} `json:"video"`
	Meta struct {
		Title           string `json:"title"`
		AdditionalTitle string `json:"additional_title"`
		PreTitle        string `json:"pre_title"`
		Subtitles       []struct {
			URL  string `json:"url"`
			Lang string `json:"lang"`
		} `json:"subtitles"`
	} `json:"meta"`
}

// name is what a video is called: its programme, then its episode's
// number and title where it has them.
func (a *francetvAnswer) name() string {
	var episode []string
	for _, part := range []string{a.Meta.PreTitle, a.Meta.AdditionalTitle} {
		if part = strings.TrimSpace(part); part != "" {
			episode = append(episode, part)
		}
	}
	return joinTitle(strings.TrimSpace(a.Meta.Title), strings.Join(episode, " - "), titleContains)
}

// protected reports an answer that declares DRM: false, absent, or an
// object saying it is not active, is clear.
func (a *francetvAnswer) protected() bool {
	raw := strings.TrimSpace(string(a.Video.DRM))
	if raw == "" || raw == "null" || raw == "false" {
		return false
	}
	var drm struct {
		Active bool `json:"active"`
	}
	return raw == "true" || json.Unmarshal(a.Video.DRM, &drm) == nil && drm.Active
}

// francetvTag is markup in an API message, a line break mostly.
var francetvTag = regexp.MustCompile(`<[^>]*>`)

// francetvPlain is an API message as plain text.
func francetvPlain(message string) string {
	return strings.Join(strings.Fields(francetvTag.ReplaceAllString(message, " ")), " ")
}

// errFranceTVFranceOnly is a video shown in France alone, refused here.
var errFranceTVFranceOnly = errors.New("shown in France only, and its stream is refused anywhere else")

// francetvGone is what the API's refusals of an id mean.
const francetvGone = "it is no longer on france.tv, or never was"

// answer asks the player API for a video, as Safari.
func (f *FranceTV) answer(ctx context.Context, id, title string) (*francetvAnswer, error) {
	query := url.Values{"domain": {"www.france.tv"}, "device_type": {"desktop"}, "browser": {"safari"}}
	var answer francetvAnswer
	err := f.client.GetJSON(ctx, f.api+url.PathEscape(id)+"?"+query.Encode(), francetvHeaders(), &answer)
	if err == nil {
		return &answer, nil
	}
	title = cmp.Or(title, id)
	refusal, ok := refusalBody[struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}](err)
	switch {
	case !ok || refusal.Code == 0:
		return nil, fmt.Errorf("francetv: %s: %w", title, err)
	case refusal.Code == 2019:
		return nil, &broadcastRefusal{"francetv", title, refusedDRM}
	case refusal.Code == 2003, refusal.Code == 2012:
		return nil, &broadcastRefusal{"francetv", title, francetvGone}
	case refusal.Code == 2009:
		return nil, fmt.Errorf("francetv: %s: %w", title, errFranceTVFranceOnly)
	}
	return nil, &broadcastRefusal{"francetv", title, "france.tv refuses it: " + francetvPlain(refusal.Message)}
}

// video asks for a video and resolves its stream, named stem where a
// listing named it, else after its programme and episode; label names it
// in a refusal that comes before the API has.
func (f *FranceTV) video(ctx context.Context, id, stem, label string) (*Target, string, error) {
	answer, err := f.answer(ctx, id, cmp.Or(stem, label))
	if err != nil {
		return nil, "", err
	}
	title := cmp.Or(stem, answer.name(), id)
	switch {
	case answer.Video.IsLive:
		return nil, "", &broadcastRefusal{"francetv", title, refusedLiveBroadcast}
	case answer.protected():
		return nil, "", &broadcastRefusal{"francetv", title, refusedDRM}
	case answer.Video.Format != "hls" || answer.Video.URL == "":
		return nil, "", &broadcastRefusal{"francetv", title, "france.tv offers no stream of it to download"}
	}
	master, err := f.sign(ctx, answer)
	if err != nil {
		return nil, "", fmt.Errorf("francetv: %s: %w", title, err)
	}
	headers := francetvHeaders()
	media, err := broadcastMedia(ctx, f.client, "francetv", title, master, headers)
	if err != nil {
		if francetvFranceOnly(answer.Video.URL) && httpx.HasStatus(err, http.StatusForbidden) {
			return nil, "", fmt.Errorf("francetv: %s: %w", title, errFranceTVFranceOnly)
		}
		return nil, "", err
	}
	target := media.target(master, title+media.extension(), headers)
	if whole := francetvSubtitles(answer); len(whole) > 0 {
		target.Subtitles = whole
	}
	return target, title, nil
}

// francetvFranceOnly reports an asset France TV shows in France alone, as
// its name says.
func francetvFranceOnly(link string) bool { return strings.Contains(link, "_france") }

// sign has the token service the answer names sign its master.
func (f *FranceTV) sign(ctx context.Context, answer *francetvAnswer) (string, error) {
	service := answer.Video.Token.Akamai
	if service == "" {
		return answer.Video.URL, nil
	}
	u, err := url.Parse(service)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("url", answer.Video.URL)
	q.Set("format", "json")
	u.RawQuery = q.Encode()
	var signed struct {
		URL string `json:"url"`
	}
	if err := f.client.GetJSON(ctx, u.String(), francetvHeaders(), &signed); err != nil {
		return "", fmt.Errorf("sign the stream: %w", err)
	}
	if signed.URL == "" {
		return "", errors.New("sign the stream: the token service answered with no link")
	}
	return signed.URL, nil
}

// francetvSubtitles are the answer's whole WebVTT files, one track each.
func francetvSubtitles(answer *francetvAnswer) []Track {
	var tracks []Track
	for _, s := range answer.Meta.Subtitles {
		if s.URL == "" {
			continue
		}
		track := Track{Segments: []string{s.URL}, Language: s.Lang, Name: s.Lang}
		switch s.Lang {
		case "qsm":
			track.Language, track.Name = "fr", "Français (sourds et malentendants)"
		case "fra", "fr":
			track.Language, track.Name = "fr", "Français"
		}
		tracks = append(tracks, track)
	}
	return tracks
}

// francetvRoot is the site the player is served from.
const francetvRoot = "https://www.france.tv"

// francetvHeaders are what the site's own player sends.
func francetvHeaders() httpx.Header { return httpx.SiteOrigin(francetvRoot) }

// francetvCardDoc is one card of a programme's listing.
type francetvCardDoc struct {
	title, id string
	whole     bool
}

// francetvCards reads the cards a page lists, in its order.
func francetvCards(flight string) []francetvCardDoc {
	var cards []francetvCardDoc
	seen := make(map[string]bool)
	for _, m := range francetvCard.FindAllStringSubmatch(flight, -1) {
		id := francetvFactoryID.FindStringSubmatch(m[3])
		if id == nil || seen[id[1]] {
			continue
		}
		seen[id[1]] = true
		title, err := strconv.Unquote(`"` + m[2] + `"`)
		if err != nil {
			title = m[2]
		}
		format := francetvFormat.FindStringSubmatch(m[3])
		cards = append(cards, francetvCardDoc{title: strings.TrimSpace(title), id: id[1], whole: format == nil || format[1] == "integrale"})
	}
	return cards
}

// programme reads a programme's page, and a "toutes-les-videos" listing a
// page at a time, into one file a whole programme; its clips are left out
// where whole programmes are listed beside them.
func (f *FranceTV) programme(ctx context.Context, u *url.URL, title, flight string, opts Options) (*Result, error) {
	cards := francetvCards(flight)
	walkNote := ""
	if strings.HasSuffix(strings.TrimSuffix(u.Path, "/"), "toutes-les-videos") {
		// The page in hand is the first. Pages are read until they hold as
		// many cards as the file cap, the cap and the clips left out being
		// counted once the cards are in.
		var err error
		seen := make(map[string]bool)
		cards, walkNote, err = walkPages(ctx, pageWalk[francetvCardDoc]{
			fetch: func(ctx context.Context, n int) (pageOf[francetvCardDoc], error) {
				page := cards
				if n > 0 {
					if len(seen) >= opts.maxFiles() {
						return pageOf[francetvCardDoc]{end: true}, nil
					}
					next := *u
					q := next.Query()
					q.Set("page", strconv.Itoa(n+1))
					next.RawQuery = q.Encode()
					html, err := f.client.GetString(ctx, next.String(), nil)
					if err != nil {
						return pageOf[francetvCardDoc]{}, err
					}
					page = francetvCards(strings.ReplaceAll(html, `\"`, `"`))
				}
				for _, c := range page {
					seen[c.id] = true
				}
				return pageOf[francetvCardDoc]{items: page, next: true}, nil
			},
			key:   func(c francetvCardDoc) string { return c.id },
			limit: -1,
			// A page past the end lists nothing new, which ends the walk
			// quietly; a page that fails is a cut, and noted.
			quiet:  true,
			failed: func(error) string { return couldNotFetchNote },
		})
		if err != nil {
			return nil, err
		}
	}
	whole := 0
	for _, c := range cards {
		if c.whole {
			whole++
		}
	}
	var files []File
	clips := 0
	limit := opts.maxFiles()
	for _, c := range cards {
		if whole > 0 && !c.whole {
			clips++
			continue
		}
		if len(files) < limit {
			files = append(files, f.lazy(c))
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("francetv: %s lists no videos", title)
	}
	clipNote := countNote(clips, "clips left out")
	if clips == 1 {
		clipNote = "1 clip left out"
	}
	return &Result{Title: title, Files: files, Note: joinNotes(cappedNote(len(files), len(cards)-clips, "videos"), clipNote, walkNote)}, nil
}

// lazy is a listed video, asked for and resolved when its transfer starts.
// It keeps the name it is listed under, so that a listing read again finds
// what was saved under it.
func (f *FranceTV) lazy(c francetvCardDoc) File {
	stem := cmp.Or(c.title, c.id)
	return lazyEpisode(stem+".mp4", "", francetvHeaders(), func(ctx context.Context) (*Target, error) {
		target, _, err := f.video(ctx, c.id, stem, stem)
		return target, err
	})
}
