// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// ZDF resolves zdf.de videos and the pages that list them.
//
// Germany's second public broadcaster describes the same video through two
// endpoints, and choosing between them is the decision everything below
// follows from. The newer one, api.zdf.de's PTMD, is much the richer
// document — an exact byte count per rendition and a DRM flag beside it —
// and it is gated behind an `Api-Auth` bearer token that each zdf.de page
// prints in its own markup and that expires within the day. The older one,
// zdf-prod-futura.zdf.de, answers anybody: no token, no cookie, no referer.
// A downloader that must scrape a rotating secret before it can fetch
// anything is a downloader that stops working the week the field is
// renamed, so futura is what this reads. The cost of that is one thing, and
// it is stated where it falls, on Size.
//
// The reward is the pleasant case: ZDF publishes its 1080p rendition as a
// progressive MP4, a plain rangeable file, so the segmented engine, resume
// and the already-downloaded check all apply. The adaptive playlist beside
// it is a fallback for the rare item that has no file, and it is checked
// rather than trusted — see below.
//
// Two things in that rendition list are traps.
//
// The first is `class`. Anything ZDF has recorded an audio description for
// publishes every quality twice, once as "main" and once as "ad", differing
// in nothing that a ranking by quality can see. Take the wrong one and the
// whole film downloads with a narrator talking over it. So the class is
// filtered first and the quality decides only afterwards.
//
// The second is that those same items publish their master playlist with
// the audio in EXT-X-MEDIA renditions of its own, and concatenating a
// variant that names one alone yields a silent video that looks like a
// finished download. The renditions come with the video instead, for the
// downloader to join to it, and only a variant naming no audio anywhere is
// refused.
//
// Geo-blocking is the last of it, and unlike NRK — which writes a sentence
// for the viewer — ZDF states it as a code: geoLocation is "none", "de",
// "dach" or "ebu". That code is not by itself a refusal, and treating it as
// one would be the worse bug: most of this catalogue is licensed for
// Germany and it is people in Germany who will mostly be downloading it. It
// says which question to ask, not what the answer is. See zdfReach for who
// asks it.
type ZDF struct {
	about
	client *httpx.Client
}

const (
	zdfRoot = "https://www.zdf.de"

	// zdfDocumentAPI is the open metadata endpoint, keyed by the id that is
	// the last segment of every zdf.de URL.
	zdfDocumentAPI = "https://zdf-prod-futura.zdf.de/mediathekV2/document/"

	// zdfProgressive and zdfPlaylist are the two rendition types this
	// accepts, and the list is deliberately closed rather than open.
	//
	// Nothing futura publishes says whether a rendition is encrypted — the
	// token-gated PTMD carries a hasVideoDrm flag per rendition, futura
	// carries no such field at all — so there is no equivalent of NRK's
	// asset.Encrypted check to make. What stands in for it is that these two
	// names mean a plain MP4 over HTTP and a plain MPEG-TS playlist. Whatever
	// ZDF encrypts will not arrive under either, and an unrecognised type is
	// passed over rather than attempted.
	zdfProgressive = "h264_aac_mp4_http_na_na"
	zdfPlaylist    = "h264_aac_ts_http_m3u8_http"

	// zdfUnrestricted is the geoLocation of something licensed everywhere.
	zdfUnrestricted = "none"

	// zdfMainAudio is the ordinary soundtrack and zdfDescribedAudio the
	// narration for blind viewers, published beside it at every quality.
	zdfMainAudio      = "main"
	zdfDescribedAudio = "ad"

	// zdfVideo is what a document calls itself when it is a programme rather
	// than a page, and what a listing entry calls itself when it is a
	// programme rather than a topic, a category or another show. The
	// vocabulary is shared, so the constant is too.
	zdfVideo = "video"

	// zdfYearDigits is the length of a season number that is a calendar year
	// rather than a position in a run.
	zdfYearDigits = 4
)

// zdfQualities are the rendition names, worst first. ZDF names its
// renditions rather than measuring them, and "auto" — which only the
// playlists carry — is the full ladder, so it belongs above the rest.
var zdfQualities = []string{"low", "med", "high", "veryhigh", "hd", "fhd", "auto"}

