package extractor

import (
	"bytes"
	"context"
	"encoding/binary"
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

// RTVE resolves rtve.es — RTVE Play, the Spanish public broadcaster's
// catalogue — as single videos and as whole programmes.
//
// It is the odd one out among the broadcasters here, and pleasantly so. SVT
// and NRK offer nothing but adaptive manifests, so both end up handing back a
// segment list; RTVE publishes a plain progressive MP4 beside the playlist,
// answers a range request on it, and states its exact length in the metadata
// before anything is fetched. That is the case this downloader was actually
// built for — the eight-way split, the resume sidecar, the skip that fires
// before a connection is opened — none of which a segment list can use.
// Against RTVE's own origin one connection reaches about 3 MB/s and eight
// about 17, so the difference is not academic. The playlist is kept only as a
// fallback for an asset that offers no file.
//
// The price is that the media list is obfuscated, and this is the bulk of
// what follows. It arrives from a path that looks like a thumbnail, is base64
// text rather than image bytes, and hides one URL per PNG tEXt chunk under a
// substitution cipher. It is the honest kind of obfuscation, though: every
// chunk ships the alphabet that decodes it, so unlike gofile's request
// signature there is no server-side secret to stay in step with, and a
// misread fails to parse instead of yielding a plausible wrong URL. What is
// written here is what the host answers today rather than what is documented
// elsewhere — the shape has already moved since the version yt-dlp publishes,
// the separator having gone from "%%" to "#".
//
// Geo-blocking is stated outright, which is what makes refusing before a
// request possible; see rtveVideo.playable for the one place that is subtle.
// RTVE offers no sentence to pass on the way NRK's endUserMessage does — its
// origin answers a bare 403 with an empty body — so the refusal below is
// worded here.
//
// Four fields in a video document look like they should be consulted and must
// not be. hasDRM and requireLogged are true on everything, including assets
// that download and play as ordinary H.264 and AAC; notDownloadable and
// paidContent are true across whole series that serve their full length to
// anyone who asks. Believing any of them would refuse most of the catalogue,
// and nothing here reads them.
type RTVE struct {
	client *httpx.Client
}

const (
	rtveRoot = "https://www.rtve.es"
	rtveAPI  = "https://api.rtve.es/api"

	// rtveVideoAPI and rtveProgramAPI are the catalogue: metadata for one
	// video, and the season and episode listings for one programme. Both
	// answer anonymously — no key, cookie or account.
	rtveVideoAPI   = rtveAPI + "/videos/"
	rtveProgramAPI = rtveAPI + "/programas/"

	// rtveMediaAPI is the media list. Nothing in the catalogue names a
	// manifest or a file; this is the only route to one.
	//
	// "rtveplayw" is the rendition set the site's own player asks for, and
	// the only one used here. The "default" manager answers too, with an
	// older and inconsistent selection — 720p for one asset and 360p for
	// another with the same presets on offer — whose size no field in the
	// catalogue predicts. Falling back to it would trade an exact length for
	// a guess about a smaller file, which is the wrong way round.
	rtveMediaAPI = rtveRoot + "/ztnr/movil/thumbnail/rtveplayw/videos/"
)

// rtvePresets ranks the renditions RTVE encodes for streaming, best first,
// and is how the published length is matched to the file that will arrive.
//
// The list is exhaustive on purpose rather than a fallback ordering. A
// catalogue entry may also carry ORIGINAL, which is the delivery master: it
// is listed at 1080p and a plausible size, and the player is never given it —
// an asset offering ORIGINAL, HIGH and MED is served the HIGH file. Ranking
// by height would take ORIGINAL and announce a length several times off, so
// anything not named here is passed over and an asset with no named preset
// left simply goes without an exact size.
var rtvePresets = []string{"HD_FULL", "HD_READY", "HQ", "HIGH", "MED"}

// rtveProgrammeID finds a programme's numeric id in a series page.
//
// The page is rendered server-side but lists no episodes of its own: the
// browser fetches those afterwards, and the id survives only in the URLs of
// those deferred requests. Two independent ones carry it, so a change to
// either still leaves the other.
var rtveProgrammeID = regexp.MustCompile(`(?:api/programas|/modulos/[a-z]+)/(\d+)`)

// NewRTVE builds the RTVE Play extractor.
func NewRTVE(client *httpx.Client) *RTVE { return &RTVE{client: client} }

func (r *RTVE) Name() string { return "rtve" }

// Match takes the site itself and nothing below it. The catalogue and the
// media live on subdomains of the same name — api.rtve.es, ztnr.rtve.es, the
// numbered origins the media lists point at — and a link to one of those is a
// plain rangeable file that the direct extractor handles better than this
// would. A path this cannot read is left alone for the same reason.
func (r *RTVE) Match(u *url.URL) bool {
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host != "rtve.es" && host != "www.rtve.es" {
		return false
	}
	return rtveTarget(u) != (rtveRef{})
}

// Extract handles a single video or a programme, which expands to everything
// the catalogue lists under it.
func (r *RTVE) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	ref := rtveTarget(u)
	switch {
	case ref.video != "":
		return r.video(ctx, ref.video)
	case ref.series != "":
		return r.series(ctx, u, ref.series)
	}
	return nil, fmt.Errorf("rtve: no video or programme in %s", u.Redacted())
}

