package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// NPOStart resolves npo.nl/start, the Dutch public broadcasters' catch-up
// service.
//
// The plumbing is the friendliest of any broadcaster here: four plain JSON
// endpoints, none of them signed, none of them needing a cookie, a key or
// even a Referer. A slug names an episode, the episode names a product id,
// the product id mints a one-programme JWT, and the JWT buys a playlist URL.
// A series is modelled just as explicitly, seasons and all, so this is
// svtplay's and nrk's sibling in shape.
//
// What is its own is that most of the catalogue is locked, and the extractor
// has to say so rather than discover it late. NPO serves roughly three
// quarters of what it publishes under FairPlay (HLS) or Widevine (DASH), and
// the stream response names the scheme in a field of its own. That check is
// the whole gate: it happens before a single byte of media is requested, and
// the commonest outcome of pasting an NPO link here is a clean refusal. The
// remaining quarter is not a scrap — it is the entire current-affairs and
// consumer-programme catalogue, which is what a downloader is wanted for.
// Asking again with profileName "dash" is not an escape and is not tried:
// that is the same asset under Widevine CENC.
//
// The one thing not to assume about that is that it follows the programme.
// It is applied per asset, and a single season of one documentary strand
// answered with seven episodes in the clear and three under FairPlay — the
// locked ones the oldest, as though the right to serve them openly had
// lapsed underneath them. So a series is expanded and asked about in full,
// and a job of half refusals is the expected shape rather than a fault.
//
// DRM is not the only refusal, either. A slice of the catalogue is licensed
// for the Netherlands alone and answers 451, and another slice sits outside
// its availability window and answers 412 — both with a Dutch sentence meant
// for a viewer, which is passed through untranslated for the same reason
// NRK's is: which programme, which country and until when is precisely what
// the caller needs, and only NPO knows it.
//
// What is unencrypted is packaged by Unified Streaming with no EXT-X-MEDIA
// entry anywhere in the master playlist, so every video variant carries its
// audio in the same segments and concatenating one yields a playable file.
// The trailing audio-only variant declares no video codec, so hlsVariant
// rejects it as unmuxed and sorts it last without any help from here.
//
// Two smaller things decided the naming. NPO titles every instalment of a
// daily strand with the programme's own name — two hundred and twenty-two
// episodes of Nieuwsuur are all called "Nieuwsuur" — so a title that only
// repeats the series name is replaced by the broadcast date, without which
// a season would download as one file overwritten two hundred times. And a
// season's human label is null for a good part of the catalogue, so the
// folder falls back to the season's slug.
type NPOStart struct {
	client *httpx.Client
}

const (
	npoRoot = "https://npo.nl/start"
	// npoDomainAPI is the site's own read-only API. Every call is a slug or
	// a guid in the query string and nothing else.
	npoDomainAPI = npoRoot + "/api/domain/"
	// npoStreamAPI turns a player token into a playlist URL. It belongs to
	// the player rather than to the site, which is why it lives elsewhere.
	npoStreamAPI = "https://prod.npoplayer.nl/stream-link"

	npoProgramEndpoint = "program-detail"
	npoSeasonsEndpoint = "series-seasons"
	// npoListEndpoint takes `guid`, and nothing else will do: `seasonGuid`
	// is accepted, silently ignored, and answered with the upstream's 404
	// for a season called "undefined".
	npoListEndpoint  = "programs-by-season"
	npoTokenEndpoint = "player-token"

	// Path words the site builds its URLs from.
	npoStartPath   = "start"
	npoSeriesPath  = "serie"
	npoPlayPath    = "afspelen"
	npoEpisodesTab = "afleveringen"

	// npoMaxEpisodes bounds how many episodes one series URL expands to, and
	// it is a good deal lower than the equivalent on the other broadcasters
	// here. The reason is worth knowing before raising it.
	//
	// Every episode costs a player token of its own, so an expansion is that
	// many requests to npo.nl and there is no way to make it fewer: a
	// segmented file has to be resolved while extracting, since File.Resolve
	// hands back a single URL and there is no late binding for a segment
	// list. And npo.nl is behind an edge that stops answering at around two
	// hundred requests — measured at one connection and six a second, so it
	// counts requests rather than punishing parallelism — and then refuses
	// everything for minutes. Being cut off is far worse than a bounded
	// answer, because what comes back is an arbitrary fraction of the series
	// with nothing to mark it as one.
	//
	// So this leaves room to be wrong, and room to be asked again: an
	// expansion that spent the whole budget would succeed and leave the next
	// thing the user pasted failing. A series with more instalments than
	// this is offered a season at a time, which is how the site divides it
	// too.
	npoMaxEpisodes = 120

	// npoMaxSeasons bounds how many season listings one series URL reads.
	// Seasons arrive newest first and the episodes behind them are truncated
	// to the above anyway, so all that reading further buys is spending the
	// same request budget the episodes need. The longest-running strand
	// looked at here had twenty-three.
	npoMaxSeasons = 30
)