// zdfRegions spells out the codes geoLocation uses. The code is what the
// document carries and it is not what someone reading a refusal wants to
// see; one this does not know is passed through rather than guessed at.
var zdfRegions = map[string]string{
	"de":   "Germany",
	"dach": "Germany, Austria and Switzerland",
	"ebu":  "the European Broadcasting Union's member countries",
}

// NewZDF builds the ZDF extractor.
func NewZDF(client *httpx.Client) *ZDF {
	return &ZDF{client: client, about: about{name: "zdf", shape: zdfShape,
		recognises: "videos and show pages (`/…/<slug>-<number>`) of the ZDF Mediathek, whose page data names its PTMD player templates, " +
			"and documents of its metadata API (`/mediathekV2/document/<id>`)"}}
}

// zdfPage is a page whose last segment is a document id, <slug>-<number>,
// filed under at least one section: /<section>/…/<slug>-<number>. That is
// nearly every article URL on the web, so it is the form recognised once
// the page is shown to be the Mediathek's, not one to follow as a link.
var zdfPage = pathShape(`^/(?:[a-z0-9-]+/)+[a-z0-9-]+-[0-9]+/?$`)

// zdfShape is the link form: a document id filed under /video/ or under
// one of the Mediathek's programme sections, or the document itself as the
// metadata API serves it (zdfDocumentAPI), whose last segment is the id
// Extract asks for. A news site's /sport/… or /news/… is not among them.
var zdfShape = pathShape(`^/(?:video|play|serien|filme|dokus|dokumentation|shows?|animation|talk|magazine|kinder|`+
	`comedy|krimi|wissen|kultur|geschichte|natur|ratgeber|unterhaltung)/(?:[a-z0-9-]+/)*[a-z0-9-]+-[0-9]+/?$`,
	zdfDocumentPath)

// zdfDocumentPath is a document on the metadata API's host.
const zdfDocumentPath = `^/mediathekV2/document/[A-Za-z0-9_-]+/?$`

// Recognise takes a Mediathek page ending in a document id: its page data
// configures the player's media service (ptmdService, ptmdTemplate).
func (z *ZDF) Recognise(_ context.Context, p *Page) bool {
	return zdfPage(p.Final) && p.HasAny("ptmdTemplate", "ptmdService") && p.Named("ZDF")
}

// Extract handles a single video, a show page that lists many, and the hub
// pages that are neither.
//
// Which of the three a URL is does not have to be read off its path: every
// zdf.de URL ends in the id of a document, and the document says what it is.
func (z *ZDF) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id := zdfDocumentID(u)
	if id == "" {
		return nil, fmt.Errorf("zdf: no document id in %s", u.Redacted())
	}
	reach := newZDFReach(z.client)

	doc, err := z.document(ctx, id)
	if err != nil {
		// The page may still name what it plays. Futura's own complaint is
		// kept for the case where it does not, being the more direct of the
		// two.
		res, pageErr := z.fromPage(ctx, u, id, "", reach)
		if pageErr != nil {
			return nil, err
		}
		return res, nil
	}

	// A document that calls itself a video is answered as one even when it
	// carries no renditions, because it is then the only thing that knows
	// why — a programme still to be broadcast says so, in a sentence, and
	// looking for a stream on the page instead would lose that.
	if strings.EqualFold(doc.Document.Type, zdfVideo) {
		return z.single(ctx, &doc.Document, id, reach)
	}
	if refs := doc.videos(); len(refs) > 0 {
		return z.expand(ctx, u, cmp.Or(doc.Document.Titel, id), refs, reach)
	}
	// A film or a stage recording is filed under a page of its own that
	// carries no renditions and lists nothing, because the one video it
	// plays lives at an id the URL never mentions. Only the page names it.
	return z.fromPage(ctx, u, id, doc.Document.Titel, reach)
}

// single resolves one video into a job of its own.
func (z *ZDF) single(ctx context.Context, doc *zdfDocument, fallback string, reach *zdfReach) (*Result, error) {
	title := cmp.Or(doc.Titel, fallback)
	file, err := z.file(ctx, doc, reach)
	if err != nil {
		// file's refusals are written to follow the programme's name as a
		// sentence about it, so there is no colon between the two.
		return nil, fmt.Errorf("zdf: %s %w", title, err)
	}
	return soleFile(title, file), nil
}

