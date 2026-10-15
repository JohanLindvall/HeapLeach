// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// RadioCanada reads ICI Radio-Canada's videos and ICI OHdio's audio through
// the media API its players call (services.radio-canada.ca/media), which
// answers without a key: validation/v2 names a stream for an application
// code and a media id, "medianet" being the one the site's media use.
//
// A page's own media is named in its data layer, as "MedianetId:<id>" in
// its IdsCollection, beside the programme it belongs to (Emission). So any
// page of the site that plays something is read the same way: a news
// video, an OHdio episode. Other media on the page, recommendations, carry
// ids too, which is why the data layer and not the first id is read. An
// OHdio programme's page lists its episodes, each with its media id, fifty
// a page; the first page is what is read, and the note says when there are
// more.
//
// The stream is HLS: TS video with its audio in a rendition of its own, or
// for OHdio TS audio with a transcript as WebVTT, nothing encrypted. The
// link the API hands back asks the CDN to leave out the renditions above
// 1.5 Mbps (aws.manifestfilter); asked without that, the same CDN serves
// the whole ladder, so the filter is taken off. What Radio-Canada shows in
// Canada alone, the API refuses to name a stream for, saying so in French,
// which is passed on.
//
// Any other page of the site, an article without a player of its own, is
// still handed to yt-dlp.
type RadioCanada struct {
	hostSet
	client *httpx.Client
	// api is the media API's base, a field so that tests can stand in for
	// it.
	api     string
	handoff *Handoff
}

const radioCanadaAPIRoot = "https://services.radio-canada.ca/media/"

// radioCanadaRest is the part of the site still handed to yt-dlp.
var radioCanadaRest = handoffSite{
	name:    "radio-canada",
	domains: []string{"radio-canada.ca", "ici.radio-canada.ca"},
	why:     "only Radio-Canada's videos and OHdio's audio are read natively, not a page like this one",
}

// NewRadioCanada builds the Radio-Canada extractor.
func NewRadioCanada(client *httpx.Client) *RadioCanada {
	return &RadioCanada{
		hostSet: radioCanadaRest.domains, client: client, api: radioCanadaAPIRoot,
		handoff: &Handoff{hostSet: radioCanadaRest.domains, client: client, site: radioCanadaRest},
	}
}

func (r *RadioCanada) Name() string { return "radio-canada" }

var (
	radioCanadaConsole   = regexp.MustCompile(`/widgets/mediaconsole/([a-z0-9]+)/([0-9]+)`)
	radioCanadaMediaPath = regexp.MustCompile(`/media-([0-9]+)(?:/|$)`)
	radioCanadaOwnMedia  = regexp.MustCompile(`MedianetId:([0-9]+)\|`)
	radioCanadaEpisodeID = regexp.MustCompile(`\|EpisodeId:([0-9]+)\|`)
	radioCanadaArticle   = regexp.MustCompile(`\|NewstoryId:[0-9-]+\|`)
	radioCanadaAttached  = regexp.MustCompile(`"media":\{"__typename":"(?:Video|Audio)","appCode":"([a-z]+)"[^{}]*?"id":([0-9]+)`)
	radioCanadaEmission  = regexp.MustCompile(`"Emission":"((?:[^"\\]|\\.)*)"`)
	radioCanadaEpisode   = regexp.MustCompile(`\{"typename":"EpisodeListItem"(?s:.*?)"mediaIds":\[([0-9]+)[^\]]*\](?s:.*?)"title":"((?:[^"\\]|\\.)*)"`)
	radioCanadaPages     = regexp.MustCompile(`"pagination":\{"pageNumber":[0-9]+,"pageSize":[0-9]+,"totalCount":([0-9]+)\}`)
	radioCanadaTitle     = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// Extract reads the media a page plays, or an OHdio programme's episodes,
// and hands anything else to yt-dlp.
func (r *RadioCanada) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	// The older links name the application and the media outright.
	if m := radioCanadaConsole.FindStringSubmatch(u.Path); m != nil {
		return r.single(ctx, m[1], m[2], "")
	}
	if m := radioCanadaMediaPath.FindStringSubmatch(u.Path); m != nil {
		return r.single(ctx, "medianet", m[1], "")
	}
	page, final, err := r.client.GetPage(ctx, u.String(), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("radio-canada: %s: %w", u.Redacted(), err)
	}
	title := ""
	if m := radioCanadaTitle.FindStringSubmatch(page); m != nil {
		title = util.TrimTitleSuffix(util.Unescape(m[1]), " | ")
	}
	if id := radioCanadaPageMedia(page); id != "" {
		var emission string
		if e := radioCanadaEmission.FindStringSubmatch(page); e != nil && strings.Contains(u.Path, "/ohdio/") {
			emission = radioCanadaText(e[1])
		}
		return r.single(ctx, "medianet", id, ardJoin(emission, title))
	}
	if radioCanadaEpisode.MatchString(page) {
		return r.programme(ctx, final, title, page, opts)
	}
	if radioCanadaArticle.MatchString(page) {
		if res, err := r.article(ctx, title, page); res != nil || err != nil {
			return res, err
		}
	}
	return r.handoff.Extract(ctx, u, opts)
}

