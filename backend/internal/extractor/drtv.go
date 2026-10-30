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

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// DRTV reads DR TV (dr.dk/drtv) through the API its web player calls
// (production-cdn.dr-massive.com/api). An item says what it is: an episode,
// a programme, a season, a show or a channel. A programme's videos are one
// HLS master for each version of it (the standard one, and versions with
// spoken subtitles, audio description or sign language), and the standard
// one is taken.
//
// The API is asked as its anonymous subscriber ("sub=Anonymous"), which it
// answers with what DR offers free, as it answers yt-dlp. The site has
// since come to ask viewers to sign in before it plays anything, and a
// signed-out browser is now another subscriber, offered nothing; the older
// one is still served. A programme the API offers no files for (8009,
// "Ingen filer") is reported as needing a login, which is what every
// programme would come back as were that subscriber retired.
//
// A master is CMAF: video-only variants, the audio in renditions of its
// own, every part a byte range of one MP4 per rendition, no key, and links
// that are not signed and last until the programme leaves DR TV. Two things
// in it are not what they say. The audio group holds one track at several
// bitrates, so only its default is taken: a second copy would be a second
// track of the same sound. And every rendition is tagged English, Danish
// programmes too, so the language is the API's: "da", or "foreign", which
// names none. Subtitles are WebVTT renditions in the master, named as DR
// names them.
//
// What DR shows in Denmark alone (IsGeoRestricted) its CDN refuses from
// elsewhere with a 403; the API does not. A DRM-protected version says so
// (drm "PlayReady") and is left alone.
//
// A show lists its seasons and a season its episodes (/children, 200 a
// page), each episode with its title as the season shows it, number and
// all, and whether it is shown in Denmark alone. So a listing costs a
// request a page, and an episode's videos are read when its transfer
// starts. Anything else on dr.dk is still handed to yt-dlp.
type DRTV struct {
	client *httpx.Client
	// api is the API's base, a field so that tests can stand in for it.
	api     string
	handoff *Handoff
}

const drAPIRoot = "https://production-cdn.dr-massive.com/api"

// drRest is the part of dr.dk still handed to yt-dlp.
var drRest = handoffSite{
	name: "drtv",
	why:  "only DR TV's programmes and series are read natively, not a page like this one",
}

// NewDRTV builds the DR TV extractor.
func NewDRTV(client *httpx.Client) *DRTV {
	return &DRTV{
		client: client, api: drAPIRoot,
		handoff: &Handoff{client: client, site: drRest},
	}
}

func (d *DRTV) Name() string { return "drtv" }

// drShell marks DR TV's app: the state it boots from, its route table.
const drShell = `window.__data = {"app":{"config"`

// Recognises is the inventory's description.
func (d *DRTV) Recognises() string {
	return "programmes, seasons and series (`/drtv/…/<slug>_<id>`) of DR TV, served by its app with the state it boots from"
}

// Shape reports a DR TV link ending in an item id.
func (d *DRTV) Shape(u *url.URL) bool { return drLink(u) != "" }

// Recognise takes a page of DR TV's app (its boot state, window.__data)
// under /drtv/. One naming no item is handed to yt-dlp, as before.
func (d *DRTV) Recognise(_ context.Context, p *Page) bool {
	segs := p.Segments()
	return len(segs) >= 2 && segs[0] == "drtv" && p.Has(drShell) && p.Named("DRTV")
}

var drItemID = regexp.MustCompile(`(?:^|_)([0-9]+)$`)

// drLink reads the item a DR TV link names: the digits after the last "_"
// of its last segment (/drtv/episode/<slug>_<id>, /drtv/serie/<slug>_<id>),
// whose slug has underscores of its own, or the segment itself
// (/drtv/tvlive/<id>).
func drLink(u *url.URL) string {
	segs := util.PathSegments(u)
	if len(segs) < 2 || segs[0] != "drtv" {
		return ""
	}
	if m := drItemID.FindStringSubmatch(segs[len(segs)-1]); m != nil {
		return m[1]
	}
	return ""
}

