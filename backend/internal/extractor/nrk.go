package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// NRK resolves tv.nrk.no programmes and series.
//
// The Norwegian broadcaster publishes its player API openly, so this is
// svtplay's sibling in almost every respect: one request names the streams
// for a programme, another lists a series by season, and what comes back is
// an adaptive manifest rather than a file. Three things are its own.
//
// The first is pleasant. NRK's master playlist carries no EXT-X-MEDIA entry
// at all — every variant already holds video and audio in the same segments,
// which is what the CDN path calls them: "muxed". So hlsVariant.muxed()
// accepts all of them and the best simply wins, the exact opposite of
// vimeo, where CODECS advertises audio that lives in a playlist of its own
// and believing it yields a silent file.
//
// The second is not. Most of the catalogue is licensed for Norway only, and
// the manifest says so in words meant for a viewer: "Ikke tilgjengelig
// utenfor Norge". That message is passed through untouched, because which
// country and for what reason is exactly what the caller needs to know and
// only NRK knows it — anything invented here would be a worse guess dressed
// up as an answer.
//
// The third decides where titles come from. The manifest does carry one, but
// only inside its analytics block and only as "Series: Episode" glued
// together, and a colon does not survive a filename. The metadata document
// the manifest links to looks like the fix and is smaller, until a
// programme that belongs to no series turns up: its subtitle is the
// synopsis, a paragraph long. So the programme document is read instead,
// which names the series and the episode in fields of their own and leaves
// both empty when there is no series to name.
type NRK struct {
	client *httpx.Client
}

const (
	nrkRoot        = "https://tv.nrk.no"
	nrkAPI         = "https://psapi.nrk.no"
	nrkManifestAPI = nrkAPI + "/playback/manifest/program/"
	nrkProgramAPI  = nrkAPI + "/programs/"
	nrkCatalogAPI  = nrkAPI + "/tv/catalog/series/"

	// nrkMaxEpisodes bounds how many episodes one series URL expands to.
	nrkMaxEpisodes = 500

	// A series' extra material is a season like any other, except that the
	// page names it in Norwegian and the API in English.
	nrkExtraSeason = "ekstramateriale"
	nrkExtraPath   = "extramaterial"
)

// nrkEpisodeLink finds programme ids in a rendered season page. An id always
// begins with letters, and that is deliberate here: the site also accepts a
// bare number in the same position, and a number is a place in the season
// rather than something that can be looked up.
var nrkEpisodeLink = regexp.MustCompile(`/episode/([A-Za-z][A-Za-z0-9]{5,})`)

// NewNRK builds the NRK TV extractor.
func NewNRK(client *httpx.Client) *NRK { return &NRK{client: client} }

func (n *NRK) Name() string { return "nrk" }

// Match takes tv.nrk.no alone. Radio and the news site share the same player
// API but not the catalogue below, and matching them would promise more than
// this resolves.
func (n *NRK) Match(u *url.URL) bool { return util.HostMatches(u.Host, "tv.nrk.no") }

// Extract handles a single programme, one season, or a whole series.
func (n *NRK) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	ref := nrkTarget(u)
	switch {
	case ref.program != "":
		return n.programme(ctx, ref.program)
	case ref.slug == "":
		return nil, fmt.Errorf("nrk: no programme or series in %s", u.Redacted())
	case ref.number > 0:
		id, err := n.numbered(ctx, ref)
		if err != nil {
			return nil, err
		}
		return n.programme(ctx, id)
	}
	return n.series(ctx, u, ref)
}

// nrkRef is what a tv.nrk.no URL points at: a programme, a series, or one
// season of one.
type nrkRef struct {
	// program is the programme id, when the URL names one outright.
	program string
	// slug identifies the series.
	slug string
	// season is the season within that series, when the URL names one.
	season string
	// number is an episode's place in that season, which the older links
	// carry instead of a programme id.
	number int
}

