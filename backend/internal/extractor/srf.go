// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// SRF resolves Play SRF programmes and series.
//
// Swiss public broadcasting is one organisation with five language services,
// and they share a single catalogue API — the "Integration Layer" — that
// answers for all of them without a session, a token or a key. So the shape
// of this is svtplay's and nrk's again: one request describes a programme,
// another lists a series. Four things are its own, and the first is the one
// the rest are arranged around.
//
// SRF's HLS needed one thing set out, because it is not the usual case.
// Every title carrying an audio description publishes a master playlist
// whose EXT-X-MEDIA AUDIO renditions have URIs of their own, and whose every
// variant names that group — which by RFC 8216 is a statement that the
// variant's own segments carry no audio, and is what hlsVariant.muxed()
// refuses. Measured, those segments do carry AAC beside the H.264: Akamai
// mixes the default track in *and* publishes it separately so a player can
// switch to the described one. Nothing in the manifest tells this apart
// from a genuine split, where the video's own segments are silent, and such
// masters used to be refused for that. Now the renditions come with the
// video as tracks of their own, and the downloader joins exactly those,
// leaving out the copy mixed into the video (download/mux.go), so either
// kind comes out whole.
//
// The same document also offers `protocol: "HTTP"` resources — a plain,
// rangeable MP4 — for most of the catalogue, and that is much the better
// download anyway: the segmented engine, resume across runs and the
// skip-what-is-already-there check all apply to a file and none of them
// apply to a segment list. So the progressive resource is taken first, and
// the playlist is the fallback.
//
// The second is a path that has to be wrong to work. SRG documents the
// endpoint with a business unit in it, and the Play pages call it that way,
// but `/2.0/srf/mediaComposition/byUrn/<urn>` answers 404 for the urns the
// site itself hands out. Dropping the `/srf/` segment answers 200 for all of
// them — and the same business-unit-neutral path serves RTS, RSI, RTR and SWI
// urns too, which is why nothing here pins the urn's prefix.
//
// The third is how a refusal arrives. Roughly one title in eight is licensed
// for Switzerland only, and the chapter says so in `blockReason` — but as a
// machine token, "GEOBLOCK", with no resources beside it. That is NRK's
// nonPlayable without NRK's sentence, so unlike nrk.go this does not pass the
// host's words through: there are none to pass through, and the prose below
// is ours. A token nobody here has seen is still reported as itself.
//
// The fourth decides where a series listing comes from, and it is not the
// page. A show page is server-rendered and full of `urn:srf:video:` strings,
// which makes scraping it look easy and makes it wrong: of the 26 urns on one
// show's page, 3 belonged to that show and 23 were recommendations for other
// programmes, while 15 of its 18 episodes appeared nowhere on it. The
// episodeComposition endpoint returns all three seasons of the same show in
// one request, numbered, so that is what is asked.
type SRF struct {
	about
	client *httpx.Client
	// il is the Integration Layer's base, a field only so a test can point
	// it at a fixture server.
	il string
}

const (
	srfRoot = "https://www.srf.ch"
	srfPlay = srfRoot + "/play/tv/"
	srfIL   = "https://il.srgssr.ch/integrationlayer/2.0"

	// srfCompositionPath describes one chapter and everything around it;
	// srfEpisodesPath lists a show's episodes, newest first.
	srfCompositionPath = "/mediaComposition/byUrn/"
	srfEpisodesPath    = "/episodeComposition/latestByShow/byUrn/"

	// srfVector names the caller. The Play pages send it on every call and
	// it costs nothing to look like them; the responses seen so far do not
	// vary with it, but which resources a business unit publishes to which
	// front end is exactly the sort of thing a broadcaster changes.
	srfVector = "vector=portalplay"

	// The protocols a resource may speak. Only these two are fetchable here:
	// the rest of the enumeration is DASH and RTMP.
	srfProtocolHTTP = "HTTP"
	srfProtocolHLS  = "HLS"

	// srfNoToken is what tokenType reads when a resource can simply be
	// fetched. Anything else is an Akamai signature this does not mint.
	srfNoToken = "NONE"

	// srfShowKind is the path segment a show page uses, and the media kind
	// its urn carries.
	srfShowKind = "show"
)

// srfURNPattern is the shape of an Integration Layer urn: a business unit, a
// media kind, an optional transmission, and an id. The business unit is left
// open because the endpoint is — one document answers for every SRG service —
// so a urn spelled for another of them resolves rather than being refused for
// a prefix this never had a reason to care about.
var srfURNPattern = regexp.MustCompile(`^urn:[a-z]+:[a-z]+:(?:[a-z]+:)?[A-Za-z0-9_-]+$`)

