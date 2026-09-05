package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// VRTMax resolves the Flemish public broadcaster's catalogue, vrt.be/vrtmax.
//
// Four requests stand between a programme URL and a segment list and not one
// of them needs a cookie, an account or a signature, which is what makes this
// worth a hand-written extractor rather than a handoff to yt-dlp. The front
// door asks for exactly one thing: the GraphQL endpoint answers a request
// without `x-vrt-client-name: WEB` with the two words "Bad Request." and
// nothing else, whatever the query says, so that header goes on every call.
//
// The id that endpoint takes for a page is the page's own URL path, and that
// single fact is what makes a series cheap. An episode tile in a listing
// carries the path of the episode's page, so walking a programme hands back
// ids that can be asked for directly — there is no numbering scheme to learn
// and no mapping table to keep. vrtmax.be is the same catalogue under a
// shorter name, and its paths are the vrt.be ones with /vrtmax cut off the
// front, so that prefix is put back rather than the redirect followed.
//
// What makes the stream usable is that VRT's packager writes audio and video
// into the same segments. The master playlist declares an EXT-X-MEDIA AUDIO
// group with no URI of its own, which is HLS's way of saying the audio is
// already inside each variant, so hlsVariant.muxed() accepts and the joined
// segments play. That is a property of the packager rather than a promise, so
// it is checked at extraction time: a variant whose audio turned out to live
// in a playlist of its own is refused outright, because this program has
// nothing to mux with and joining one anyway yields a silent video that looks
// like a finished download. The locator is likewise named "_nodrm_" and the
// document beside it carries a drm field that is null — the name is not what
// is trusted, the field is, since a field that exists is a field that can one
// day be filled in.
//
// The rest of the catalogue's character is geography. Most of it is licensed
// for Belgium alone and the media service says so before any manifest exists,
// with HTTP 400 and a body of one field:
// {"code":"CONTENT_AVAILABLE_ONLY_FOR_BE_RESIDENTS_AND_EXPATS"}. That is a
// constant rather than prose written for a viewer — NRK's endUserMessage is
// the opposite case, and is passed through untouched for exactly that reason
// — so it is put into words here, with the code kept beside them so a reader
// can check the translation against what VRT actually said.
type VRTMax struct {
	client *httpx.Client

	// mu guards the player token, which every episode of a series shares.
	mu     sync.Mutex
	token  string
	expiry time.Time
}

const (
	vrtRoot     = "https://www.vrt.be"
	vrtGraphQL  = vrtRoot + "/vrtnu-api/graphql/public/v1"
	vrtMediaAPI = "https://media-services-public.vrt.be/vualto-video-aggregator-web/rest/external/v2/"

	vrtDomain    = "vrt.be"
	vrtMaxDomain = "vrtmax.be"

	// vrtSection is where the catalogue sits under vrt.be, and what a
	// vrtmax.be path is missing. vrtOldSection is the name it had before the
	// service was renamed from VRT NU; the API still answers to it, and the
	// links are still in circulation.
	vrtSection    = "/vrtmax"
	vrtOldSection = "/vrtnu"

	// vrtClient names the player the media service thinks it is serving.
	vrtClient = "vrtnu-web@PROD"

	// vrtClientHeader is the one thing the GraphQL endpoint insists on.
	vrtClientHeader = "x-vrt-client-name"
	vrtClientName   = "WEB"

	// vrtMaxEpisodes bounds how many episodes one programme URL expands to.
	// VRT's daily strands run to thousands: Thuis alone lists six thousand
	// across thirty-one seasons, which is a mistake being carried out
	// efficiently rather than a download.
	vrtMaxEpisodes = 500

	// vrtTokenLife is assumed when the token service does not say when its
	// token expires, and vrtTokenMargin is how long before that moment a
	// fresh one is minted instead.
	vrtTokenLife   = 15 * time.Minute
	vrtTokenMargin = time.Minute
)

// The GraphQL type names the walk below reads. They are string constants
// because __typename is the only thing distinguishing one component of a
// page from another: every fragment's fields land on the same object.
const (
	vrtEpisodePage   = "EpisodePage"
	vrtProgramPage   = "ProgramPage"
	vrtNavigation    = "ContainerNavigation"
	vrtPaginatedList = "PaginatedTileList"
	vrtLazyList      = "LazyTileList"
	vrtEpisodeTile   = "EpisodeTile"
)