// nrkTarget reads a tv.nrk.no URL. Every shape the site has used still
// resolves — it answers the old ones with a redirect to the current one, so
// they are what sits in bookmarks and in other people's pages.
func nrkTarget(u *url.URL) nrkRef {
	query := u.Query()
	if id := strings.TrimSpace(query.Get("v")); id != "" {
		return nrkRef{program: id} // the share link, /se?v=<id>
	}
	segs := util.PathSegments(u)
	if len(segs) >= 2 {
		switch segs[0] {
		case "program":
			return nrkRef{program: segs[1]}
		case "serie":
			return nrkSeriesTarget(segs[1:])
		}
	}
	if slug := strings.TrimSpace(query.Get("s")); slug != "" {
		return nrkRef{slug: slug} // the series share link, /se?s=<slug>
	}
	return nrkRef{}
}

// nrkSeriesTarget reads what follows the slug in a /serie/... path.
func nrkSeriesTarget(segs []string) nrkRef {
	ref := nrkRef{slug: segs[0]}
	switch {
	case len(segs) == 1:
		return ref // the whole series
	case segs[1] != "sesong":
		return nrkRef{program: segs[1]} // the older shape, /serie/<slug>/<id>
	case len(segs) == 2:
		return ref // a season page that names no season is the series itself
	}

	ref.season = segs[2]
	if len(segs) >= 5 && segs[3] == "episode" {
		// An episode is named either by its programme id or, in the links
		// the site still redirects, by its place in the season.
		if n, err := strconv.Atoi(segs[4]); err == nil {
			ref.number = n
		} else {
			return nrkRef{program: segs[4]}
		}
	}
	return ref
}

// programme resolves one programme id into a job of its own.
func (n *NRK) programme(ctx context.Context, id string) (*Result, error) {
	file, err := n.episode(ctx, id)
	if err != nil {
		return nil, err
	}
	// A lone file is written straight into the download directory, with no
	// folder to say what it belongs to, so the name carries the series as
	// well: NRK episode titles are routinely no more than "1. episode".
	title := util.FirstNonEmpty(n.title(ctx, id), id)
	file.Name = title + file.Name
	return &Result{Title: title, Files: []File{*file}}, nil
}

// series expands a series, or one season of it, into one file per episode.
func (n *NRK) series(ctx context.Context, u *url.URL, ref nrkRef) (*Result, error) {
	title, episodes, err := n.seasons(ctx, ref)
	if err != nil || len(episodes) == 0 {
		// Fall back to reading ids off the rendered page, which still works
		// for the odd listing the catalogue does not describe.
		title, episodes, err = n.episodesFromPage(ctx, u)
		if err != nil {
			return nil, err
		}
	}
	if len(episodes) > nrkMaxEpisodes {
		episodes = episodes[:nrkMaxEpisodes]
	}

	// Only nest by season when there is more than one to tell apart.
	nest := nestByLabel(episodes, func(ep nrkEpisodeRef) string { return ep.season })

	result := &Result{Title: util.FirstNonEmpty(title, ref.slug)}
	var refused string
	for _, ep := range episodes {
		file, err := n.episode(ctx, ep.id)
		if err != nil {
			// One geo-blocked or expired episode must not sink the rest.
			// Its reason is kept, though: when every episode is refused it
			// is refused for the same reason, and NRK's own words for it
			// are better than anything this could conclude.
			var refusal *nrkRefused
			if refused == "" && errors.As(err, &refusal) {
				refused = refusal.message
			}
			continue
		}
		file.Name = util.FirstNonEmpty(ep.name, ep.id) + file.Name
		if nest {
			file.Dir = ep.season
		}
		result.Files = append(result.Files, *file)
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("nrk: none of the %d episodes of %s are playable: %s",
			len(episodes), u.Redacted(),
			util.FirstNonEmpty(refused, "the catalogue lists them but the player offers no stream"))
	}
	return result, nil
}

// nrkEpisodeRef is one episode of a series, with the season it belongs to.
type nrkEpisodeRef struct {
	id     string
	name   string
	season string
	number int
}