// srfOwnURN is a media urn of the broadcasting group's units, which no other
// site's query carries: the urn namespace of every other service is its own.
var srfOwnURN = regexp.MustCompile(`^urn:(?:srf|rts|rsi|rtr|swi):(?:video|audio)(?::[a-z]+)?:[A-Za-z0-9_-]+$`)

// srfUUIDPattern is the bare id a show page carries in its query, which has
// to be grown back into a urn before the API will take it.
var srfUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NewSRF builds the Play SRF extractor.
func NewSRF(client *httpx.Client) *SRF {
	return &SRF{client: client, il: srfIL, about: about{name: "srf", shape: srfShape,
		recognises: "programmes and shows of Play SRF (a `urn=` or a show's `id=` in the query) whose page names its `urn:srf:` media"}}
}

// srfShape is a link naming a media urn, or a Play show page by its id.
func srfShape(u *url.URL) bool { return srfTarget(u).urn != "" }

// Recognise takes a page of Play SRF naming what it plays: its media are
// addressed by urn:srf: urns, which the Integration Layer resolves.
//
// A link whose query names an urn:srf: urn outright is read from the urn
// alone, so the page it was found on need not say anything: an article of
// the broadcaster's that is not Play SRF's, or a page the probe could not
// read, names the same media.
func (s *SRF) Recognise(_ context.Context, p *Page) bool {
	if srfOwnURN.MatchString(p.URL.Query().Get("urn")) || srfOwnURN.MatchString(p.Final.Query().Get("urn")) {
		return true
	}
	return s.Shape(p.Final) && p.Has("urn:srf:") && p.Named("Play SRF")
}

// Extract handles a single programme or a whole show.
func (s *SRF) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	ref := srfTarget(u)
	switch {
	case ref.urn == "":
		return nil, fmt.Errorf("srf: no urn in %s — a Play SRF link carries it in the query, as "+
			"/play/tv/<show>/video/<title>?urn=urn:srf:video:<id>, and a show page as "+
			"/play/tv/sendung/<show>?id=<id>", u.Redacted())
	case ref.show:
		return s.series(ctx, u, ref.urn)
	}
	return s.programme(ctx, ref.urn)
}

// srfRef is what a Play SRF URL points at.
type srfRef struct {
	// urn identifies the media or the show.
	urn string
	// show distinguishes the two, since one urn shape carries both.
	show bool
}

// srfTarget reads a Play SRF URL. The identifier is always in the query
// rather than the path — the path segments are slugs, generated from titles
// and regenerated when a title is corrected — so nothing here is parsed out
// of them except the transmission.
func srfTarget(u *url.URL) srfRef {
	query := u.Query()
	if urn := srfURN(query.Get("urn")); urn != "" {
		return srfRef{urn: urn, show: srfIsShow(urn)}
	}
	// A show page names its show by the bare id, with the kind and the
	// transmission left implicit in the path it was reached by.
	if id := strings.TrimSpace(query.Get("id")); srfUUIDPattern.MatchString(id) {
		if segs := util.PathSegments(u); len(segs) >= 2 && segs[0] == "play" {
			return srfRef{urn: "urn:srf:" + srfShowKind + ":" + segs[1] + ":" + id, show: true}
		}
	}
	return srfRef{}
}

// srfURN validates a urn as it arrived, and returns "" for anything that is
// not one. Nothing is normalised: the API is the authority on which urns
// exist, and a "helpful" rewrite here would only turn a 404 into a lie about
// what was asked for.
func srfURN(raw string) string {
	raw = strings.TrimSpace(raw)
	if srfURNPattern.MatchString(raw) {
		return raw
	}
	return ""
}

// srfIsShow reports whether a urn names a show rather than one video.
func srfIsShow(urn string) bool {
	parts := strings.Split(urn, ":")
	return len(parts) > 2 && parts[2] == srfShowKind
}

// programme resolves one video urn into a job of its own.
func (s *SRF) programme(ctx context.Context, urn string) (*Result, error) {
	file, title, err := s.chapter(ctx, urn)
	if err != nil {
		return nil, err
	}
	return soleFile(cmp.Or(title, urn), file), nil
}

