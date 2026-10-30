// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// SVTPlay resolves svtplay.se videos and series.
//
// SVT publishes an open player API, so no scraping trickery is needed to
// find a stream — but everything it offers is an adaptive manifest rather
// than a file. The extractor picks a rendition that carries audio and video
// together and hands back its segment list, which concatenates into a
// playable file without needing to be muxed. Newer videos come as CMAF and
// DASH alone, every rendition keeping its audio apart: those come with each
// audio rendition as a track of its own, which the downloader joins to the
// video. They used to fail, saying to use the external downloader.
type SVTPlay struct {
	client *httpx.Client
	// api is the player API's base, and graphql SVT Play's own API, fields
	// so that tests can stand in for them.
	api     string
	graphql string
}

const (
	svtRoot     = "https://www.svtplay.se"
	svtVideoAPI = "https://api.svt.se/videoplayer-api/video/"
	svtGraphQL  = "https://api.svt.se/contento/graphql?ua=svtplaywebb-play-render-prod-client"
)

// svtSeasonsQuery asks for a programme's episodes grouped by season. SVT
// renders its series pages client-side, so the delivered HTML carries no
// episode list worth trusting: it mixes in trailers and recommendations
// from elsewhere on the page.
const svtSeasonsQuery = `query($slug:String!){
  listablesBySlug(slugs:[$slug]){
    __typename name slug
    ... on TvSeries {
      associatedContent(include:[season]){
        id name type
        items { item { __typename ... on Episode { name videoSvtId } } }
      }
    }
  }
}`

// svtAvailabilityQuery asks SVT Play's own API what its player API does not
// say when it offers a video no stream at all: the window the video can be
// watched in, and whether it is shown in Sweden alone. A video page asks
// it of its path, and /video/<id> is enough of one.
const svtAvailabilityQuery = `query($path:String!){
  detailsPageByPath(path:$path){
    item{ __typename
      ... on Single{ name validFrom validTo restrictions{ onlyAvailableInSweden } }
      ... on Episode{ name validFrom validTo restrictions{ onlyAvailableInSweden } }
    }
  }
}`

// errSVTNoStream is a video the player API offers no stream for at all,
// which it answers without saying why (svtVideo.HasVodReferences false).
var errSVTNoStream = errors.New("offers no stream")

// svtVideoLink finds episode ids embedded in a series page.
var svtVideoLink = regexp.MustCompile(`/video/([A-Za-z0-9_-]{4,})[/"]`)

// NewSVTPlay builds the SVT Play extractor.
func NewSVTPlay(client *httpx.Client) *SVTPlay {
	return &SVTPlay{client: client, api: svtVideoAPI, graphql: svtGraphQL}
}

func (s *SVTPlay) Name() string { return "svtplay" }

// svtShape is a video link: /video/<id>[/<programme>[/<episode>]].
var svtShape = pathShape(`^/video/[A-Za-z0-9_-]{4,}(?:/[^/]+){0,2}/?$`)

// svtSeriesShape is a programme's own page, /<slug>: too common a form for
// the link scanner to single out, but one a marked page may claim.
var svtSeriesShape = pathShape(`^/[a-z0-9][a-z0-9-]*/?$`)

// Recognises is the inventory's description.
func (s *SVTPlay) Recognises() string {
	return "videos (`/video/<id>`) and programme pages of SVT Play whose page data names the player API's `videoSvtId`s"
}

// svtIDLetter and svtIDDigit tell an SVT video id, which mixes the two,
// from the words and the bare numbers other sites put after /video/.
var (
	svtIDLetter = regexp.MustCompile(`[A-Za-z]`)
	svtIDDigit  = regexp.MustCompile(`[0-9]`)
)

// Shape reports a video link whose id has SVT's mixed form. Only the link
// scanner needs it that narrow: a probed page says for itself.
func (s *SVTPlay) Shape(u *url.URL) bool {
	if !svtShape(u) {
		return false
	}
	id := util.PathSegments(u)[1]
	return svtIDLetter.MatchString(id) && svtIDDigit.MatchString(id)
}