// NewNPOStart builds the NPO Start extractor.
func NewNPOStart(client *httpx.Client) *NPOStart { return &NPOStart{client: client} }

func (n *NPOStart) Name() string { return "npo" }

// Match takes the catch-up service and the domain it used to live on.
//
// npo.nl carries the corporation's own pages beside the player, and this
// resolves none of them, so only paths under /start are claimed — leaving
// the rest to the fallback, which will at least say something true about
// them. npostart.nl is claimed whole: every address on it was a player link,
// and every one of them redirects.
func (n *NPOStart) Match(u *url.URL) bool {
	if util.HostMatches(u.Host, "npostart.nl") {
		return true
	}
	if !util.HostMatches(u.Host, "npo.nl") {
		return false
	}
	segs := util.PathSegments(u)
	return len(segs) > 0 && segs[0] == npoStartPath
}

// Extract handles a single episode, one season, or a whole series.
func (n *NPOStart) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	target := npoRoute(u)
	if target.empty() {
		// Either a pre-relaunch npostart.nl link, which names a product id
		// rather than a slug, or some shape of npo.nl/start this does not
		// read. Both are answered with a redirect to the current URL, so
		// where the browser would have landed is asked for directly.
		canonical, err := n.canonical(ctx, u)
		if err != nil {
			return nil, err
		}
		if target = npoRoute(canonical); target.empty() {
			return nil, fmt.Errorf("npo: %s names no episode or series", u.Redacted())
		}
	}
	if target.episode != "" {
		return n.programme(ctx, target.episode)
	}
	return n.series(ctx, target)
}

// npoTarget is what an npo.nl/start URL points at.
type npoTarget struct {
	// episode is the episode slug, when the URL names one.
	episode string
	// series is the series slug.
	series string
	// season is what followed the series slug in the path. It is a
	// candidate rather than a season: the site's tabs sit in the same
	// position, and only the season list can tell the two apart.
	season string
}

func (t npoTarget) empty() bool { return t.episode == "" && t.series == "" }

// npoRoute reads an npo.nl/start URL. Five shapes reach this, because the
// site answers the older ones with a redirect rather than retiring them, and
// a redirect is no help to something that never fetched the page:
//
//	/start/afspelen/<episode>                     the player, and the
//	                                              address every other
//	                                              episode link ends at
//	/start/serie/<series>/<season>/<ep>/afspelen  the long form still in
//	                                              circulation
//	/start/serie/<series>                         the whole series
//	/start/serie/<series>/afleveringen            the same page, named
//	/start/serie/<series>[/afleveringen]/<season> one season of it
func npoRoute(u *url.URL) npoTarget {
	segs := util.PathSegments(u)
	if len(segs) > 0 && segs[0] == npoStartPath {
		segs = segs[1:]
	}
	if len(segs) < 2 {
		return npoTarget{}
	}
	switch {
	case segs[len(segs)-1] == npoPlayPath:
		return npoTarget{episode: segs[len(segs)-2]}
	case segs[0] == npoPlayPath:
		return npoTarget{episode: segs[1]}
	case segs[0] != npoSeriesPath:
		return npoTarget{}
	}

	target := npoTarget{series: segs[1]}
	// Whatever follows the series slug is either a tab or a season, and the
	// site puts both in the same place. Keeping a list of tab names would
	// mean trailing whichever one NPO adds next, so the candidate is looked
	// up in the season list instead and one that names no season is a tab.
	for _, seg := range segs[2:] {
		if seg != npoEpisodesTab {
			target.season = seg
			break
		}
	}
	return target
}

