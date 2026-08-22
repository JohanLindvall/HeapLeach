package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// ARD resolves ardmediathek.de programmes, films and series.
//
// The German public broadcasters publish an open page gateway — no key, no
// cookie, no token, not even a Referer — and, alone among the broadcasters
// here, they publish a plain progressive MP4 beside the adaptive manifest.
// That single fact is what makes this extractor worth having in the shape it
// is: the MP4 is a rangeable file, so it lands on the segmented engine with
// its eight connections, its resume across runs and its pre-connect skip,
// where svtplay and nrk can only hand back a segment list to be fetched part
// by part and concatenated. The manifest is kept as a fallback for the
// handful of items that have no file behind them, and nothing more.
//
// Three properties of the payload decide the rest of the code.
//
// The first is which media entry to take, and it is emphatically not the
// largest. One item's media list carries the same programme three times over
// — the ordinary broadcast sound, a dialogue-boosted "klare Sprache" remix,
// and an audio description with a narrator talking over the picture —
// distinguished by nothing but audios[].kind, and with the remixed and
// described versions routinely listed *before* the ordinary one. Choosing by
// resolution alone downloads a film with somebody describing it, at 1080p,
// and it finishes cleanly. So the soundtrack is compared first and the
// picture only after.
//
// The second is geo-blocking, which the payload states in two places and gets
// right in one. The widget's own `geoblocked` reads false on material the CDN
// then refuses from outside Germany with a bare 403;
// mediaCollection.embedded.isGeoBlocked is the field that reads true, and it
// is what this refuses on — before fetching anything, so the caller is told
// what happened rather than handed a transfer error. ARD supplies no sentence
// to pass through the way NRK's endUserMessage does, only booleans, so the
// wording of a refusal here is our own.
//
// The third is that a listing mixes its accessibility copies in with its
// episodes, and the season endpoint that filters them out only works for a
// series that has seasons. They are recognised by their ids instead: see
// ardWithoutVariants.
type ARD struct {
	client *httpx.Client
}

const (
	ardRoot = "https://www.ardmediathek.de"

	// ardGateway is the API the site itself is built on. Every endpoint below
	// answers an anonymous request.
	ardGateway  = "https://api.ardmediathek.de/page-gateway"
	ardItemAPI  = ardGateway + "/pages/ard/item/"
	ardAssetAPI = ardGateway + "/widgets/ard/asset/"

	// ardMimeMP4 and ardMimeHLS are how the media list names what it offers.
	ardMimeMP4 = "video/mp4"
	ardMimeHLS = "application/vnd.apple.mpegurl"

	// ardMinIDLen separates an id from the decoration around it in a path. An
	// id is base64 of a content identifier and so is never short; what
	// follows it in a series link is a season number and, on the
	// audio-described variant of a season page, the two letters "AD".
	ardMinIDLen = 8

	// ardSeasonPrefix marks the season segment of a series link.
	ardSeasonPrefix = "staffel-"
)

// NewARD builds the ARD Mediathek extractor.
func NewARD(client *httpx.Client) *ARD { return &ARD{client: client} }

func (a *ARD) Name() string { return "ard" }

func (a *ARD) Match(u *url.URL) bool { return util.HostMatches(u.Host, "ardmediathek.de") }

// Extract handles a single video, or a programme that expands to every
// episode listed under it.
func (a *ARD) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	ref := ardTarget(u)
	switch {
	case ref.item != "":
		return a.video(ctx, ref.item)
	case ref.show != "":
		return a.series(ctx, u, ref)
	}
	return nil, fmt.Errorf("ard: no video, film or programme in %s", u.Redacted())
}

// ardRef is what an ardmediathek.de URL points at.
type ardRef struct {
	// item is a single video's content id.
	item string
	// show is a programme's content id, which a film has as much as a series
	// does: ARD files a one-off film as a programme with one episode in it.
	show string
	// season is the season within that programme, when the link names one.
	season string
}