// rtveRef is what an rtve.es URL points at.
type rtveRef struct {
	// video is the numeric asset id, when the URL names one.
	video string
	// series is the programme's slug, which is all a programme page carries;
	// its numeric id has to be read out of the page itself.
	series string
}

// rtveTarget reads an rtve.es URL.
//
// An episode is identified by the numeric id at the end of its path, and
// nothing else in the path is trustworthy — the slugs before it are
// decorative, and the site answers a wrong one with a redirect rather than a
// 404. So the id is taken as the last numeric segment and a path with none is
// read as the programme its first segment names.
func rtveTarget(u *url.URL) rtveRef {
	segs := util.PathSegments(u)

	// /v/<id> is the share link the site's own buttons produce.
	if len(segs) == 2 && segs[0] == "v" && rtveIsID(segs[1]) {
		return rtveRef{video: segs[1]}
	}

	// Everything else lives under /play/videos/, or under the older
	// /alacarta/videos/, which the site still answers with a redirect and
	// which is therefore what sits in other people's pages.
	if len(segs) < 3 || segs[1] != "videos" || (segs[0] != "play" && segs[0] != "alacarta") {
		return rtveRef{}
	}
	rest := segs[2:]
	for i := len(rest) - 1; i >= 0; i-- {
		if rtveIsID(rest[i]) {
			return rtveRef{video: rest[i]}
		}
	}
	return rtveRef{series: rest[0]}
}

// rtveIsID reports whether a path segment is an asset id, which is decimal
// digits and nothing else.
func rtveIsID(seg string) bool {
	for i := 0; i < len(seg); i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return false
		}
	}
	return seg != ""
}

// video resolves one asset id into a job of its own.
func (r *RTVE) video(ctx context.Context, id string) (*Result, error) {
	meta, err := r.metadata(ctx, id)
	if err != nil {
		return nil, err
	}
	file, err := r.file(ctx, meta)
	if err != nil {
		return nil, err
	}
	return &Result{Title: meta.name(), Files: []File{*file}}, nil
}