// canonical follows a link the site redirects and reads back where it
// landed. The page that ends the chain states its own address in og:url,
// which is the same answer a browser following the link would arrive at, and
// saves this from having to know the redirector in between.
func (n *NPOStart) canonical(ctx context.Context, u *url.URL) (*url.URL, error) {
	doc, err := n.client.GetString(ctx, u.String(), httpx.Referer(npoRoot))
	if err != nil {
		return nil, fmt.Errorf("npo: fetch %s: %w", u.Redacted(), err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("npo: parse %s: %w", u.Redacted(), err)
	}
	landed := metaContent(root, "og:url")
	if landed == "" {
		return nil, fmt.Errorf("npo: %s names no episode or series", u.Redacted())
	}
	return ParseURL(landed)
}

// programme resolves one episode into a job of its own.
func (n *NPOStart) programme(ctx context.Context, slug string) (*Result, error) {
	var doc npoProgram
	if err := n.fetch(ctx, npoProgramEndpoint, "slug", slug, &doc); err != nil {
		return nil, fmt.Errorf("npo: episode %s: %w", slug, err)
	}

	file, err := n.stream(ctx, doc.ProductID)
	if err != nil {
		return nil, err
	}
	// A lone file lands straight in the download directory with no folder to
	// say what it belongs to, so the name carries the series as well.
	title := util.FirstNonEmpty(doc.name(), slug)
	file.Name = title + file.Name
	return &Result{Title: title, Files: []File{*file}}, nil
}

// series expands a series, or one season of it, into one file per episode.
func (n *NPOStart) series(ctx context.Context, target npoTarget) (*Result, error) {
	seasons, err := n.seasons(ctx, target.series)
	if err != nil {
		return nil, err
	}
	if len(seasons) == 0 {
		return nil, fmt.Errorf("npo: %s is not a series", target.series)
	}

	named := false
	if target.season != "" {
		var only npoSeason
		if only, named = npoPick(seasons, target.season); named {
			seasons = []npoSeason{only}
		}
	}
	if len(seasons) > npoMaxSeasons {
		seasons = seasons[:npoMaxSeasons]
	}

	episodes := n.listings(ctx, seasons)
	if len(episodes) == 0 {
		return nil, fmt.Errorf("npo: %s lists no episodes", target.series)
	}
	if len(episodes) > npoMaxEpisodes {
		episodes = episodes[:npoMaxEpisodes]
	}

	title := util.FirstNonEmpty(episodes[0].series, target.series)
	if named {
		// One season goes in the job title rather than in a folder below it,
		// which would say nothing; and two seasons fetched separately would
		// otherwise share a folder, where episode names that start again at
		// the top of the season collide.
		title = npoJoin(title, seasons[0].name())
	}

	result := &Result{Title: title}
	var refused string
	var throttled int
	for _, ep := range n.resolve(ctx, episodes) {
		if ep.err != nil {
			// One locked or withdrawn episode must not sink the rest — a
			// mixed series is the normal case here, not an edge one. The
			// reason is kept, though: when nothing at all is playable it is
			// for one reason, and NPO's own words for it once are worth more
			// than several hundred identical failures.
			var refusal *npoUnavailable
			switch {
			case errors.As(ep.err, &refusal):
				if refused == "" {
					refused = refusal.reason
				}
			case errors.Is(ep.err, errNPOThrottled):
				throttled++
			}
			continue
		}
		ep.file.Name = util.FirstNonEmpty(ep.ref.name, ep.ref.productID) + ep.file.Name
		if len(seasons) > 1 {
			ep.file.Dir = ep.ref.season
		}
		result.Files = append(result.Files, *ep.file)
	}

	// Being turned away by the edge is not a refusal of anything, and must
	// never be reported as one. What it produces is a job holding however
	// many episodes happened to be asked before the shutter came down, with
	// nothing about it to say the rest exist — so it is an error, and the
	// one thing the caller can do about it is said outright.
	if throttled > 0 {
		return nil, fmt.Errorf("npo: %d of the %d episodes of %s went unasked: %w",
			throttled, len(episodes), target.series, errNPOThrottled)
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("npo: none of the %d episodes of %s are playable: %s",
			len(episodes), target.series,
			util.FirstNonEmpty(refused, "the catalogue lists them but the player offers no stream"))
	}
	return result, nil
}

// npoEpisodeRef is one episode of a series, with the season it belongs to.
type npoEpisodeRef struct {
	productID string
	name      string
	series    string
	season    string
}

// npoResolved pairs an episode with what resolving it produced. FanOut drops
// a fetch that returned an error, and a refusal is exactly what must not be
// dropped here, so the error travels as a value instead.
type npoResolved struct {
	ref  npoEpisodeRef
	file *File
	err  error
}

// resolve turns a series' episodes into files, all at once.
//
// Where NRK and SVT reach a stream in one request, this needs four — token,
// stream link, master playlist, variant playlist — so a season resolved
// strictly in turn would spend minutes before a single byte moved. FanOut
// collects by index, so the files stay in the order the site listed them
// however the requests interleave.
//
// Every episode is asked, and there is no shortcut past the ones that will
// be refused, tempting as it is when three quarters of the catalogue is
// encrypted. DRM here is a property of the asset and not of the title: one
// season of a documentary strand answered with seven clear episodes and
// three FairPlay ones, the locked ones being the oldest, as though the
// rights to serve them in the clear had lapsed underneath them. So a series
// judged from one episode would be judged wrong, and the only honest way to
// find out which instalments are downloadable is to ask about each.
func (n *NPOStart) resolve(ctx context.Context, episodes []npoEpisodeRef) []npoResolved {
	return FanOut(ctx, episodes, func(ctx context.Context, ref npoEpisodeRef) ([]npoResolved, error) {
		file, err := n.stream(ctx, ref.productID)
		return []npoResolved{{ref: ref, file: file, err: err}}, nil
	})
}

// seasons lists a series' seasons, newest first.
func (n *NPOStart) seasons(ctx context.Context, slug string) ([]npoSeason, error) {
	var seasons []npoSeason
	if err := n.fetch(ctx, npoSeasonsEndpoint, "slug", slug, &seasons); err != nil {
		return nil, fmt.Errorf("npo: seasons of %s: %w", slug, err)
	}
	return seasons, nil
}

// listings reads every season's episode list. These are independent requests
// and a news strand has twenty of them, so they go out together.
func (n *NPOStart) listings(ctx context.Context, seasons []npoSeason) []npoEpisodeRef {
	return FanOut(ctx, seasons, func(ctx context.Context, season npoSeason) ([]npoEpisodeRef, error) {
		var listing []npoProgram
		if err := n.fetch(ctx, npoListEndpoint, "guid", season.GUID, &listing); err != nil {
			return nil, err
		}
		out := make([]npoEpisodeRef, 0, len(listing))
		for _, ep := range listing {
			if ep.ProductID == "" {
				continue
			}
			out = append(out, npoEpisodeRef{
				productID: ep.ProductID,
				name:      ep.name(),
				series:    ep.Series.Title,
				season:    season.name(),
			})
		}
		return out, nil
	})
}

// stream resolves one product id to a playable segment list. The returned
// File carries only the extension in Name; the caller supplies the stem,
// which differs between a standalone episode and one of a series.
func (n *NPOStart) stream(ctx context.Context, productID string) (*File, error) {
	if productID == "" {
		return nil, errors.New("npo: the catalogue names no product for this episode")
	}

	var token struct {
		JWT string `json:"jwt"`
	}
	if err := n.fetch(ctx, npoTokenEndpoint, "productId", productID, &token); err != nil {
		return nil, fmt.Errorf("npo: player token for %s: %w", productID, err)
	}
	if token.JWT == "" {
		return nil, fmt.Errorf("npo: no player token for %s", productID)
	}

	// The token goes in Authorization raw, with no "Bearer" in front of it,
	// and it is minted for this product alone — its subject is the product
	// id — so it cannot be reused for the next episode. The body cannot be
	// empty either: a stream-link request that names no profile is refused
	// by the CDN rather than defaulted.
	var link npoStreamLink
	err := n.client.PostJSON(ctx, npoStreamAPI,
		httpx.Header{httpx.HeaderAuthorization: token.JWT, httpx.HeaderReferer: npoRoot},
		map[string]string{"profileName": "hls", "referrerUrl": npoRoot}, &link)
	if err != nil {
		if reason := npoReason(err); reason != "" {
			return nil, &npoUnavailable{productID: productID, reason: reason}
		}
		return nil, fmt.Errorf("npo: stream for %s: %w", productID, err)
	}

	if scheme, ok := link.Stream.encrypted(); ok {
		return nil, &npoUnavailable{productID: productID,
			reason: "DRM protected — NPO serves it under " + scheme +
				", and there is no key here to unlock it with"}
	}
	if link.Stream.URL == "" {
		return nil, fmt.Errorf("npo: %s offers no stream", productID)
	}

	// The playlist URL carries a signed token in its path with a day's life
	// in it, which outlasts any queue this program will hold, so the
	// segments are resolved now rather than through File.Resolve.
	headers := httpx.Referer(npoRoot)
	segments, variant, err := resolvePlaylist(ctx, n.client, link.Stream.URL, headers)
	if err != nil {
		return nil, fmt.Errorf("npo: %s: %w", productID, err)
	}
	return &File{
		Name:     playlistExtension(variant),
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}, nil
}

// errNPONotFound is NPO answering a slug it does not know.
var errNPONotFound = errors.New("no such episode or series")

// errNPOThrottled is the edge in front of npo.nl turning a caller away, said
// in words. Left as it arrives it is a page of HTML inside a 403 with
// nothing in it a caller could act on, and it is reachable by ordinary use
// rather than by abuse: the site stops answering for a few minutes at around
// two hundred requests, and expanding a series needs one per episode.
var errNPOThrottled = errors.New("npo.nl has stopped answering for the moment — it throttles " +
	"a caller at roughly two hundred requests, and expanding a series costs one per episode. " +
	"Wait a few minutes, then ask for less at a time: one season rather than a whole series")

// npoThrottling tells the edge's refusal from NPO's own.
//
// Both arrive as a 403, and they mean opposite things: the player answers
// one when it will not authorise a particular programme, and says so in a
// sentence, while the edge answers one when it will not serve this caller
// anything at all. Only the second is worth waiting out.
func npoThrottling(err error) bool {
	return httpx.HasStatus(err, http.StatusForbidden) && npoReason(err) == ""
}

// fetch reads one of the site's domain endpoints.
//
// A slug NPO has never heard of is not a 404. The endpoint answers 200 with
// the JSON string "" where the document should be, which decoded straight
// into a struct would surface as a complaint about a string not being an
// object — true, and no use to anybody. So the body is taken raw first and
// the empty document recognised for what it is.
func (n *NPOStart) fetch(ctx context.Context, endpoint, key, value string, out any) error {
	var raw json.RawMessage
	target := npoDomainAPI + endpoint + "?" + key + "=" + url.QueryEscape(value)
	if err := n.client.GetJSON(ctx, target, httpx.Referer(npoRoot), &raw); err != nil {
		if npoThrottling(err) {
			return errNPOThrottled
		}
		return err
	}
	if npoEmpty(raw) {
		return errNPONotFound
	}
	return json.Unmarshal(raw, out)
}

// npoEmpty reports the site's way of saying it found nothing.
func npoEmpty(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", `""`, "null":
		return true
	}
	return false
}

