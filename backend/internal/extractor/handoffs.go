package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Hosts that are recognised here and downloaded by yt-dlp.
//
// Every other extractor in this package earns its place by reaching media a
// plain fetch cannot. These hosts are the other case: each was probed, each
// is perfectly reachable, and each would cost more to keep working than it
// could ever save. Handing them over is not the lesser outcome — it is the
// right one for a wall that moves, since yt-dlp exists to follow such walls
// and this program does not.
//
// The wall is invisible from the outside, so each is written down. Without
// that, the obvious improvement is to write the native extractor that walks
// straight into it:
//
//   - odysee / lbry: a page names no media of its own. The bytes are minted
//     by the LBRY streaming API, per claim, on request — so "read the page"
//     resolves nothing and the whole protocol would have to be spoken.
//   - dailymotion: the manifest answers 403 with x-error-code E003 to any
//     client whose TLS fingerprint is not a browser's, and nothing is left
//     behind it to want: the progressive MP4s are gone and a single "auto"
//     HLS manifest is all that is served, so a pass would still need muxing.
//   - bilibili: playurl is WBI-signed — a key fetched from
//     /x/web-interface/nav, permuted through a hardcoded 64-element table,
//     then md5'd with the filtered and sorted query, and expiring within the
//     session. Past that signature, fnval=4048 answers DASH with video and
//     audio apart, so ffmpeg has to mux regardless.
//   - niconico: its DMS playlist is demuxed, so hlsVariant.muxed would
//     correctly refuse every variant and leave nothing self-contained to
//     fetch. Taking a variant anyway is the trap muxed() exists to prevent —
//     a silent file that looks like a finished download.
//   - bandcamp: every endpoint, its own CDN included, answers with an
//     F5-style client challenge rather than with data.
//   - soundcloud: api-v2 wants a client_id scraped out of a JS bundle that
//     rotates, and a browser TLS fingerprint alongside it.
//   - mixcloud: its stream URLs are XOR-obfuscated, and the obfuscation is
//     the site's to change whenever it likes.
//   - rumble: its media endpoint sits behind a Cloudflare interstitial
//     (yt-dlp issue #17496). Its oEmbed endpoint is not, which is what makes
//     the naming below possible where nothing else here is.
//
// What is still worth doing is the name. An item queued as its own URL tells
// the user nothing while it waits, so each of these is asked what it is
// called — and no further. The name carries no extension on purpose: yt-dlp
// picks the container and renames the finished file itself, so an extension
// guessed here would be wrong about as often as not.
//
// A link naming a collection rather than one piece of media — an album, a
// set, a channel — is passed on exactly as it stands, and shows up as one
// item in the queue however many files come out of it. Splitting it would
// mean resolving the collection here, which is the work these hosts were
// handed over to avoid.
type Handoff struct {
	hostSet
	client *httpx.Client
	site   handoffSite
}

// handoffSite is one host passed over whole.
type handoffSite struct {
	name    string
	domains []string
	// why completes the sentence explaining what yt-dlp is needed for. It
	// only ever reaches the user when yt-dlp is missing, which is the one
	// moment the reason is something they can act on.
	why string
	// oembed, when set, names the media through the host's own oEmbed
	// endpoint instead of through yt-dlp. Worth doing only where it reaches
	// something yt-dlp cannot, which on rumble is the whole point: the
	// endpoint that would answer yt-dlp is challenged and this one is not.
	oembed string
}

// rumbleOEmbed is the one endpoint on rumble that is served to anyone.
const rumbleOEmbed = "https://rumble.com/api/Media/oembed.json"

