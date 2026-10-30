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
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Yle reads Yle Areena, in Finnish (areena.yle.fi) or Swedish
// (arenan.yle.fi): its TV programmes, its radio and podcasts, and their
// series and seasons.
//
// What plays comes from the player API's preview
// (player.api.yle.fi/v1/preview/<id>.json), asked with the app id and key of
// Yle's static player, the pair its own script on player-v2.yle.fi sends.
// The preview names a programme's title and series in both languages, the
// region it is shown in, and its stream: for video an HLS master, for audio
// one MP3. Both are signed for some hours, so they are minted again when the
// transfer starts. A master's subtitles are WebVTT renditions of their own,
// which the downloader joins in (download/mux.go).
//
// The preview's is_content_protected says nothing about the stream: the
// programmes it marked that are shown worldwide were all served as plain
// HLS, with no key anywhere. So it is not read; a stream that really is
// encrypted is refused by the playlist reader, as everywhere else.
//
// What Yle shows in Finland alone is given a stream like any other, which
// its CDN then refuses from anywhere else with a 403, the MP3s as well as
// the masters. The preview's region says which those are, so the refusal is
// reported as what it is.
//
// A series is what its page lists: the first tab's episodes, a request for
// each season its filter names, through Areena's UI API
// (areena.api.yle.fi/v1/ui/content/list). Each list is signed by Yle and
// asked for with the pair the page publishes for that API
// (appIdFrontend), read off the page rather than written down here. A
// season's own page lists that season alone. Whether an id is a series or a
// programme is the page's to say: asked of the preview, a series answers
// with one of its episodes.
type Yle struct {
	client *httpx.Client
	// player and site are where the preview and the pages are, fields so
	// that tests can stand in for them.
	player string
	site   map[string]string // the preview's language → the site's root
}

const (
	ylePlayerRoot = "https://player.api.yle.fi/v1/preview/"
	// ylePlayerAppID and ylePlayerAppKey are the static player's own.
	ylePlayerAppID  = "player_static_prod"
	ylePlayerAppKey = "8930d72170e48303cf5f3867780d549b"
	// yleUIVersion is the version of the UI API the site's own client asks
	// for; a list asked for without one is refused.
	yleUIVersion = "10"
	// yleListPage is how many cards a page of a list asks for.
	yleListPage = 100
)

// yleUILanguage is how the UI API spells each of the preview's languages.
var yleUILanguage = map[string]string{"fin": "fi", "swe": "sv"}

// NewYle builds the Yle Areena extractor.
func NewYle(client *httpx.Client) *Yle {
	return &Yle{
		client: client, player: ylePlayerRoot,
		site: map[string]string{"fin": "https://areena.yle.fi", "swe": "https://arenan.yle.fi"},
	}
}

func (y *Yle) Name() string { return "yle" }

// yleShape is a link ending in an item id: /1-<n>, /podcastit/1-<n>, …
var yleShape = pathShape(`(?:^|/)1-[0-9]+/?$`)

// Recognises is the inventory's description.
func (y *Yle) Recognises() string {
	return "programmes and series (`…/1-<id>`) of Yle Areena, whose page carries the Areena web app's configuration"
}

// Shape reports a link ending in an item id.
func (y *Yle) Shape(u *url.URL) bool { return yleShape(u) }

// Recognise takes an item page of Areena or Arenan: its environment names
// the Areena web app (areena-web-items) whose APIs this reads. The link as
// submitted counts as well as where it landed, since an older item id is
// redirected to an address of the newer kind, which this does not read.
func (y *Yle) Recognise(_ context.Context, p *Page) bool {
	return (yleShape(p.Final) || yleShape(p.URL)) && p.Has("areena-web-items") && p.Named("Yle Areena", "Yle Arenan")
}

var yleItemID = regexp.MustCompile(`^1-[0-9]+$`)

// yleLink reads a page's language and the item it names:
// /<id>, /podcastit/<id>, /poddar/<id>, /tv/<anything>/<id>.
func yleLink(u *url.URL) (lang, id string) {
	lang = "fin"
	if util.HostMatches(u.Host, "arenan.yle.fi") {
		lang = "swe"
	}
	segs := util.PathSegments(u)
	for i := len(segs) - 1; i >= 0; i-- {
		if yleItemID.MatchString(segs[i]) {
			return lang, segs[i]
		}
	}
	return lang, ""
}