// ardTarget reads an ardmediathek.de URL.
//
// The id is at neither end of the path. In front of it sit slugs that exist
// for search engines and carry no meaning, a varying number of them; behind
// it, on a series link, sit the season number and — on the link to that
// season's audio-described listing — the two letters "AD". So the id is found
// by length instead: it is base64 of a content identifier and never short,
// while everything that may follow it is one or two characters.
func ardTarget(u *url.URL) ardRef {
	segs := util.PathSegments(u)
	if len(segs) < 2 {
		return ardRef{}
	}
	switch segs[0] {
	case "video":
		return ardRef{item: ardID(segs[1:])}
	case "sendung", "serie", "film":
		// A programme link always carries its slug, so the id is never the
		// segment straight after the kind. Skipping it is what stops a
		// programme link that has lost its id from being read as one.
		id := ardID(segs[2:])
		if id == "" {
			return ardRef{}
		}
		return ardRef{show: id, season: ardSeason(segs)}
	}
	return ardRef{}
}

// ardID picks the content id out of a path's segments. The season segment is
// long enough to be taken for one, so it is named and stepped over.
func ardID(segs []string) string {
	for i := len(segs) - 1; i >= 0; i-- {
		if seg := segs[i]; len(seg) >= ardMinIDLen && !strings.HasPrefix(seg, ardSeasonPrefix) {
			return seg
		}
	}
	return ""
}

// ardSeason reads the season out of a series link. The trailing segment
// numbers it too, but only the named segment says what it is, and a link to
// the audio-described season has a further segment after the number.
func ardSeason(segs []string) string {
	for _, seg := range segs {
		if !strings.HasPrefix(seg, ardSeasonPrefix) {
			continue
		}
		number := strings.TrimPrefix(seg, ardSeasonPrefix)
		if i := strings.IndexFunc(number, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			number = number[:i] // "staffel-1-mit-audiodeskription"
		}
		if number != "" {
			return number
		}
	}
	return ""
}

// video resolves one item id into a job of its own.
func (a *ARD) video(ctx context.Context, id string) (*Result, error) {
	file, show, episode, err := a.episode(ctx, id)
	if err != nil {
		return nil, err
	}
	// A lone file is written straight into the download directory with no
	// folder to say what it belongs to, so the name carries the programme as
	// well — unless, as is usual here, the episode title already repeats it.
	title := ardJoin(show, util.FirstNonEmpty(episode, id))
	file.Name = title + file.Name
	return &Result{Title: title, Files: []File{*file}}, nil
}

// series expands a programme into one file per episode, filed by season when
// the programme has more than one.
func (a *ARD) series(ctx context.Context, u *url.URL, ref ardRef) (*Result, error) {
	title, episodes, err := a.episodes(ctx, ref)
	if err != nil {
		return nil, err
	}
	if len(episodes) == 0 {
		return nil, fmt.Errorf("ard: %s lists nothing to download", u.Redacted())
	}
	if len(episodes) > config.MaxListingVideos {
		episodes = episodes[:config.MaxListingVideos]
	}

	// Only nest by season when there is more than one to tell apart.
	distinct := make(map[string]bool)
	for _, ep := range episodes {
		if ep.season != "" {
			distinct[ep.season] = true
		}
	}
	nest := len(distinct) > 1

	// Every episode needs a request of its own, and a series runs to
	// hundreds; fetched one after another the job would sit resolving for
	// minutes before a byte moved.
	resolved := FanOut(ctx, episodes, func(ctx context.Context, ep ardEpisodeRef) ([]ardResolved, error) {
		file, _, episodeTitle, err := a.episode(ctx, ep.id)
		if err != nil {
			// One refused episode must not sink the rest, but its reason is
			// kept: when every episode is refused they are refused for the
			// same reason, and reporting it once is the only useful answer.
			var refused *ardRefused
			if errors.As(err, &refused) {
				return []ardResolved{{refusal: refused.reason}}, nil
			}
			return nil, err
		}
		file.Name = util.FirstNonEmpty(episodeTitle, ep.name, ep.id) + file.Name
		if nest {
			file.Dir = ep.season
		}
		return []ardResolved{{file: file}}, nil
	})

	result := &Result{Title: util.FirstNonEmpty(title, ref.show)}
	var refusal string
	for _, r := range resolved {
		if r.file == nil {
			if refusal == "" {
				refusal = r.refusal
			}
			continue
		}
		result.Files = append(result.Files, *r.file)
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("ard: none of the %d entries of %s are playable: %s",
			len(episodes), u.Redacted(),
			util.FirstNonEmpty(refusal, "the listing names them but no stream is offered"))
	}
	return result, nil
}

// ardResolved is what expanding one listing entry produced: a file, or ARD's
// reason for offering none.
type ardResolved struct {
	file    *File
	refusal string
}