// npoUnavailable is the player declining to serve something. It is a type
// rather than a message so that expanding a series can report the reason
// once instead of several hundred times, which matters more here than on the
// other broadcasters: a whole-series expansion routinely refuses most of what
// it lists.
type npoUnavailable struct {
	productID string
	reason    string
}

func (e *npoUnavailable) Error() string { return "npo: " + e.productID + ": " + e.reason }

// npoReason is the player's own explanation for a refusal, dug out of the
// body of the response that carried it.
//
// The player declines with a status and a Dutch sentence written for a
// viewer, and both are worth more than either alone. A programme licensed
// only for the Netherlands answers 451 with "Dit programma mag niet bekeken
// worden vanaf jouw locatie"; one outside its window answers 412 with a
// sentence that deliberately will not say which end of the window was
// missed, and the dates beside it that settle the question. Translating
// either would be inventing an answer only NPO has, so the sentence is
// passed through as written and the dates are appended to it.
func npoReason(err error) string {
	var status *httpx.StatusError
	if !errors.As(err, &status) {
		return ""
	}
	var refusal npoRefusal
	if json.Unmarshal([]byte(status.Body), &refusal) != nil {
		return ""
	}
	return refusal.message()
}

// npoRefusal is the document a refusal arrives in. There are two envelopes
// and they disagree about where the sentence goes: a programme the player
// will not serve puts it in "body", while a rejected token puts it in
// "message". Reading only the first would leave an authorisation failure
// looking like an edge that had stopped answering, which is a different
// problem with a different remedy.
type npoRefusal struct {
	Body    string `json:"body"`
	Message string `json:"message"`
	Context struct {
		From  string `json:"programAvailableFrom"`
		Until string `json:"programAvailableUntil"`
	} `json:"context"`
}