// series expands a programme into one file per video the catalogue lists,
// filed by season when the programme has more than one.
func (r *RTVE) series(ctx context.Context, u *url.URL, slug string) (*Result, error) {
	program, err := r.programme(ctx, u)
	if err != nil {
		return nil, err
	}
	episodes, err := r.episodes(ctx, program)
	if err != nil {
		return nil, err
	}
	if len(episodes) == 0 {
		return nil, fmt.Errorf("rtve: programme %s lists no videos", program)
	}
	// A long-running strand runs to thousands of instalments, which is a
	// mistake being carried out efficiently rather than a download.
	if len(episodes) > config.MaxListingVideos {
		episodes = episodes[:config.MaxListingVideos]
	}

	// Only nest by season when there is more than one to tell apart.
	distinct := make(map[string]bool)
	for _, ep := range episodes {
		if ep.Season != "" {
			distinct[ep.Season] = true
		}
	}
	nest := len(distinct) > 1

	outcomes := FanOut(ctx, episodes, func(ctx context.Context, ep rtveVideo) ([]rtveOutcome, error) {
		file, err := r.episode(ctx, ep)
		if err != nil {
			// Kept rather than dropped: a whole programme refused is refused
			// for one reason, and reporting it once beats several hundred
			// identical failures. FanOut discards an entry whose fetch
			// errored, so the failure travels as a value instead.
			return []rtveOutcome{{err: err}}, nil
		}
		if nest {
			file.Dir = ep.Season
		}
		return []rtveOutcome{{file: file}}, nil
	})

	result := &Result{Title: util.FirstNonEmpty(episodes[0].Program.Title, slug)}
	var (
		refused *rtveRefused
		first   error
	)
	for _, outcome := range outcomes {
		if outcome.file != nil {
			result.Files = append(result.Files, *outcome.file)
			continue
		}
		if first == nil {
			first = outcome.err
		}
		if refused == nil {
			errors.As(outcome.err, &refused)
		}
	}
	if len(result.Files) == 0 {
		// A territory refusal is preferred over whatever failed first,
		// because when everything is refused it is refused for that reason
		// and it is the one the caller can act on.
		reason := "nothing there offered a file to fetch"
		switch {
		case refused != nil:
			reason = refused.reason()
		case first != nil:
			reason = first.Error()
		}
		return nil, fmt.Errorf("rtve: none of the %d videos of %s can be downloaded: %s",
			len(episodes), u.Redacted(), reason)
	}
	return result, nil
}

// rtveOutcome is one video of a programme resolved, or the reason it was not.
type rtveOutcome struct {
	file *File
	err  error
}

// programme reads a programme's numeric id off its page. Nothing in the URL
// carries it — the path names the programme by slug, and the catalogue is
// keyed by number.
func (r *RTVE) programme(ctx context.Context, u *url.URL) (string, error) {
	doc, err := r.client.GetString(ctx, u.String(), httpx.Referer(rtveRoot+"/"))
	if err != nil {
		return "", fmt.Errorf("rtve: fetch %s: %w", u.Redacted(), err)
	}
	if match := rtveProgrammeID.FindStringSubmatch(doc); match != nil {
		return match[1], nil
	}
	return "", fmt.Errorf("rtve: %s names no programme "+
		"(a link to one video of it does resolve)", u.Redacted())
}

// episodes lists everything a programme holds, by season where it has any.
//
// The two routes are not interchangeable and the split is the catalogue's,
// not a preference: a drama divides into seasons and its /videos listing runs
// to thousands of entries mixing clips and trailers in with the episodes,
// while a documentary strand has no seasons at all and answers the seasons
// endpoint with an empty page.
func (r *RTVE) episodes(ctx context.Context, program string) ([]rtveVideo, error) {
	seasons, err := r.seasons(ctx, program)
	if err != nil || len(seasons) == 0 {
		// A programme with no seasons answers that endpoint with an empty
		// page, and one whose seasons cannot be read at all is still
		// described in full by the flat listing, so both land here.
		return r.listing(ctx, rtveProgramAPI+url.PathEscape(program)+"/videos")
	}

	// Seasons are fetched together and collected in order, so a programme of
	// twenty-odd seasons does not resolve one request at a time. A season
	// that will not load is skipped rather than failing the rest.
	return FanOut(ctx, seasons, func(ctx context.Context, season string) ([]rtveVideo, error) {
		return r.listing(ctx, rtveProgramAPI+url.PathEscape(program)+
			"/temporadas/"+url.PathEscape(season)+"/videos")
	}), nil
}