// Recognise takes a video or programme page whose page data names the
// player API's ids (videoSvtId), which is what everything here resolves.
func (s *SVTPlay) Recognise(_ context.Context, p *Page) bool {
	return (svtShape(p.Final) || svtSeriesShape(p.Final)) && p.Has("videoSvtId") && p.Named("SVT Play")
}

// Extract handles a single video (/video/<id>/...) or a series page, which
// expands to every episode listed on it.
//
// SVT links an episode as /video/<id>/<programme>/<episode>, but a
// programme's own card, on its page and in every list of recommendations, as
// /video/<id>/<programme>: the episode it would start playing, named by the
// programme alone. That link means the programme, so a series' slug there
// expands like its page does. A film is listed with no seasons, and stays
// the one video.
func (s *SVTPlay) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) == 3 && segs[0] == "video" {
		if title, episodes, err := s.seasons(ctx, segs[2]); err == nil && len(episodes) > 0 {
			return s.expand(ctx, u, segs[2], title, episodes)
		}
	}
	if len(segs) >= 2 && segs[0] == "video" {
		file, program, episode, err := s.episode(ctx, segs[1])
		if errors.Is(err, errSVTNoStream) {
			return nil, s.unavailable(ctx, segs[1], time.Now())
		}
		if err != nil {
			return nil, err
		}
		title := util.FirstNonEmpty(episode, program, segs[1])
		file.Name = title + file.Name // Name holds only the extension here
		return &Result{Title: title, Files: []File{*file}}, nil
	}
	return s.series(ctx, u)
}

// svtEpisodeRef is one episode of a series, with the season it belongs to.
type svtEpisodeRef struct {
	videoID string
	name    string
	season  string
}

// series expands a programme into one file per episode, filed by season
// when the programme has more than one.
func (s *SVTPlay) series(ctx context.Context, u *url.URL) (*Result, error) {
	slug := strings.Trim(u.Path, "/")

	title, episodes, err := s.seasons(ctx, slug)
	if err != nil || len(episodes) == 0 {
		// Fall back to reading ids off the page, which still works for the
		// odd programme the seasons query does not cover.
		title, episodes, err = s.episodesFromPage(ctx, u)
		if err != nil {
			return nil, err
		}
	}
	return s.expand(ctx, u, slug, title, episodes)
}

// expand resolves a programme's episodes into one file each, filed by season
// when the programme has more than one. Without a title, the job is named
// for the programme as the player API gives it, or failing that its slug.
func (s *SVTPlay) expand(ctx context.Context, u *url.URL, slug, title string, episodes []svtEpisodeRef) (*Result, error) {
	// svtListed is an episode with the programme its video names.
	type svtListed struct {
		svtEpisodeRef
		program string
	}
	items := make([]*svtListed, len(episodes))
	for i, ep := range episodes {
		items[i] = &svtListed{svtEpisodeRef: ep}
	}

	// Every episode is its own API call, and a series runs to hundreds;
	// fetched one after another the job would sit resolving for minutes
	// before a byte moved. An expired or geo-blocked episode is dropped
	// rather than sinking the rest.
	resolved, note, err := expandSeries(ctx, series[*svtListed]{
		site: "svtplay", noun: "episodes", link: u.Redacted(),
		items: items,
		label: func(ep *svtListed) string { return ep.season },
		resolve: func(ctx context.Context, ep *svtListed) (*File, error) {
			file, program, episodeTitle, err := s.episode(ctx, ep.videoID)
			if err != nil {
				return nil, err
			}
			ep.program = program
			file.Name = util.FirstNonEmpty(ep.name, episodeTitle, program, ep.videoID) + file.Name
			return file, nil
		},
		unplayable: "they may be outside their availability window, or geo-blocked",
	})
	if err != nil {
		return nil, err
	}

	result := &Result{Title: util.FirstNonEmpty(title, slug), Files: listedFiles(resolved), Note: note}
	for _, r := range resolved {
		if result.Title == slug && r.item.program != "" {
			result.Title = r.item.program
		}
	}
	return result, nil
}