// series expands a show into one file per episode, filed by season when the
// show has more than one.
func (s *SRF) series(ctx context.Context, u *url.URL, showURN string) (*Result, error) {
	title, episodes, more, walkNote, err := s.episodes(ctx, showURN, config.MaxResolvedEpisodes)
	if err != nil {
		return nil, err
	}

	// Every episode needs a mediaComposition of its own, and a show with
	// several hundred of them would otherwise sit in "resolving" for minutes
	// before a byte moved.
	return listSeries(ctx, cmp.Or(title, showURN), series[broadcastEpisode]{
		site: "srf", noun: "episodes", link: u.Redacted(),
		items: episodes, more: more,
		label:      broadcastEpisode.seasonOf,
		resolve:    listedAs(s.chapter, nil),
		unplayable: "the catalogue lists them but the Integration Layer offers no stream for any of them",
	}, walkNote)
}

// episodes reads a show's episodes, following the listing's own paging up
// to limit of them, and says whether the listing went on past that. A daily
// strand has thousands of instalments and reaches it every time. A page that
// will not load part-way down a long show still leaves everything above it
// worth downloading, and the note says the listing was cut there.
func (s *SRF) episodes(ctx context.Context, showURN string, limit int) (title string, episodes []broadcastEpisode, more bool, note string, err error) {
	next := s.il + srfEpisodesPath + url.PathEscape(showURN) + ".json?" +
		"pageSize=" + strconv.Itoa(config.FolderPageSize) + "&" + srfVector
	episodes, more, note, err = walkEpisodes(ctx, limit, pageWalk[broadcastEpisode]{
		fetch: func(ctx context.Context, _ int) (pageOf[broadcastEpisode], error) {
			var doc srfEpisodeList
			if err := s.client.GetJSON(ctx, next, httpx.Referer(srfPlay), &doc); err != nil {
				return pageOf[broadcastEpisode]{}, err
			}
			title = cmp.Or(title, doc.Show.Title)
			var refs []broadcastEpisode
			for _, ep := range doc.EpisodeList {
				if urn := ep.urn(); urn != "" {
					refs = append(refs, broadcastEpisode{id: urn, name: ep.Title, season: ep.season()})
				}
			}
			next = doc.Next
			return pageOf[broadcastEpisode]{items: refs, next: next != "", end: len(refs) == 0}, nil
		},
		key: func(ep broadcastEpisode) string { return ep.id },
		// A next page that adds nothing new is the listing's end.
		ends: walkAccepts{repeated: true},
	})
	if err != nil {
		return "", nil, false, "", fmt.Errorf("srf: episodes of %s: %w", showURN, err)
	}
	if len(episodes) == 0 {
		return "", nil, false, "", fmt.Errorf("srf: %s lists no episodes", showURN)
	}
	return title, episodes, more, note, nil
}

// chapter resolves one video urn to something downloadable. The returned File
// carries only the extension in Name; the caller supplies the stem, which
// differs between a standalone programme and one episode of a show.
func (s *SRF) chapter(ctx context.Context, urn string) (*File, string, error) {
	var doc srfComposition
	endpoint := s.il + srfCompositionPath + url.PathEscape(urn) + ".json?onlyChapters=false&" + srfVector
	if err := s.client.GetJSON(ctx, endpoint, httpx.Referer(srfPlay), &doc); err != nil {
		return nil, "", fmt.Errorf("srf: composition for %s: %w", urn, err)
	}

	chapter, ok := doc.chapter()
	if !ok {
		return nil, "", fmt.Errorf("srf: %s names nothing the Integration Layer knows about", urn)
	}
	if chapter.BlockReason != "" {
		return nil, "", &itemRefusal{"srf", urn, srfBlockMessage(chapter.BlockReason)}
	}

	title := joinTitle(doc.Show.Title, cmp.Or(chapter.Title, doc.Episode.Title), titlePrefixed)
	headers := httpx.Referer(srfPlay)

	// A plain file first, and by a wide margin: it ranges, so it resumes and
	// splits, and the skip check can compare its length against what is on
	// disk. The API publishes no byte count of its own, so Size stays unknown
	// and the response's own Content-Length settles it.
	if best, ok := chapter.pick(srfProtocolHTTP); ok {
		return progressiveFile(srfSecure(best.URL), linkExt(best.URL, ".mp4"), headers), title, nil
	}

	best, ok := chapter.pick(srfProtocolHLS)
	if !ok {
		return nil, "", fmt.Errorf("srf: %s %s", urn, chapter.refusal())
	}

	media, err := broadcastMedia(ctx, s.client, "srf", urn, best.URL, headers)
	if err != nil {
		return nil, "", err
	}
	variant := media.Variant
	// bestVariant only *prefers* a self-contained rendition; when a master
	// offers none it still returns the best of a bad set, so the caller has
	// to look. The URL comparison is what tells a master from a media
	// playlist, which the resolver hands back unchanged: a media playlist
	// declares no codecs, so muxed() is false for one and checking it
	// unconditionally would refuse a perfectly good stream.
	//
	// A variant that declares its audio as tracks of its own, as SRF does
	// for anything carrying an audio description, comes with them
	// (media.Audio). The downloader joins them to the video, keeping
	// whatever audio the video's own segments carry as well, so it no longer
	// matters which of the two kinds of playlist this is. Only a variant with
	// no sound named anywhere is refused.
	if variant.URL != best.URL && !variant.muxed() && len(media.Audio) == 0 {
		return nil, "", fmt.Errorf("srf: %s offers no progressive download, and its playlist names no "+
			"audio, in the video or beside it: joining it would save video with no sound", urn)
	}

	// Named off the segments, not the variant URL: Akamai packages these out
	// of a stored MP4 and names the path after it — ".mp4.csmil/" over
	// MPEG-TS parts. See segmentsExtension.
	return new(media.file(media.extension(), headers)), title, nil
}