// Extract reads a programme into its file, or a series or season into one
// file an episode.
func (y *Yle) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	lang, id := yleLink(u)
	if id == "" {
		return nil, fmt.Errorf("yle: %s names no programme or series", u.Redacted())
	}
	page, err := y.page(ctx, lang, id)
	if err != nil {
		return nil, err
	}
	if page.listing() {
		return y.series(ctx, lang, id, page)
	}
	target, title, err := y.programme(ctx, lang, id)
	if err != nil {
		return nil, err
	}
	return &Result{Title: title, Files: []File{refreshing(target, func(ctx context.Context) (*Target, error) {
		target, _, err := y.programme(ctx, lang, id)
		return target, err
	})}}, nil
}

// yleText is a text Yle gives in each of its languages.
type yleText map[string]string

// in is the text in lang, or in the other language where it has none.
func (t yleText) in(lang string) string {
	return strings.TrimSpace(cmp.Or(t[lang], t["fin"], t["swe"]))
}

// yleOndemand is a programme the preview offers to play.
type yleOndemand struct {
	Title  yleText `json:"title"`
	Series *struct {
		Title yleText `json:"title"`
	} `json:"series"`
	Region      string `json:"region"`
	ManifestURL string `json:"manifest_url"`
	MediaURL    string `json:"media_url"`
}

// title is the programme's name, after its series where it has one.
func (od *yleOndemand) title(lang, id string) string {
	var series string
	if od.Series != nil {
		series = od.Series.Title.in(lang)
	}
	return cmp.Or(ardJoin(series, od.Title.in(lang)), id)
}

// finlandOnly reports a programme Yle shows in Finland alone.
func (od *yleOndemand) finlandOnly() bool { return strings.EqualFold(od.Region, "Finland") }

// extension is what the programme is saved as: its MP3's own, or for a
// master the MPEG-TS that Areena's HLS is.
func (od *yleOndemand) extension() string {
	if od.ManifestURL == "" && od.MediaURL != "" {
		return linkExt(od.MediaURL, ".mp3")
	}
	return ".ts"
}

// yleStates says why a programme in each of the preview's other states is
// not downloaded. They are the states Yle's player itself knows.
var yleStates = map[string]string{
	"gone":                "it is no longer on Areena",
	"ongoing_event":       "it is a live broadcast, which has no end to download to",
	"ongoing_channel":     "it is a live channel, which has no end to download to",
	"pending_event":       "it is a broadcast still to come",
	"pending_ondemand":    "it is not published yet",
	"blocked":             "Areena blocks it",
	"not_allowed":         "Areena does not allow it to be played here",
	"not_allowed_channel": "Areena does not allow its channel to be played here",
	"identification":      "Areena shows it only to viewers who have identified themselves",
	"error":               "Areena answers that it cannot be played",
}

// errYleFinlandOnly is a programme Yle shows in Finland alone, refused here.
var errYleFinlandOnly = errors.New("shown in Finland only, and its stream is refused anywhere else")

// preview asks the player API what plays for an id.
func (y *Yle) preview(ctx context.Context, lang, id string) (*yleOndemand, error) {
	query := url.Values{
		"language": {lang}, "ssl": {"true"}, "host": {y.host(lang)},
		"app_id": {ylePlayerAppID}, "app_key": {ylePlayerAppKey},
	}
	var doc struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := y.client.GetJSON(ctx, y.player+url.PathEscape(id)+".json?"+query.Encode(), y.headers(lang), &doc); err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			return nil, &broadcastRefusal{"yle", id, "it does not exist"}
		}
		return nil, fmt.Errorf("yle: %s: %w", id, err)
	}
	if raw, ok := doc.Data["ongoing_ondemand"]; ok {
		var od yleOndemand
		if err := json.Unmarshal(raw, &od); err != nil {
			return nil, fmt.Errorf("yle: %s: read what plays: %w", id, err)
		}
		return &od, nil
	}
	for state, raw := range doc.Data {
		if reason, ok := yleStates[state]; ok {
			var named struct {
				Title yleText `json:"title"`
			}
			_ = json.Unmarshal(raw, &named)
			return nil, &broadcastRefusal{"yle", cmp.Or(named.Title.in(lang), id), reason}
		}
	}
	return nil, fmt.Errorf("yle: %s: the player API names no state it is in", id)
}

