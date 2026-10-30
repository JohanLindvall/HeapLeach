// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// RTBF reads Auvio, RTBF's player (auvio.rtbf.be), through the APIs its
// own pages use: RTBF's (bff-service.rtbf.be) for what a media or a
// programme is called and lists, and Red Bee Media's Exposure API
// (exposure.api.redbee.live) for the stream, asked with an anonymous
// session that needs no account, as the player asks before anyone signs
// in. That session plays what RTBF publishes to everyone: news clips,
// archives, some programmes, many podcasts. The play answer is the
// authority on the rest, and says it in its own terms: a free account
// (LOGIN), Belgium alone (no action at all, asked from elsewhere), paid
// (BUY_WATCH_NOW) or expired (NOT_PUBLISHED). RTBF's own
// isAnonymousAllowed is not: it was false on archives that played.
//
// A video is Unified Streaming HLS: fragmented MP4 parts, the audio in
// renditions of its own, subtitles as WebVTT renditions, nothing encrypted
// and nothing signed. Some packages declare one recording three times
// over, as the French track, the original version and the audio
// description, where the asset itself has one audio track; the asset
// record (content/asset) says how many it has, and where it has one, only
// the default rendition is taken. The same packages declare subtitle
// tracks with no cues, which the downloader leaves out. A podcast is one
// MP3.
//
// A programme page (/emission/<slug>-<id>) lists its episodes in a widget
// of tabs, one a season or a year, each a list paged by its own next link.
// Its entries say nothing of who may play them, so each is asked of the
// play API as the listing is read, and what cannot be had here is left
// out, the note counting each reason once. Any other page on rtbf.be, an
// article say, is read for the Auvio players embedded in it.
//
// It used to go to yt-dlp, whose RTBF extractor no longer works.
type RTBF struct {
	client *httpx.Client
	// bff and exposure are the APIs' bases, fields so that tests can stand
	// in for them.
	bff, exposure string

	mu      sync.Mutex
	session string // the anonymous session's token
	device  string // the device id it was asked with
}

const (
	rtbfBFF      = "https://bff-service.rtbf.be/auvio/v1.23"
	rtbfExposure = "https://exposure.api.redbee.live"
	// rtbfAgent is what the site's pages call themselves to its own API.
	rtbfAgent = "Chrome-web-3.0"
	// rtbfUnit is where RTBF's media sit in Red Bee's API.
	rtbfUnit = "/customer/RTBF/businessunit/Auvio"
	// rtbfPage is how many entries a page of a list asks for, the most the
	// API takes.
	rtbfPage = 50
)

// NewRTBF builds the RTBF Auvio extractor.
func NewRTBF(client *httpx.Client) *RTBF {
	return &RTBF{client: client, bff: rtbfBFF, exposure: rtbfExposure}
}

func (r *RTBF) Name() string { return "rtbf" }

// rtbfAuvioAPI marks an Auvio page: its widgets are addressed by the BFF
// API's paths, which this reads too.
var rtbfAuvioAPI = regexp.MustCompile(`/auvio/v1\.[0-9]+/widgets/`)

// Recognises is the inventory's description.
func (r *RTBF) Recognises() string {
	return "media, programmes and live pages of RTBF Auvio (`/media/<slug>-<id>`, `/emission/<slug>-<id>`) whose page addresses the Auvio API, and RTBF pages embedding Auvio players"
}

// Shape reports a media, programme, live or embedded-player link.
func (r *RTBF) Shape(u *url.URL) bool {
	kind, _ := rtbfLink(u)
	return kind != rtbfOther
}

// Recognise takes an Auvio page whose widgets are addressed by the Auvio
// API, or a page of RTBF's own that embeds Auvio players by asset id.
func (r *RTBF) Recognise(_ context.Context, p *Page) bool {
	if kind, _ := rtbfLink(p.Final); kind != rtbfOther {
		return rtbfAuvioAPI.MatchString(p.Body) && p.Named("RTBF Auvio", "Auvio")
	}
	return len(p.Segments()) > 0 && p.Named("RTBF", "RTBF Actus") &&
		(rtbfAssetAttr.MatchString(p.Body) || rtbfEmbedLink.MatchString(p.Body))
}