// ardEpisodeRef is one entry of a programme's listing.
type ardEpisodeRef struct {
	id     string
	name   string
	season string
}

// episodes names a programme and lists what to download from it.
func (a *ARD) episodes(ctx context.Context, ref ardRef) (string, []ardEpisodeRef, error) {
	// A season named in the link is taken on its own, and its name goes in
	// the job title rather than into a folder below it: two seasons fetched
	// from two links would otherwise land in one folder, where episode names
	// that start again at "Folge 1" collide.
	if ref.season != "" {
		widget, teasers, err := a.pages(ctx, ref.show, ref.season)
		if err != nil {
			return "", nil, err
		}
		return ardJoin(ardShowTitle(teasers), widget.Title), ardRefs(teasers, ""), nil
	}

	first, teasers, err := a.pages(ctx, ref.show, "")
	if err != nil {
		return "", nil, err
	}
	seasons := ardSeasons(teasers)
	if len(seasons) < 2 {
		return first.Title, ardRefs(teasers, ""), nil
	}

	// A series with seasons is fetched again, one season at a time, and the
	// flat listing above is discarded rather than sorted: the season endpoint
	// is the only one that names the seasons ("Staffel 2") and the only one
	// that leaves out the audio-described copies.
	var refs []ardEpisodeRef
	for _, season := range seasons {
		widget, seasonTeasers, err := a.pages(ctx, ref.show, season)
		if err != nil {
			continue // one unreadable season should not lose the others
		}
		// A season with no name of its own still needs a folder, or its
		// episodes would be written beside the folders of every other season
		// rather than into one of their own.
		refs = append(refs, ardRefs(seasonTeasers, util.FirstNonEmpty(widget.Title, "Staffel "+season))...)
	}
	if len(refs) == 0 {
		refs = ardRefs(teasers, "")
	}
	return first.Title, refs, nil
}

// pages fetches every page of one listing and returns the first page's widget
// — which carries the name and the season list — alongside the whole of it.
func (a *ARD) pages(ctx context.Context, show, season string) (*ardWidget, []ardTeaser, error) {
	first, err := a.page(ctx, show, season, 0)
	if err != nil {
		return nil, nil, err
	}
	teasers := first.Teasers
	// Only totalElements is worth reading out of the pagination block: on
	// every page after the first, pageNumber and pageSize come back as an
	// offset and a count instead, so a loop that trusted them would either
	// stop early or never stop.
	for n := 1; len(teasers) < first.Pagination.TotalElements && len(teasers) < config.MaxListingVideos; n++ {
		next, err := a.page(ctx, show, season, n)
		if err != nil || len(next.Teasers) == 0 {
			break
		}
		teasers = append(teasers, next.Teasers...)
	}
	return first, teasers, nil
}

// page fetches one page of a programme's listing, or of one season of it.
func (a *ARD) page(ctx context.Context, show, season string, number int) (*ardWidget, error) {
	query := url.Values{
		"pageNumber": {strconv.Itoa(number)},
		"pageSize":   {strconv.Itoa(config.FolderPageSize)},
		"embedded":   {"true"}, // without this the widget answers with no teasers at all
	}
	if season != "" {
		query.Set("seasoned", "true")
		query.Set("seasonNumber", season)
	}

	endpoint := ardAssetAPI + url.PathEscape(show) + "?" + query.Encode()
	var widget ardWidget
	if err := a.client.GetJSON(ctx, endpoint, httpx.Referer(ardRoot+"/"), &widget); err != nil {
		return nil, fmt.Errorf("ard: listing for %s: %w", show, err)
	}
	return &widget, nil
}