// seasons lists a programme's season ids, oldest first as the catalogue gives
// them.
func (r *RTVE) seasons(ctx context.Context, program string) ([]string, error) {
	seasons, err := rtveList[rtveSeason](ctx, r.client, rtveProgramAPI+url.PathEscape(program)+"/temporadas")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, season := range seasons {
		if season.ID > 0 {
			out = append(out, strconv.Itoa(season.ID))
		}
	}
	return out, nil
}

// listing walks one paginated video listing.
func (r *RTVE) listing(ctx context.Context, endpoint string) ([]rtveVideo, error) {
	return rtveList[rtveVideo](ctx, r.client, endpoint)
}

// rtveList walks a paginated catalogue endpoint to its end.
//
// A page is asked for at the size the catalogue accepts rather than at its
// default of twenty, which is what a season of thirty-three episodes was
// being silently truncated to. Paging then stops at the page count the
// response itself reports, and that bound is the one that matters: asked for
// a page past the end, this API answers with the whole listing over again
// rather than with nothing, so counting short pages would never terminate.
func rtveList[T any](ctx context.Context, client *httpx.Client, endpoint string) ([]T, error) {
	var out []T
	for page := 1; page <= config.MaxAlbumPages; page++ {
		var doc rtvePage[T]
		request := fmt.Sprintf("%s?size=%d&page=%d", endpoint, config.FolderPageSize, page)
		if err := client.GetJSON(ctx, request, httpx.Referer(rtveRoot+"/"), &doc); err != nil {
			if page == 1 {
				return nil, fmt.Errorf("rtve: listing %s: %w", endpoint, err)
			}
			break // a later page failing still leaves the earlier ones
		}
		out = append(out, doc.Page.Items...)
		if len(doc.Page.Items) == 0 || page >= doc.Page.TotalPages || len(out) >= config.MaxListingVideos {
			break
		}
	}
	return out, nil
}

// episode resolves one entry of a programme listing.
//
// A listing entry is a complete video document and answers everything this
// needs but for the one field it answers wrongly — see rtveVideo.playable —
// so a video the listing marks as licensed by territory is asked about on its
// own before anything is concluded either way. One the listing says is not
// restricted at all costs no extra request.
func (r *RTVE) episode(ctx context.Context, video rtveVideo) (*File, error) {
	if video.Restricted {
		fresh, err := r.metadata(ctx, video.ID)
		if err != nil {
			return nil, err
		}
		video = *fresh
	}
	return r.file(ctx, &video)
}

// metadata reads one video's own document, which is the only one whose
// availability answer describes the caller.
func (r *RTVE) metadata(ctx context.Context, id string) (*rtveVideo, error) {
	var doc rtvePage[rtveVideo]
	if err := r.client.GetJSON(ctx, rtveVideoAPI+url.PathEscape(id)+".json",
		httpx.Referer(rtveRoot+"/"), &doc); err != nil {
		return nil, fmt.Errorf("rtve: video %s: %w", id, err)
	}
	if len(doc.Page.Items) == 0 {
		return nil, fmt.Errorf("rtve: %s is not a video", id)
	}
	return &doc.Page.Items[0], nil
}

