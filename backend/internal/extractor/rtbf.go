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
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

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
	hostSet
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
	return &RTBF{hostSet: hostSet{"rtbf.be", "auvio.rtbf.be"}, client: client, bff: rtbfBFF, exposure: rtbfExposure}
}

func (r *RTBF) Name() string { return "rtbf" }

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
	if util.HostMatches(u.Host, "auvio.rtbf.be") && len(segs) > 0 {
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
func (r *RTBF) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	switch kind, id := rtbfLink(u); kind {
	case rtbfMedia:
		return r.media(ctx, id)
	case rtbfProgramme:
		return r.programme(ctx, id, opts)
	case rtbfLive:
		return nil, fmt.Errorf("rtbf: %s is a live stream, which has no end to download to", u.Redacted())
	}
	return r.embedded(ctx, u, opts)
}

// rtbfRefused is a media Auvio does not play here: gone, live, protected,
// or kept for viewers signed in, in Belgium or paying. It is a type so
// that a listing can leave such entries out, counting each reason once.
type rtbfRefused struct{ title, reason string }

func (e *rtbfRefused) Error() string { return "rtbf: " + e.title + ": " + e.reason }

// rtbfGone is what RTBF's answer means for an id it has nothing under.
const rtbfGone = "it is not on Auvio: removed, expired, or not published yet"

// rtbfHeaders are what the site's own player sends.
func rtbfHeaders() httpx.Header {
	return httpx.RefererOrigin("https://auvio.rtbf.be/", "https://auvio.rtbf.be")
}

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
			return nil, &rtbfRefused{title: id, reason: rtbfGone}
		}
		return nil, fmt.Errorf("rtbf: %s: %w", id, err)
	}
	data := doc.Data
	var programme string
	if data.Program != nil {
		programme = data.Program.Title
	}
	stem := cmp.Or(ardJoin(strings.TrimSpace(cmp.Or(programme, data.Title)), strings.TrimSpace(data.Subtitle)), id)
	if strings.EqualFold(data.PlayerType, "LIVE") {
		return nil, &rtbfRefused{title: stem, reason: "it is a live stream, which has no end to download to"}
	}
	target, err := r.stream(ctx, cmp.Or(data.AssetID, id+"_6BA97Bb"), stem)
	if err != nil {
		return nil, err
	}
	return &Result{Title: stem, Files: []File{target.file("")}}, nil
}

// file is a target as the file it resolves to.
func (t *Target) file(dir string) File {
	return File{
		Name: t.Name, URL: t.URL, Size: t.Size, Dir: dir, Headers: t.Headers, Segments: t.Segments,
		SegmentKey: t.SegmentKey, Audio: t.Audio, Subtitles: t.Subtitles,
	}
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
	return &rtbfRefused{title: title, reason: reason}
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
			return nil, &rtbfRefused{title: title, reason: rtbfGone}
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
		return nil, &rtbfRefused{title: stem, reason: "it is a live stream, which has no end to download to"}
	}
	headers := rtbfHeaders()
	hls, file, protected := answer.playable()
	switch {
	case hls != "":
		media, err := resolveMediaPlaylist(ctx, r.client, hls, headers)
		if err != nil {
			return nil, fmt.Errorf("rtbf: %s: %w", stem, err)
		}
		audio := media.Audio
		if len(audio) > 1 && r.audioTracks(ctx, assetID) == 1 {
			audio = audio[:1] // the default, which the group is sorted to begin with
		}
		return &Target{
			URL: hls, Name: stem + media.extension(), Size: -1, Headers: headers,
			Segments: media.Segments, SegmentKey: media.Key, Audio: audio, Subtitles: media.Subtitles,
		}, nil
	case file != "":
		return &Target{URL: file, Name: stem + rtbfExtension(file), Size: -1, Headers: headers}, nil
	case protected:
		return nil, &rtbfRefused{title: stem, reason: "it is DRM-protected"}
	}
	return nil, &rtbfRefused{title: stem, reason: "Auvio offers no stream of it to download"}
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