// programme asks the preview what plays and resolves its stream.
func (y *Yle) programme(ctx context.Context, lang, id string) (*Target, string, error) {
	od, err := y.preview(ctx, lang, id)
	if err != nil {
		return nil, "", err
	}
	title := od.title(lang, id)
	headers := y.headers(lang)
	switch {
	case od.ManifestURL != "":
		media, err := resolveMediaPlaylist(ctx, y.client, od.ManifestURL, headers)
		if err != nil {
			if od.finlandOnly() && httpx.HasStatus(err, http.StatusForbidden) {
				return nil, "", fmt.Errorf("yle: %s: %w", title, errYleFinlandOnly)
			}
			return nil, "", fmt.Errorf("yle: %s: %w", title, err)
		}
		return media.target(od.ManifestURL, title+media.extension(), headers), title, nil
	case od.MediaURL != "":
		if od.finlandOnly() && y.refusedHere(ctx, od.MediaURL, headers) {
			return nil, "", fmt.Errorf("yle: %s: %w", title, errYleFinlandOnly)
		}
		return &Target{URL: od.MediaURL, Name: title + od.extension(), Size: -1, Headers: headers}, title, nil
	}
	return nil, "", &broadcastRefusal{"yle", title, "it offers no stream to download"}
}

// refusedHere asks for a file's first byte, to learn whether its host serves
// it to this address at all. A master is asked for anyway when it is
// resolved; an MP3 is not, and would otherwise be refused only once its
// transfer starts, as a bare 403.
func (y *Yle) refusedHere(ctx context.Context, link string, headers httpx.Header) bool {
	status, err := y.client.StatusOnce(ctx, http.MethodGet, link,
		headers.Merge(httpx.Header{httpx.HeaderRange: "bytes=0-0"}))
	return err == nil && status == http.StatusForbidden
}

// headers are what the site's own player sends.
func (y *Yle) headers(lang string) httpx.Header {
	site := y.site[lang]
	return httpx.RefererOrigin(site+"/", site)
}

// host is the site's host, which the player names to the preview.
func (y *Yle) host(lang string) string {
	if u, err := url.Parse(y.site[lang]); err == nil {
		return u.Host
	}
	return ""
}

// ylePageDoc is the part of an item's page read here.
type ylePageDoc struct {
	Props struct {
		PageProps struct {
			Meta struct {
				Item struct {
					Type string `json:"type"`
				} `json:"item"`
				Title string `json:"title"`
			} `json:"meta"`
			View struct {
				Title string `json:"title"`
				Tabs  []struct {
					Content []yleContent `json:"content"`
				} `json:"tabs"`
			} `json:"view"`
		} `json:"pageProps"`
	} `json:"props"`
}

// yleContent is one block of a page's tab.
type yleContent struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Source struct {
		URI string `json:"uri"`
	} `json:"source"`
	Filters []struct {
		Options []yleOption `json:"options"`
	} `json:"filters"`
}

// yleOption is one choice a list's filter offers: on a series, a season.
type yleOption struct {
	Title      string            `json:"title"`
	Parameters map[string]string `json:"parameters"`
	Default    bool              `json:"default"`
}

// ylePage is an item's page: what it is, and the pair the site publishes
// for its UI API.
type ylePage struct {
	doc        ylePageDoc
	appID, key string
}

// listing reports a page that lists programmes rather than plays one: a
// series, or a season of one.
func (p *ylePage) listing() bool {
	return strings.HasSuffix(p.doc.Props.PageProps.Meta.Item.Type, "Series") || p.season()
}

// season reports a season's own page.
func (p *ylePage) season() bool {
	return strings.HasSuffix(p.doc.Props.PageProps.Meta.Item.Type, "Season")
}

// yleList is a list a series is read from, with the parameters its season's
// option adds and that season's name.
type yleList struct {
	uri, season string
	params      map[string]string
}

// lists names what the page lists its episodes in: the first tab's untitled
// list (the titled ones are recommendations, the other tabs clips and what
// is still to come), once for each season its filter offers. A season's own
// page offers the series' seasons too, itself the default, and lists that
// one alone.
func (p *ylePage) lists(id string) []yleList {
	tabs := p.doc.Props.PageProps.View.Tabs
	if len(tabs) == 0 {
		return nil
	}
	for _, c := range tabs[0].Content {
		if c.Type != "list" || c.Title != "" || c.Source.URI == "" {
			continue
		}
		for _, f := range c.Filters {
			var lists []yleList
			for _, o := range f.Options {
				if yleSeasonOption(o) {
					lists = append(lists, yleList{uri: c.Source.URI, season: strings.TrimSpace(o.Title), params: o.Parameters})
				}
			}
			if len(lists) == 0 {
				continue
			}
			if p.season() {
				return yleOwnSeason(lists, f.Options, id)
			}
			return lists
		}
		return []yleList{{uri: c.Source.URI}}
	}
	return nil
}