// episode resolves one item id to something downloadable. The returned File
// carries only the extension in Name; the caller supplies the stem, which
// differs between a standalone video and one episode of a series.
func (a *ARD) episode(ctx context.Context, id string) (file *File, show, episode string, err error) {
	endpoint := ardItemAPI + url.PathEscape(id) + "?embedded=false&mcV6=true"
	var doc ardItem
	if err := a.client.GetJSON(ctx, endpoint, httpx.Referer(ardRoot+"/"), &doc); err != nil {
		return nil, "", "", fmt.Errorf("ard: item %s: %w", id, err)
	}
	if len(doc.Widgets) == 0 {
		return nil, "", "", fmt.Errorf("ard: %s is not a video", id)
	}

	widget := doc.Widgets[0]
	if reason := widget.refusal(); reason != "" {
		return nil, "", "", &ardRefused{id: id, reason: reason}
	}
	media := widget.MediaCollection.Embedded
	show = util.FirstNonEmpty(widget.Show.Title, media.Meta.SeriesTitle)
	episode = util.FirstNonEmpty(widget.Title, media.Meta.Title)
	headers := httpx.Referer(ardRoot + "/")

	// The file, whenever there is one: rangeable, resumable, and skipped on a
	// second run once its length matches what is on disk. ARD publishes no
	// byte count of its own, so that length comes from the response headers.
	if best, ok := media.best(ardMimeMP4); ok {
		return &File{
			Name:    ".mp4",
			URL:     best.URL,
			Size:    -1,
			Headers: headers,
		}, show, episode, nil
	}

	best, ok := media.best(ardMimeHLS)
	if !ok {
		return nil, "", "", fmt.Errorf("ard: %s offers no stream", id)
	}
	segments, variant, err := resolvePlaylist(ctx, a.client, best.URL, headers)
	if err != nil {
		return nil, "", "", fmt.Errorf("ard: %s: %w", id, err)
	}
	return &File{
		Name:     ardExtension(segments, variant),
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}, show, episode, nil
}

// ardExtension names the container the joined segments will be in.
//
// playlistExtension answers from the variant's own URL, and ARD's packager
// makes that URL lie: it builds the manifest path out of the name of the
// source file, so an MPEG-TS stream is served from ".../<name>.mp4.csmil/"
// and the shared rule reads that ".mp4" and calls a pile of TS segments an
// MP4. A segment's own name cannot be misread that way, so it is what is
// asked, and the shared rule is kept for the case where it says nothing.
func ardExtension(segments []string, variant hlsVariant) string {
	if len(segments) > 0 {
		switch strings.ToLower(path.Ext(util.NameFromURL(segments[0]))) {
		case ".ts":
			return ".ts"
		case ".mp4", ".m4s":
			return ".mp4"
		}
	}
	return playlistExtension(variant)
}

// ardJoin names something by its programme and its episode together.
//
// Containment rather than equality, unlike nrkJoin: ARD writes an episode
// title that spells the series out in full — "Folge 12 · Staffel 4 | A Drama
// (S04/E12)" — so prefixing the programme as well would say it twice.
func ardJoin(show, episode string) string {
	switch {
	case episode == "":
		return show
	case show == "" || strings.Contains(strings.ToLower(episode), strings.ToLower(show)):
		return episode
	}
	return show + " - " + episode
}

// ardRefused is ARD declining to play something. It is a type rather than a
// message so that expanding a series can report one reason once instead of
// the same refusal several hundred times.
type ardRefused struct {
	id     string
	reason string
}

func (e *ardRefused) Error() string { return "ard: " + e.id + ": " + e.reason }

// ardItem is the item document, which describes one video.
type ardItem struct {
	Widgets []ardItemWidget `json:"widgets"`
}

// ardItemWidget is the player widget, where everything about the video lives.
type ardItemWidget struct {
	Title string `json:"title"`
	Show  struct {
		Title string `json:"title"`
	} `json:"show"`
	// BlockedByFsk marks a title German law confines to late evening.
	BlockedByFsk bool `json:"blockedByFsk"`
	// BlockedByLoginOnly marks a title held back from anonymous viewers.
	BlockedByLoginOnly bool `json:"blockedByLoginOnly"`
	// AvailableTo is when the licence runs out, as a timestamp.
	AvailableTo string `json:"availableTo"`
	// Geoblocked is the widget's own geo flag, and it is not the one to read;
	// see refusal. It is named here so nothing reaches for it by accident.
	Geoblocked bool `json:"geoblocked"`
	// MediaCollection is absent altogether for anything ARD will not serve
	// right now, which is why it is a pointer.
	MediaCollection *struct {
		Embedded ardMedia `json:"embedded"`
	} `json:"mediaCollection"`
}