// The kinds of link read.
const (
	rtbfMedia = iota + 1
	rtbfProgramme
	rtbfLive
	rtbfOther
)

var (
	rtbfTrailingID = regexp.MustCompile(`(?:^|-)([0-9]+)$`)
	rtbfNumber     = regexp.MustCompile(`^[0-9]+$`)
)

// rtbfLink tells what a link names: Auvio's /media/<slug>-<id>,
// /emission/<slug>-<id>, an embedded player's /embed/…/media/<id>, the
// older www.rtbf.be/auvio/…?id=<id>, or a live stream; anything else is a
// page to look in for players.
func rtbfLink(u *url.URL) (kind int, id string) {
	segs := util.PathSegments(u)
	if len(segs) > 0 {
		trailing := func(seg string) string {
			if m := rtbfTrailingID.FindStringSubmatch(seg); m != nil {
				return m[1]
			}
			return ""
		}
		switch last := segs[len(segs)-1]; {
		case segs[0] == "media" && len(segs) >= 2 && trailing(segs[1]) != "":
			return rtbfMedia, trailing(segs[1])
		case segs[0] == "emission" && len(segs) >= 2 && trailing(segs[1]) != "":
			return rtbfProgramme, segs[1]
		case segs[0] == "live", segs[0] == "chaine", segs[0] == "direct":
			return rtbfLive, ""
		case segs[0] == "embed" && len(segs) >= 3 && rtbfNumber.MatchString(last):
			switch segs[len(segs)-2] {
			case "media":
				return rtbfMedia, last
			case "live", "radio":
				return rtbfLive, ""
			}
		}
	}
	if len(segs) > 0 && segs[0] == "auvio" && rtbfNumber.MatchString(u.Query().Get("id")) {
		return rtbfMedia, u.Query().Get("id")
	}
	return rtbfOther, ""
}

// Extract reads a media, a programme, or the players a page embeds.
func (r *RTBF) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	switch kind, id := rtbfLink(u); kind {
	case rtbfMedia:
		return r.media(ctx, id)
	case rtbfProgramme:
		return r.programme(ctx, id)
	case rtbfLive:
		return nil, fmt.Errorf("rtbf: %s is a live stream, which has no end to download to", u.Redacted())
	}
	return r.embedded(ctx, u)
}

// rtbfGone is what RTBF's answer means for an id it has nothing under.
const rtbfGone = "it is not on Auvio: removed, expired, or not published yet"

// rtbfRoot is the site the player is served from.
const rtbfRoot = "https://auvio.rtbf.be"

// rtbfHeaders are what the site's own player sends.
func rtbfHeaders() httpx.Header { return httpx.SiteOrigin(rtbfRoot) }

// bffURL is an endpoint of RTBF's own API, asked as the site's pages ask.
func (r *RTBF) bffURL(endpoint string) string {
	return r.bff + "/" + endpoint + "?userAgent=" + rtbfAgent
}

// rtbfMediaDoc is the part of a media's description read here.
type rtbfMediaDoc struct {
	Data struct {
		AssetID    string `json:"assetId"`
		PlayerType string `json:"playerType"`
		Title      string `json:"title"`
		Subtitle   string `json:"subtitle"`
		Program    *struct {
			Title string `json:"title"`
		} `json:"program"`
	} `json:"data"`
}

// media reads one media: what it is called, and its stream.
func (r *RTBF) media(ctx context.Context, id string) (*Result, error) {
	var doc rtbfMediaDoc
	if err := r.client.GetJSON(ctx, r.bffURL("embed/media/"+url.PathEscape(id)), rtbfHeaders(), &doc); err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			return nil, &broadcastRefusal{"rtbf", id, rtbfGone}
		}
		return nil, fmt.Errorf("rtbf: %s: %w", id, err)
	}
	data := doc.Data
	var programme string
	if data.Program != nil {
		programme = data.Program.Title
	}
	stem := cmp.Or(joinTitle(strings.TrimSpace(cmp.Or(programme, data.Title)), strings.TrimSpace(data.Subtitle), titleContains), id)
	if strings.EqualFold(data.PlayerType, "LIVE") {
		return nil, &broadcastRefusal{"rtbf", stem, "it is a live stream, which has no end to download to"}
	}
	target, err := r.stream(ctx, cmp.Or(data.AssetID, id+"_6BA97Bb"), stem)
	if err != nil {
		return nil, err
	}
	return &Result{Title: stem, Files: []File{target.file("")}}, nil
}