// yleSeasonOption reports an option that fills in part of the list's path,
// which is how a season is chosen.
func yleSeasonOption(o yleOption) bool {
	for k := range o.Parameters {
		if strings.HasPrefix(k, "path.") {
			return true
		}
	}
	return false
}

// yleOwnSeason is the season a season's page is: the option naming its id,
// or failing that the default.
func yleOwnSeason(lists []yleList, options []yleOption, id string) []yleList {
	for _, l := range lists {
		if l.params["path.season"] == id {
			return []yleList{l}
		}
	}
	for _, o := range options {
		if o.Default && yleSeasonOption(o) {
			return []yleList{{uri: lists[0].uri, season: strings.TrimSpace(o.Title), params: o.Parameters}}
		}
	}
	return nil
}

var (
	yleNextData    = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__"[^>]*>(.*?)</script>`)
	yleFrontendID  = regexp.MustCompile(`appIdFrontend\W{1,4}([A-Za-z0-9_-]+)`)
	yleFrontendKey = regexp.MustCompile(`appKeyFrontend\W{1,4}([A-Za-z0-9_-]+)`)
)

// page reads an item's page off the site.
func (y *Yle) page(ctx context.Context, lang, id string) (*ylePage, error) {
	html, err := fetchPage(ctx, y.client, y.site[lang]+"/"+url.PathEscape(id), nil)
	if err != nil {
		if httpx.HasStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("yle: %s does not exist", id)
		}
		return nil, fmt.Errorf("yle: %s: %w", id, err)
	}
	m := yleNextData.FindStringSubmatch(html)
	if m == nil {
		return nil, fmt.Errorf("yle: %s: the page carries no description of what it shows", id)
	}
	page := &ylePage{}
	if err := json.Unmarshal([]byte(m[1]), &page.doc); err != nil {
		return nil, fmt.Errorf("yle: %s: read the page: %w", id, err)
	}
	if m := yleFrontendID.FindStringSubmatch(html); m != nil {
		page.appID = m[1]
	}
	if m := yleFrontendKey.FindStringSubmatch(html); m != nil {
		page.key = m[1]
	}
	return page, nil
}

// yleCards is one page of a UI list.
type yleCards struct {
	Meta struct {
		Count int `json:"count"`
	} `json:"meta"`
	Data []struct {
		Title   string `json:"title"`
		Pointer struct {
			Type string `json:"type"`
			URI  string `json:"uri"`
		} `json:"pointer"`
	} `json:"data"`
}

// yleEpisode is one episode a list names, and the folder it goes into.
type yleEpisode struct{ id, title, folder string }