// Extract reads a programme, a season or a show, and hands anything else on
// dr.dk to yt-dlp.
func (d *DRTV) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	id := drLink(u)
	if id == "" {
		return d.handoff.Extract(ctx, u, opts)
	}
	item, err := d.item(ctx, id)
	if err != nil {
		return nil, err
	}
	switch item.Type {
	case "episode", "program":
		stem := item.name(item.show())
		target, err := d.programme(ctx, item, stem)
		if err != nil {
			return nil, err
		}
		return &Result{Title: stem, Files: []File{target.file("")}}, nil
	case "season":
		return d.season(ctx, item, opts)
	case "show":
		return d.series(ctx, item, opts)
	case "channel":
		return nil, &broadcastRefusal{"drtv", cmp.Or(strings.TrimSpace(item.Title), id), "it is a live channel, which has no end to download to"}
	}
	// A list, an event or another page DR TV composes: as before.
	return d.handoff.Extract(ctx, u, opts)
}

// drItem is the part of an item read here: a programme, a season or a show,
// or an entry of a listing, which carries the same fields.
type drItem struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	Title           string `json:"title"`
	ContextualTitle string `json:"contextualTitle"`
	SeasonNumber    int    `json:"seasonNumber"`
	CustomFields    struct {
		IsGeoRestricted string `json:"IsGeoRestricted"`
	} `json:"customFields"`
	Season *struct {
		Show *drShow `json:"show"`
	} `json:"season"`
	Show *drShow `json:"show"`
}

// drShow is the show an item belongs to.
type drShow struct {
	Title string `json:"title"`
}

// show is the title of the show the item belongs to, where it names one.
func (it *drItem) show() string {
	switch {
	case it.Show != nil:
		return strings.TrimSpace(it.Show.Title)
	case it.Season != nil && it.Season.Show != nil:
		return strings.TrimSpace(it.Season.Show.Title)
	}
	return ""
}

// name is what a programme is called: within a show, the show's title and
// the programme's own as its season shows it, its number with it
// ("1. A title"); otherwise DR's own title.
func (it *drItem) name(show string) string {
	if title := strings.TrimSpace(it.ContextualTitle); title != "" {
		return ardJoin(show, title)
	}
	return cmp.Or(strings.TrimSpace(it.Title), it.ID)
}

// geoRestricted reports a programme DR shows in Denmark alone.
func (it *drItem) geoRestricted() bool {
	return strings.EqualFold(it.CustomFields.IsGeoRestricted, "true")
}

// drSeasonLabel is what a season is called within its show: its own title,
// less the show's, where that says something, or else its number.
func drSeasonLabel(show string, season *drItem) string {
	title := strings.TrimSpace(season.Title)
	if rest, ok := strings.CutPrefix(title, show); ok && show != "" {
		title = strings.TrimLeft(rest, " :-–")
	}
	switch {
	case title != "":
		return title
	case season.SeasonNumber > 0:
		return fmt.Sprintf("Sæson %d", season.SeasonNumber)
	}
	return season.ID
}

// drGone is what DR TV's answer means for an id it does not have.
const drGone = "it is not on DR TV: removed, expired, or not published yet"

// errDRDenmarkOnly is a programme DR shows in Denmark alone, refused here.
var errDRDenmarkOnly = errors.New("shown in Denmark only, and its stream is refused anywhere else")

// drHeaders are what the site's own player sends.
func drHeaders() httpx.Header {
	return httpx.RefererOrigin("https://www.dr.dk/", "https://www.dr.dk")
}

// endpoint is an API URL, asked as the anonymous subscriber and the web
// player.
func (d *DRTV) endpoint(path string, extra url.Values) string {
	q := url.Values{"sub": {"Anonymous"}, "device": {"web_browser"}, "lang": {"da"}}
	for k, v := range extra {
		q[k] = v
	}
	return d.api + "/" + path + "?" + q.Encode()
}

// item reads an item, with its season and show.
func (d *DRTV) item(ctx context.Context, id string) (*drItem, error) {
	var item drItem
	if err := d.client.GetJSON(ctx, d.endpoint("items/"+url.PathEscape(id), url.Values{"expand": {"all"}}), drHeaders(), &item); err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			return nil, &broadcastRefusal{"drtv", id, drGone}
		}
		return nil, fmt.Errorf("drtv: %s: %w", id, err)
	}
	return &item, nil
}