func (r npoRefusal) message() string {
	message := strings.TrimSpace(util.FirstNonEmpty(r.Body, r.Message))
	from, until := npoDay(r.Context.From), npoDay(r.Context.Until)
	switch {
	case message == "":
		return ""
	case from != "" && until != "":
		return message + " (available from " + from + " to " + until + ")"
	case until != "":
		return message + " (available until " + until + ")"
	case from != "":
		return message + " (available from " + from + ")"
	}
	return message
}

// npoDay keeps the date of an availability timestamp and drops the time,
// which is more precision than a person reading a refusal has any use for.
func npoDay(stamp string) string {
	day, _, _ := strings.Cut(strings.TrimSpace(stamp), "T")
	return day
}

// npoStreamLink is the part of a stream-link response this needs.
type npoStreamLink struct {
	Stream npoStream `json:"stream"`
}

// npoStream is the stream the player was given.
type npoStream struct {
	URL     string `json:"streamURL"`
	Profile string `json:"streamProfile"`
	DRMType string `json:"drmType"`
	// DRM is the licence server and certificate the player would need. It is
	// read as raw JSON because only its presence matters here.
	DRM json.RawMessage `json:"drm"`
}

// encrypted names the scheme guarding this stream, and reports whether there
// is one.
//
// Both fields answer and both are read, because neither is quite a flag.
// drmType names the scheme — "fairplay" for the HLS profile, "widevine" for
// DASH — but a clear response omits the key altogether rather than sending
// it null, so a rename upstream would read as unencrypted. The drm object
// beside it is null exactly when there is nothing to unlock, which is the
// corroborating half: if either says protected, it is.
func (s npoStream) encrypted() (string, bool) {
	if scheme := strings.TrimSpace(s.DRMType); scheme != "" {
		return scheme, true
	}
	if !npoEmpty(s.DRM) {
		return "a scheme it does not name", true
	}
	return "", false
}