// vrtTilesSelection is the shape of one tile list. A listing holds tiles of
// several kinds — programmes, podcasts, banners — and only an EpisodeTile
// names something playable, so the type is asked for rather than assumed.
const vrtTilesSelection = `items { __typename ... on EpisodeTile {
    title action { ... on LinkAction { link } }
  } }`

// vrtListsSelection covers both forms a tile list arrives in. Only the season
// the site itself displays comes with its tiles; the others are a LazyTileList
// holding an opaque id that has to be fetched separately.
const vrtListsSelection = `... on ` + vrtPaginatedList + ` { ` + vrtTilesSelection + ` }
      ... on ` + vrtLazyList + ` { listId }`

// vrtPageQuery asks what a page is and, in the same request, everything
// needed for either answer. A URL does not say which it is — an episode and a
// programme differ only by how many segments the path has, which is a
// convention rather than a contract — and asking __typename first would mean
// two round trips to learn something the one request already knows.
const vrtPageQuery = `query($id:ID!){
  page(id:$id){
    __typename
    ... on ` + vrtEpisodePage + ` { title player { title modes { streamId } } }
    ... on ` + vrtProgramPage + ` {
      title
      components { ... on ` + vrtNavigation + ` { items {
        title
        components {
          __typename
          ` + vrtListsSelection + `
          ... on ` + vrtNavigation + ` { items {
            title
            components { __typename ` + vrtListsSelection + ` }
          } }
        }
      } } }
    }
  }
}`

// vrtEpisodeQuery is the same question asked of an episode alone, which is
// what a series expansion repeats several hundred times.
const vrtEpisodeQuery = `query($id:ID!){
  page(id:$id){
    __typename
    ... on ` + vrtEpisodePage + ` { title player { title modes { streamId } } }
  }
}`

// vrtListQuery fetches a season the programme page named but did not inline.
const vrtListQuery = `query($id:ID!){
  list(listId:$id){ __typename ... on ` + vrtPaginatedList + ` { ` + vrtTilesSelection + ` } }
}`

// vrtAPIHeader is what every GraphQL call carries, and is only ever read.
var vrtAPIHeader = httpx.Header{
	vrtClientHeader:     vrtClientName,
	httpx.HeaderReferer: vrtRoot + "/",
	httpx.HeaderOrigin:  vrtRoot,
}

// vrtRefusals puts the media service's refusal codes into words. A code this
// does not know is reported as itself: it is still the only fact there is,
// and inventing a reason for it would be worse than quoting it.
var vrtRefusals = map[string]string{
	"CONTENT_AVAILABLE_ONLY_FOR_BE_RESIDENTS":            "VRT serves this inside Belgium only",
	"CONTENT_AVAILABLE_ONLY_FOR_BE_RESIDENTS_AND_EXPATS": "VRT serves this inside Belgium only, or to a signed-in Belgian abroad",
	"CONTENT_IS_AGE_RESTRICTED":                          "VRT serves this only to a signed-in account old enough for it",
}

// NewVRTMax builds the VRT MAX extractor.
func NewVRTMax(client *httpx.Client) *VRTMax { return &VRTMax{client: client} }

func (v *VRTMax) Name() string { return "vrtmax" }

// Match takes the VRT MAX catalogue and nothing else on vrt.be. The domain
// also carries the news site, the radio, the sports service and the
// broadcaster's own pages, none of which this resolves — and claiming them
// would leave the link harvester treating every navigation link on a VRT page
// as something to download.
func (v *VRTMax) Match(u *url.URL) bool {
	if util.HostMatches(u.Host, vrtMaxDomain) {
		return true
	}
	return util.HostMatches(u.Host, vrtDomain) && vrtPageID(u) != ""
}

// Extract handles a single episode or a whole programme.
func (v *VRTMax) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id := vrtPageID(u)
	if id == "" {
		return nil, fmt.Errorf("vrtmax: %s is not a VRT MAX page "+
			"(the catalogue lives under /vrtmax/)", u.Redacted())
	}

	resp, err := v.gql(ctx, vrtPageQuery, id)
	if err != nil {
		return nil, err
	}
	page := resp.Data.Page
	switch {
	case page == nil:
		return nil, fmt.Errorf("vrtmax: VRT MAX has no page at %s", id)
	case page.TypeName == vrtEpisodePage:
		return v.single(ctx, page, id)
	case page.TypeName == vrtProgramPage:
		return v.series(ctx, u, page, id)
	}
	return nil, fmt.Errorf("vrtmax: %s is a %s, which names neither a programme nor an episode",
		u.Redacted(), util.FirstNonEmpty(page.TypeName, "page of some other kind"))
}

