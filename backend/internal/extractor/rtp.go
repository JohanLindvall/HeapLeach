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

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// RTPPlay resolves rtp.pt/play, the Portuguese public broadcaster's archive.
//
// There is no player API to ask, and none is needed: the episode page carries
// the stream itself, base64 inside a percent-encoded string array that the
// page's own JavaScript joins and hands to atob. Reversing that is the whole
// of the plumbing — no token, no signature, no session — and the manifests it
// names are served to anyone who asks.
//
// Four things about that page decide the shape of this file.
//
// The first is that the obfuscated blob appears several times and only the
// last one is the programme. Above it sit a commented-out player example and
// a decoy assignment, both naming the same 2021 sample clip; the browser is
// unbothered because a later `var f =` overwrites an earlier one, and this
// follows the same rule. That alone is not enough, though. An episode that is
// gone answers with a perfectly ordinary page carrying ONLY the sample, and so
// does a radio programme, so the last blob is routinely a real, fetchable
// video of something else entirely. Every stream RTP serves carries the
// programme's own id in its path, so that is what the chosen URL is required
// to carry — and a page with nothing else on offer is then reported in the
// site's own words rather than downloaded.
//
// The second is DRM, and it is why a good half of the catalogue is refused
// before a byte moves. News and factual are served clear; entertainment and
// fiction are FairPlay through DRMtoday, with a Widevine sibling. The page
// announces which in one word, `drm : true`, and the manifest says the same
// thing in its first path segment, `drm-fps` or `drm-dash`. Both are read:
// the flag is what the site believes, the path is what the CDN acts on.
// Waiting to find out is not an option, because a protected master still
// answers 200 and the FairPlay key only appears in the media playlist below
// it — one request too late to be an honest refusal.
//
// The third is pleasant. The master playlist is nginx-vod-module's "urlset"
// and carries no EXT-X-MEDIA line at all: the `-v1-a1` in every variant name
// is video track one plus audio track one, in the same MPEG-TS segments. So
// hlsVariant.muxed() accepts them all and concatenation yields something that
// plays, the opposite of vimeo.
//
// The fourth is that RTP's radio is on the same site under the same URL
// shape, and it is the better case rather than a harder one: those pages
// assign `var f` a plain MP3 URL with no obfuscation at all, which becomes an
// ordinary rangeable file — segmented, resumable, skippable when it is
// already on disk — instead of a segment list.
type RTPPlay struct {
	client *httpx.Client
}

const (
	rtpRoot = "https://www.rtp.pt"
	rtpPlay = rtpRoot + "/play/"

	// rtpEpisodesEndpoint is the listing the site's own infinite scroll asks
	// for, and the reason a season is not simply what its page rendered: a
	// programme page ships twelve episodes and a hidden count of the rest,
	// which for a daily news strand is two hundred more. `page` is the whole
	// cursor — the row id the site sends alongside it changes nothing — and a
	// page past the end comes back empty, which is what ends the walk.
	rtpEpisodesEndpoint = rtpPlay + "bg_l_ep/"

	// rtpTitleSuffix is trimmed as one string rather than cut at the first
	// " - ", which would behead the many titles that carry one of their own:
	// "Cinemax Episódio 17 - de 24 abr 2025" is the ordinary shape here, not
	// the exception.
	rtpTitleSuffix = " - RTP Play"

	// rtpNoResultClass marks the paragraph RTP renders where the player would
	// have gone when it will not serve an episode.
	rtpNoResultClass = "vod-no-result"

	// The first path segment of a protected manifest: FairPlay for the HLS
	// side, Widevine for the DASH one.
	rtpFairPlayPath = "drm-fps"
	rtpWidevinePath = "drm-dash"
)

// rtpObfuscated matches the site's single piece of obfuscation, an array of
// base64 fragments joined and decoded inline.
var rtpObfuscated = regexp.MustCompile(`atob\(\s*decodeURIComponent\(\s*\[([^\]]*)\]\s*\.join\(`)

// rtpChunk pulls the fragments out of that array.
var rtpChunk = regexp.MustCompile(`"([^"]*)"`)