// fromPage resolves a page by the videos it links to, for the pages futura
// describes without listing anything.
//
// Only links whose middle segment is this page's own id are followed. That
// is what keeps a hub page's recommendations out: a /video/ link filed under
// some other id belongs to some other page.
func (z *ZDF) fromPage(ctx context.Context, u *url.URL, id, title string, reach *zdfReach) (*Result, error) {
	page, _, err := fetchDocument(ctx, z.client, u, zdfHeaders())
	if err != nil {
		return nil, fmt.Errorf("zdf: fetch %s: %w", u.Redacted(), err)
	}
	ids := zdfVideoLinks(page, id)
	if len(ids) == 0 {
		return nil, fmt.Errorf("zdf: %s plays nothing and lists nothing", u.Redacted())
	}

	// A film's page is a film, not a listing of one, and saying so is what
	// lets its refusals name it rather than counting it.
	if len(ids) == 1 {
		doc, err := z.document(ctx, ids[0])
		if err != nil {
			return nil, err
		}
		return z.single(ctx, &doc.Document, cmp.Or(title, ids[0]), reach)
	}

	refs := make([]broadcastEpisode, 0, len(ids))
	for _, videoID := range ids {
		refs = append(refs, broadcastEpisode{id: videoID})
	}
	return z.expand(ctx, u, cmp.Or(title, pageTitle(page)), refs, reach)
}

// expand resolves a listing's videos into one file each.
func (z *ZDF) expand(ctx context.Context, u *url.URL, title string, refs []broadcastEpisode, reach *zdfReach) (*Result, error) {
	return listSeries(ctx, title, series[broadcastEpisode]{
		site: "zdf", noun: "videos", link: u.Redacted(),
		items: refs,
		label: func(ref broadcastEpisode) string {
			if ref.season == "" {
				return ""
			}
			return zdfSeasonFolder(ref.season)
		},
		// The listing's own name for a video wins: it is what the page
		// showed, and for a series it is the episode title where the
		// document's is occasionally the whole strand's.
		resolve: listedAs(func(ctx context.Context, id string) (*File, string, error) {
			return z.video(ctx, id, reach)
		}, nil),
		// "each" carries the reason, which is written as a sentence about
		// one video: when a whole run is refused it is refused the same way
		// all the way down, and ZDF's words for it beat anything concluded
		// here.
		unplayable: "each offers no stream",
	})
}

// video resolves one listed video, with its document's title. A document
// that will not load is passed over without a reason, since it says
// nothing about the others; ZDF's refusal of the video is the reason, as a
// sentence about each one.
func (z *ZDF) video(ctx context.Context, id string, reach *zdfReach) (*File, string, error) {
	doc, err := z.document(ctx, id)
	if err != nil {
		return nil, "", nil
	}
	file, err := z.file(ctx, &doc.Document, reach)
	if err != nil {
		// ZDF declining the video — nothing offered, or licensed
		// elsewhere — is a refusal, which the listing tallies by its
		// reason; anything else is a video that could not be read.
		_, noStream := errors.AsType[*zdfNoStream](err)
		_, blocked := errors.AsType[*zdfBlocked](err)
		if noStream || blocked {
			return nil, "", &itemRefusal{site: "zdf", title: doc.Document.Titel, reason: "each " + err.Error()}
		}
		return nil, "", fmt.Errorf("each %w", err)
	}
	return file, doc.Document.Titel, nil
}