// drStream is one version of a programme, as its videos name it.
type drStream struct {
	AccessService string `json:"accessService"`
	DRM           string `json:"drm"`
	Format        string `json:"format"`
	Language      string `json:"language"`
	URL           string `json:"url"`
}

// videos reads a programme's versions.
func (d *DRTV) videos(ctx context.Context, id, title string) ([]drStream, error) {
	var streams []drStream
	err := d.client.GetJSON(ctx, d.endpoint("items/"+url.PathEscape(id)+"/videos",
		url.Values{"delivery": {"stream"}, "resolution": {"HD-1080"}}), drHeaders(), &streams)
	if err == nil {
		return streams, nil
	}
	if se, ok := errors.AsType[*httpx.StatusError](err); ok {
		var answer struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal([]byte(se.Body), &answer)
		switch {
		case answer.Code == 8009:
			return nil, &broadcastRefusal{"drtv", title, "DR TV offers no stream of it without signing in"}
		case strings.Contains(strings.ToLower(answer.Message), "not found"):
			return nil, &broadcastRefusal{"drtv", title, drGone}
		}
	}
	return nil, fmt.Errorf("drtv: %s: %w", title, err)
}

// programme resolves a programme's standard version under the name stem.
// Only that version is taken, whatever else is clear: the others carry a
// narrator, spoken subtitles or a signer, which nobody asked for.
func (d *DRTV) programme(ctx context.Context, item *drItem, stem string) (*Target, error) {
	streams, err := d.videos(ctx, item.ID, stem)
	if err != nil {
		return nil, err
	}
	var chosen *drStream
	protected := false
	for i := range streams {
		s := &streams[i]
		switch {
		case s.AccessService != "StandardVideo" || !strings.Contains(s.Format, "hls") || s.URL == "":
			continue
		case s.DRM != "" && !strings.EqualFold(s.DRM, "None"):
			protected = true
			continue
		}
		chosen = s
		break
	}
	if chosen == nil {
		if protected {
			return nil, &broadcastRefusal{"drtv", stem, "it is DRM-protected"}
		}
		return nil, &broadcastRefusal{"drtv", stem, "DR TV offers no stream of it to download"}
	}
	headers := drHeaders()
	media, err := resolveMediaPlaylist(ctx, d.client, chosen.URL, headers)
	if err != nil {
		if item.geoRestricted() && httpx.HasStatus(err, http.StatusForbidden) {
			return nil, fmt.Errorf("drtv: %s: %w", stem, errDRDenmarkOnly)
		}
		return nil, fmt.Errorf("drtv: %s: %w", stem, err)
	}
	target := media.target(chosen.URL, stem+media.extension(), headers)
	target.Audio = drAudio(media.Audio, chosen.Language)
	return target, nil
}

// drAudio is the one audio track a master's group holds, whatever bitrates
// it is offered at, in the language the API gives the programme.
func drAudio(renditions []Track, language string) []Track {
	if len(renditions) == 0 {
		return nil
	}
	track := renditions[0] // the default, which the group is sorted to begin with
	track.Language, track.Name, track.Default = "", "", true
	if language == "da" {
		track.Language, track.Name = "da", "Dansk"
	}
	return []Track{track}
}

// drChildren is one page of an item's children.
type drChildren struct {
	Items  []drItem `json:"items"`
	Paging struct {
		Total int `json:"total"` // the number of pages, not of items
	} `json:"paging"`
}

// children reads every page of an item's children, in the order DR gives
// them. What was read before a page failed comes back with the error.
func (d *DRTV) children(ctx context.Context, id string) ([]drItem, error) {
	var all []drItem
	for page := 1; ; page++ {
		var doc drChildren
		if err := d.client.GetJSON(ctx, d.endpoint("items/"+url.PathEscape(id)+"/children",
			url.Values{"page": {strconv.Itoa(page)}}), drHeaders(), &doc); err != nil {
			return all, err
		}
		all = append(all, doc.Items...)
		if len(doc.Items) == 0 || page >= doc.Paging.Total {
			return all, nil
		}
	}
}