// radioCanadaPageMedia is the media a page names as its own in its data
// layer: a video's or a podcast episode's MedianetId, or for a daily show's
// episode, which names its EpisodeId alone, the media its player's item
// for that episode plays.
func radioCanadaPageMedia(page string) string {
	if m := radioCanadaOwnMedia.FindStringSubmatch(page); m != nil {
		return m[1]
	}
	if m := radioCanadaEpisodeID.FindStringSubmatch(page); m != nil {
		item := regexp.MustCompile(`"playlistItemId":\{"globalId2":\{"contentType":\{[^{}]*\},"id":"` + m[1] + `"\}[^{}]*?"mediaId":"([0-9]+)"`)
		if m := item.FindStringSubmatch(page); m != nil {
			return m[1]
		}
	}
	return ""
}

// article reads the media an article's attachments play: one is named
// after the article, several each by the media API, under the article's
// title. An article attaching none is nil, for yt-dlp.
func (r *RadioCanada) article(ctx context.Context, title, page string) (*Result, error) {
	type attached struct{ app, id string }
	var media []attached
	seen := make(map[string]bool)
	for _, m := range radioCanadaAttached.FindAllStringSubmatch(page, -1) {
		if !seen[m[2]] {
			seen[m[2]] = true
			media = append(media, attached{app: m[1], id: m[2]})
		}
	}
	switch len(media) {
	case 0:
		return nil, nil
	case 1:
		return r.single(ctx, media[0].app, media[0].id, title)
	}
	type found struct {
		file   *File
		reason string
	}
	results := FanOut(ctx, media, func(ctx context.Context, a attached) ([]found, error) {
		target, err := r.stream(ctx, a.app, a.id, "")
		if refused, ok := errors.AsType[*radioCanadaRefused](err); ok {
			return []found{{reason: refused.reason}}, nil
		}
		if err != nil {
			return nil, err
		}
		file := r.file(a.app, a.id, target)
		return []found{{file: &file}}, nil
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res := &Result{Title: cmp.Or(title, "Radio-Canada")}
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
			return nil, fmt.Errorf("radio-canada: %s: none of its %d media can be had: %s", res.Title, len(media), reasons[0])
		}
		return nil, fmt.Errorf("radio-canada: %s: none of its media could be read", res.Title)
	}
	if len(reasons) > 0 {
		res.Note = fmt.Sprintf("%d left out: %s", len(reasons), reasons[0])
	}
	return res, nil
}

// radioCanadaText is a string written into the page's data, its escapes
// and entities read.
func radioCanadaText(s string) string {
	if unquoted, err := strconv.Unquote(`"` + s + `"`); err == nil {
		s = unquoted
	}
	return strings.TrimSpace(strings.ReplaceAll(util.Unescape(s), " ", " "))
}

// single is one media, named stem, or by the API where the page did not.
func (r *RadioCanada) single(ctx context.Context, app, id, stem string) (*Result, error) {
	target, err := r.stream(ctx, app, id, stem)
	if err != nil {
		return nil, err
	}
	file := r.file(app, id, target)
	return &Result{Title: strings.TrimSuffix(file.Name, path.Ext(file.Name)), Files: []File{file}}, nil
}

// file is a resolved media as its file, read again when its transfer
// starts: some media are signed, their master for two minutes and their
// segments for a day, which a queue can outwait.
func (r *RadioCanada) file(app, id string, target *Target) File {
	stem := strings.TrimSuffix(target.Name, path.Ext(target.Name))
	return File{
		Name: target.Name, URL: target.URL, Size: -1, Headers: target.Headers, Segments: target.Segments,
		SegmentKey: target.SegmentKey, Audio: target.Audio, Subtitles: target.Subtitles,
		Resolve: func(ctx context.Context) (*Target, error) {
			return r.stream(ctx, app, id, stem)
		},
	}
}

// radioCanadaRefused is a media the API names no stream for.
type radioCanadaRefused struct{ title, reason string }

func (e *radioCanadaRefused) Error() string { return "radio-canada: " + e.title + ": " + e.reason }