// file turns a document's rendition list into something to fetch.
//
// The returned File carries only the extension in Name; the caller supplies
// the stem, which differs between a lone video and one entry of a listing.
// Its refusals are phrased as sentences about a programme rather than as
// complaints of their own, because both callers put them somewhere — after
// the programme's name, or after "each" — where that is what reads.
func (z *ZDF) file(ctx context.Context, doc *zdfDocument, reach *zdfReach) (*File, error) {
	best, ok := zdfPick(doc.Formitaeten)
	if !ok {
		return nil, &zdfNoStream{why: zdfWhyNot(doc)}
	}
	if err := reach.refuse(ctx, doc.GeoLocation, best.URL); err != nil {
		return nil, err
	}

	headers := zdfHeaders()
	if best.Type == zdfProgressive {
		// Size stays unknown on purpose. The document does print one —
		// downloadFilesize, "408 MB" — but it is the length of the rendition
		// ZDF itself offers for download, which is the 1628k file, while what
		// this takes is the 6628k one four times its size. A rounded figure
		// for a different file is not an approximation of this one, so it may
		// carry neither Size nor SizeApprox; the transfer learns the real
		// length from the response headers, and the skip check with it.
		return progressiveFile(best.URL, ".mp4", headers), nil
	}

	media, err := resolveMediaPlaylist(ctx, z.client, best.URL, headers)
	if err != nil {
		return nil, fmt.Errorf("could not be resolved: %w", err)
	}
	// A variant keeping its audio in a playlist of its own comes with it
	// (media.Audio), which the downloader joins to the video. Only one
	// naming no audio anywhere would be silent.
	if !media.Variant.muxed() && len(media.Audio) == 0 {
		return nil, errors.New("names no audio, in its renditions or beside them, and publishes " +
			"no progressive file to take instead")
	}
	return new(media.file(media.extensionOr(playlistExtension(media.Variant)), headers)), nil
}

// document reads one futura document.
func (z *ZDF) document(ctx context.Context, id string) (*zdfResponse, error) {
	var res zdfResponse
	if err := z.client.GetJSON(ctx, zdfDocumentAPI+url.PathEscape(id), zdfHeaders(), &res); err != nil {
		return nil, fmt.Errorf("zdf: document %s: %w", id, err)
	}
	return &res, nil
}

// zdfHeaders are what every request here carries. Nothing ZDF serves demands
// a referer, but the media hosts are shared with the player and sending what
// the player sends costs nothing.
func zdfHeaders() httpx.Header { return httpx.Referer(zdfRoot + "/") }

// zdfDocumentID is the id a zdf.de URL points at, which is always its last
// path segment — for a video, for a show, and for the film pages that are
// neither.
func zdfDocumentID(u *url.URL) string {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1]
}

// zdfVideoLinks finds the videos a page plays, keeping only those filed under
// pageID. Ids are returned once each, in the order the page had them.
func zdfVideoLinks(page, pageID string) []string {
	return matchedIDs(page, regexp.MustCompile(`/video/[A-Za-z0-9_-]+/`+regexp.QuoteMeta(pageID)+`/([A-Za-z0-9_-]+)`))
}

// zdfSeasonFolder names the folder a season's videos go in.
//
// ZDF numbers a drama's seasons 1, 2, 3 and a magazine's by the calendar
// year it went out in, so the two would otherwise sit side by side as "5"
// and "2023" meaning different things. A four-digit number is a year and
// needs no label; anything else gets one.
func zdfSeasonFolder(season string) string {
	if len(season) == zdfYearDigits {
		return season
	}
	return "Staffel " + season
}

// zdfPick chooses what to fetch: the progressive file when there is one, and
// the adaptive playlist only when there is not. A file is much the better
// answer — it is rangeable, so it resumes, splits across connections, and
// can be recognised on disk before a byte is requested.
func zdfPick(formats []zdfFormat) (zdfFormat, bool) {
	if best, ok := zdfBestOf(formats, zdfProgressive); ok {
		return best, true
	}
	return zdfBestOf(formats, zdfPlaylist)
}

// zdfBestOf picks the largest rendition of one kind.
func zdfBestOf(formats []zdfFormat, kind string) (zdfFormat, bool) {
	var (
		best  zdfFormat
		score int
		found bool
	)
	for _, f := range formats {
		if f.Type != kind || f.URL == "" || f.HasVideoDrm {
			continue
		}
		// The audio description is excluded outright rather than ranked
		// below the ordinary soundtrack. It is not a worse copy of the
		// programme, it is a different one, and it is published at every
		// quality — so a rendition preference alone will happily choose it.
		if strings.EqualFold(f.Class, zdfDescribedAudio) {
			continue
		}
		if s := zdfScore(f); !found || s > score {
			best, score, found = f, s, true
		}
	}
	return best, found
}