// rtpPlainMedia matches the radio pages' assignment, which is not obfuscated
// and points straight at an MP3.
var rtpPlainMedia = regexp.MustCompile(`var\s+f\s*=\s*"(https?://[^"]+)"`)

// rtpDRMFlag matches the player's protection flag. It is written both with
// and without a space before the colon on different pages, which is the only
// reason this is a pattern rather than a search for a string.
var rtpDRMFlag = regexp.MustCompile(`\bdrm\s*:\s*(true|false)\b`)

// The two id shapes a play URL is built from: p<n> for a programme, e<n> for
// one of its episodes.
var (
	rtpProgramID = regexp.MustCompile(`^p[0-9]+$`)
	rtpEpisodeID = regexp.MustCompile(`^e[0-9]+$`)
)

// NewRTPPlay builds the RTP Play extractor.
func NewRTPPlay(client *httpx.Client) *RTPPlay { return &RTPPlay{client: client} }

func (r *RTPPlay) Name() string { return "rtpplay" }

// Match takes rtp.pt paths under /play. The rest of the domain is the
// broadcaster's news site and its radio station pages, which share nothing
// with this but the host.
func (r *RTPPlay) Match(u *url.URL) bool {
	if !util.HostMatches(u.Host, "rtp.pt") {
		return false
	}
	segs := util.PathSegments(u)
	return len(segs) > 0 && strings.EqualFold(segs[0], "play")
}

// Extract handles one episode or one season.
func (r *RTPPlay) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	ref := rtpTarget(u)
	switch {
	case ref.program == "":
		return nil, fmt.Errorf("rtpplay: no programme in %s", u.Redacted())
	case ref.episode != "":
		file, title, err := r.episode(ctx, ref.page, ref.program)
		if err != nil {
			return nil, err
		}
		return &Result{Title: title, Files: []File{*file}}, nil
	}
	return r.season(ctx, ref)
}

// rtpRef is what a play URL points at.
//
// There is no third shape for a whole series: RTP gives every season a
// programme id of its own, so a programme URL is always one season.
type rtpRef struct {
	// program is the programme id, "p16136".
	program string
	// episode is the episode id, "e944378", empty when the URL names a season.
	episode string
	// page is the URL as given, which is what gets fetched.
	page string
}

// rtpTarget reads a play URL.
func rtpTarget(u *url.URL) rtpRef {
	segs := util.PathSegments(u)
	if len(segs) < 2 || !strings.EqualFold(segs[0], "play") || !rtpProgramID.MatchString(segs[1]) {
		return rtpRef{}
	}
	ref := rtpRef{program: strings.ToLower(segs[1]), page: u.String()}
	if len(segs) >= 3 && rtpEpisodeID.MatchString(segs[2]) {
		ref.episode = strings.ToLower(segs[2])
	}
	return ref
}