// stream asks the API for a media's stream and resolves it under the name
// stem, or under the programme and title the API gives it.
func (r *RadioCanada) stream(ctx context.Context, app, id, stem string) (*Target, error) {
	query := url.Values{
		"appCode": {app}, "idMedia": {id}, "connectionType": {"hd"}, "deviceType": {"ipad"},
		"multibitrate": {"true"}, "output": {"json"},
	}
	var answer struct {
		URL     string `json:"url"`
		Message string `json:"message"`
	}
	if err := r.client.GetJSON(ctx, r.api+"validation/v2/?"+query.Encode(), radioCanadaHeaders(), &answer); err != nil {
		return nil, fmt.Errorf("radio-canada: %s: %w", cmp.Or(stem, id), err)
	}
	if stem == "" {
		stem = r.name(ctx, app, id)
	}
	if answer.URL == "" {
		return nil, &radioCanadaRefused{title: stem, reason: cmp.Or(strings.TrimSpace(answer.Message), "the media API names no stream for it")}
	}
	headers := radioCanadaHeaders()
	master := radioCanadaUnfiltered(answer.URL)
	media, err := resolveMediaPlaylist(ctx, r.client, master, headers)
	if err != nil && master != answer.URL {
		// The whole ladder is the CDN's to refuse; the API's own link,
		// filter and all, is what the site plays.
		master = answer.URL
		media, err = resolveMediaPlaylist(ctx, r.client, master, headers)
	}
	if err != nil {
		return nil, fmt.Errorf("radio-canada: %s: %w", stem, err)
	}
	audio := media.Audio
	for i := range audio {
		// "audio1_fr" is the packager's, not a name, and the language says
		// it; "audio0_frdv" is the audio description (vidéodescription).
		if m := radioCanadaPackagerName.FindStringSubmatch(audio[i].Name); m != nil {
			audio[i].Name = ""
			if strings.HasSuffix(m[1], "dv") {
				audio[i].Name = "Vidéodescription"
			}
		}
	}
	return &Target{
		URL: master, Name: stem + media.extension(), Size: -1, Headers: headers,
		Segments: media.Segments, SegmentKey: media.Key, Audio: audio, Subtitles: media.Subtitles,
	}, nil
}

var radioCanadaPackagerName = regexp.MustCompile(`^audio[0-9]+_([a-z]+)$`)

// name is what the API calls a media: its programme and its title, or its
// id where it says nothing.
func (r *RadioCanada) name(ctx context.Context, app, id string) string {
	query := url.Values{"appCode": {app}, "idMedia": {id}, "output": {"json"}}
	var doc struct {
		Metas []struct {
			Name string `json:"name"`
			Text string `json:"text"`
		} `json:"Metas"`
	}
	if err := r.client.GetJSON(ctx, r.api+"meta/v1/index.ashx?"+query.Encode(), radioCanadaHeaders(), &doc); err != nil {
		return id
	}
	metas := make(map[string]string, len(doc.Metas))
	for _, m := range doc.Metas {
		metas[m.Name] = strings.TrimSpace(m.Text)
	}
	return cmp.Or(ardJoin(metas["Emission"], metas["Title"]), id)
}

// radioCanadaUnfiltered is a master's link without the filter the API puts
// on it, which leaves out every rendition above 1.5 Mbps.
func radioCanadaUnfiltered(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	q := u.Query()
	if !q.Has("aws.manifestfilter") {
		return link
	}
	q.Del("aws.manifestfilter")
	u.RawQuery = q.Encode()
	return u.String()
}

// radioCanadaHeaders are what the site's own player sends.
func radioCanadaHeaders() httpx.Header {
	return httpx.RefererOrigin("https://ici.radio-canada.ca/", "https://ici.radio-canada.ca")
}

// radioCanadaMaxPages is the most pages of a list the site's own loader asks
// for.
const radioCanadaMaxPages = 200

var (
	radioCanadaSeasonPath = regexp.MustCompile(`/saison-([0-9]+)$`)
	radioCanadaSeasons    = regexp.MustCompile(`"numberOfSeasons":([0-9]+)`)
	radioCanadaPageSize   = regexp.MustCompile(`"pagination":\{"pageNumber":[0-9]+,"pageSize":([0-9]+)`)
	radioCanadaSeasonName = regexp.MustCompile(`"selectedSeason":\{"fullName":"((?:[^"\\]|\\.)*)","name":"((?:[^"\\]|\\.)*)"`)
)