// rtbfPlay is Red Bee's answer to a request to play: the formats a media
// is offered in, or why it is not.
type rtbfPlay struct {
	Formats []struct {
		Format       string          `json:"format"`
		MediaLocator string          `json:"mediaLocator"`
		DRM          json.RawMessage `json:"drm"`
	} `json:"formats"`
	StreamInfo struct {
		Live bool `json:"live"`
	} `json:"streamInfo"`
	Message string `json:"message"`
	Actions []struct {
		Type        string `json:"type"`
		Publication *struct {
			AvailableAt time.Time `json:"availableAt"`
		} `json:"publication"`
	} `json:"actions"`
}

// playable is where a play answer's media is: an HLS master, or one file,
// which is what a podcast is.
func (p *rtbfPlay) playable() (hls, file string, protected bool) {
	for _, f := range p.Formats {
		if drm := strings.TrimSpace(string(f.DRM)); drm != "" && drm != "null" && drm != "{}" {
			protected = true
			continue
		}
		switch strings.ToUpper(f.Format) {
		case "HLS":
			hls = cmp.Or(hls, f.MediaLocator)
		case "MP3", "AAC", "MP4":
			file = cmp.Or(file, f.MediaLocator)
		}
	}
	return hls, file, protected
}

// refusal says why a play answer refused, in words.
func (p *rtbfPlay) refusal(title string, now time.Time) error {
	reason := "Auvio refuses to play it (" + p.Message + ")"
	switch p.Message {
	case "NOT_PUBLISHED":
		reason = rtbfGone
	case "VPN_BLOCKED":
		reason = "Auvio refuses this address as a VPN or a proxy"
	case "NOT_ENTITLED":
		reason = "RTBF shows it in Belgium only, or not yet"
		for _, action := range p.Actions {
			switch action.Type {
			case "LOGIN":
				if action.Publication != nil && action.Publication.AvailableAt.After(now) {
					reason = "it is not on Auvio before " + action.Publication.AvailableAt.Format(time.DateOnly)
				} else {
					reason = "RTBF shows it to signed-in viewers only"
				}
			case "BUY_WATCH_NOW":
				reason = "RTBF shows it to paying viewers only"
			}
		}
	}
	return &broadcastRefusal{"rtbf", title, reason}
}

// play asks Red Bee to play a media, with the anonymous session, made
// again once if Red Bee no longer takes it.
func (r *RTBF) play(ctx context.Context, assetID, title string) (*rtbfPlay, error) {
	token, err := r.anonymous(ctx, "")
	if err != nil {
		return nil, err
	}
	for renewed := false; ; renewed = true {
		headers := rtbfHeaders()
		headers[httpx.HeaderAuthorization] = "Bearer " + token
		var answer rtbfPlay
		err := r.client.GetJSON(ctx, r.exposure+"/v2"+rtbfUnit+"/entitlement/"+url.PathEscape(assetID)+"/play?supportedFormats=hls,mp3", headers, &answer)
		if err == nil {
			return &answer, nil
		}
		se, ok := errors.AsType[*httpx.StatusError](err)
		switch {
		case ok && se.Code == http.StatusUnauthorized && !renewed:
			if token, err = r.anonymous(ctx, token); err != nil {
				return nil, err
			}
			continue
		case ok && se.Code == http.StatusForbidden && json.Unmarshal([]byte(se.Body), &answer) == nil && answer.Message != "":
			return nil, answer.refusal(title, time.Now())
		case ok && se.Code == http.StatusNotFound:
			return nil, &broadcastRefusal{"rtbf", title, rtbfGone}
		}
		return nil, fmt.Errorf("rtbf: %s: %w", title, err)
	}
}