// refusal is why ARD will not play this, in our own words, or "" when it
// will. Unlike NRK's manifest the document carries no sentence written for a
// viewer, only flags, so the wording is ours and the ordering matters: an
// age-rated title arrives with no media collection at all, and "offers no
// stream" would be a true statement that explains nothing.
func (w ardItemWidget) refusal() string {
	switch {
	case w.BlockedByFsk:
		return "its age rating confines it to the late-evening window German broadcasting law sets, " +
			"and ARD serves it only within that window"
	case w.BlockedByLoginOnly:
		return "ARD serves it only to signed-in viewers"
	case w.MediaCollection == nil:
		if until, err := time.Parse(time.RFC3339, w.AvailableTo); err == nil && until.Before(time.Now()) {
			return "its availability ended on " + until.Format(time.DateOnly)
		}
		return "ARD lists it but offers no stream"
	case w.MediaCollection.Embedded.IsGeoBlocked:
		// The authoritative flag, and the widget's own `geoblocked` beside it
		// is not: that one reads false on material the CDN answers with 403
		// to every address outside Germany.
		return "ARD holds the rights for Germany alone and blocks it everywhere else"
	case w.MediaCollection.Embedded.Live != nil:
		// A live relay's playlist is a rolling window with no end declared,
		// so joining what it lists would produce a fragment of the broadcast
		// and call it a finished download.
		return "it is a live stream rather than a recording"
	}
	return ""
}

// ardMedia is the media collection: every rendition of one video.
type ardMedia struct {
	Meta struct {
		Title       string `json:"title"`
		SeriesTitle string `json:"seriesTitle"`
	} `json:"meta"`
	// IsGeoBlocked is ARD's own statement that this is for Germany only.
	IsGeoBlocked bool `json:"isGeoBlocked"`
	// Live is present, and non-null, only for a live relay.
	Live    *ardLive    `json:"live"`
	Streams []ardStream `json:"streams"`
}

// ardLive marks a live relay and says how far back its window reaches.
type ardLive struct {
	DVRWindowSeconds int `json:"dvrWindowSeconds"`
}

// ardStream is one presentation of a video.
type ardStream struct {
	Media []ardMedium `json:"media"`
}

// ardMedium is one rendition: one container, one picture size, one soundtrack.
type ardMedium struct {
	URL      string `json:"url"`
	MimeType string `json:"mimeType"`
	// MaxVResolutionPx is the picture height.
	MaxVResolutionPx int `json:"maxVResolutionPx"`
	// Audios names the soundtrack this rendition carries. It is the only
	// thing separating the broadcast mix from an audio description of it.
	Audios []struct {
		Kind string `json:"kind"`
	} `json:"audios"`
}

// best picks the rendition to fetch out of every stream on offer.
func (m ardMedia) best(mime string) (ardMedium, bool) {
	var best ardMedium
	found := false
	for _, stream := range m.Streams {
		for _, medium := range stream.Media {
			if medium.URL == "" || !strings.EqualFold(medium.MimeType, mime) {
				continue
			}
			if !found || ardBetter(medium, best) {
				best, found = medium, true
			}
		}
	}
	return best, found
}

// ardBetter compares two renditions, soundtrack first.
//
// The order is the whole point. A 1080p audio description is a worse download
// than a 540p broadcast mix, because it is a different programme: somebody
// narrating the picture over the top of it. Resolution decides only between
// renditions carrying the same soundtrack.
func ardBetter(a, b ardMedium) bool {
	if x, y := ardAudioRank(a), ardAudioRank(b); x != y {
		return x > y
	}
	return a.MaxVResolutionPx > b.MaxVResolutionPx
}

// ardAudioRank orders the soundtracks one video is offered with. An unknown
// kind ranks below the ordinary mix and above the two that are known to be
// something else, on the grounds that a name nobody here has seen is far more
// likely to be another ordinary soundtrack than another accessibility track.
func ardAudioRank(m ardMedium) int {
	if len(m.Audios) == 0 {
		return 2
	}
	switch strings.ToLower(m.Audios[0].Kind) {
	case "standard":
		return 3
	case "speech-optimized":
		return 1 // "klare Sprache": the programme, remixed for intelligibility
	case "audio-description":
		return 0
	}
	return 2
}

// ardWidget is one listing: a programme's items, or one season of them.
type ardWidget struct {
	// Title is the programme's name on the flat listing and the season's
	// name ("Staffel 2") on a seasoned one.
	Title      string      `json:"title"`
	Teasers    []ardTeaser `json:"teasers"`
	Pagination struct {
		TotalElements int `json:"totalElements"`
	} `json:"pagination"`
}