// single resolves one episode page into a job of its own.
func (v *VRTMax) single(ctx context.Context, page *vrtPage, id string) (*Result, error) {
	file, programme, episode, err := v.play(ctx, page)
	if err != nil {
		return nil, err
	}
	title := vrtTitle(programme, util.FirstNonEmpty(episode, vrtSlug(id)))
	file.Name = title + file.Name
	return &Result{Title: title, Files: []File{*file}}, nil
}

// series expands a programme into one file per episode, filed by season when
// there is more than one season to tell apart.
func (v *VRTMax) series(ctx context.Context, u *url.URL, page *vrtPage, id string) (*Result, error) {
	episodes := vrtRefs(FanOut(ctx, vrtLists(page), v.fill))
	if len(episodes) == 0 {
		return nil, fmt.Errorf("vrtmax: %s lists no episodes "+
			"(a programme page can outlive the run of the programme on it)", u.Redacted())
	}
	// The newest season is listed first, so what a cap drops is the oldest
	// material and the extras that follow it rather than a slice from the
	// middle.
	if len(episodes) > vrtMaxEpisodes {
		episodes = episodes[:vrtMaxEpisodes]
	}

	// Only nest by season when there is more than one to tell apart.
	nest := nestByLabel(episodes, func(ep vrtEpisodeRef) string { return ep.season })

	resolved := FanOut(ctx, episodes, func(ctx context.Context, ep vrtEpisodeRef) ([]vrtOutcome, error) {
		file, _, episode, err := v.episode(ctx, ep.id)
		if err != nil {
			return []vrtOutcome{{err: err}}, nil
		}
		file.Name = util.FirstNonEmpty(ep.name, episode, vrtSlug(ep.id)) + file.Name
		if nest {
			file.Dir = ep.season
		}
		return []vrtOutcome{{file: file}}, nil
	})

	result := &Result{Title: util.FirstNonEmpty(page.Title, vrtSlug(id))}
	var refused, failed string
	for _, out := range resolved {
		refusal, refusedHere := errors.AsType[*vrtRefused](out.err)
		switch {
		case out.file != nil:
			result.Files = append(result.Files, *out.file)
		case refusedHere:
			// A whole season refused is a whole season refused for one
			// reason, and VRT's code for it is the only thing the caller can
			// act on. A refusal therefore outranks a transport failure.
			if refused == "" {
				refused = vrtReason(refusal.code)
			}
		case failed == "" && out.err != nil:
			failed = out.err.Error()
		}
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("vrtmax: none of the %d episodes of %s are playable: %s",
			len(episodes), u.Redacted(), util.FirstNonEmpty(refused, failed,
				"the listing named them but the player offered no stream"))
	}
	return result, nil
}

// vrtOutcome is what one episode came to: a file, or the reason there is
// none. FanOut drops an error outright and the reason is the whole point
// here, so it travels as a value rather than as a failure.
type vrtOutcome struct {
	file *File
	err  error
}

// vrtEpisodeRef is one episode of a programme, with the season it belongs to.
// The id is the episode page's own path, which is what the tile carried.
type vrtEpisodeRef struct {
	id     string
	name   string
	season string
}

// vrtList is one list of episodes found on a programme page, with the label
// its files are filed under. Exactly one of tiles and listID is set.
type vrtList struct {
	label  string
	listID string
	tiles  []vrtItem
}

// vrtLists walks a programme page for the lists of episodes on it.
//
// The page is a tab bar. Each tab holds either a tile list of its own or a
// second navigation whose items are the seasons. Which tab holds the episodes
// is not something to look for by name — VRT labels it "Afleveringen" on one
// programme and "Alle seizoenen" on the next, both Dutch prose that can
// change — so every tab is walked and the shape decides.
//
// Seasons are returned before the loose tabs, and that ordering is what makes
// the two later steps land the right way round. vrtRefs keeps the first copy
// of a repeated episode, so a "today's episode" tab is absorbed into the
// season that already listed it rather than filed beside it; and the episode
// cap, when it bites, takes the oldest seasons and the bloopers rather than
// this week's programmes.
func vrtLists(page *vrtPage) []vrtList {
	var seasons, extras []vrtList
	for _, component := range page.Components {
		for _, tab := range component.Items {
			for _, held := range tab.Components {
				if held.TypeName != vrtNavigation {
					extras = append(extras, vrtListOf(tab.Title, held)...)
					continue
				}
				for _, season := range held.Items {
					for _, list := range season.Components {
						seasons = append(seasons, vrtListOf(season.Title, list)...)
					}
				}
			}
		}
	}
	return append(seasons, extras...)
}