// anonymous is the anonymous session's token: made once a process, with a
// device id of its own, and made again when Red Bee stops taking stale.
func (r *RTBF) anonymous(ctx context.Context, stale string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session != "" && r.session != stale {
		return r.session, nil
	}
	if r.device == "" {
		r.device = rtbfDeviceID()
	}
	body := map[string]any{"device": map[string]string{"name": "Browser", "type": "WEB"}, "deviceId": r.device}
	var answer struct {
		SessionToken string `json:"sessionToken"`
	}
	if err := r.client.PostJSON(ctx, r.exposure+"/v2"+rtbfUnit+"/auth/anonymous", rtbfHeaders(), body, &answer); err != nil {
		return "", fmt.Errorf("rtbf: an anonymous session: %w", err)
	}
	if answer.SessionToken == "" {
		return "", errors.New("rtbf: an anonymous session: Red Bee answered with no token")
	}
	r.session = answer.SessionToken
	return r.session, nil
}

// rtbfDeviceID is a random device id, in the UUID form the player sends.
func rtbfDeviceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// stream resolves a media's stream under the name stem.
func (r *RTBF) stream(ctx context.Context, assetID, stem string) (*Target, error) {
	answer, err := r.play(ctx, assetID, stem)
	if err != nil {
		return nil, err
	}
	if answer.StreamInfo.Live {
		return nil, &broadcastRefusal{"rtbf", stem, "it is a live stream, which has no end to download to"}
	}
	headers := rtbfHeaders()
	hls, file, protected := answer.playable()
	switch {
	case hls != "":
		media, err := resolveMediaPlaylist(ctx, r.client, hls, headers)
		if err != nil {
			return nil, fmt.Errorf("rtbf: %s: %w", stem, err)
		}
		target := media.target(hls, stem+media.extension(), headers)
		if len(target.Audio) > 1 && r.audioTracks(ctx, assetID) == 1 {
			target.Audio = target.Audio[:1] // the default, which the group is sorted to begin with
		}
		return target, nil
	case file != "":
		return &Target{URL: file, Name: stem + linkExt(file, ".mp3"), Size: -1, Headers: headers}, nil
	case protected:
		return nil, &broadcastRefusal{"rtbf", stem, "it is DRM-protected"}
	}
	return nil, &broadcastRefusal{"rtbf", stem, "Auvio offers no stream of it to download"}
}

// audioTracks is how many audio tracks the asset itself has, or 0 where
// its record cannot be read.
func (r *RTBF) audioTracks(ctx context.Context, assetID string) int {
	var record struct {
		Audio []json.RawMessage `json:"audio"`
	}
	if err := r.client.GetJSON(ctx, r.exposure+"/v1"+rtbfUnit+"/content/asset/"+url.PathEscape(assetID), rtbfHeaders(), &record); err != nil {
		return 0
	}
	return len(record.Audio)
}

// rtbfWidget is one block of a programme's page, or a tab of one.
type rtbfWidget struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	ContentPath string `json:"contentPath"`
}

// rtbfEntry is one media a list names.
type rtbfEntry struct {
	ID           string `json:"id"`
	ResourceType string `json:"resourceType"`
	AssetID      string `json:"assetId"`
	Title        string `json:"title"`
	Subtitle     string `json:"subtitle"`
}

// rtbfListed is an entry and the tab it was listed under.
type rtbfListed struct {
	entry rtbfEntry
	tab   string
}