// seasons reads a series' episodes from the catalogue.
func (n *NRK) seasons(ctx context.Context, ref nrkRef) (string, []nrkEpisodeRef, error) {
	if ref.season != "" {
		return n.season(ctx, ref.slug, ref.season)
	}

	var doc nrkCatalogue
	if err := n.client.GetJSON(ctx, nrkCatalogAPI+url.PathEscape(ref.slug),
		httpx.Referer(nrkRoot+"/"), &doc); err != nil {
		return "", nil, fmt.Errorf("nrk: catalogue for %s: %w", ref.slug, err)
	}

	var episodes []nrkEpisodeRef
	seen := make(map[string]bool)
	for _, season := range doc.Embedded.Seasons {
		episodes = append(episodes, nrkRefs(season, season.Titles.Title, seen)...)
	}
	if len(episodes) == 0 {
		// A drama series embeds every season's episodes and is done in this
		// one request. A news or magazine series, whose "seasons" are months
		// or years and run to the hundreds, embeds only the one NRK itself
		// shows, and its other seasons have URLs of their own to be pasted.
		episodes = nrkRefs(doc.Embedded.Instalments, "", seen)
	}
	return doc.title(), episodes, nil
}

// season reads one season, which is the one listing shape the catalogue
// serves the same way whatever kind of series it belongs to.
func (n *NRK) season(ctx context.Context, slug, name string) (string, []nrkEpisodeRef, error) {
	path := nrkCatalogAPI + url.PathEscape(slug) + "/seasons/" + url.PathEscape(name)
	if strings.EqualFold(name, nrkExtraSeason) {
		path = nrkCatalogAPI + url.PathEscape(slug) + "/" + nrkExtraPath
	}

	var doc nrkSeason
	if err := n.client.GetJSON(ctx, path, httpx.Referer(nrkRoot+"/"), &doc); err != nil {
		return "", nil, fmt.Errorf("nrk: season %s of %s: %w", name, slug, err)
	}

	// The season goes in the job title rather than in a folder below it.
	// There is only one season here, so a folder would say nothing; and
	// several seasons downloaded separately would otherwise share a folder,
	// where an episode list that starts again at "1. episode" collides.
	title := nrkJoin(util.FirstNonEmpty(doc.Links.Series.Title, slug), doc.Titles.Title)
	return title, nrkRefs(doc, "", make(map[string]bool)), nil
}

// numbered resolves the link shape that names an episode by its place in the
// season rather than by its programme id.
func (n *NRK) numbered(ctx context.Context, ref nrkRef) (string, error) {
	_, episodes, err := n.season(ctx, ref.slug, ref.season)
	if err != nil {
		return "", err
	}
	for _, ep := range episodes {
		if ep.number == ref.number {
			return ep.id, nil
		}
	}
	// A listing that numbers nothing — which is how the catalogue describes
	// everything that is not a drama series — is taken in the order it came.
	if ref.number >= 1 && ref.number <= len(episodes) {
		return episodes[ref.number-1].id, nil
	}
	return "", fmt.Errorf("nrk: %s season %s has no episode %d", ref.slug, ref.season, ref.number)
}

// episodesFromPage recovers programme ids straight from the rendered page.
func (n *NRK) episodesFromPage(ctx context.Context, u *url.URL) (string, []nrkEpisodeRef, error) {
	doc, err := n.client.GetString(ctx, u.String(), httpx.Referer(nrkRoot+"/"))
	if err != nil {
		return "", nil, fmt.Errorf("nrk: fetch %s: %w", u.Redacted(), err)
	}

	var episodes []nrkEpisodeRef
	seen := make(map[string]bool)
	for _, match := range nrkEpisodeLink.FindAllStringSubmatch(doc, -1) {
		if id := match[1]; !seen[id] {
			seen[id] = true
			episodes = append(episodes, nrkEpisodeRef{id: id})
		}
	}
	if len(episodes) == 0 {
		return "", nil, fmt.Errorf("nrk: no episodes listed on %s", u.Redacted())
	}
	return nrkPageTitle(doc), episodes, nil
}

// nrkPageTitle reads the series name out of a page title. The suffix is a
// fixed string and is trimmed as one: cutting at the first " - " instead,
// the way most of these hosts are handled, would behead every series whose
// own name contains a dash.
func nrkPageTitle(doc string) string {
	return strings.TrimSpace(strings.TrimSuffix(firstTitleOf(doc), " - NRK TV"))
}