// programme reads an OHdio programme's episodes into one file an episode,
// each resolved when its transfer starts: every page of its list
// (?pageNumber=N, fifty a page, as the site's own loader asks), of every
// season its page links to (/saison-N), each season in a folder of its own.
// The programme's page shows its latest season alone, and a season's link
// that season alone. OHdio's audio is MPEG-TS, and an episode is listed
// under the name it resolves to, so that a listing read again finds what
// was saved under it.
func (r *RadioCanada) programme(ctx context.Context, u *url.URL, title, page string, opts Options) (*Result, error) {
	title = cmp.Or(title, "OHdio")
	base := strings.TrimSuffix(u.Path, "/")
	var seasons []int // none for a programme without seasons
	pastedSeason := false
	if m := radioCanadaSeasonPath.FindStringSubmatch(base); m != nil {
		n, _ := strconv.Atoi(m[1])
		seasons, base, pastedSeason = []int{n}, strings.TrimSuffix(base, m[0]), true
	} else {
		seasons = radioCanadaSeasonsOf(page, base)
	}

	limit := opts.maxFiles()
	var (
		files       []File
		listed      int
		walkErr     error
		seen        = make(map[string]bool)
		seasonTitle string
	)
	read := func(season int) {
		path := base
		if season > 0 {
			path += "/saison-" + strconv.Itoa(season)
		}
		folder := ""
		for number := 1; number <= radioCanadaMaxPages; number++ {
			doc := page
			if season > 0 || number > 1 || u.RawQuery != "" {
				next := *u
				next.Path, next.RawQuery = path, "pageNumber="+strconv.Itoa(number)
				var err error
				if doc, err = r.client.GetString(ctx, next.String(), nil); err != nil {
					walkErr = cmp.Or(walkErr, err)
					return
				}
			}
			if number == 1 && season > 0 {
				label := "Saison " + strconv.Itoa(season)
				if m := radioCanadaSeasonName.FindStringSubmatch(doc); m != nil {
					label = cmp.Or(ardJoin(radioCanadaText(m[2]), radioCanadaText(m[1])), label)
				}
				switch {
				case pastedSeason:
					// A season pasted alone is filed under its own name.
					seasonTitle = ardJoin(title, label)
				case len(seasons) > 1:
					folder = sourceFolder(label)
				}
			}
			added := 0
			for _, e := range radioCanadaEpisode.FindAllStringSubmatch(doc, -1) {
				id := e[1]
				if seen[id] {
					continue
				}
				seen[id] = true
				added++
				listed++
				if len(files) < limit {
					files = append(files, r.lazy(id, cmp.Or(ardJoin(title, radioCanadaText(e[2])), id), folder))
				}
			}
			m := radioCanadaPages.FindStringSubmatch(doc)
			if added == 0 || m == nil {
				return
			}
			if total, err := strconv.Atoi(m[1]); err != nil || number*radioCanadaPageSizeOf(doc) >= total {
				return
			}
		}
	}
	if len(seasons) == 0 {
		read(0)
	}
	for _, season := range seasons {
		read(season)
	}
	if len(files) == 0 {
		if walkErr != nil {
			return nil, fmt.Errorf("radio-canada: %s: %w", title, walkErr)
		}
		return nil, errors.New("radio-canada: " + title + " lists no episodes")
	}
	var notes []string
	if listed > len(files) {
		notes = append(notes, fmt.Sprintf("%d of %d episodes", len(files), listed))
	}
	if walkErr != nil {
		notes = append(notes, "the listing broke off partway")
	}
	return &Result{Title: cmp.Or(seasonTitle, title), Files: files, Note: strings.Join(notes, "; ")}, nil
}

// radioCanadaSeasonsOf is the seasons a programme's page links to, in
// order: its season selector's links, and as many as its structured data
// says it has, where the selector names fewer.
func radioCanadaSeasonsOf(page, base string) []int {
	found := make(map[int]bool)
	for _, m := range regexp.MustCompile(regexp.QuoteMeta(base)+`/saison-([0-9]+)`).FindAllStringSubmatch(page, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			found[n] = true
		}
	}
	if len(found) == 0 {
		return nil
	}
	if m := radioCanadaSeasons.FindStringSubmatch(page); m != nil {
		if count, err := strconv.Atoi(m[1]); err == nil && count > len(found) && count <= radioCanadaMaxPages {
			for n := 1; n <= count; n++ {
				found[n] = true
			}
		}
	}
	seasons := make([]int, 0, len(found))
	for n := range found {
		seasons = append(seasons, n)
	}
	slices.Sort(seasons)
	return seasons
}

// radioCanadaPageSizeOf is how many episodes a page of a list holds.
func radioCanadaPageSizeOf(doc string) int {
	if m := radioCanadaPageSize.FindStringSubmatch(doc); m != nil {
		if size, err := strconv.Atoi(m[1]); err == nil && size > 0 {
			return size
		}
	}
	return 50
}

// lazy is a listed episode, its stream resolved when its transfer starts.
func (r *RadioCanada) lazy(id, stem, folder string) File {
	return File{
		Name: stem + ".ts", Size: -1, Dir: folder, Headers: radioCanadaHeaders(),
		Resolve: func(ctx context.Context) (*Target, error) {
			return r.stream(ctx, "medianet", id, stem)
		},
	}
}