// rtbfExtension is a file's extension, as its link names it.
func rtbfExtension(link string) string {
	if u, err := url.Parse(link); err == nil {
		if ext := strings.ToLower(path.Ext(u.Path)); len(ext) > 1 && len(ext) <= 5 {
			return ext
		}
	}
	return ".mp3"
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

// rtbfListed is an entry and the folder it goes into.
type rtbfListed struct {
	entry  rtbfEntry
	folder string
}

// programme reads a programme's episodes: each tab of its episodes widget,
// a season or a year, into a folder of its own when there are several.
func (r *RTBF) programme(ctx context.Context, slug string, opts Options) (*Result, error) {
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
			return nil, &rtbfRefused{title: slug, reason: "it is not on Auvio"}
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

	limit := opts.maxFiles()
	var (
		entries []rtbfListed
		listed  int
		walkErr error
		seen    = make(map[string]bool)
	)
	for _, tab := range tabs {
		folder := ""
		if len(tabs) > 1 {
			folder = sourceFolder(tab.Title)
		}
		err := r.walk(ctx, tab.ContentPath, func(e rtbfEntry) bool {
			if e.AssetID == "" || seen[e.AssetID] || e.ResourceType != "" && e.ResourceType != "MEDIA" {
				return false
			}
			seen[e.AssetID] = true
			listed++
			if len(entries) < limit {
				entries = append(entries, rtbfListed{entry: e, folder: folder})
			}
			return true
		})
		walkErr = cmp.Or(walkErr, err)
	}

	type checked struct {
		listed rtbfListed
		ext    string
		reason string
	}
	results := FanOut(ctx, entries, func(ctx context.Context, l rtbfListed) ([]checked, error) {
		c := checked{listed: l}
		answer, err := r.play(ctx, l.entry.AssetID, l.entry.ID)
		if refused, ok := errors.AsType[*rtbfRefused](err); ok {
			c.reason = refused.reason
		} else if answer != nil {
			switch hls, file, protected := answer.playable(); {
			case answer.StreamInfo.Live:
				c.reason = "it is a live stream"
			case hls != "":
				c.ext = ".mp4"
			case file != "":
				c.ext = rtbfExtension(file)
			case protected:
				c.reason = "it is DRM-protected"
			}
		}
		return []checked{c}, nil
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var (
		files   []File
		reasons []string
		counts  = make(map[string]int)
	)
	for _, c := range results {
		if c.reason != "" {
			if counts[c.reason] == 0 {
				reasons = append(reasons, c.reason)
			}
			counts[c.reason]++
			continue
		}
		files = append(files, r.lazy(c.listed, cmp.Or(c.ext, ".mp4")))
	}
	if len(files) == 0 {
		switch {
		case len(reasons) > 0:
			return nil, fmt.Errorf("rtbf: %s: none of its %d episodes can be had here: %s", title, len(results), reasons[0])
		case walkErr != nil:
			return nil, fmt.Errorf("rtbf: %s: %w", title, walkErr)
		}
		return nil, fmt.Errorf("rtbf: %s lists no episodes", title)
	}
	var notes []string
	if listed > len(entries) {
		notes = append(notes, fmt.Sprintf("%d of %d episodes", len(entries), listed))
	}
	if walkErr != nil {
		notes = append(notes, "the listing broke off partway")
	}
	for _, reason := range reasons {
		notes = append(notes, fmt.Sprintf("%d left out: %s", counts[reason], reason))
	}
	return &Result{Title: title, Files: files, Note: strings.Join(notes, "; ")}, nil
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
// link, and hands each entry on; each says whether it was new. A page that
// adds nothing ends the walk, whatever its next link says, so a list that
// pages in a circle cannot hold it.
func (r *RTBF) walk(ctx context.Context, list string, each func(rtbfEntry) bool) error {
	next := rtbfWithQuery(list, url.Values{
		"_page": {"1"}, "_limit": {strconv.Itoa(rtbfPage)}, "_embed": {"content"}, "userAgent": {rtbfAgent},
	})
	for next != "" {
		var doc struct {
			Data struct {
				Content []rtbfEntry `json:"content"`
			} `json:"data"`
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		}
		if err := r.client.GetJSON(ctx, next, rtbfHeaders(), &doc); err != nil {
			return err
		}
		added := 0
		for _, e := range doc.Data.Content {
			if each(e) {
				added++
			}
		}
		if added == 0 {
			return nil
		}
		next = doc.Links.Next
	}
	return nil
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
	stem := cmp.Or(ardJoin(strings.TrimSpace(l.entry.Title), strings.TrimSpace(l.entry.Subtitle)), l.entry.ID)
	assetID := l.entry.AssetID
	return File{
		Name: stem + ext, Size: -1, Dir: l.folder, Headers: rtbfHeaders(),
		Resolve: func(ctx context.Context) (*Target, error) {
			return r.stream(ctx, assetID, stem)
		},
	}
}

var (
	rtbfAssetAttr = regexp.MustCompile(`data-asset-id="([0-9]+)"`)
	rtbfEmbedLink = regexp.MustCompile(`auvio\.rtbf\.be/embed/[^"'\s<>]*?/media/([0-9]+)`)
	rtbfPageTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// embedded reads the Auvio players a page embeds, an article's say, each
// into its file.
func (r *RTBF) embedded(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	html, err := r.client.GetString(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("rtbf: %s: %w", u.Redacted(), err)
	}
	text := strings.ReplaceAll(html, `\/`, "/")
	var ids []string
	seen := make(map[string]bool)
	for _, re := range []*regexp.Regexp{rtbfAssetAttr, rtbfEmbedLink} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				ids = append(ids, m[1])
			}
		}
	}
	switch len(ids) {
	case 0:
		return nil, fmt.Errorf("rtbf: %s embeds no Auvio player", u.Redacted())
	case 1:
		return r.media(ctx, ids[0])
	}
	ids = ids[:min(len(ids), opts.maxFiles())]
	type found struct {
		file   *File
		reason string
	}
	results := FanOut(ctx, ids, func(ctx context.Context, id string) ([]found, error) {
		res, err := r.media(ctx, id)
		if refused, ok := errors.AsType[*rtbfRefused](err); ok {
			return []found{{reason: refused.reason}}, nil
		}
		if err != nil {
			return nil, err
		}
		return []found{{file: &res.Files[0]}}, nil
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	title := u.Redacted()
	if m := rtbfPageTitle.FindStringSubmatch(html); m != nil {
		title = cmp.Or(strings.TrimSpace(util.Unescape(m[1])), title)
	}
	res := &Result{Title: title}
	var reasons []string
	for _, f := range results {
		if f.file != nil {
			res.Files = append(res.Files, *f.file)
		} else {
			reasons = append(reasons, f.reason)
		}
	}
	if len(res.Files) == 0 {
		if len(reasons) > 0 {
			return nil, fmt.Errorf("rtbf: %s: none of its %d players can be had here: %s", title, len(ids), reasons[0])
		}
		return nil, fmt.Errorf("rtbf: %s: none of its players could be read", title)
	}
	if len(reasons) > 0 {
		res.Note = fmt.Sprintf("%d left out: %s", len(reasons), reasons[0])
	}
	return res, nil
}
