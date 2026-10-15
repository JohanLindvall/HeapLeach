// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
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
	page, err := r.client.GetString(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("radio-canada: %s: %w", u.Redacted(), err)
	}
	title := ""
	if m := radioCanadaTitle.FindStringSubmatch(page); m != nil {
		title = util.TrimTitleSuffix(util.Unescape(m[1]), " | ")
	}
	if m := radioCanadaOwnMedia.FindStringSubmatch(page); m != nil {
		var emission string
		if e := radioCanadaEmission.FindStringSubmatch(page); e != nil && strings.Contains(u.Path, "/ohdio/") {
			emission = radioCanadaText(e[1])
		}
		return r.single(ctx, "medianet", m[1], ardJoin(emission, title))
	}
	if episodes := radioCanadaEpisode.FindAllStringSubmatch(page, -1); len(episodes) > 0 {
		return r.programme(title, page, episodes, opts)
	}
	return r.handoff.Extract(ctx, u, opts)
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
	return &Result{Title: strings.TrimSuffix(target.Name, ".mp4"), Files: []File{{
		Name: target.Name, URL: target.URL, Size: -1, Headers: target.Headers, Segments: target.Segments,
		SegmentKey: target.SegmentKey, Audio: target.Audio, Subtitles: target.Subtitles,
	}}}, nil
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
	master := radioCanadaUnfiltered(answer.URL)
	headers := radioCanadaHeaders()
	media, err := resolveMediaPlaylist(ctx, r.client, master, headers)
	if err != nil {
		return nil, fmt.Errorf("radio-canada: %s: %w", stem, err)
	}
	audio := media.Audio
	for i := range audio {
		// "audio1_fr" is the packager's, not a name; the language says it.
		if radioCanadaPackagerName.MatchString(audio[i].Name) {
			audio[i].Name = ""
		}
	}
	return &Target{
		URL: master, Name: stem + media.extension(), Size: -1, Headers: headers,
		Segments: media.Segments, SegmentKey: media.Key, Audio: audio, Subtitles: media.Subtitles,
	}, nil
}

var radioCanadaPackagerName = regexp.MustCompile(`^audio[0-9]+_[a-z]+$`)

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

// programme reads an OHdio programme's episodes, as its page lists them,
// into one file an episode, each resolved when its transfer starts. OHdio's
// audio is MPEG-TS, and an episode is listed under the name it resolves to,
// so that a listing read again finds what was saved under it.
func (r *RadioCanada) programme(title, page string, episodes [][]string, opts Options) (*Result, error) {
	title = cmp.Or(title, "OHdio")
	limit := opts.maxFiles()
	var files []File
	seen := make(map[string]bool)
	for _, e := range episodes {
		id, name := e[1], radioCanadaText(e[2])
		if seen[id] {
			continue
		}
		seen[id] = true
		if len(files) < limit {
			stem := cmp.Or(ardJoin(title, name), id)
			files = append(files, File{
				Name: stem + ".ts", Size: -1, Headers: radioCanadaHeaders(),
				Resolve: func(ctx context.Context) (*Target, error) {
					return r.stream(ctx, "medianet", id, stem)
				},
			})
		}
	}
	if len(files) == 0 {
		return nil, errors.New("radio-canada: " + title + " lists no episodes")
	}
	res := &Result{Title: title, Files: files}
	if m := radioCanadaPages.FindStringSubmatch(page); m != nil {
		if total, err := strconv.Atoi(m[1]); err == nil && total > len(files) {
			res.Note = fmt.Sprintf("%d of %d episodes", len(files), total)
		}
	} else if len(seen) > len(files) {
		res.Note = fmt.Sprintf("%d of %d episodes", len(files), len(seen))
	}
	return res, nil
}