// episode resolves one episode page into a file, returning the name it chose
// so a standalone episode can use it as the job title too.
func (r *RTPPlay) episode(ctx context.Context, page, program string) (*File, string, error) {
	headers := httpx.Referer(rtpRoot + "/")
	doc, err := r.client.GetString(ctx, page, headers)
	if err != nil {
		return nil, "", fmt.Errorf("rtpplay: fetch %s: %w", page, err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, "", fmt.Errorf("rtpplay: %s: %w", page, err)
	}
	title := rtpTitle(root)

	// Protection is settled first, before anything is chosen or fetched, and
	// it is settled entirely from the page.
	manifest := rtpManifest(doc, program)
	if rtpProtected(doc, manifest) {
		return nil, "", &rtpRefused{page: page, reason: "DRM protected — RTP serves its entertainment " +
			"and fiction under FairPlay and Widevine, which nothing here can decrypt"}
	}

	// Radio needs none of what follows. Its pages name an MP3 outright, and a
	// plain rangeable file is much the better thing to hand the downloader.
	if link := rtpPlainMediaURL(doc); link != "" {
		name := util.FirstNonEmpty(title, util.NameFromURL(link), program)
		return &File{
			Name:    name + rtpLinkExtension(link),
			URL:     link,
			Size:    -1,
			Headers: headers,
		}, name, nil
	}

	if manifest == "" {
		// The page is served whether or not there is anything behind it, and
		// when there is not it says so in words meant for a viewer. Those are
		// passed through: whether the episode expired, was never published or
		// is licensed elsewhere is something only RTP knows.
		return nil, "", &rtpRefused{page: page, reason: util.FirstNonEmpty(rtpUnavailable(root),
			"the page carries no stream for this programme")}
	}

	segments, variant, err := resolvePlaylist(ctx, r.client, manifest, headers)
	if err != nil {
		return nil, "", fmt.Errorf("rtpplay: %s: %w", page, err)
	}
	name := util.FirstNonEmpty(title, program)
	return &File{
		Name:     name + rtpSegmentExtension(segments, variant),
		Size:     -1,
		Headers:  headers,
		Segments: segments,
	}, name, nil
}

// season expands a programme page into its episodes.
//
// It resolves the season that was asked for and not the whole run, although
// the page carries a selector naming every other season and following it
// would be four more lines. Two facts settle that. The page a person is
// looking at is titled "temporada 18" and lists that season alone, so
// eighteen seasons is not what they pointed at; and one season of a daily
// strand is already several hundred episodes, so the whole run is a job
// nobody meant to start. Every other season is one paste away, and its link
// is in the selector the reader can already see.
func (r *RTPPlay) season(ctx context.Context, ref rtpRef) (*Result, error) {
	headers := httpx.Referer(rtpRoot + "/")
	doc, err := r.client.GetString(ctx, ref.page, headers)
	if err != nil {
		return nil, fmt.Errorf("rtpplay: fetch %s: %w", ref.page, err)
	}
	root, err := parseHTML(doc)
	if err != nil {
		return nil, fmt.Errorf("rtpplay: %s: %w", ref.page, err)
	}

	// The rendered page is page one of the listing, so the walk starts at two
	// rather than fetching what is already in hand.
	seen := make(map[string]bool)
	episodes := rtpEpisodeLinks(doc, ref, seen)
	capped := false
	for page := 2; ; page++ {
		if page > config.MaxAlbumPages {
			capped = true
			break
		}
		more, err := r.listing(ctx, ref, page, seen)
		// A listing request that fails ends the walk with what it has: a
		// truncated season is worth more than no season, and the title below
		// says it was truncated.
		if err != nil {
			capped = true
			break
		}
		if len(more) == 0 {
			break // past the end of the season, which is how a season ends
		}
		episodes = append(episodes, more...)
		if len(episodes) >= config.MaxListingVideos {
			capped = true
			break
		}
	}
	if len(episodes) == 0 {
		return nil, fmt.Errorf("rtpplay: no episodes listed on %s", ref.page)
	}
	if len(episodes) > config.MaxListingVideos {
		episodes = episodes[:config.MaxListingVideos]
	}

	// The error travels in the value rather than being returned, because
	// FanOut drops an item whose fetch failed and a season refused in full has
	// exactly one thing worth reporting: the reason.
	outcomes := FanOut(ctx, episodes, func(ctx context.Context, ep rtpEpisodeRef) ([]rtpOutcome, error) {
		file, _, err := r.episode(ctx, ep.page, ref.program)
		return []rtpOutcome{{id: ep.id, file: file, err: err}}, nil
	})

	title := util.FirstNonEmpty(rtpTitle(root), ref.program)
	if capped {
		title = fmt.Sprintf("%s (first %d episodes)", title, len(episodes))
	}

	result := &Result{Title: title}
	names := make(map[string]bool)
	var refused string
	for _, out := range outcomes {
		if out.err != nil {
			// One refused episode must not sink the rest, but its reason is
			// kept: when every episode is refused it is refused for the same
			// reason, and RTP's own words for it beat anything concluded here.
			var refusal *rtpRefused
			if refused == "" && errors.As(out.err, &refusal) {
				refused = refusal.reason
			}
			continue
		}
		file := *out.file
		// Two episodes of one programme can share a page title, because RTP
		// names an untitled instalment after its programme and the date and
		// then repeats a date across parts. Two files with one name is one
		// file, so the episode id is what tells them apart.
		if names[file.Name] {
			file.Name = out.id + " - " + file.Name
		}
		names[file.Name] = true
		result.Files = append(result.Files, file)
	}
	if len(result.Files) == 0 {
		return nil, fmt.Errorf("rtpplay: none of the %d episodes of %s are playable: %s",
			len(episodes), ref.page,
			util.FirstNonEmpty(refused, "the page lists them but none carries a stream"))
	}
	return result, nil
}

// rtpEpisodeRef is one episode of a season listing.
type rtpEpisodeRef struct {
	id   string
	page string
}

// rtpOutcome is what resolving one episode produced, which is either a file
// or the reason there is none.
type rtpOutcome struct {
	id   string
	file *File
	err  error
}

// listing asks for one page of a programme's episodes.
func (r *RTPPlay) listing(ctx context.Context, ref rtpRef, page int, seen map[string]bool) ([]rtpEpisodeRef, error) {
	query := url.Values{
		// The endpoint wants the bare number, not the "p" the URL spells it
		// with.
		"listProgram": {strings.TrimPrefix(ref.program, "p")},
		"page":        {strconv.Itoa(page)},
	}
	link := rtpEpisodesEndpoint + "?" + query.Encode()
	doc, err := r.client.GetString(ctx, link, httpx.Referer(ref.page))
	if err != nil {
		return nil, fmt.Errorf("rtpplay: episode listing page %d of %s: %w", page, ref.program, err)
	}
	return rtpEpisodeLinks(doc, ref, seen), nil
}

// rtpEpisodeLinks collects the episode links a listing document carries, in
// the order the site put them, skipping any already taken.
//
// It is given the whole document rather than a container, because the listing
// endpoint answers with a bare fragment of articles while the programme page
// wraps the same markup in a site. Restricting the links to this programme is
// what keeps the two honest — a page also carries navigation into other
// programmes, and an episode id is only unique beneath its own.
func rtpEpisodeLinks(doc string, ref rtpRef, seen map[string]bool) []rtpEpisodeRef {
	root, err := parseHTML(doc)
	if err != nil {
		return nil
	}
	base, err := ParseURL(ref.page)
	if err != nil {
		return nil
	}

	var out []rtpEpisodeRef
	for _, anchor := range findAll(root, func(n *html.Node) bool { return isElem(n, atom.A) }) {
		link := resolveRef(base, attr(anchor, "href"))
		if link == "" {
			continue
		}
		u, err := ParseURL(link)
		if err != nil {
			continue
		}
		found := rtpTarget(u)
		if found.program != ref.program || found.episode == "" || seen[found.episode] {
			continue
		}
		seen[found.episode] = true
		out = append(out, rtpEpisodeRef{id: found.episode, page: link})
	}
	return out
}

// rtpManifest picks the stream a page is actually about.
//
// Several blobs decode to a playlist and the last one wins, the way the last
// `var f =` wins in the browser. The programme id is then required to appear
// in the path, and that requirement is load-bearing rather than belt and
// braces: the blobs above the real one name the player's built-in sample
// clip, and a page for an episode that is no longer available — or for a
// radio programme, which keeps its media elsewhere — carries nothing else.
// Taken, that sample would download in full, of the wrong programme, and
// report success.
func rtpManifest(doc, program string) string {
	var chosen string
	for _, blob := range rtpObfuscated.FindAllStringSubmatch(doc, -1) {
		link := rtpDecode(blob[1])
		if !strings.Contains(link, ".m3u8") {
			continue // the DASH sibling, or the file key that sits beside it
		}
		u, err := ParseURL(link)
		if err != nil || !strings.Contains(u.Path, "/"+program+"/") {
			continue
		}
		chosen = link
	}
	return chosen
}

// rtpPlainMediaURL is the file a radio page names outright, and there is no
// obfuscation to reverse: the last such assignment wins, exactly as the last
// obfuscated one does.
//
// A page that assigned a manifest this way is declined and left to the
// playlist path. Handed over as a URL it would "download" perfectly happily,
// as a few kilobytes of text that no player will open, and nothing further
// down could tell that from a real file.
func rtpPlainMediaURL(doc string) string {
	matches := rtpPlainMedia.FindAllStringSubmatch(doc, -1)
	if len(matches) == 0 {
		return ""
	}
	link := matches[len(matches)-1][1]
	if strings.Contains(link, ".m3u8") || strings.Contains(link, ".mpd") {
		return ""
	}
	return link
}

// rtpProtected reports whether the page announces DRM, which RTP does twice
// over and this reads both times.
//
// The flag is what the site believes and the manifest's first path segment is
// what the CDN acts on. They have agreed on every programme looked at, so
// either alone would do today; reading one of them would be a bet on that
// continuing, and the cost of not betting is four lines. The flag is taken
// from its last occurrence for the same reason the manifest is, and note that
// the radio pages carry no flag at all — an absent flag is not a "false".
func rtpProtected(doc, manifest string) bool {
	if flags := rtpDRMFlag.FindAllStringSubmatch(doc, -1); len(flags) > 0 &&
		strings.EqualFold(flags[len(flags)-1][1], "true") {
		return true
	}
	u, err := ParseURL(manifest)
	if err != nil {
		return false
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	return first == rtpFairPlayPath || first == rtpWidevinePath
}

// rtpDecode reverses one obfuscated blob: the fragments are joined,
// percent-decoded and base64-decoded, exactly as the page's own JavaScript
// does it.
//
// The unescape has to be the path flavour. Base64's alphabet includes "+",
// and a query unescape turns every one of them into a space —
// decodeURIComponent does not, and neither may this.
func rtpDecode(array string) string {
	var joined strings.Builder
	for _, chunk := range rtpChunk.FindAllStringSubmatch(array, -1) {
		joined.WriteString(chunk[1])
	}
	text, err := url.PathUnescape(joined.String())
	if err != nil {
		return ""
	}
	raw, err := util.DecodeBase64(text)
	if err != nil {
		return ""
	}
	return string(raw)
}

// rtpTitle reads a page's own name.
//
// The parser collapses runs of whitespace as it reads the text, which matters
// here: the site writes two spaces before its own name in a programme page's
// title and one in an episode page's, and the suffix would otherwise have to
// be trimmed twice.
func rtpTitle(root *html.Node) string {
	return strings.TrimSpace(strings.TrimSuffix(firstText(root, atomTitle), rtpTitleSuffix))
}

// rtpUnavailable is RTP's own explanation for an episode it will not serve,
// rendered into the page where the player would have gone.
func rtpUnavailable(root *html.Node) string {
	n := findFirst(root, func(n *html.Node) bool { return hasClass(n, rtpNoResultClass) })
	if n == nil {
		return ""
	}
	return textOf(n)
}

// rtpSegmentExtension names the container the joined segments will be in.
//
// playlistExtension cannot answer this one and would answer it wrongly. RTP's
// manifests are nginx-vod-module urlsets, which spell the source MP4s out in
// the path — twice over, for a two-rendition set — so it would call an
// MPEG-TS stream ".mp4" and leave a file that nothing opens. The segments
// themselves are unambiguous, so they are what is asked, and playlistExtension
// remains the answer for a host that names its segments some other way.
func rtpSegmentExtension(segments []string, variant hlsVariant) string {
	if len(segments) > 0 {
		switch rtpLinkExtension(segments[0]) {
		case ".ts":
			return ".ts"
		case ".mp4", ".m4s":
			return ".mp4"
		}
	}
	return playlistExtension(variant)
}

// rtpLinkExtension is the suffix of a URL's own path, ignoring any query.
func rtpLinkExtension(link string) string {
	u, err := ParseURL(link)
	if err != nil {
		return ""
	}
	return strings.ToLower(path.Ext(u.Path))
}

// rtpRefused is RTP declining an episode, carrying the reason. It is a type
// rather than a message so that a season of several hundred can report one
// reason instead of the same refusal several hundred times.
type rtpRefused struct {
	page   string
	reason string
}

func (e *rtpRefused) Error() string { return "rtpplay: " + e.page + ": " + e.reason }
