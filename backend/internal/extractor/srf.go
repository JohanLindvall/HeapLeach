package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
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
// SRF's HLS is only half usable, and the reason is worth setting out because
// it is not the usual one. Every title carrying an audio description
// publishes a master playlist whose EXT-X-MEDIA AUDIO renditions have URIs of
// their own, and whose every variant names that group — which by RFC 8216 is
// a statement that the variant's own segments carry no audio, and is what
// hlsVariant.muxed() refuses. Measured, those segments do carry AAC beside
// the H.264: Akamai mixes the default track in *and* publishes it separately
// so a player can switch to the described one. The manifest is therefore
// lying in the safe direction for a player and the dangerous one for us, and
// nothing in it distinguishes this host's non-conformance from vimeo's
// genuine split, where believing the same declaration yields a silent file.
// So those masters are refused rather than guessed at — a loud wrong answer
// over a quiet one.
//
// What keeps that from mattering much is that the same document also offers
// `protocol: "HTTP"` resources — a plain, rangeable MP4 — for most of the
// catalogue, and that is much the better download anyway: the segmented
// engine, resume across runs and the skip-what-is-already-there check all
// apply to a file and none of them apply to a segment list. So the
// progressive resource is taken first, the playlist is the fallback, and only
// a title that is both audio-described and published without a file is turned
// away.
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

// srfUUIDPattern is the bare id a show page carries in its query, which has
// to be grown back into a urn before the API will take it.
var srfUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NewSRF builds the Play SRF extractor.
func NewSRF(client *httpx.Client) *SRF { return &SRF{client: client, il: srfIL} }

func (s *SRF) Name() string { return "srf" }

// Match claims the Play section of srf.ch, and any srf.ch URL that names a
// media urn outright. The rest of srf.ch is a news site whose articles embed
// a player this does not read, and claiming those would promise more than it
// resolves.
func (s *SRF) Match(u *url.URL) bool {
	if !util.HostMatches(u.Host, "srf.ch") {
		return false
	}
	return srfURN(u.Query().Get("urn")) != "" ||
		strings.HasPrefix(strings.ToLower(u.Path), "/play/")
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
	// A lone file lands straight in the download directory with no folder to
	// say what it belongs to, so the name carries the show as well.
	title = util.FirstNonEmpty(title, urn)
	file.Name = title + file.Name
	return &Result{Title: title, Files: []File{*file}}, nil
}

// series expands a show into one file per episode, filed by season when the
// show has more than one.
func (s *SRF) series(ctx context.Context, u *url.URL, showURN string) (*Result, error) {
	title, episodes, err := s.episodes(ctx, showURN)
	if err != nil {
		return nil, err
	}

	// Only nest by season when there is more than one to tell apart.
	nest := nestByLabel(episodes, func(ep srfEpisodeRef) string { return ep.season })

	// Every episode needs a mediaComposition of its own, and a show with
	// several hundred of them would otherwise sit in "resolving" for minutes
	// before a byte moved.
	resolved := FanOut(ctx, episodes, func(ctx context.Context, ep srfEpisodeRef) ([]srfResolved, error) {
		file, name, err := s.chapter(ctx, ep.urn)
		if err != nil {
			// A refusal comes back as a *result* rather than an error,
			// because FanOut drops what fails and the reason is the one
			// thing worth keeping when a whole show is refused for it.
			var blocked *srfBlocked
			if errors.As(err, &blocked) {
				return []srfResolved{{blocked: blocked.reason}}, nil
			}
			return nil, err
		}
		file.Name = util.FirstNonEmpty(ep.name, name, ep.urn) + file.Name
		if nest {
			file.Dir = ep.season
		}
		return []srfResolved{{file: *file}}, nil
	})

	result := &Result{Title: util.FirstNonEmpty(title, showURN)}
	var blocked string
	for _, got := range resolved {
		if got.blocked != "" {
			blocked = util.FirstNonEmpty(blocked, got.blocked)
			continue
		}
		result.Files = append(result.Files, got.file)
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("srf: none of the %d episodes of %s are playable: %s",
			len(episodes), u.Redacted(), util.FirstNonEmpty(blocked,
				"the catalogue lists them but the Integration Layer offers no stream for any of them"))
	}
	return result, nil
}

// srfResolved is one episode after its composition was read: a file, or the
// reason there is none.
type srfResolved struct {
	file    File
	blocked string
}

// srfEpisodeRef is one episode of a show, with the season it belongs to.
type srfEpisodeRef struct {
	urn    string
	name   string
	season string
}

