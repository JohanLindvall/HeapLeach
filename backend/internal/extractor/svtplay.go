package extractor

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// SVTPlay resolves svtplay.se videos and series.
//
// SVT publishes an open player API, so no scraping trickery is needed to
// find a stream — but everything it offers is an adaptive manifest rather
// than a file. The extractor picks a rendition that carries audio and video
// together and hands back its segment list, which concatenates into a
// playable file without needing to be muxed.
type SVTPlay struct {
	client *httpx.Client
}

const (
	svtRoot        = "https://www.svtplay.se"
	svtVideoAPI    = "https://api.svt.se/videoplayer-api/video/"
	svtGraphQL     = "https://api.svt.se/contento/graphql?ua=svtplaywebb-play-render-prod-client"
	svtMaxEpisodes = 500
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
        items { item { __typename ... on Episode { name videoSvtId positionInSeason } } }
      }
    }
  }
}`

// svtVideoLink finds episode ids embedded in a series page.
var svtVideoLink = regexp.MustCompile(`/video/([A-Za-z0-9_-]{4,})[/"]`)

// NewSVTPlay builds the SVT Play extractor.
func NewSVTPlay(client *httpx.Client) *SVTPlay { return &SVTPlay{client: client} }

func (s *SVTPlay) Name() string { return "svtplay" }

func (s *SVTPlay) Match(u *url.URL) bool { return util.HostMatches(u.Host, "svtplay.se") }

// Extract handles a single video (/video/<id>/...) or a series page, which
// expands to every episode listed on it.
func (s *SVTPlay) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	segs := util.PathSegments(u)
	if len(segs) >= 2 && segs[0] == "video" {
		file, program, episode, err := s.episode(ctx, segs[1])
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
	if len(episodes) > svtMaxEpisodes {
		episodes = episodes[:svtMaxEpisodes]
	}

	// Only nest by season when there is more than one to tell apart.
	distinct := make(map[string]bool)
	for _, ep := range episodes {
		if ep.season != "" {
			distinct[ep.season] = true
		}
	}
	nest := len(distinct) > 1

	result := &Result{Title: util.FirstNonEmpty(title, slug)}
	for _, ep := range episodes {
		file, program, episodeTitle, err := s.episode(ctx, ep.videoID)
		if err != nil {
			// One expired or geo-blocked episode should not sink the rest.
			continue
		}
		if result.Title == slug && program != "" {
			result.Title = program
		}
		file.Name = util.FirstNonEmpty(ep.name, episodeTitle, program, ep.videoID) + file.Name
		if nest {
			file.Dir = ep.season
		}
		result.Files = append(result.Files, *file)
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("svtplay: none of the %d episodes of %s are playable "+
			"(they may be outside their availability window, or geo-blocked)", len(episodes), u.Redacted())
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
							TypeName         string `json:"__typename"`
							Name             string `json:"name"`
							VideoSvtID       string `json:"videoSvtId"`
							PositionInSeason string `json:"positionInSeason"`
						} `json:"item"`
					} `json:"items"`
				} `json:"associatedContent"`
			} `json:"listablesBySlug"`
		} `json:"data"`
	}
	if err := s.client.PostJSON(ctx, svtGraphQL, httpx.Referer(svtRoot+"/"), body, &out); err != nil {
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
	doc, err := s.client.GetString(ctx, u.String(), httpx.Referer(svtRoot+"/"))
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
	if err := s.client.GetJSON(ctx, svtVideoAPI+url.PathEscape(id),
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
	if manifest == "" {
		return nil, "", "", fmt.Errorf("svtplay: %s offers no usable stream", id)
	}

	headers := httpx.Referer(svtRoot + "/")
	segments, variant, err := resolvePlaylist(ctx, s.client, manifest, headers)
	if err != nil {
		return nil, "", "", fmt.Errorf("svtplay: %s: %w", id, err)
	}

	return &File{
		Name:     playlistExtension(variant),
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}, meta.ProgramTitle, meta.EpisodeTitle, nil
}

// playlistExtension names the container the joined segments will be in.
func playlistExtension(v hlsVariant) string {
	if strings.Contains(strings.ToLower(v.URL), "cmaf") || strings.Contains(v.URL, ".mp4") {
		return ".mp4"
	}
	return ".ts"
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