// file turns a video document into the file to fetch.
func (r *RTVE) file(ctx context.Context, video *rtveVideo) (*File, error) {
	if !video.playable() {
		return nil, &rtveRefused{name: video.name(), country: video.Country}
	}

	links, err := r.media(ctx, video.ID)
	if err != nil {
		return nil, err
	}
	headers := httpx.Referer(rtveRoot + "/")

	// The progressive file first, and by a distance: it is one rangeable
	// resource with a length the catalogue already published, so the transfer
	// splits, resumes and can be skipped outright when it is already on disk.
	if direct := rtveByExtension(links, ".mp4"); direct != "" {
		return &File{
			Name:    video.name() + ".mp4",
			URL:     direct,
			Size:    video.size(),
			Headers: headers,
		}, nil
	}

	// Nothing seen so far lacks one, but an asset that does is still worth
	// having through the playlist.
	playlist := rtveByExtension(links, ".m3u8")
	if playlist == "" {
		return nil, fmt.Errorf("rtve: %s offers neither a file nor a playlist", video.ID)
	}
	segments, variant, err := resolvePlaylist(ctx, r.client, playlist, headers)
	if err != nil {
		return nil, fmt.Errorf("rtve: %s: %w", video.ID, err)
	}
	return &File{
		Name:     video.name() + rtveExtension(segments, variant),
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}, nil
}

// rtveByExtension picks the media list entry whose path ends in ext.
//
// The path, not the string: RTVE's playlist lives in a directory named after
// the progressive file, so "<name>.mp4/video.m3u8" is a real URL here and a
// search for ".mp4" anywhere in it would take the manifest for the file.
func rtveByExtension(links []string, ext string) string {
	for _, link := range links {
		if u, err := url.Parse(link); err == nil && strings.EqualFold(path.Ext(u.Path), ext) {
			return link
		}
	}
	return ""
}

// rtveExtension names the container the joined segments will be in.
//
// playlistExtension reads the variant's own URL, and that same directory
// naming misleads it: ".mp4" occurs in the path of a stream whose parts are
// MPEG-TS. A .ts file named .mp4 is one players refuse and remux.go never
// converts, so the parts are asked what they are before the URL is.
func rtveExtension(segments []string, variant hlsVariant) string {
	if len(segments) > 0 {
		if u, err := url.Parse(segments[0]); err == nil && strings.EqualFold(path.Ext(u.Path), ".ts") {
			return ".ts"
		}
	}
	return playlistExtension(variant)
}

// rtveRefused is RTVE declining to serve something to this caller. It is a
// type rather than a message so that a whole programme refused can be
// reported as one reason instead of several hundred failures.
type rtveRefused struct {
	name    string
	country string
}

func (e *rtveRefused) Error() string { return "rtve: " + e.name + ": " + e.reason() }

// reason says what happened in this extractor's own words, since RTVE
// supplies none: the origin answers a bare 403 with an empty body, and the
// document that predicts it carries flags rather than a sentence. The country
// it names is the one RTVE placed the caller in, which is the fact worth
// having — it is what the licence was tested against.
func (e *rtveRefused) reason() string {
	where := "this country"
	if e.country != "" {
		where = e.country
	}
	return "RTVE licenses it by territory and does not serve it to " + where
}

// rtvePage is the envelope every catalogue endpoint answers in.
type rtvePage[T any] struct {
	Page struct {
		Items      []T `json:"items"`
		TotalPages int `json:"totalPages"`
	} `json:"page"`
}

// rtveSeason is one season of a programme. Only the id is used: the season's
// own name is repeated on every video it holds, which is where the folder
// name is taken from.
type rtveSeason struct {
	ID int `json:"id"`
}