// vrtListOf recognises the two components that hold episodes, and nothing
// else: a tab can equally hold a block of text, a tag cloud or a link to a
// podcast on another service.
func vrtListOf(label string, c vrtComponent) []vrtList {
	switch c.TypeName {
	case vrtPaginatedList:
		return []vrtList{{label: vrtSeasonLabel(label), tiles: c.Items}}
	case vrtLazyList:
		return []vrtList{{label: vrtSeasonLabel(label), listID: c.ListID}}
	}
	return nil
}

// vrtSeasonLabel is the folder a list's episodes are filed under.
//
// VRT writes a season tab as "Seizoen 2026 (166 van 262 afl.)", and the count
// in brackets moves every time an episode is published or leaves the
// catalogue. Kept, it would file the same season under a different name on a
// later run and download the lot again beside the first copy, so the count
// goes and the name stays.
func vrtSeasonLabel(title string) string {
	if name, _, ok := strings.Cut(title, " ("); ok {
		return strings.TrimSpace(name)
	}
	return strings.TrimSpace(title)
}

// fill fetches a season the programme page named but did not inline.
func (v *VRTMax) fill(ctx context.Context, list vrtList) ([]vrtList, error) {
	if list.listID == "" {
		return []vrtList{list}, nil
	}
	resp, err := v.gql(ctx, vrtListQuery, list.listID)
	if err != nil {
		return nil, err
	}
	if resp.Data.List == nil {
		return nil, fmt.Errorf("vrtmax: season %q holds no list", list.label)
	}
	list.tiles = resp.Data.List.Items
	return []vrtList{list}, nil
}

// vrtRefs flattens the filled lists into episode references, keeping the
// order they were listed in and dropping both the tiles that name something
// other than an episode and anything an earlier list already claimed.
func vrtRefs(lists []vrtList) []vrtEpisodeRef {
	var out []vrtEpisodeRef
	seen := make(map[string]bool)
	for _, list := range lists {
		for _, tile := range list.tiles {
			link := strings.TrimSpace(tile.Action.Link)
			if tile.TypeName != vrtEpisodeTile || link == "" || seen[link] {
				continue
			}
			seen[link] = true
			out = append(out, vrtEpisodeRef{id: link, name: tile.Title, season: list.label})
		}
	}
	return out
}

// episode resolves one episode page id to a playable segment list.
func (v *VRTMax) episode(ctx context.Context, id string) (*File, string, string, error) {
	resp, err := v.gql(ctx, vrtEpisodeQuery, id)
	if err != nil {
		return nil, "", "", err
	}
	if resp.Data.Page == nil || resp.Data.Page.TypeName != vrtEpisodePage {
		return nil, "", "", fmt.Errorf("vrtmax: %s is not an episode page", id)
	}
	return v.play(ctx, resp.Data.Page)
}

// play turns an episode page into a playable segment list. The returned File
// carries only the extension in Name; the caller supplies the stem, which
// differs between a lone episode and one of a series.
func (v *VRTMax) play(ctx context.Context, page *vrtPage) (file *File, programme, episode string, err error) {
	streamID := page.streamID()
	if streamID == "" {
		return nil, "", "", errors.New("vrtmax: this page carries a player that names no stream")
	}

	media, err := v.media(ctx, streamID)
	if err != nil {
		return nil, "", "", err
	}
	if media.protected() {
		return nil, "", "", fmt.Errorf("vrtmax: %s is DRM protected", streamID)
	}
	manifest := media.hls()
	if manifest == "" {
		return nil, "", "", fmt.Errorf("vrtmax: %s offers no HLS playlist", streamID)
	}

	headers := httpx.Referer(vrtRoot + "/")
	segments, variant, err := resolvePlaylist(ctx, v.client, manifest, headers)
	if err != nil {
		return nil, "", "", fmt.Errorf("vrtmax: %s: %w", streamID, err)
	}
	if vrtSilent(variant) {
		return nil, "", "", fmt.Errorf("vrtmax: %s offers no rendition carrying its own audio, "+
			"so joining its segments would give a silent video; this host needs an external "+
			"downloader now that VRT packages it this way", streamID)
	}

	return &File{
		Name: playlistExtension(variant),
		// The media document publishes a duration, not a length: nothing here
		// knows how many bytes the parts add up to until they arrive, and a
		// Size that is not exact is worse than none at all.
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}, media.Title, util.FirstNonEmpty(page.Title, page.Player.Title), nil
}