// npoSeason is one season of a series.
type npoSeason struct {
	GUID  string `json:"guid"`
	Slug  string `json:"slug"`
	Label string `json:"label"`
	Key   string `json:"seasonKey"`
}

// name is what the season's folder is called.
//
// The label is the only field written for a person to read, and it is null
// for a good part of the catalogue. The slug is the fallback rather than
// seasonKey because the key is the site's internal numbering and means
// different things on different series — "26" on one and "2026" on the next
// — while the slug is always filled in and unique within the series, which
// is what a folder name has to be.
func (s npoSeason) name() string { return util.FirstNonEmpty(s.Label, s.Slug, s.Key) }

// npoPick finds the season a URL named, if it named one at all.
func npoPick(seasons []npoSeason, slug string) (npoSeason, bool) {
	for _, season := range seasons {
		if strings.EqualFold(season.Slug, slug) {
			return season, true
		}
	}
	return npoSeason{}, false
}

// npoProgram is one episode, as both the detail endpoint and a season
// listing describe it. Each entry names its own series, so expanding a
// series needs no second request to find out what it is called.
type npoProgram struct {
	Title     string `json:"title"`
	Slug      string `json:"slug"`
	ProductID string `json:"productId"`
	Published int64  `json:"publishedDateTime"`
	Series    struct {
		Slug  string `json:"slug"`
		Title string `json:"title"`
	} `json:"series"`
}

// name is what this episode's file is called.
//
// NPO titles every instalment of a daily strand with the programme's own
// name, so a season of Nieuwsuur is two hundred and twenty-two episodes all
// called "Nieuwsuur" — one file, written over and over. When the title says
// nothing the series has not already said, the broadcast date takes its
// place, which is what a viewer calls that episode anyway.
func (p npoProgram) name() string {
	if p.Title == "" || strings.EqualFold(p.Series.Title, p.Title) {
		return npoJoin(util.FirstNonEmpty(p.Series.Title, p.Title), npoDate(p.Published))
	}
	return npoJoin(p.Series.Title, p.Title)
}

// npoJoin names something by its series and what distinguishes it, and copes
// with either half being missing or the two being the same.
func npoJoin(series, part string) string {
	switch {
	case part == "":
		return series
	case series == "" || strings.EqualFold(series, part):
		return part
	}
	return series + " - " + part
}

// npoDate renders a broadcast timestamp as a plain date.
//
// UTC rather than the machine's own zone, so an episode is named the same
// thing wherever it is downloaded. A late-night Dutch broadcast therefore
// lands on the day before, which costs nothing that matters: the date is
// here to tell two instalments apart and to be recognised, not to be cited.
func npoDate(unix int64) string {
	if unix <= 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.DateOnly)
}