// episode resolves one programme id to a playable segment list. The returned
// File carries only the extension in Name; the caller supplies the stem,
// which differs between a standalone programme and one episode of a series.
func (n *NRK) episode(ctx context.Context, id string) (*File, error) {
	var doc nrkManifest
	if err := n.client.GetJSON(ctx, nrkManifestAPI+url.PathEscape(id),
		httpx.Referer(nrkRoot+"/"), &doc); err != nil {
		return nil, fmt.Errorf("nrk: manifest for %s: %w", id, err)
	}
	if !strings.EqualFold(doc.Playability, "playable") {
		return nil, &nrkRefused{id: id, message: doc.refusal()}
	}

	// stream's refusals are written to follow the programme id as a sentence
	// about it, so there is no colon between the two.
	stream, err := doc.stream()
	if err != nil {
		return nil, fmt.Errorf("nrk: %s %w", id, err)
	}

	headers := httpx.Referer(nrkRoot + "/")
	segments, variant, err := resolvePlaylist(ctx, n.client, stream, headers)
	if err != nil {
		return nil, fmt.Errorf("nrk: %s: %w", id, err)
	}
	return &File{
		Name:     playlistExtension(variant),
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}, nil
}

// title names a programme, and never fails: a missing title costs a good
// filename, which is no reason to refuse a download that would otherwise
// work.
func (n *NRK) title(ctx context.Context, id string) string {
	var doc nrkProgram
	if err := n.client.GetJSON(ctx, nrkProgramAPI+url.PathEscape(id),
		httpx.Referer(nrkRoot+"/"), &doc); err != nil {
		return ""
	}
	return nrkJoin(doc.SeriesTitle, util.FirstNonEmpty(doc.EpisodeTitle, doc.Title))
}

// nrkJoin names something by its series and its episode together, and copes
// with a programme that has only one of the two.
func nrkJoin(series, episode string) string {
	switch {
	case episode == "":
		return series
	case series == "" || strings.EqualFold(series, episode):
		return episode
	}
	return series + " - " + episode
}

// nrkRefused is NRK declining to play something, carrying its own words for
// why. It is a type rather than a message so that expanding a whole series
// can report the reason once instead of the same refusal several hundred
// times.
type nrkRefused struct {
	id      string
	message string
}

func (e *nrkRefused) Error() string { return "nrk: " + e.id + ": " + e.message }

// nrkManifest is the part of a playback manifest this needs.
type nrkManifest struct {
	Playability string `json:"playability"`
	Playable    struct {
		Assets []nrkAsset `json:"assets"`
	} `json:"playable"`
	NonPlayable struct {
		Reason                   string `json:"reason"`
		EndUserMessage           string `json:"endUserMessage"`
		EndUserMessageSupplement string `json:"endUserMessageSupplement"`
	} `json:"nonPlayable"`
}

// nrkAsset is one of the streams a playable manifest offers.
type nrkAsset struct {
	URL       string `json:"url"`
	Format    string `json:"format"`
	MimeType  string `json:"mimeType"`
	Encrypted bool   `json:"encrypted"`
}

// stream picks the playlist to fetch.
//
// An encrypted asset is skipped rather than preferred away, because NRK
// publishes those alongside the rest instead of withholding them: taken, it
// would download to the end and then play as noise.
func (m nrkManifest) stream() (string, error) {
	var encrypted bool
	for _, asset := range m.Playable.Assets {
		switch {
		case asset.Encrypted:
			encrypted = true
		case asset.URL == "":
			// Listed but with nowhere to fetch it from: skip it.
		case strings.EqualFold(asset.Format, "HLS"),
			strings.Contains(strings.ToLower(asset.MimeType), "mpegurl"):
			return asset.URL, nil
		}
	}
	if encrypted {
		return "", errors.New("is DRM protected")
	}
	return "", errors.New("offers no playlist to fetch")
}