// srfSecure upgrades a progressive link to https. The Integration Layer hands
// these out as http and the same object is served over TLS, so there is no
// reason to fetch several gigabytes in the clear.
func srfSecure(raw string) string {
	if rest, ok := strings.CutPrefix(raw, "http://"); ok {
		return "https://" + rest
	}
	return raw
}

// srfBlockReasons turns the Integration Layer's block tokens into a sentence.
//
// This is where SRF parts company with NRK, whose refusals arrive already
// written for a viewer and are passed through untouched. SRF states the same
// facts as a machine token, and a token is not an explanation, so the prose
// is supplied here.
var srfBlockReasons = map[string]string{
	"GEOBLOCK":             "SRF licenses this for Switzerland only, and serves nothing at all to an address outside it",
	"VPNORPROXYDETECTED":   "SRF took this connection for a VPN or a proxy and refused it on that basis",
	"LEGAL":                "SRF has withdrawn this for legal reasons",
	"JOURNALISTIC":         "SRF has withdrawn this for journalistic reasons",
	"COMMERCIAL":           "this is commercial content SRF does not publish for playback",
	"STARTDATE":            "this has not been published yet",
	"ENDDATE":              "this has passed the end of its availability window",
	"AGERATING12":          "this is age-restricted (12) and SRF serves it only to a viewer who is signed in",
	"AGERATING18":          "this is age-restricted (18) and SRF serves it only to a viewer who is signed in",
	"UNKNOWN":              "SRF will not serve this and did not say why",
	"SRGSSR_UNSPECIFIED":   "SRF will not serve this and did not say why",
	"NEED_TO_BE_LOGGED_IN": "SRF serves this only to a viewer who is signed in",
}

// srfBlockMessage explains a block token, or reports it unchanged. Naming
// what the host actually said beats reporting nothing, so an unrecognised
// token is passed on as it stands rather than flattened into "unavailable".
func srfBlockMessage(reason string) string {
	if message, ok := srfBlockReasons[strings.ToUpper(strings.TrimSpace(reason))]; ok {
		return message
	}
	return "SRF refused it: " + reason
}

// srfComposition is the part of a mediaComposition document this needs.
type srfComposition struct {
	// ChapterURN names which of the chapters the request was actually for.
	ChapterURN  string       `json:"chapterUrn"`
	Show        srfTitled    `json:"show"`
	Episode     srfTitled    `json:"episode"`
	ChapterList []srfChapter `json:"chapterList"`
}

// srfTitled is anything the document gives a name to.
type srfTitled struct {
	Title string `json:"title"`
}

// chapter picks the chapter the composition is about.
//
// This matters more than the single-element lists suggest, because asking for
// a *clip* answers with the programme the clip was cut from: the requested urn
// then appears only in that chapter's segmentList, as a pair of time offsets
// with no resources of its own. There is nothing here that can cut a range
// out of a file, so what a clip link resolves to is the whole programme —
// named after the programme, which is at least honest about what lands on
// disk.
func (c srfComposition) chapter() (srfChapter, bool) {
	for _, chapter := range c.ChapterList {
		if chapter.URN != "" && chapter.URN == c.ChapterURN {
			return chapter, true
		}
	}
	if len(c.ChapterList) > 0 {
		return c.ChapterList[0], true
	}
	return srfChapter{}, false
}

