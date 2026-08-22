package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// PBS resolves pbs.org videos and the shows they belong to.
//
// The American public broadcaster is the least guarded of these: no sign-in,
// no geo gate, no token. Probed from outside the United States, the player
// page, the redirector and a byte range of the file itself all answer. What
// this host does instead of walling anything off is expire it — a title past
// its availability window is served as an ordinary page carrying two
// sentences of apology, and a show's back catalogue is full of them.
//
// Two facts decide the shape of everything below.
//
// The first is that the state a player needs is not an API response but a
// script assignment: player.pbs.org/portalplayer/<id>/ answers anyone with a
// page whose `window.videoBridge` holds the whole thing, protection included.
// It states `has_drm`, `is_playable` and `availability` as fields rather than
// leaving them to be inferred, so a refusal here is PBS's own word and not a
// guess made from a failed download.
//
// The second is that PBS's HLS master must not be touched. Every variant of
// it names AUDIO="multiple_audio_tracks", and that group's EXT-X-MEDIA entry
// has a playlist of its own — so the variants carry video and nothing else,
// while CODECS advertises "avc1.640028,mp4a.40.2" on all four exactly the way
// vimeo's does. Concatenating one, which is all the playlist engine here
// does, writes a silent video that finishes cleanly and looks like a complete
// download.
//
// That would ordinarily send this host to the external downloader, and does
// not, because PBS publishes a progressive MP4 beside the stream: one plain,
// rangeable, exactly-sized file. So this takes the best path there is —
// segmented, resumable, skippable — and the manifest never enters the
// picture. The cost is picture quality, and it is a real one: the MP4 tops
// out at 720p where the stream carries 1080p. Closing that gap needs a muxer,
// which is the whole trade this program declines to make.
type PBS struct {
	client *httpx.Client
}

const (
	pbsRoot = "https://www.pbs.org"

	// pbsPlayerAPI is the player state, keyed by the numeric id a video page
	// carries. /viralplayer/ and /widget/partnerplayer/ answer with the
	// identical document; this one is no more canonical than the others.
	pbsPlayerAPI = "https://player.pbs.org/portalplayer/"

	// pbsBridgeVar names the assignment holding that state.
	pbsBridgeVar = "window.videoBridge"

	// pbsAvailable is the one value of `availability` that means fetchable.
	pbsAvailable = "available"

	// pbsErrorClass marks the paragraph an error page explains itself in.
	pbsErrorClass = "error-message"

	// pbsMaxEpisodes bounds how many videos one show URL expands to.
	pbsMaxEpisodes = 500
)

// pbsPlayerID finds the numeric id a video page embeds its player with. The
// site has three names for the same player and uses them interchangeably.
var pbsPlayerID = regexp.MustCompile(`player\.pbs\.org/(?:widget/)?(?:viral|portal|partner)player/(\d+)`)

// pbsSeasonInfo reads the season out of `series_info`, which PBS writes for a
// viewer rather than for a parser: "Season 41 Episode 31" for a numbered
// episode, and "Special" for everything belonging to no season at all.
var pbsSeasonInfo = regexp.MustCompile(`(?i)^season\s+(\d+)`)

// NewPBS builds the PBS extractor.
func NewPBS(client *httpx.Client) *PBS { return &PBS{client: client} }

func (p *PBS) Name() string { return "pbs" }

// Match claims the page shapes rather than the domain. Several hosts under
// pbs.org serve bytes instead of pages — the signed-link redirector and the
// CDN behind it — and what they hand back is a plain rangeable file, which is
// precisely what the direct extractor already downloads. Claiming those here
// could only refuse them.
func (p *PBS) Match(u *url.URL) bool { return pbsTargetOf(u) != nil }

// Extract handles one video or a whole show.
func (p *PBS) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	target := pbsTargetOf(u)
	if target == nil {
		return nil, fmt.Errorf("pbs: no video or show in %s", u.Redacted())
	}
	if target.show != "" {
		return p.show(ctx, u, target.show)
	}

	id := target.id
	if id == "" {
		var err error
		if id, err = p.playerID(ctx, target.slug); err != nil {
			return nil, err
		}
	}
	episode, err := p.episode(ctx, id)
	if err != nil {
		return nil, err
	}

	// A lone file lands in the download directory with no folder around it to
	// say what it belongs to, so its name carries the programme as well. PBS
	// numbers the episodes of its long-running strands and calls them nothing
	// else, and "Episode 4" on its own names nothing.
	title := util.FirstNonEmpty(pbsJoin(episode.program, episode.title), id)
	episode.file.Name = title + ".mp4"
	return &Result{Title: title, Files: []File{episode.file}}, nil
}