// series reads a series or a season into one file an episode, each resolved
// when its transfer starts; a series files each season's episodes in a
// folder of their own.
//
// Each episode is asked of the preview as the listing is read, several at a
// time, because the cards say neither where an episode is shown nor whether
// it is audio. One Areena does not offer is left out, the note saying why
// once; so, once one of them has been refused here, is every episode Yle
// shows in Finland alone. One whose preview could not be read is kept, for
// its transfer to try again.
func (y *Yle) series(ctx context.Context, lang, id string, page *ylePage) (*Result, error) {
	props := page.doc.Props.PageProps
	name := cmp.Or(strings.TrimSpace(props.View.Title), strings.TrimSpace(props.Meta.Title), id)
	title := name
	lists := page.lists(id)
	if len(lists) == 0 {
		return nil, fmt.Errorf("yle: %s lists no episodes", title)
	}
	if page.season() {
		title = ardJoin(name, lists[0].season)
	}
	if page.appID == "" || page.key == "" {
		return nil, fmt.Errorf("yle: %s: the page names no key for its listings", title)
	}
	listing, listed, walkErr := y.walk(ctx, lang, page, lists, !page.season(), config.MaxResolvedEpisodes)
	if len(listing) == 0 {
		if walkErr != nil {
			return nil, fmt.Errorf("yle: %s: %w", title, walkErr)
		}
		return nil, fmt.Errorf("yle: %s lists no episodes", title)
	}
	episodes := make([]*yleChecked, len(listing))
	for i, ep := range listing {
		episodes[i] = &yleChecked{episode: ep}
	}

	audio := strings.HasPrefix(props.Meta.Item.Type, "Radio")
	var refused []string
	resolved, note, err := expandSeries(ctx, series[*yleChecked]{
		site: "yle", noun: "episodes", link: title,
		items: episodes, listed: listed,
		refused: &refused,
		resolve: func(ctx context.Context, c *yleChecked) (*File, error) {
			od, err := y.preview(ctx, lang, c.episode.id)
			if _, refused := refusalReason(err); refused {
				return nil, err
			}
			c.preview = od
			ext := ".ts"
			switch {
			case od != nil:
				ext = od.extension()
			case audio:
				ext = ".mp3"
			}
			file := y.lazy(lang, ardJoin(name, cmp.Or(c.episode.title, c.episode.id)), ext, c.episode)
			return &file, nil
		},
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	// Whether what Yle shows in Finland alone is refused here is asked
	// once, of the first such episode in the listing's order, and holds for
	// the rest.
	var (
		files   []File
		finland int
		blocked *bool
	)
	for _, r := range resolved {
		if r.item.preview != nil && r.item.preview.finlandOnly() {
			if blocked == nil {
				_, _, err := y.programme(ctx, lang, r.item.episode.id)
				refusedHere := errors.Is(err, errYleFinlandOnly)
				blocked = &refusedHere
			}
			if *blocked {
				finland++
				continue
			}
		}
		files = append(files, r.file)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("yle: %s: %w", title, errYleFinlandOnly)
	}
	note = listingNote(note, brokenNote(walkErr),
		leftOutNote(finland, " that Yle shows in Finland only"), refusedNote(refused))
	return &Result{Title: title, Files: files, Note: note}, nil
}

// yleChecked is a listed episode and what its preview said of it, once
// asked.
type yleChecked struct {
	episode yleEpisode
	preview *yleOndemand
}

// walk reads each list a page at a time, keeping its episodes up to limit
// and counting the rest by what the lists state of themselves. Pages are
// followed by that count and by what each adds, never by the size asked
// for, so a page cut shorter than asked does not end the list. The first
// page that fails ends the walk, and what was read before it comes back
// with the error.
func (y *Yle) walk(ctx context.Context, lang string, page *ylePage, lists []yleList, folders bool, limit int) ([]yleEpisode, int, error) {
	var (
		episodes []yleEpisode
		listed   int
		seen     = make(map[string]bool)
	)
	for _, l := range lists {
		folder := ""
		if folders {
			folder = sourceFolder(l.season)
		}
		for offset := 0; ; {
			if len(episodes) >= limit {
				// Full: the rest of the list is only counted.
				cards, err := y.cards(ctx, lang, page, l, offset, 1)
				if err != nil {
					return episodes, listed, err
				}
				listed += max(cards.Meta.Count-offset, 0)
				break
			}
			cards, err := y.cards(ctx, lang, page, l, offset, yleListPage)
			if err != nil {
				return episodes, listed, err
			}
			added := 0
			for _, card := range cards.Data {
				ep := strings.TrimPrefix(card.Pointer.URI, "yleareena://items/")
				if !yleItemID.MatchString(ep) || seen[ep] || card.Pointer.Type != "program" && card.Pointer.Type != "clip" {
					continue
				}
				seen[ep] = true
				added++
				listed++
				if len(episodes) < limit {
					episodes = append(episodes, yleEpisode{id: ep, title: strings.TrimSpace(card.Title), folder: folder})
				}
			}
			offset += len(cards.Data)
			if added == 0 || cards.Meta.Count > 0 && offset >= cards.Meta.Count {
				break
			}
		}
	}
	return episodes, listed, nil
}

// cards reads one page of a list, with the parameters its season adds.
func (y *Yle) cards(ctx context.Context, lang string, page *ylePage, l yleList, offset, size int) (*yleCards, error) {
	u, err := url.Parse(l.uri)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	for k, v := range l.params {
		q.Set(k, v)
	}
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(size))
	q.Set("language", yleUILanguage[lang])
	q.Set("v", yleUIVersion)
	q.Set("client", "yle-areena-web")
	q.Set("app_id", page.appID)
	q.Set("app_key", page.key)
	u.RawQuery = q.Encode()
	var cards yleCards
	if err := y.client.GetJSON(ctx, u.String(), y.headers(lang), &cards); err != nil {
		return nil, err
	}
	return &cards, nil
}

// lazy is a series' episode, resolved when its transfer starts. It keeps the
// name it is listed under, which carries the episode's number where the
// series shows one, and under which a listing read again finds what was
// saved; the extension is the stream's own.
func (y *Yle) lazy(lang, stem, ext string, ep yleEpisode) File {
	return lazyEpisode(stem+ext, ep.folder, y.headers(lang), func(ctx context.Context) (*Target, error) {
		target, _, err := y.programme(ctx, lang, ep.id)
		if err != nil {
			return nil, err
		}
		target.Name = stem + path.Ext(target.Name)
		return target, nil
	})
}