// srfChapter is one playable thing, with every way of fetching it.
//
// Two fields beside these are deliberately unread. podcastSdUrl and
// podcastHdUrl look like a free extra progressive source — and for most
// chapters they are byte-for-byte the resource already listed, on another
// CDN hostname. But not always: one magazine episode dated the 10th carries
// podcast links to an asset produced on the 18th, a different programme
// entirely. A source that is usually right is worse than no source at all
// when being wrong means silently downloading somebody else's episode.
type srfChapter struct {
	URN   string `json:"urn"`
	Title string `json:"title"`
	// BlockReason is why nothing may be served, as a machine token. It comes
	// with resourceList absent rather than merely empty, so it has to be read
	// before anything is chosen.
	BlockReason  string        `json:"blockReason"`
	ResourceList []srfResource `json:"resourceList"`
}

// srfResource is one way the Integration Layer offers a chapter.
type srfResource struct {
	URL      string `json:"url"`
	Quality  string `json:"quality"`
	Protocol string `json:"protocol"`
	// TokenType is "NONE" for a resource that can simply be fetched, and
	// names an Akamai token scheme otherwise.
	TokenType string `json:"tokenType"`
	// DRMList is the schema's place for content protection. Nothing SRF has
	// served here carried one, which is precisely why it is checked: a
	// resource that turns up encrypted would otherwise download to the end
	// and then play as noise.
	DRMList []struct {
		Type string `json:"type"`
	} `json:"drmList"`
}

// usable reports whether this resource can be fetched as it stands.
func (r srfResource) usable() bool {
	return r.URL != "" && len(r.DRMList) == 0 &&
		(r.TokenType == "" || strings.EqualFold(r.TokenType, srfNoToken))
}

// srfQualities are the rendition labels, best first. SRF publishes these two
// for video; a label this does not recognise ranks below both but is still
// taken when it is all there is.
var srfQualities = []string{"HD", "SD"}

// srfRank orders a rendition by its label.
func srfRank(quality string) int { return rankIn(srfQualities, quality, len(srfQualities)) }

// pick returns the best usable resource speaking one protocol.
func (c srfChapter) pick(protocol string) (srfResource, bool) {
	best, rank, found := srfResource{}, 0, false
	for _, r := range c.ResourceList {
		if !strings.EqualFold(r.Protocol, protocol) || !r.usable() {
			continue
		}
		if got := srfRank(r.Quality); !found || got < rank {
			best, rank, found = r, got, true
		}
	}
	return best, found
}

// refusal explains a chapter that is not blocked and still offers nothing,
// naming whichever obstacle its resources actually carried.
func (c srfChapter) refusal() string {
	var drm, token bool
	for _, r := range c.ResourceList {
		switch {
		case len(r.DRMList) > 0:
			drm = true
		case r.TokenType != "" && !strings.EqualFold(r.TokenType, srfNoToken):
			token = true
		}
	}
	switch {
	case drm:
		return "is DRM protected"
	case token:
		return "is served only against an Akamai token, which this does not mint"
	}
	return "offers no stream that can be fetched"
}

// srfEpisodeList is a show's listing, one page of it.
type srfEpisodeList struct {
	Show        srfTitled    `json:"show"`
	EpisodeList []srfEpisode `json:"episodeList"`
	// Next is the following page, already built. It is absent on the last.
	Next string `json:"next"`
}

// srfEpisode is one entry of a show listing.
type srfEpisode struct {
	Title string `json:"title"`
	// FullLengthURN names the whole programme, as opposed to the pieces cut
	// out of it.
	FullLengthURN string `json:"fullLengthUrn"`
	SeasonNumber  int    `json:"seasonNumber"`
	Number        int    `json:"number"`
	MediaList     []struct {
		URN string `json:"urn"`
	} `json:"mediaList"`
}

// urn is the episode's own full-length video.
//
// mediaList beside it is the trap. It holds the programme *and* every segment
// cut out of it, all typed EPISODE and indistinguishable by anything in the
// listing — one instalment of a news strand lists eight — so taking that list
// downloads the programme once whole and then again in pieces. fullLengthUrn
// names the single entry that is the programme.
func (e srfEpisode) urn() string {
	if e.FullLengthURN != "" {
		return e.FullLengthURN
	}
	// An episode described by its media list alone is led by the programme;
	// its segments follow in broadcast order.
	for _, m := range e.MediaList {
		if m.URN != "" {
			return m.URN
		}
	}
	return ""
}

// season labels the folder an episode belongs in. The wording is the site's
// own, so a folder reads the same way as the titles inside it, which already
// end "(Staffel 3, Folge 1)". A show that numbers no seasons — every news and
// magazine strand — returns nothing and is left unnested.
func (e srfEpisode) season() string {
	if e.SeasonNumber <= 0 {
		return ""
	}
	return "Staffel " + strconv.Itoa(e.SeasonNumber)
}