// handoffSites are the hosts recognised here. See the type comment for what
// each `why` is short for.
var handoffSites = []handoffSite{
	{
		name:    "odysee",
		domains: []string{"odysee.com", "lbry.tv"},
		why:     "its pages name no media URL, only an LBRY claim to resolve",
	},
	{
		name:    "dailymotion",
		domains: []string{"dailymotion.com", "dai.ly"},
		why:     "its manifest is refused to anything but a browser, and it offers no plain file",
	},
	{
		name:    "bilibili",
		domains: []string{"bilibili.com", "b23.tv"},
		why:     "its playback call is signed per session and answers with video and audio apart",
	},
	{
		name:    "niconico",
		domains: []string{"nicovideo.jp", "nico.ms"},
		why:     "its stream carries video and audio separately, so they have to be muxed",
	},
	{
		name:    "bandcamp",
		domains: []string{"bandcamp.com"},
		why:     "it answers every endpoint, its own CDN included, with a client challenge",
	},
	{
		name:    "soundcloud",
		domains: []string{"soundcloud.com"},
		why:     "its API needs a client key scraped from a script bundle that rotates",
	},
	{
		name:    "mixcloud",
		domains: []string{"mixcloud.com"},
		why:     "its stream URLs are obfuscated rather than published",
	},
	{
		name:    "rumble",
		domains: []string{"rumble.com"},
		why:     "its media endpoint sits behind a browser challenge",
		oembed:  rumbleOEmbed,
	},

	// Public broadcasters that are DRM-free and perfectly reachable, and
	// still cannot be fetched natively — every one of them for the same
	// reason, which is worth stating once here rather than seven times
	// below: their streams carry video and audio in separate renditions.
	//
	// HeapLeach joins HLS parts by concatenating them, so a demuxed
	// variant produces a silent video that looks like a finished download.
	// hlsVariant.muxed() refuses exactly that, which is the right answer
	// and leaves nothing to fetch. yt-dlp has ffmpeg and can mux, so these
	// go to it. That is not a judgement about the host: several of these
	// are easier to reach than hosts implemented natively here — Arte's
	// API answers anyone in six languages — and the sibling broadcasters
	// that happen to mux their audio in (SVT, NRK, RÚV, VRT and the rest)
	// are native for that reason alone.
	{
		name:    "arte",
		domains: []string{"arte.tv"},
		why:     "its renditions carry video and audio apart, so they have to be muxed",
	},
	{
		name:    "orf",
		domains: []string{"orf.at", "tvthek.orf.at", "on.orf.at"},
		why:     "its streams are demuxed, and its API wants a credential published in the page",
	},
	{
		name:    "yle",
		domains: []string{"areena.yle.fi", "arenan.yle.fi"},
		why:     "no variant it offers is self-contained, so the audio must be muxed back in",
	},
	{
		name:    "drtv",
		domains: []string{"dr.dk"},
		why:     "its variants are video-only, and are byte ranges of one file rather than parts",
	},
	{
		name:    "rtbf",
		domains: []string{"rtbf.be", "auvio.rtbf.be"},
		why:     "its packager offers no self-contained rendition to fetch",
	},
	{
		name:    "francetv",
		domains: []string{"france.tv"},
		why:     "most of its catalogue is DASH, which needs muxing this cannot do",
	},
	{
		name:    "radio-canada",
		domains: []string{"radio-canada.ca", "ici.radio-canada.ca"},
		why:     "its streams carry video and audio apart, so they have to be muxed",
	},
}

// NewHandoffs builds one extractor per host, so each is matched on its own
// domains and named for itself in the UI exactly as a hand-written host is.
func NewHandoffs(client *httpx.Client) []Extractor {
	out := make([]Extractor, 0, len(handoffSites))
	for _, site := range handoffSites {
		out = append(out, &Handoff{hostSet: site.domains, client: client, site: site})
	}
	return out
}

func (h *Handoff) Name() string { return h.site.name }