// vrtSilent reports a variant whose audio lives in a playlist of its own,
// which is the one thing that would make this host unservable: the segments
// are joined by concatenation and there is no muxer on that path, so such a
// variant downloads to the end and then plays without sound.
//
// An empty codec list is not that case. It means the locator answered with a
// media playlist rather than a master, which says nothing either way and must
// not be read as a refusal.
func vrtSilent(v hlsVariant) bool { return v.Codecs != "" && !v.muxed() }

// media reads the document naming the streams for one stream id.
func (v *VRTMax) media(ctx context.Context, streamID string) (*vrtMedia, error) {
	token, err := v.playerToken(ctx)
	if err != nil {
		return nil, err
	}
	query := url.Values{"vrtPlayerToken": {token}, "client": {vrtClient}}
	endpoint := vrtMediaAPI + "videos/" + url.PathEscape(streamID) + "?" + query.Encode()

	var doc vrtMedia
	if err := v.client.GetJSON(ctx, endpoint, nil, &doc); err != nil {
		if code := vrtRefusalCode(err); code != "" {
			return nil, &vrtRefused{code: code}
		}
		return nil, fmt.Errorf("vrtmax: media for %s: %w", streamID, err)
	}
	return &doc, nil
}

// playerToken mints a token for the media service, or reuses the one already
// in hand.
//
// The token stands in for a session, is good for about a quarter of an hour,
// and is not tied to the item it is spent on — so one covers a whole series.
// It is cached with its expiry rather than minted once per job because the
// alternative fails in the case that matters: expanding several hundred
// episodes takes longer than a token lives, and a job that began with a valid
// one would start failing partway down the list.
func (v *VRTMax) playerToken(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token != "" && time.Now().Before(v.expiry) {
		return v.token, nil
	}

	var out struct {
		Token   string `json:"vrtPlayerToken"`
		Expires string `json:"expirationDate"`
	}
	if err := v.client.PostJSON(ctx, vrtMediaAPI+"tokens", nil, nil, &out); err != nil {
		return "", fmt.Errorf("vrtmax: player token: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("vrtmax: the token service answered without a token")
	}

	v.token = out.Token
	v.expiry = time.Now().Add(vrtTokenLife)
	if when, err := time.Parse(time.RFC3339, out.Expires); err == nil {
		v.expiry = when.Add(-vrtTokenMargin)
	}
	return v.token, nil
}

// gql posts one query and returns the decoded response.
//
// A GraphQL failure arrives as HTTP 200 with an errors array beside a data
// object full of nulls, so it has to be looked for: left alone it would
// surface much later as "this page holds no episodes".
func (v *VRTMax) gql(ctx context.Context, query, id string) (*vrtResponse, error) {
	body := map[string]any{"query": query, "variables": map[string]string{"id": id}}
	var out vrtResponse
	if err := v.client.PostJSON(ctx, vrtGraphQL, vrtAPIHeader, body, &out); err != nil {
		return nil, fmt.Errorf("vrtmax: query %s: %w", id, err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("vrtmax: query %s: %s", id, out.Errors[0].Message)
	}
	return &out, nil
}

// vrtPageID is the id the API takes for a URL, which is the URL's own path.
// It returns nothing for a path outside the catalogue.
func vrtPageID(u *url.URL) string {
	path := vrtPath(u)
	// vrtmax.be redirects to vrt.be with the section put back in front, so
	// that is done here instead of spending a request to be told so.
	if util.HostMatches(u.Host, vrtMaxDomain) && !vrtInCatalogue(path) {
		path = vrtSection + path
	}
	if !vrtInCatalogue(path) {
		return ""
	}
	return path
}

// vrtPath normalises a URL path into the leading-and-trailing-slash form the
// API's own links are written in. Either slash may be missing from something
// pasted by hand, and the endpoint accepts a missing trailing one but not a
// missing leading one.
func vrtPath(u *url.URL) string {
	path := strings.TrimSpace(u.Path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return path
}

// vrtInCatalogue reports whether a path addresses VRT MAX rather than one of
// the other services sharing the domain.
func vrtInCatalogue(path string) bool {
	return strings.HasPrefix(path, vrtSection+"/") || strings.HasPrefix(path, vrtOldSection+"/")
}

// vrtSlug is the last segment of a page id, which is what names a file when
// nothing else did. It is unique within a programme, which a title is not.
func vrtSlug(id string) string {
	trimmed := strings.Trim(id, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// vrtTitle names a lone episode, which lands in the download directory with
// no folder above it to say what it belongs to. Neither half is enough by
// itself: the media document names the programme and the episode's place in
// it — "A Programme S2026 E167" is the shape — but not what it is about, and
// the page names the episode but not what it belongs to.
func vrtTitle(programme, episode string) string {
	switch {
	case programme == "":
		return episode
	case episode == "" || strings.EqualFold(programme, episode):
		return programme
	}
	return programme + " - " + episode
}

// vrtRefused is the media service declining to serve something, carrying the
// code it declined with. It is a type rather than a message so that expanding
// a whole programme can report the reason once instead of the same line
// several hundred times.
type vrtRefused struct {
	code string
}

func (e *vrtRefused) Error() string { return "vrtmax: " + vrtReason(e.code) }

// vrtReason puts a refusal code into words, keeping the code beside them.
func vrtReason(code string) string {
	if text, ok := vrtRefusals[code]; ok {
		return text + " (" + code + ")"
	}
	return "VRT declined to serve this: " + code
}

// vrtRefusalCode digs a refusal out of a failed request. The media service
// answers one with HTTP 400 and a body of a single field, so the code arrives
// inside the status error rather than in a document of its own.
func vrtRefusalCode(err error) string {
	status, ok := errors.AsType[*httpx.StatusError](err)
	if !ok || status.Code != http.StatusBadRequest {
		return ""
	}
	var body struct {
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(status.Body), &body) != nil {
		return ""
	}
	return body.Code
}

// vrtResponse is the envelope both queries come back in.
type vrtResponse struct {
	Data struct {
		Page *vrtPage `json:"page"`
		// List is what the second query returns, and is a tile list of the
		// same shape a programme page carries inline.
		List *vrtComponent `json:"list"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// vrtPage is the part of a page document this reads. A GraphQL union arrives
// flattened — every fragment's fields land on the one object — so one struct
// covers both page types and __typename says which of them was filled in.
type vrtPage struct {
	TypeName string `json:"__typename"`
	Title    string `json:"title"`
	Player   struct {
		Title string `json:"title"`
		Modes []struct {
			StreamID string `json:"streamId"`
		} `json:"modes"`
	} `json:"player"`
	Components []vrtComponent `json:"components"`
}

// streamID is the id the media service takes, of the form
// pbs-pub-<uuid>$vid-<uuid>. A player lists one mode per playback route and
// the first that names a stream is the programme itself.
func (p *vrtPage) streamID() string {
	for _, mode := range p.Player.Modes {
		if id := strings.TrimSpace(mode.StreamID); id != "" {
			return id
		}
	}
	return ""
}

// vrtComponent is one entry of a components list — the page is a tree of
// these, navigations holding navigations holding tile lists. Items covers
// both a navigation's tabs and a tile list's tiles, which is not a compromise
// but the flattening again: both are called items and neither carries a field
// the other does.
type vrtComponent struct {
	TypeName string    `json:"__typename"`
	ListID   string    `json:"listId"`
	Items    []vrtItem `json:"items"`
}

// vrtItem is one tab of a navigation or one tile of a list.
type vrtItem struct {
	TypeName string `json:"__typename"`
	Title    string `json:"title"`
	Action   struct {
		// Link is the next page's id, which is why no id mapping is needed
		// anywhere in this file.
		Link string `json:"link"`
	} `json:"action"`
	Components []vrtComponent `json:"components"`
}

// vrtMedia is the part of a media document this reads.
type vrtMedia struct {
	// Title names the programme and the episode's place in it.
	Title string `json:"title"`
	// DRM is whatever VRT puts there, which for everything playable here is
	// null. The type is not guessed at because the only thing that matters is
	// whether the field was filled in at all.
	DRM        json.RawMessage `json:"drm"`
	TargetURLs []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"targetUrls"`
}

// protected reports whether VRT filled the drm field in.
func (m *vrtMedia) protected() bool {
	body := strings.TrimSpace(string(m.DRM))
	return body != "" && body != "null"
}

// hls is the playlist to fetch. The locator carries a query of its own that
// tells the packager which audio codecs to include, so it is passed on
// exactly as it arrived.
func (m *vrtMedia) hls() string {
	for _, target := range m.TargetURLs {
		if strings.EqualFold(target.Type, "hls") && target.URL != "" {
			return target.URL
		}
	}
	return ""
}