// pbsTarget is what one pbs.org URL asks for.
type pbsTarget struct {
	// id is the numeric player id, when the URL names one outright.
	id string
	// slug names a video page, off which that id has to be read.
	slug string
	// show names a programme, which expands to what its page lists.
	show string
}

// pbsTargetOf reads what a URL asks for, and returns nothing for every other
// shape on the site — a signed media link deliberately among them, so it
// still reaches the direct extractor that can fetch it.
func pbsTargetOf(u *url.URL) *pbsTarget {
	if !util.HostMatches(u.Host, "pbs.org") {
		return nil
	}
	segs := util.PathSegments(u)

	// The player is served under three paths that answer identically, so its
	// id is taken as the last numeric segment rather than by a pattern per
	// path.
	if util.HostMatches(u.Host, "player.pbs.org") {
		for i := len(segs) - 1; i >= 0; i-- {
			if pbsIsID(segs[i]) {
				return &pbsTarget{id: segs[i]}
			}
		}
		return nil
	}

	if len(segs) >= 2 {
		switch segs[0] {
		case "video":
			return &pbsTarget{slug: segs[1]}
		case "show":
			return &pbsTarget{show: segs[1]}
		}
	}
	return nil
}

// pbsIsID reports whether a path segment is a player id.
func pbsIsID(seg string) bool {
	if seg == "" {
		return false
	}
	return strings.IndexFunc(seg, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// pbsEpisode is one resolved video, with what the caller needs to name and
// file it. The File carries no Name: the stem differs between a standalone
// video and one entry of a show.
type pbsEpisode struct {
	file    File
	slug    string
	title   string
	program string
	season  string
}

// show expands a programme into one file per video its page lists.
//
// This is the whole of PBS's series support, and it is deliberately the
// shallow half. The per-season listing the page itself calls for lives at
// content.services.pbs.org and answers 401 with a Basic-auth challenge, so
// what a show resolves to is what the page rendered — the current season and
// whatever specials sit beside it — rather than the full catalogue.
func (p *PBS) show(ctx context.Context, u *url.URL, slug string) (*Result, error) {
	page := pbsRoot + "/show/" + url.PathEscape(slug) + "/"
	doc, err := p.client.GetString(ctx, page, nil)
	if err != nil {
		return nil, fmt.Errorf("pbs: fetch %s: %w", u.Redacted(), err)
	}
	base, err := ParseURL(page)
	if err != nil {
		return nil, err
	}

	slugs := pbsVideoSlugs(doc, base)
	if len(slugs) == 0 {
		return nil, fmt.Errorf("pbs: no videos listed on %s", u.Redacted())
	}
	if len(slugs) > pbsMaxEpisodes {
		slugs = slugs[:pbsMaxEpisodes]
	}

	// Two requests per video, and a show page lists a couple of dozen. Doing
	// those one after another leaves the job resolving for a minute before a
	// byte moves; one that will not resolve is skipped, which on this host is
	// routine rather than exceptional.
	episodes := FanOut(ctx, slugs, func(ctx context.Context, slug string) ([]pbsEpisode, error) {
		id, err := p.playerID(ctx, slug)
		if err != nil {
			return nil, err
		}
		episode, err := p.episode(ctx, id)
		if err != nil {
			return nil, err
		}
		episode.slug = slug
		return []pbsEpisode{*episode}, nil
	})
	if len(episodes) == 0 {
		return nil, fmt.Errorf("pbs: none of the %d videos listed on %s are playable "+
			"(PBS answers a title past its availability window with a page saying so, "+
			"and a show's back catalogue is mostly those)", len(slugs), u.Redacted())
	}

	// Only nest by season when there is more than one to tell apart. A show
	// whose page is all specials has no season anywhere, and a folder named
	// after nothing helps nobody.
	distinct := make(map[string]bool)
	for _, episode := range episodes {
		if episode.season != "" {
			distinct[episode.season] = true
		}
	}
	nest := len(distinct) > 1

	result := &Result{Title: slug}
	for _, episode := range episodes {
		if result.Title == slug && episode.program != "" {
			result.Title = episode.program
		}
		file := episode.file
		file.Name = util.FirstNonEmpty(episode.title, episode.slug) + ".mp4"
		if nest {
			file.Dir = episode.season
		}
		result.Files = append(result.Files, file)
	}
	return result, nil
}

// pbsVideoSlugs collects the videos a show page links to, in its own order.
//
// The links are read as anchors, resolved against the page, and kept only
// when they land back on pbs.org. That last check is the point: a show page
// carries "buy this on ..." links to other shops, and one of those has a
// /video/detail/<id> path of its own. A pattern run over the document picks
// it up as a slug and spends two requests discovering it is somebody else's.
func pbsVideoSlugs(doc string, base *url.URL) []string {
	root, err := parseHTML(doc)
	if err != nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, n := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		link, err := url.Parse(resolveRef(base, attr(n, "href")))
		if err != nil || !util.HostMatches(link.Host, "pbs.org") {
			continue
		}
		segs := util.PathSegments(link)
		if len(segs) != 2 || segs[0] != "video" || seen[segs[1]] {
			continue
		}
		seen[segs[1]] = true
		out = append(out, segs[1])
	}
	return out
}

// playerID reads the numeric id a video page embeds its player with. There is
// no other route to it: the id is what the player state is keyed by, and the
// catalogue that maps a slug to one is the endpoint that wants credentials.
func (p *PBS) playerID(ctx context.Context, slug string) (string, error) {
	page := pbsRoot + "/video/" + url.PathEscape(slug) + "/"
	doc, err := p.client.GetString(ctx, page, nil)
	if err != nil {
		return "", fmt.Errorf("pbs: fetch %s: %w", page, err)
	}
	match := pbsPlayerID.FindStringSubmatch(doc)
	if match == nil {
		return "", fmt.Errorf("pbs: %s embeds no player, so there is no video on it", page)
	}
	return match[1], nil
}

// episode resolves one player id to a downloadable file.
func (p *PBS) episode(ctx context.Context, id string) (*pbsEpisode, error) {
	page := pbsPlayerAPI + url.PathEscape(id) + "/"
	doc, err := p.client.GetString(ctx, page, nil)
	if err != nil {
		return nil, fmt.Errorf("pbs: player for %s: %w", id, err)
	}
	bridge, err := pbsBridgeOf(doc)
	if err != nil {
		return nil, fmt.Errorf("pbs: %s: %w", id, err)
	}
	// The refusals below follow the id as a sentence about it, so there is no
	// colon between the two.
	if err := bridge.playable(); err != nil {
		return nil, fmt.Errorf("pbs: %s %w", id, err)
	}
	link, size, err := p.progressive(ctx, bridge.Encodings)
	if err != nil {
		return nil, fmt.Errorf("pbs: %s %w", id, err)
	}

	return &pbsEpisode{
		file:    File{URL: link, Size: size},
		title:   bridge.Title,
		program: bridge.Program.Title,
		season:  pbsSeasonOf(bridge.SeriesInfo),
	}, nil
}

// progressive finds the encoding that leads to a plain file, and its length.
//
// PBS publishes two links per title and labels neither, so each is asked
// where it leads rather than indexed — their order is not something to rely
// on. A HEAD settles it for the price of no body at all, and brings back the
// exact Content-Length on the way, which is what lets the downloader skip a
// file it already has without opening a connection. The bridge does carry a
// has_mp4_encodings flag beside the links, and it is deliberately not read:
// it describes what was transcoded rather than what is served, and believing
// a false one would refuse a title whose own links say otherwise.
//
// Finding none is refused rather than fallen back on, and that refusal is
// what this extractor exists to make. The alternative on offer is the HLS
// master, whose every variant keeps its audio in a rendition of its own;
// joining one would write a video with no sound, at full length, reported as
// finished.
func (p *PBS) progressive(ctx context.Context, encodings []string) (string, int64, error) {
	for _, link := range encodings {
		final, mediaType, size, err := p.probe(ctx, link)
		if err != nil || !pbsIsProgressive(final, mediaType) {
			continue
		}
		// What the file keeps is the redirect, not what it resolved to. The
		// location carries a session id minted per request, so one signed
		// while the item sat in the queue would be stale by the time its turn
		// came; handing over the redirect lets every attempt, and every
		// retry, mint its own. That is a File.Resolve avoided rather than
		// needed.
		return link, size, nil
	}
	return "", 0, errors.New("offers no progressive file, only an adaptive stream that keeps " +
		"its audio in a rendition of its own — joining that would write a silent video")
}

// probe asks one encoding where it leads, without fetching what is there.
func (p *PBS) probe(ctx context.Context, link string) (final *url.URL, mediaType string, size int64, err error) {
	req, err := p.client.NewRequest(ctx, http.MethodHead, link, nil)
	if err != nil {
		return nil, "", 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", 0, &httpx.StatusError{Code: resp.StatusCode, Status: resp.Status, URL: link}
	}

	size = resp.ContentLength
	if size <= 0 {
		size = -1
	}
	return resp.Request.URL, resp.Header.Get(httpx.HeaderContentType), size, nil
}

// pbsIsProgressive reports whether an encoding turned out to be a plain file.
// Either signal alone is enough, and the playlist beside it carries neither.
func pbsIsProgressive(final *url.URL, mediaType string) bool {
	if final != nil && strings.EqualFold(path.Ext(final.Path), ".mp4") {
		return true
	}
	base, _, _ := strings.Cut(mediaType, ";")
	return strings.EqualFold(strings.TrimSpace(base), "video/mp4")
}

// pbsBridgeOf reads the player state out of a fetched player page.
//
// The state is a script assignment rather than a document, so it is decoded
// from where the assignment opens and the decoder is left to find where it
// ends. Cutting at the terminating "};" instead would be a guess about what a
// title or a synopsis may contain; a JSON decoder does not have to guess.
func pbsBridgeOf(doc string) (*pbsBridge, error) {
	start := strings.Index(doc, pbsBridgeVar)
	if start < 0 {
		return nil, pbsPageError(doc)
	}
	rest := doc[start+len(pbsBridgeVar):]
	open := strings.Index(rest, "{")
	if open < 0 {
		return nil, pbsPageError(doc)
	}

	var bridge pbsBridge
	if err := json.NewDecoder(strings.NewReader(rest[open:])).Decode(&bridge); err != nil {
		return nil, fmt.Errorf("decode player state: %w", err)
	}
	return &bridge, nil
}

// pbsPageError is PBS's own explanation for a page carrying no video, passed
// through as it was written.
//
// It is the one thing that page has worth reporting. A title that has left
// its availability window and an id that never existed are the same HTTP 200
// otherwise, and the sentence PBS writes is what tells them apart. They are
// generic sentences — nothing here names a country, a date or a reason — so a
// show expanding into hundreds of them reports the count instead of repeating
// this several hundred times.
func pbsPageError(doc string) error {
	if root, err := parseHTML(doc); err == nil {
		if n := findFirst(root, func(n *html.Node) bool { return hasClass(n, pbsErrorClass) }); n != nil {
			if text := textOf(n); text != "" {
				return errors.New(text)
			}
		}
	}
	return errors.New("the player page carries no video state")
}

// pbsSeasonOf names the season a video belongs to, or nothing when it belongs
// to none. Only the season half of series_info is wanted: the episode number
// beside it says nothing a title does not, and a special has neither.
func pbsSeasonOf(info string) string {
	match := pbsSeasonInfo.FindStringSubmatch(strings.TrimSpace(info))
	if match == nil {
		return ""
	}
	return "Season " + match[1]
}

// pbsJoin names a video by its programme and its own title together, and
// copes with one that has only the second.
func pbsJoin(program, title string) string {
	switch {
	case title == "":
		return program
	case program == "" || strings.EqualFold(program, title):
		return title
	}
	return program + " - " + title
}

// pbsBridge is the part of the player state this needs. These fields are why
// the host is the straightforward one: protection and availability are stated
// outright rather than inferred from a download that failed.
type pbsBridge struct {
	Availability string   `json:"availability"`
	Encodings    []string `json:"encodings"`
	HasDRM       bool     `json:"has_drm"`
	IsPlayable   bool     `json:"is_playable"`
	// IsMVOD marks a PBS Passport title — the donation tier — which is the
	// one restriction here that a caller could act on.
	IsMVOD     bool   `json:"is_mvod"`
	Title      string `json:"title"`
	SeriesInfo string `json:"series_info"`
	Program    struct {
		Title string `json:"title"`
	} `json:"program"`
}

// playable is PBS declining to serve something, in its own terms. Each of the
// three is a published field, so this refuses before a single byte is asked
// for rather than letting a restriction arrive as a bare 403.
func (b *pbsBridge) playable() error {
	switch {
	case b.HasDRM:
		return errors.New("is DRM protected")
	case !b.IsPlayable:
		return errors.New("is not playable" + b.passport())
	case !strings.EqualFold(strings.TrimSpace(b.Availability), pbsAvailable):
		return fmt.Errorf("is listed as %q rather than available%s", b.Availability, b.passport())
	}
	return nil
}

// passport names the restriction when PBS has said which one it is.
func (b *pbsBridge) passport() string {
	if b.IsMVOD {
		return " (it is a PBS Passport title, which needs a member sign-in nothing here can supply)"
	}
	return ""
}