// seasons reads a programme's episodes, grouped by season, from the API.
func (s *SVTPlay) seasons(ctx context.Context, slug string) (string, []svtEpisodeRef, error) {
	body := map[string]any{
		"query":     svtSeasonsQuery,
		"variables": map[string]string{"slug": slug},
	}
	var out struct {
		Data struct {
			ListablesBySlug []struct {
				Name              string `json:"name"`
				AssociatedContent []struct {
					Name  string `json:"name"`
					Type  string `json:"type"`
					Items []struct {
						Item struct {
							TypeName   string `json:"__typename"`
							Name       string `json:"name"`
							VideoSvtID string `json:"videoSvtId"`
						} `json:"item"`
					} `json:"items"`
				} `json:"associatedContent"`
			} `json:"listablesBySlug"`
		} `json:"data"`
	}
	if err := s.client.PostJSON(ctx, s.graphql, httpx.Referer(svtRoot+"/"), body, &out); err != nil {
		return "", nil, fmt.Errorf("svtplay: seasons for %s: %w", slug, err)
	}
	if len(out.Data.ListablesBySlug) == 0 {
		return "", nil, fmt.Errorf("svtplay: %s is not a programme", slug)
	}

	listable := out.Data.ListablesBySlug[0]
	var episodes []svtEpisodeRef
	seen := make(map[string]bool)
	for _, group := range listable.AssociatedContent {
		if !strings.EqualFold(group.Type, "Season") {
			continue
		}
		for _, entry := range group.Items {
			item := entry.Item
			if item.TypeName != "Episode" || item.VideoSvtID == "" || seen[item.VideoSvtID] {
				continue
			}
			seen[item.VideoSvtID] = true
			episodes = append(episodes, svtEpisodeRef{
				videoID: item.VideoSvtID,
				name:    item.Name,
				season:  group.Name,
			})
		}
	}
	return listable.Name, episodes, nil
}

// episodesFromPage recovers ids straight from the rendered page.
func (s *SVTPlay) episodesFromPage(ctx context.Context, u *url.URL) (string, []svtEpisodeRef, error) {
	doc, _, err := fetchDocument(ctx, s.client, u, httpx.Referer(svtRoot+"/"))
	if err != nil {
		return "", nil, fmt.Errorf("svtplay: fetch %s: %w", u.Redacted(), err)
	}
	var episodes []svtEpisodeRef
	seen := make(map[string]bool)
	for _, match := range svtVideoLink.FindAllStringSubmatch(doc, -1) {
		if id := match[1]; !seen[id] {
			seen[id] = true
			episodes = append(episodes, svtEpisodeRef{videoID: id})
		}
	}
	if len(episodes) == 0 {
		return "", nil, fmt.Errorf("svtplay: no episodes listed on %s", u.Redacted())
	}
	return "", episodes, nil
}

// episode resolves one video id to a playable segment list. The returned
// File carries only the extension in Name; the caller supplies the stem,
// which differs between a standalone video and one episode of a series.
func (s *SVTPlay) episode(ctx context.Context, id string) (file *File, program, episode string, err error) {
	var meta svtVideo
	if err := s.client.GetJSON(ctx, s.api+url.PathEscape(id),
		httpx.Referer(svtRoot+"/"), &meta); err != nil {
		return nil, "", "", fmt.Errorf("svtplay: video %s: %w", id, err)
	}
	if meta.Rights.DRMCopyProtection {
		return nil, "", "", fmt.Errorf("svtplay: %s is DRM protected", id)
	}
	if meta.Live {
		return nil, "", "", fmt.Errorf("svtplay: %s is a live stream", id)
	}

	manifest := meta.bestManifest()
	if manifest == "" && len(meta.VideoReferences) == 0 {
		return nil, "", "", fmt.Errorf("svtplay: %s %w", id, errSVTNoStream)
	}
	if manifest == "" {
		return nil, "", "", fmt.Errorf("svtplay: %s offers no usable stream (only %s)", id, meta.formats())
	}

	headers := httpx.Referer(svtRoot + "/")
	media, err := resolveMediaPlaylist(ctx, s.client, manifest, headers)
	if err != nil {
		return nil, "", "", fmt.Errorf("svtplay: %s: %w", id, err)
	}

	return new(media.file(media.extension(), headers)), meta.ProgramTitle, meta.EpisodeTitle, nil
}