// zdfScore ranks a rendition by quality, with the ordinary soundtrack
// breaking a tie rather than deciding the order.
//
// That ordering is deliberate. The audio description is already gone by the
// time this runs, so what a tie is between is the ordinary soundtrack and a
// class ZDF has not used before — a version in the original language, say —
// and the larger rendition of an unfamiliar class beats a smaller one of a
// familiar class.
//
// The remaining tie is a rendition published twice, once on each of ZDF's
// two media hosts. Nothing in this document distinguishes them, so the order
// ZDF listed them in decides, and that costs nothing: the ladder only reaches
// its top rungs on one of the two, so the duplicate exists at qualities that
// were never going to win.
func zdfScore(f zdfFormat) int {
	rank := 2 * zdfRank(f.Quality)
	if strings.EqualFold(f.Class, zdfMainAudio) {
		rank++
	}
	return rank
}

// zdfRank orders a rendition by name. A name this does not recognise ranks
// below every one it does but above nothing at all, so a ladder ZDF relabels
// still yields a download.
func zdfRank(quality string) int { return rankIn(zdfQualities, quality, -1) }

// zdfWhyNot explains a document with nothing to fetch, in ZDF's own words.
//
// Two fields carry them and they complement each other exactly.
// availabilityInfo is the sentence about when and where a programme may be
// had — "verfügbar bis 25.07.2028, in Deutschland, von 22 bis 6 Uhr" is a
// watershed, and it is how a programme with an ordinary licence and an
// ordinary expiry date can still publish no renditions at all. videoInfo
// covers what that does not: a broadcast still to happen gives its time
// there.
//
// videoInfo carries something else as well, though — the listing line
// printed under a thumbnail, "43 min | 17.07.2026 | UT" — which explains
// nothing while sitting in the place where an explanation goes. The pipes
// are what tell the two apart.
func zdfWhyNot(doc *zdfDocument) string {
	if info := strings.TrimSpace(doc.AvailabilityInfo); info != "" {
		return " (" + info + ")"
	}
	if info := strings.TrimSpace(doc.VideoInfo); info != "" && !strings.Contains(info, "|") {
		return " (" + info + ")"
	}
	return ""
}

// zdfRestricted reports whether a geoLocation names a region at all.
func zdfRestricted(region string) bool {
	region = strings.TrimSpace(region)
	return region != "" && !strings.EqualFold(region, zdfUnrestricted)
}

// zdfNoStream is a document offering nothing to fetch, with ZDF's reason
// when it gives one, as a sentence about the programme.
type zdfNoStream struct{ why string }

func (e *zdfNoStream) Error() string { return "offers no stream" + e.why }

// zdfBlocked is ZDF licensing something for a region this address is not in.
// Like every refusal here it reads as a sentence about a programme, so that
// a listing where each entry is refused for the same licence can say so once
// instead of several hundred times.
type zdfBlocked struct {
	region string
}

func (e *zdfBlocked) Error() string {
	region := strings.ToLower(strings.TrimSpace(e.region))
	named, ok := zdfRegions[region]
	if !ok {
		// A code this does not know is quoted rather than guessed at.
		named = "the region ZDF calls " + region
	}
	return "is licensed for " + named + " only, and this address is outside that region"
}

// zdfReach answers, once per licence region, whether this address is inside
// it.
//
// The document's geoLocation names what a programme is licensed for, not
// whether the caller may have it, and those are different questions with
// different answers depending on where the caller is sitting. Refusing on
// the field alone would turn away the audience ZDF actually made this for.
//
// So the field decides which question to ask and a single request answers
// it, for every item licensed the same way: a season of thirty episodes
// costs one HEAD, and outside the region the licence is named while the
// queue is still being built rather than arriving thirty times as a bare
// 403.
type zdfReach struct {
	client *httpx.Client
	mu     sync.Mutex
	// inside is keyed by the geoLocation code. The lock is held across the
	// probe deliberately: videos resolve concurrently, and asking once is
	// the entire point.
	inside map[string]bool
}

func newZDFReach(client *httpx.Client) *zdfReach {
	return &zdfReach{client: client, inside: make(map[string]bool)}
}