// refusal is NRK's explanation for a programme it will not play, passed
// through as it was written. It is geo-blocking almost every time, and the
// message names the country.
func (m nrkManifest) refusal() string {
	message := strings.TrimSpace(m.NonPlayable.EndUserMessage)
	if extra := strings.TrimSpace(m.NonPlayable.EndUserMessageSupplement); extra != "" {
		message = strings.TrimSpace(message + " " + extra)
	}
	if message != "" {
		return message
	}
	// A refusal with nothing to say to a viewer still has a machine-readable
	// reason, which is better than reporting nothing at all.
	return util.FirstNonEmpty(m.NonPlayable.Reason, m.Playability, "not playable")
}

// nrkProgram is the part of a programme document this needs. Both title
// fields are null for a programme that belongs to no series, which is what
// makes them usable: there is no synopsis to mistake for an episode name.
type nrkProgram struct {
	Title        string `json:"title"`
	SeriesTitle  string `json:"seriesTitle"`
	EpisodeTitle string `json:"episodeTitle"`
}

// nrkCatalogue is a series' listing.
type nrkCatalogue struct {
	// The series' own titles sit under a key named after its type —
	// "sequential" for a drama, "standard" for a magazine, "news" for a
	// current-affairs strand — so all three are read and whichever answered
	// wins. A type nobody here has seen costs the title and nothing else.
	Sequential nrkTitled `json:"sequential"`
	Standard   nrkTitled `json:"standard"`
	News       nrkTitled `json:"news"`
	Embedded   struct {
		Seasons []nrkSeason `json:"seasons"`
		// Instalments is the season the site itself displays, for the kinds
		// of series that embed no episodes above. It is an object wrapping
		// the same list a season carries, which is why it reads as one.
		Instalments nrkSeason `json:"instalments"`
	} `json:"_embedded"`
}

// title is the series name, wherever the document filed it.
func (c nrkCatalogue) title() string {
	return util.FirstNonEmpty(c.Sequential.Titles.Title, c.Standard.Titles.Title, c.News.Titles.Title)
}

// nrkTitled is anything the catalogue gives a name to.
type nrkTitled struct {
	Titles nrkTitles `json:"titles"`
}

// nrkTitles is the catalogue's name pair. The subtitle is a description
// rather than a second name, so nothing here reads it.
type nrkTitles struct {
	Title string `json:"title"`
}

// nrkSeason is one season, and also the wrapper the catalogue puts the
// displayed season in.
type nrkSeason struct {
	Titles nrkTitles `json:"titles"`
	Links  struct {
		Series struct {
			Title string `json:"title"`
		} `json:"series"`
	} `json:"_links"`
	Embedded struct {
		// A drama series calls them episodes and numbers them; everything
		// else calls them instalments and does not.
		Episodes    []nrkEpisode `json:"episodes"`
		Instalments []nrkEpisode `json:"instalments"`
	} `json:"_embedded"`
}

// items is whichever of the two lists this season came with.
func (s nrkSeason) items() []nrkEpisode {
	if len(s.Embedded.Episodes) > 0 {
		return s.Embedded.Episodes
	}
	return s.Embedded.Instalments
}

// nrkEpisode is one entry of a season listing.
type nrkEpisode struct {
	// PrfID is the programme id, which is what the player API is asked for.
	// The id beside it identifies the listing entry and is no use here.
	PrfID          string    `json:"prfId"`
	Titles         nrkTitles `json:"titles"`
	SequenceNumber int       `json:"sequenceNumber"`
}

// nrkRefs collects a season's entries, skipping any the caller has already
// taken from another season.
func nrkRefs(season nrkSeason, label string, seen map[string]bool) []nrkEpisodeRef {
	var out []nrkEpisodeRef
	for _, ep := range season.items() {
		if ep.PrfID == "" || seen[ep.PrfID] {
			continue
		}
		seen[ep.PrfID] = true
		out = append(out, nrkEpisodeRef{
			id:     ep.PrfID,
			name:   ep.Titles.Title,
			season: label,
			number: ep.SequenceNumber,
		})
	}
	return out
}