// svtVideo is the part of the player API response we use.
type svtVideo struct {
	ProgramTitle string `json:"programTitle"`
	EpisodeTitle string `json:"episodeTitle"`
	Live         bool   `json:"live"`
	Rights       struct {
		DRMCopyProtection bool `json:"drmCopyProtection"`
	} `json:"rights"`
	VideoReferences []struct {
		Format string `json:"format"`
		URL    string `json:"url"`
	} `json:"videoReferences"`
}

// formats lists the kinds of stream the player API offers, for saying
// which it offered when none of them will do.
func (v svtVideo) formats() string {
	var kinds []string
	for _, ref := range v.VideoReferences {
		kinds = append(kinds, ref.Format)
	}
	return strings.Join(kinds, ", ")
}

// bestManifest prefers plain MPEG-TS HLS: its renditions carry audio and
// video together, so the segments join into something playable without any
// remuxing. Other HLS flavours are taken as a fallback.
func (v svtVideo) bestManifest() string {
	preference := []string{"hls-ts-full", "hls-ts-lb-full", "hls-ts", "hls-cmaf-full", "hls-cmaf-avc", "hls"}
	for _, want := range preference {
		for _, ref := range v.VideoReferences {
			if strings.EqualFold(ref.Format, want) && ref.URL != "" {
				return ref.URL
			}
		}
	}
	for _, ref := range v.VideoReferences {
		if strings.HasPrefix(strings.ToLower(ref.Format), "hls") && ref.URL != "" {
			return ref.URL
		}
	}
	return ""
}

// svtAvailability is what SVT Play's page knows of when and where a video
// can be watched.
type svtAvailability struct {
	Name         string `json:"name"`
	ValidFrom    string `json:"validFrom"`
	ValidTo      string `json:"validTo"`
	Restrictions struct {
		OnlyAvailableInSweden bool `json:"onlyAvailableInSweden"`
	} `json:"restrictions"`
}

// unavailable says why the player API offers a video no stream, as far as
// SVT Play's page knows: its window has closed or not yet opened, or it is
// shown in Sweden alone. The API answers such a video with an empty list
// and no reason, which said "offers no usable stream" of a programme that
// could be watched until two years before.
func (s *SVTPlay) unavailable(ctx context.Context, id string, now time.Time) error {
	body := map[string]any{"query": svtAvailabilityQuery, "variables": map[string]string{"path": "/video/" + id}}
	var out struct {
		Data struct {
			DetailsPageByPath *struct {
				Item *svtAvailability `json:"item"`
			} `json:"detailsPageByPath"`
		} `json:"data"`
	}
	err := s.client.PostJSON(ctx, s.graphql, httpx.Referer(svtRoot+"/"), body, &out)
	if err != nil || out.Data.DetailsPageByPath == nil || out.Data.DetailsPageByPath.Item == nil {
		return fmt.Errorf("svtplay: SVT offers no stream for %s (it may no longer be available, or not be yet)", id)
	}
	item := out.Data.DetailsPageByPath.Item
	name := util.FirstNonEmpty(item.Name, id)
	from, fromErr := time.Parse(time.RFC3339, item.ValidFrom)
	to, toErr := time.Parse(time.RFC3339, item.ValidTo)
	switch {
	case toErr == nil && now.After(to):
		return fmt.Errorf("svtplay: %s could be watched until %s and is no longer available", name, to.Format(time.DateOnly))
	case fromErr == nil && now.Before(from):
		return fmt.Errorf("svtplay: %s cannot be watched before %s", name, from.Format(time.DateOnly))
	case item.Restrictions.OnlyAvailableInSweden:
		return fmt.Errorf("svtplay: SVT offers no stream for %s here: it is shown in Sweden only", name)
	}
	return fmt.Errorf("svtplay: SVT offers no stream for %s, and gives no reason", name)
}