// Extract names the media and hands the page to the external downloader.
func (h *Handoff) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	ytdlp, ok := tools.Find(ytdlpBinary)
	if !ok {
		return nil, fmt.Errorf("%s: %s, and yt-dlp is what fetches from hosts like this; %s (%s)",
			h.site.name, h.site.why, tools.NotInstalled(ytdlpBinary), u.Redacted())
	}

	title, err := h.title(ctx, ytdlp, u)
	if err != nil {
		return nil, err
	}
	return &Result{Title: title, Files: []File{{
		Name:     title,
		Size:     -1,
		External: u.String(),
	}}}, nil
}

// title asks what the media is called, so the queue shows something a person
// recognises while the item waits its turn.
func (h *Handoff) title(ctx context.Context, ytdlp string, u *url.URL) (string, error) {
	if h.site.oembed != "" {
		title, err := h.oembedTitle(ctx, u)
		if err != nil {
			return "", err
		}
		return util.FirstNonEmpty(title, handoffName(u)), nil
	}

	// Elsewhere the probe is yt-dlp reading exactly what it will read again
	// when the transfer starts, so a refusal here predicts a refusal there.
	// Reporting it now, in the host's own words, beats queueing an item that
	// dies later with the same message and less context.
	title, err := ytdlpTitle(ctx, ytdlp, u.String())
	if err != nil {
		return "", fmt.Errorf("%s: yt-dlp could not read %s: %w", h.site.name, u.Redacted(), err)
	}
	return util.FirstNonEmpty(title, handoffName(u)), nil
}

// oembedTitle reads the title from the host's oEmbed endpoint.
//
// Two failures are told apart here, because they mean opposite things. An
// endpoint that answers a challenge instead of JSON says the host is turning
// automated clients away wholesale — the media endpoint is behind the same
// one, so the download would be turned away too, and saying so now is worth
// far more than a raw 403 from a URL the user never typed. An endpoint that
// simply does not know the link is ordinary: oEmbed describes videos and
// nothing else, so a channel or a playlist is answered with a 404, and yt-dlp
// may well cope with it. That costs a good name, not the download.
func (h *Handoff) oembedTitle(ctx context.Context, u *url.URL) (string, error) {
	endpoint := h.site.oembed + "?" + url.Values{"url": {u.String()}}.Encode()

	var out struct {
		Title string `json:"title"`
	}
	if err := h.client.GetJSON(ctx, endpoint, httpx.Referer(util.Origin(u)+"/"), &out); err != nil {
		if handoffChallenged(err) {
			return "", fmt.Errorf("%s: this host is currently blocking automated clients — "+
				"even the endpoint it normally serves to anyone answered with a challenge page, "+
				"and its media sits behind the same one, so there is nothing to fetch until that "+
				"lifts (%s)", h.site.name, u.Redacted())
		}
		return "", nil
	}
	return strings.TrimSpace(out.Title), nil
}

// handoffChallenged reports whether a failure is an anti-bot challenge rather
// than an answer.
//
// A challenge arrives as a page, so it shows up either as a status this
// endpoint has no other use for or as HTML where JSON was asked for — and the
// text of the failure carries the interstitial's own wording either way,
// which is the one part of it that is reliably present.
func handoffChallenged(err error) bool {
	if httpx.HasStatus(err, http.StatusForbidden, http.StatusTooManyRequests,
		http.StatusServiceUnavailable) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"just a moment", "cf-chl", "challenge-platform", "cloudflare", "enable javascript",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// handoffName labels an item the host would not name.
//
// The last path segment is what these links carry a slug in, and any suffix
// on it belongs to the page rather than to the media. It is not a pretty
// name, but it is enough to tell two queued items apart, which is all a name
// has to do before yt-dlp renames the finished file.
func handoffName(u *url.URL) string {
	segs := util.PathSegments(u)
	if len(segs) == 0 {
		return strings.ToLower(u.Hostname())
	}
	last := segs[len(segs)-1]
	return util.FirstNonEmpty(strings.TrimSuffix(last, path.Ext(last)), last,
		strings.ToLower(u.Hostname()))
}