// refuse reports the licence when media is out of reach, and nothing at all
// otherwise.
func (r *zdfReach) refuse(ctx context.Context, region, media string) error {
	if !zdfRestricted(region) {
		return nil
	}
	region = strings.ToLower(strings.TrimSpace(region))

	r.mu.Lock()
	defer r.mu.Unlock()

	inside, known := r.inside[region]
	if !known {
		inside = r.probe(ctx, media)
		r.inside[region] = inside
	}
	if inside {
		return nil
	}
	return &zdfBlocked{region: region}
}

// probe asks the media host for one file's headers.
//
// Only an outright refusal counts. A request that could not be made at all
// says nothing about where this address is, and reading "outside" into it
// would refuse a download that was going to work.
func (r *zdfReach) probe(ctx context.Context, media string) bool {
	status, err := r.client.StatusOnce(ctx, http.MethodHead, media, httpx.Referer(zdfRoot+"/"))
	return err != nil || status != http.StatusForbidden
}

// zdfResponse is one futura document and whatever it lists.
type zdfResponse struct {
	Document zdfDocument  `json:"document"`
	Cluster  []zdfCluster `json:"cluster"`
}

// videos collects the videos a page lists, once each and in the order it
// listed them.
//
// Deduplication is not tidiness. ZDF's clusters are editorial shelves rather
// than a season table — "Staffel 5", "Special-Folgen zu Halloween", whatever
// the editors are pushing this week — so the same episode routinely appears
// on two of them, and a page's featured entry is always a repeat of one
// further down.
func (r *zdfResponse) videos() []broadcastEpisode {
	var refs []broadcastEpisode
	seen := make(map[string]bool)
	for _, cluster := range r.Cluster {
		for _, teaser := range cluster.Teaser {
			if !strings.EqualFold(teaser.Type, zdfVideo) || teaser.ID == "" || seen[teaser.ID] {
				continue
			}
			seen[teaser.ID] = true
			refs = append(refs, broadcastEpisode{
				id:     teaser.ID,
				name:   teaser.Titel,
				season: strings.TrimSpace(teaser.SeasonNumber),
			})
		}
	}
	return refs
}

// zdfDocument is the part of a futura document this needs.
type zdfDocument struct {
	// Type separates a programme from a page. Pages come as "brand" for a
	// show or a film, "topic" for a strand and "category" for a section, and
	// nothing here needs to tell those three apart — only whether the thing
	// asked for is a programme, since a page's own job is to list some.
	Type        string `json:"type"`
	Titel       string `json:"titel"`
	GeoLocation string `json:"geoLocation"`
	// AvailabilityInfo and VideoInfo are sentences for the viewer, and
	// between them the only thing that explains a document publishing no
	// renditions. See zdfWhyNot for which says what.
	AvailabilityInfo string      `json:"availabilityInfo"`
	VideoInfo        string      `json:"videoInfo"`
	Formitaeten      []zdfFormat `json:"formitaeten"`
}

// zdfFormat is one rendition.
type zdfFormat struct {
	Type    string `json:"type"`
	Quality string `json:"quality"`
	// Class is "main" for the ordinary soundtrack and "ad" for the audio
	// description, and it is the field that must be read before the quality.
	Class string `json:"class"`
	URL   string `json:"url"`
	// HasVideoDrm is what api.zdf.de sets on an encrypted rendition. Futura
	// has never been seen to set it; it is read anyway because the day it
	// starts is the day this would otherwise download ciphertext, and the
	// cost of asking is a field.
	HasVideoDrm bool `json:"hasVideoDrm"`
}

// zdfCluster is one shelf of a page's listing.
type zdfCluster struct {
	Teaser []zdfTeaser `json:"teaser"`
}

// zdfTeaser is one entry on a shelf, which may be a video, a topic, a
// category or another show.
//
// SeasonNumber is a string rather than a number because that is what ZDF
// sends, zero-padded in some documents and not in others. Nothing here does
// arithmetic on it: it is a folder name, and a listing is internally
// consistent about how it spells one.
type zdfTeaser struct {
	Type         string `json:"type"`
	ID           string `json:"id"`
	Titel        string `json:"titel"`
	SeasonNumber string `json:"seasonNumber"`
}