// drListed is an entry of a listing and the folder it goes into.
type drListed struct {
	item   drItem
	folder string
}

// series reads a show's seasons, and their episodes, into one file an
// episode, each season's in a folder of its own.
func (d *DRTV) series(ctx context.Context, show *drItem, opts Options) (*Result, error) {
	title := cmp.Or(strings.TrimSpace(show.Title), show.ID)
	children, walkErr := d.children(ctx, show.ID)
	var entries []drListed
	for _, child := range children {
		switch child.Type {
		case "season":
			episodes, err := d.children(ctx, child.ID)
			walkErr = cmp.Or(walkErr, err)
			folder := sourceFolder(drSeasonLabel(title, &child))
			for _, ep := range episodes {
				entries = append(entries, drListed{item: ep, folder: folder})
			}
		case "episode", "program":
			entries = append(entries, drListed{item: child})
		}
	}
	return d.listing(ctx, title, title, entries, walkErr, opts)
}

// season reads a season into one file an episode.
func (d *DRTV) season(ctx context.Context, season *drItem, opts Options) (*Result, error) {
	show := cmp.Or(season.show(), strings.TrimSpace(season.Title), season.ID)
	episodes, walkErr := d.children(ctx, season.ID)
	entries := make([]drListed, 0, len(episodes))
	for _, ep := range episodes {
		entries = append(entries, drListed{item: ep})
	}
	return d.listing(ctx, ardJoin(show, drSeasonLabel(show, season)), show, entries, walkErr, opts)
}

// listing makes a listing's programmes into files, each resolved when its
// transfer starts. What DR shows in Denmark alone is left out, the note
// saying so, once one of them has been refused here.
func (d *DRTV) listing(ctx context.Context, title, show string, entries []drListed, walkErr error, opts Options) (*Result, error) {
	blocked, probes := false, 0
	for i := range entries {
		if probes == config.GeoProbesDR {
			break
		}
		if !entries[i].item.geoRestricted() {
			continue
		}
		probes++
		if refused, known := d.refusedHere(ctx, &entries[i].item); known {
			blocked = refused
			break
		}
	}

	var (
		files          []File
		denmark, shown int
	)
	limit := opts.maxFiles()
	for i := range entries {
		e := &entries[i]
		if blocked && e.item.geoRestricted() {
			denmark++
			continue
		}
		shown++
		if len(files) < limit {
			files = append(files, d.lazy(e, show))
		}
	}
	if len(files) == 0 {
		switch {
		case denmark > 0:
			return nil, fmt.Errorf("drtv: %s: %w", title, errDRDenmarkOnly)
		case walkErr != nil:
			return nil, fmt.Errorf("drtv: %s: %w", title, walkErr)
		}
		return nil, fmt.Errorf("drtv: %s lists nothing DR TV offers now", title)
	}
	note := listingNote(cappedNote(len(files), shown, "episodes"), brokenNote(walkErr),
		leftOutNote(denmark, " that DR shows in Denmark only"))
	return &Result{Title: title, Files: files, Note: note}, nil
}

// refusedHere asks for a programme's master, to learn whether DR's CDN
// serves it to this address at all; known is false where it could not
// tell.
func (d *DRTV) refusedHere(ctx context.Context, item *drItem) (refused, known bool) {
	streams, err := d.videos(ctx, item.ID, item.ID)
	if err != nil {
		return false, false
	}
	for _, s := range streams {
		if s.URL == "" || !strings.Contains(s.Format, "hls") {
			continue
		}
		status, err := d.client.StatusOnce(ctx, http.MethodGet, s.URL, drHeaders())
		if err != nil {
			return false, false
		}
		return status == http.StatusForbidden, true
	}
	return false, false
}

// lazy is a listed programme, its videos read when its transfer starts. It
// keeps the name it is listed under, so that a listing read again finds
// what was saved under it.
func (d *DRTV) lazy(e *drListed, show string) File {
	stem := e.item.name(show)
	id := e.item.ID
	return lazyEpisode(stem+".mp4", e.folder, drHeaders(), func(ctx context.Context) (*Target, error) {
		item, err := d.item(ctx, id)
		if err != nil {
			return nil, err
		}
		return d.programme(ctx, item, stem)
	})
}