// rtveVideo is the part of a video document this needs, and it is the same
// document whether it came from the video's own endpoint or out of a listing.
type rtveVideo struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	LongTitle  string `json:"longTitle"`
	ShortTitle string `json:"shortTitle"`
	// Season is the short season label, "T1", carried on every video that
	// belongs to one and null on everything else.
	Season string `json:"temporadaShortTitle"`
	// Restricted says the video is licensed by territory. Every document
	// agrees about it.
	Restricted bool `json:"geolocalizado"`
	// Allowed says the caller is in one of those territories, and Country is
	// where the answer placed them. Both are only as good as the document
	// they came from — see playable.
	Allowed bool   `json:"allowedInCountry"`
	Country string `json:"country"`
	Program struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"programInfo"`
	Qualities []rtveQuality `json:"qualities"`
}

// rtveQuality is one encoded rendition.
type rtveQuality struct {
	Preset string `json:"preset"`
	Type   string `json:"type"`
	// Filesize is null often enough to be worth expecting, which lands here
	// as zero.
	Filesize int64 `json:"filesize"`
}

// playable reports whether RTVE will serve this video to the caller.
//
// Two fields answer that and only one of them travels. Restricted says the
// video is licensed by territory, and every document agrees about it. Allowed
// says whether this caller is inside one — and a listing document answers
// that for Spain whoever asked, alongside a country field reading "ES" for a
// caller its own video document places in "SE". So a listing entry can rule
// the question out but never in, which is why episode re-asks before it
// refuses, and why this is written to be safe when it is handed either.
func (v rtveVideo) playable() bool { return !v.Restricted || v.Allowed }

// name is what the file is called, and it is the long title on purpose.
//
// RTVE writes that as the programme, the season and the episode together —
// "A Drama - T1 - Episodio 33: Something" — which is more than a lone video
// needs and exactly what an episode needs. The episode title on its own is a
// phrase like "Something": inside a season folder it says nothing about where
// it belongs, and across a strand that has run for twenty years it collides.
func (v rtveVideo) name() string {
	return util.FirstNonEmpty(v.LongTitle, v.Title, v.ShortTitle, "rtve-"+v.ID)
}

// size is the exact length of the file the media list will point at, or -1.
//
// This is what makes the pre-connect skip work, and it is exact rather than
// approximate deliberately: measured against the origin's own Content-Length
// it was byte-for-byte right on every asset whose best rendition carried a
// figure at all. It is RTVE's record rather than a measurement, so an asset
// re-encoded since it was written can disagree — one in a sweep of fourteen
// did — and the only cost of that is a skip that does not fire, since
// alreadyOnDisk compares for equality and the response's own length replaces
// this the moment the headers arrive.
func (v rtveVideo) size() int64 {
	if best, ok := v.best(); ok && best.Filesize > 0 {
		return best.Filesize
	}
	return -1
}

// best is the rendition the media list will point at: the first named preset
// on offer, which is what the player is given.
func (v rtveVideo) best() (rtveQuality, bool) {
	for _, preset := range rtvePresets {
		for _, quality := range v.Qualities {
			if strings.EqualFold(quality.Preset, preset) && strings.Contains(quality.Type, "mp4") {
				return quality, true
			}
		}
	}
	return rtveQuality{}, false
}

// The media list, and the cipher over it.
//
// The response is served from a thumbnail path and is a PNG in shape only:
// the body is base64 text, and every URL sits in a tEXt chunk of the image it
// decodes to. Each chunk is self-describing — a keyword, a preamble and a run
// of digits, from which the chunk's own substitution alphabet is built and
// then indexed. That is the whole trick, and it is why this keeps working:
// nothing here has to match a secret the server holds.

// rtvePNGSignature opens every PNG, and is the first thing that would fail if
// this endpoint ever started answering with something else.
var rtvePNGSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// rtveCipherSeparator divides a chunk's alphabet material from its digits.
const rtveCipherSeparator = '#'

// media fetches and decodes one video's media list.
func (r *RTVE) media(ctx context.Context, id string) ([]string, error) {
	// q=v2 selects the current encoding. Without it the same path answers in
	// the older one, which this does not read.
	page := rtveMediaAPI + url.PathEscape(id) + ".png?q=v2"
	body, err := r.client.GetString(ctx, page, httpx.Referer(rtveRoot+"/"))
	if err != nil {
		return nil, fmt.Errorf("rtve: media list for %s: %w", id, err)
	}
	links, err := rtveMediaList(body)
	if err != nil {
		return nil, fmt.Errorf("rtve: media list for %s: %w", id, err)
	}
	return links, nil
}

// rtveMediaList decodes a media list response into its URLs, in the order the
// chunks carried them.
func rtveMediaList(body string) ([]string, error) {
	raw, err := util.DecodeBase64(strings.Join(strings.Fields(body), ""))
	if err != nil {
		return nil, fmt.Errorf("not base64: %w", err)
	}
	if !bytes.HasPrefix(raw, rtvePNGSignature) {
		return nil, errors.New("not a PNG")
	}

	var links []string
	for rest := raw[len(rtvePNGSignature):]; len(rest) >= 8; {
		length := binary.BigEndian.Uint32(rest[:4])
		kind := string(rest[4:8])
		// Length, type and the trailing CRC. The bound is computed in 64 bits
		// so a declared length near the top of the range cannot wrap.
		if uint64(length)+12 > uint64(len(rest)) {
			return nil, errors.New("truncated chunk")
		}
		payload := rest[8 : 8+int(length)]
		rest = rest[8+int(length)+4:]

		switch kind {
		case "IEND":
			rest = nil
		case "tEXt":
			link, err := rtveDecodeChunk(payload)
			if err != nil {
				return nil, err
			}
			links = append(links, link)
		}
	}
	if len(links) == 0 {
		return nil, errors.New("no entries in it")
	}
	return links, nil
}

// rtveDecodeChunk decodes one tEXt chunk into the URL it hides.
//
// The payload is a PNG keyword, a NUL, then a preamble, a '#' and the digits.
// Keyword and preamble together are the material the alphabet is walked out
// of; the digits index it. Splitting them at the '#' is what changed since
// the shape yt-dlp documents, where the separator was "%%".
func rtveDecodeChunk(payload []byte) (string, error) {
	keyword, text, ok := bytes.Cut(payload, []byte{0})
	if !ok {
		return "", errors.New("an entry carries no keyword")
	}
	preamble, digits, ok := bytes.Cut(text, []byte{rtveCipherSeparator})
	if !ok {
		return "", errors.New("an entry carries no cipher text")
	}

	alphabet := rtveAlphabet(keyword, preamble)
	if len(alphabet) == 0 {
		return "", errors.New("an entry names an empty alphabet")
	}

	// Each character is two digits with filler between them, and the run of
	// filler grows and resets on a four-step cycle of its own. tens marks
	// which of the two real digits is next; skip counts the filler still to
	// come; round drives the cycle.
	var (
		out   []byte
		index int
		tens  bool
		skip  = 3
		round = 1
	)
	for _, c := range digits {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("%q is not a digit", string(c))
		}
		switch {
		case !tens:
			index, tens = int(c-'0')*10, true
		case skip > 0:
			skip--
		default:
			index += int(c - '0')
			if index >= len(alphabet) {
				return "", fmt.Errorf("index %d is past a %d-entry alphabet", index, len(alphabet))
			}
			out = append(out, alphabet[index])
			skip, tens, round = (round+3)%4, false, round+1
		}
	}
	return string(out), nil
}

// rtveAlphabet walks a chunk's keyword and preamble into its alphabet, taking
// one character and then dropping a run that grows 1, 2, 3, 0 and repeats.
// A hundred and eighty-odd characters of material yield the seventy-odd
// entries a URL is indexed out of.
//
// Bytes rather than runes: a PNG tEXt chunk is Latin-1 by specification, so
// one byte is one character however the payload happens to decode.
func rtveAlphabet(keyword, preamble []byte) []byte {
	alphabet := make([]byte, 0, len(keyword)+len(preamble))
	var drop, gap int
	for _, source := range [2][]byte{keyword, preamble} {
		for _, c := range source {
			if drop > 0 {
				drop--
				continue
			}
			alphabet = append(alphabet, c)
			gap = (gap + 1) % 4
			drop = gap
		}
	}
	return alphabet
}