// ardTeaser is one entry of a listing.
type ardTeaser struct {
	ShortTitle string `json:"shortTitle"`
	// CoreAssetType tells an episode from the clips and trailers filed
	// alongside it.
	CoreAssetType string `json:"coreAssetType"`
	Links         struct {
		Target struct {
			ID string `json:"id"`
		} `json:"target"`
	} `json:"links"`
	// Show describes the programme the entry belongs to, and is where the
	// season list is published — there is none at the top of the listing.
	Show struct {
		Title            string   `json:"title"`
		AvailableSeasons []string `json:"availableSeasons"`
	} `json:"show"`
}

// id is the content id to ask the item endpoint for. The teaser's own id
// names the row and is no use here.
func (t ardTeaser) id() string { return t.Links.Target.ID }

// ardShowTitle names the programme a listing belongs to.
func ardShowTitle(teasers []ardTeaser) string {
	for _, t := range teasers {
		if t.Show.Title != "" {
			return t.Show.Title
		}
	}
	return ""
}

// ardSeasons reads the season list, which the listing publishes on each entry
// rather than once at the top.
func ardSeasons(teasers []ardTeaser) []string {
	for _, t := range teasers {
		if len(t.Show.AvailableSeasons) > 0 {
			return t.Show.AvailableSeasons
		}
	}
	return nil
}

// ardRefs turns a listing into the episodes to fetch from it.
func ardRefs(teasers []ardTeaser, season string) []ardEpisodeRef {
	teasers = ardWithoutVariants(ardEpisodesOnly(teasers))

	var refs []ardEpisodeRef
	seen := make(map[string]bool, len(teasers))
	for _, t := range teasers {
		id := t.id()
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		refs = append(refs, ardEpisodeRef{id: id, name: t.ShortTitle, season: season})
	}
	return refs
}

// ardEpisodesOnly drops the trailers, bonus features and clip extracts a
// listing files beside the episodes — but only when there are episodes to
// keep. A magazine or a topic strand publishes nothing else, and a listing
// reduced to nothing would be reported as a programme with no content.
func ardEpisodesOnly(teasers []ardTeaser) []ardTeaser {
	var episodes []ardTeaser
	for _, t := range teasers {
		if strings.EqualFold(t.CoreAssetType, "EPISODE") {
			episodes = append(episodes, t)
		}
	}
	if len(episodes) == 0 {
		return teasers
	}
	return episodes
}

// ardWithoutVariants drops the accessibility copies a listing mixes in.
//
// An audio-described episode is not a programme of its own: it is the same
// content id with a further segment appended — "…_ganzeSendung" and
// "…_ganzeSendung/audiodeskription" — and the listing's ids are base64 of
// exactly those identifiers. So the relationship can be seen without knowing
// what the segment is called, which matters because the vocabulary is
// ARD's and open-ended: wherever the listing holds both, the shorter id is
// the programme and the longer one a copy of it. Filtering by a list of
// suffixes instead would silently stop working the day a new one appeared,
// and filtering by the "(Audiodeskription)" the title also carries would
// depend on editorial text.
//
// A listing holding only the copy keeps it. That is not a duplicate, it is
// all ARD is offering.
func ardWithoutVariants(teasers []ardTeaser) []ardTeaser {
	ids := make(map[string]bool, len(teasers))
	for _, t := range teasers {
		ids[ardContentID(t.id())] = true
	}

	out := make([]ardTeaser, 0, len(teasers))
	for _, t := range teasers {
		if !ardIsVariant(ardContentID(t.id()), ids) {
			out = append(out, t)
		}
	}
	return out
}

// ardIsVariant reports whether id extends one of the others by a path
// segment.
func ardIsVariant(id string, ids map[string]bool) bool {
	for i := strings.LastIndex(id, "/"); i > 0; i = strings.LastIndex(id[:i], "/") {
		if ids[id[:i]] {
			return true
		}
	}
	return false
}

// ardContentID decodes a listing id back to the identifier it stands for. An
// id that is not base64 of one is its own answer: it cannot be in a prefix
// relation with anything, which is all the caller asks.
func ardContentID(id string) string {
	decoded, err := util.DecodeBase64(id)
	if err != nil || !strings.HasPrefix(string(decoded), "crid://") {
		return id
	}
	return string(decoded)
}