// programme reads a programme's episodes: each tab of its episodes widget,
// a season or a year, into a folder of its own when there are several.
func (r *RTBF) programme(ctx context.Context, slug string) (*Result, error) {
	var page struct {
		Data struct {
			Content struct {
				Title string `json:"title"`
			} `json:"content"`
			Widgets []rtbfWidget `json:"widgets"`
		} `json:"data"`
	}
	if err := r.client.GetJSON(ctx, r.bffURL("pages/emission/"+url.PathEscape(slug)), rtbfHeaders(), &page); err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			return nil, &broadcastRefusal{"rtbf", slug, "it is not on Auvio"}
		}
		return nil, fmt.Errorf("rtbf: %s: %w", slug, err)
	}
	title := cmp.Or(strings.TrimSpace(page.Data.Content.Title), slug)
	tabs, err := r.tabs(ctx, page.Data.Widgets)
	if err != nil {
		return nil, fmt.Errorf("rtbf: %s: %w", title, err)
	}
	if len(tabs) == 0 {
		return nil, fmt.Errorf("rtbf: %s lists no episodes", title)
	}

	// Each episode kept is asked of the player as it is listed, so the
	// walk stops at config.MaxResolvedEpisodes, saying the listing went on.
	var (
		entries  []rtbfListed
		more     bool
		walkNote string
		walkErr  error
		seen     = make(map[string]bool)
	)
	for _, tab := range tabs {
		budget := config.MaxResolvedEpisodes - len(entries)
		if budget == 0 {
			more = true
			break
		}
		listed, cut, note, err := r.walk(ctx, tab.ContentPath, seen, budget)
		walkErr, walkNote = cmp.Or(walkErr, err), cmp.Or(walkNote, note)
		for _, e := range listed {
			seen[e.AssetID] = true
			entries = append(entries, rtbfListed{entry: e, tab: tab.Title})
		}
		if cut {
			more = true
			break
		}
	}
	if len(entries) == 0 {
		if walkErr != nil {
			return nil, fmt.Errorf("rtbf: %s: %w", title, walkErr)
		}
		return nil, fmt.Errorf("rtbf: %s lists no episodes", title)
	}

	// An episode whose answer did not arrive, or names nothing this knows,
	// is still listed: its transfer asks again, and says why if it fails.
	var refusals []string
	resolved, note, err := expandSeries(ctx, series[rtbfListed]{
		site: "rtbf", noun: "episodes", link: title,
		items: entries, more: more,
		// Each tab of several, a season or a year, is a folder of its own.
		label:   func(l rtbfListed) string { return l.tab },
		split:   len(tabs) > 1,
		refused: &refusals,
		resolve: r.listedFile,
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	note = joinNotes(note, walkNote, brokenNote(walkErr), refusedNote(refusals))
	return &Result{Title: title, Files: listedFiles(resolved), Note: note}, nil
}

// listedFile is a listed episode's file, its stream resolved when its
// transfer starts, or the reason the player gave for refusing it.
func (r *RTBF) listedFile(ctx context.Context, l rtbfListed) (*File, error) {
	answer, err := r.play(ctx, l.entry.AssetID, l.entry.ID)
	if _, ok := refusalReason(err); ok {
		return nil, err
	}
	ext := ""
	if answer != nil {
		switch hls, file, protected := answer.playable(); {
		case answer.StreamInfo.Live:
			return nil, &broadcastRefusal{site: "rtbf", reason: "it is a live stream"}
		case hls != "":
			ext = ".mp4"
		case file != "":
			ext = linkExt(file, ".mp3")
		case protected:
			return nil, &broadcastRefusal{site: "rtbf", reason: "it is DRM-protected"}
		}
	}
	file := r.lazy(l, cmp.Or(ext, ".mp4"))
	return &file, nil
}

// tabs is what a programme's page lists its episodes in: the tabs of its
// first tab list, or failing one its first list.
func (r *RTBF) tabs(ctx context.Context, widgets []rtbfWidget) ([]rtbfWidget, error) {
	for _, w := range widgets {
		if w.Type != "TAB_LIST" || w.ContentPath == "" {
			continue
		}
		var doc struct {
			Data struct {
				Content []rtbfWidget `json:"content"`
			} `json:"data"`
		}
		if err := r.client.GetJSON(ctx, rtbfWithQuery(w.ContentPath, url.Values{"userAgent": {rtbfAgent}}), rtbfHeaders(), &doc); err != nil {
			return nil, err
		}
		var tabs []rtbfWidget
		for _, tab := range doc.Data.Content {
			if tab.Type == "MEDIA_LIST" && tab.ContentPath != "" {
				tabs = append(tabs, tab)
			}
		}
		return tabs, nil
	}
	for _, w := range widgets {
		if w.Type == "MEDIA_LIST" && w.ContentPath != "" {
			return []rtbfWidget{w}, nil
		}
	}
	return nil, nil
}

// walk reads a list a page at a time, following each page's own next
// link, into the media it names that are not in seen, up to limit of them,
// saying whether the list went on past them. A page that names nothing new
// ends the list, whatever its next link says. A page that fails is err,
// whether or not pages were read before it, for the caller to say the
// listing broke off; note is the walk's own, for a list that pages in a
// circle or past the page cap.
func (r *RTBF) walk(ctx context.Context, list string, seen map[string]bool, limit int) (entries []rtbfEntry, cut bool, note string, err error) {
	next := rtbfWithQuery(list, url.Values{
		"_page": {"1"}, "_limit": {strconv.Itoa(rtbfPage)}, "_embed": {"content"}, "userAgent": {rtbfAgent},
	})
	var broke error
	entries, cut, note, err = walkEpisodes(ctx, limit, pageWalk[rtbfEntry]{
		fetch: func(ctx context.Context, _ int) (pageOf[rtbfEntry], error) {
			var doc struct {
				Data struct {
					Content []rtbfEntry `json:"content"`
				} `json:"data"`
				Links struct {
					Next string `json:"next"`
				} `json:"links"`
			}
			if err := r.client.GetJSON(ctx, next, rtbfHeaders(), &doc); err != nil {
				return pageOf[rtbfEntry]{}, err
			}
			var media []rtbfEntry
			for _, e := range doc.Data.Content {
				if e.AssetID != "" && !seen[e.AssetID] && (e.ResourceType == "" || e.ResourceType == "MEDIA") {
					media = append(media, e)
				}
			}
			next = doc.Links.Next
			return pageOf[rtbfEntry]{items: media, next: next != "", end: len(media) == 0}, nil
		},
		key: func(e rtbfEntry) string { return e.AssetID },
		failed: func(err error) string {
			broke = err
			return ""
		},
	})
	return entries, cut, note, cmp.Or(err, broke)
}

// rtbfWithQuery is link with the parameters set on it.
func rtbfWithQuery(link string, params url.Values) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// lazy is a listed media, its stream resolved when its transfer starts. It
// keeps the name it is listed under, so that a listing read again finds
// what was saved under it.
func (r *RTBF) lazy(l rtbfListed, ext string) File {
	stem := cmp.Or(joinTitle(strings.TrimSpace(l.entry.Title), strings.TrimSpace(l.entry.Subtitle), titleContains), l.entry.ID)
	assetID := l.entry.AssetID
	return lazyEpisode(stem+ext, "", rtbfHeaders(), func(ctx context.Context) (*Target, error) {
		return r.stream(ctx, assetID, stem)
	})
}

var (
	rtbfAssetAttr = regexp.MustCompile(`data-asset-id="([0-9]+)"`)
	rtbfEmbedLink = regexp.MustCompile(`auvio\.rtbf\.be/embed/[^"'\s<>]*?/media/([0-9]+)`)
)

// embedded reads the Auvio players a page embeds, an article's say, each
// into its file.
func (r *RTBF) embedded(ctx context.Context, u *url.URL) (*Result, error) {
	html, _, err := fetchDocument(ctx, r.client, u, nil)
	if err != nil {
		return nil, fmt.Errorf("rtbf: %s: %w", u.Redacted(), err)
	}
	text := strings.ReplaceAll(html, `\/`, "/")
	ids := matchedIDs(text, rtbfAssetAttr, rtbfEmbedLink)
	switch len(ids) {
	case 0:
		return nil, fmt.Errorf("rtbf: %s embeds no Auvio player", u.Redacted())
	case 1:
		return r.media(ctx, ids[0])
	}
	title := u.Redacted()
	title = cmp.Or(strings.TrimSpace(writtenTitle(html)), title)
	// Each player is a request to resolve, so they are cut to the request
	// budget and the registry holds the files to the user's cap. When none
	// plays, the first reason one gave is the answer.
	return listSeries(ctx, title, series[string]{
		site: "rtbf", noun: "players", link: title,
		items: ids, noteRefused: true,
		resolve: func(ctx context.Context, id string) (*File, error) {
			res, err := r.media(ctx, id)
			if err != nil {
				return nil, err
			}
			return &res.Files[0], nil
		},
		unplayable: "they hold nothing to download",
	})
}