// episodes reads a show's episodes, following the listing's own paging.
func (s *SRF) episodes(ctx context.Context, showURN string) (string, []srfEpisodeRef, error) {
	next := s.il + srfEpisodesPath + url.PathEscape(showURN) + ".json?" +
		"pageSize=" + strconv.Itoa(config.FolderPageSize) + "&" + srfVector

	var (
		title    string
		episodes []srfEpisodeRef
		seen     = make(map[string]bool)
	)
	for next != "" && len(episodes) < config.MaxListingVideos {
		var doc srfEpisodeList
		if err := s.client.GetJSON(ctx, next, httpx.Referer(srfPlay), &doc); err != nil {
			if len(episodes) > 0 {
				// A page that will not load part-way down a long show still
				// leaves everything above it worth downloading.
				break
			}
			return "", nil, fmt.Errorf("srf: episodes of %s: %w", showURN, err)
		}
		title = util.FirstNonEmpty(title, doc.Show.Title)

		added := 0
		for _, ep := range doc.EpisodeList {
			urn := ep.urn()
			if urn == "" || seen[urn] {
				continue
			}
			seen[urn] = true
			added++
			episodes = append(episodes, srfEpisodeRef{
				urn:    urn,
				name:   ep.Title,
				season: ep.season(),
			})
		}
		if added == 0 {
			// A listing that keeps answering with what has already been
			// taken is not paging any more.
			break
		}
		next = doc.Next
	}

	if len(episodes) == 0 {
		return "", nil, fmt.Errorf("srf: %s lists no episodes", showURN)
	}
	// The cap is checked between pages, so the last one may carry it past.
	// A daily strand has thousands of instalments and reaches this every time.
	if len(episodes) > config.MaxListingVideos {
		episodes = episodes[:config.MaxListingVideos]
	}
	return title, episodes, nil
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
		return nil, "", &srfBlocked{urn: urn, reason: srfBlockMessage(chapter.BlockReason)}
	}

	title := srfJoin(doc.Show.Title, util.FirstNonEmpty(chapter.Title, doc.Episode.Title))
	headers := httpx.Referer(srfPlay)

	// A plain file first, and by a wide margin: it ranges, so it resumes and
	// splits, and the skip check can compare its length against what is on
	// disk. The API publishes no byte count of its own, so Size stays unknown
	// and the response's own Content-Length settles it.
	if best, ok := chapter.pick(srfProtocolHTTP); ok {
		return &File{
			Name:    srfExtension(best.URL),
			URL:     srfSecure(best.URL),
			Size:    -1,
			Headers: headers,
		}, title, nil
	}

	best, ok := chapter.pick(srfProtocolHLS)
	if !ok {
		return nil, "", fmt.Errorf("srf: %s %s", urn, chapter.refusal())
	}

	segments, variant, err := resolvePlaylist(ctx, s.client, best.URL, headers)
	if err != nil {
		return nil, "", fmt.Errorf("srf: %s: %w", urn, err)
	}
	// bestVariant only *prefers* a self-contained rendition; when a master
	// offers none it still returns the best of a bad set, so the caller has
	// to look. The URL comparison is what tells a master from a media
	// playlist, which resolvePlaylist hands back unchanged: a media playlist
	// declares no codecs, so muxed() is false for one and checking it
	// unconditionally would refuse a perfectly good stream.
	if variant.URL != best.URL && !variant.muxed() {
		return nil, "", fmt.Errorf("srf: %s offers no progressive download, and its playlist declares "+
			"the audio as a track of its own — which is how SRF publishes anything carrying an audio "+
			"description. Nothing here can tell that apart from a playlist whose audio is only in that "+
			"track, and joining the second kind saves video with no sound, so this declines rather "+
			"than guess. Hand the page to the external downloader (yt-dlp), which has ffmpeg and "+
			"reads the tracks separately", urn)
	}

	return &File{
		// Named off the segments, not the variant URL: Akamai packages these
		// out of a stored MP4 and names the path after it — ".mp4.csmil/"
		// over MPEG-TS parts. See segmentsExtension.
		Name:     segmentsExtension(segments, variant),
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}, title, nil
}

// srfJoin names a programme by its show and its own title.
//
// SRF names each instalment of a daily strand after the strand — "10 vor 10
// vom 21.08.2026" — so prefixing the show again would say it twice. The
// prefix is added only when the title does not already open with it.
func srfJoin(show, title string) string {
	switch {
	case title == "":
		return show
	case show == "":
		return title
	case strings.HasPrefix(strings.ToLower(title), strings.ToLower(show)):
		return title
	}
	return show + " - " + title
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

// srfExtension names the container a progressive resource arrives in. SRF
// serves MP4 and says so in the path; the default exists only so a host that
// changes its naming still produces a usable filename.
func srfExtension(raw string) string {
	if ext := strings.ToLower(path.Ext(util.NameFromURL(raw))); ext != "" {
		return ext
	}
	return ".mp4"
}

// srfBlocked is the Integration Layer declining to serve a chapter, carrying
// the reason it gave. It is a type rather than a message so that expanding a
// whole show can report the reason once instead of several hundred times.
type srfBlocked struct {
	urn    string
	reason string
}

func (e *srfBlocked) Error() string { return "srf: " + e.urn + ": " + e.reason }

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
func srfRank(quality string) int {
	for i, known := range srfQualities {
		if strings.EqualFold(quality, known) {
			return i
		}
	}
	return len(srfQualities)
}

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
