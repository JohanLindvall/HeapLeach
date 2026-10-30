// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
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
//     client whose TLS fingerprint is not a browser's, and the progressive
//     MP4s are gone: a single "auto" HLS manifest is all that is served.
//     Muxing it was the other half of the wall, which the playlist engine
//     now does itself (download/mux.go); the fingerprint is untried.
//   - bilibili: playurl is WBI-signed — a key fetched from
//     /x/web-interface/nav, permuted through a hardcoded 64-element table,
//     then md5'd with the filtered and sorted query, and expiring within the
//     session. Past that signature, fnval=4048 answers DASH with video and
//     audio apart, so ffmpeg has to mux regardless.
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
	// hostSet is set only by an extractor not yet moved to recognition that
	// hands part of its site over; the handoffs registered on their own
	// are recognised by their pages and leave it empty.
	// TODO(generic-dispatch): remove with the last such caller.
	hostSet
	client *httpx.Client
	site   handoffSite
}

// handoffSite is one host passed over whole.
type handoffSite struct {
	name string
	// domains is the host list of a handoff still claimed by host, as part
	// of an extractor not yet moved to recognition. The sites below are
	// recognised by their pages instead and leave it empty.
	// TODO(generic-dispatch): remove once no caller sets it.
	domains []string
	// shape is the URL forms the site's media and collections have, for the
	// link scanner; recognise says whether a probed page is the site's,
	// by what the page carries that yt-dlp goes on to read; recognises
	// describes both for the inventory.
	shape      func(u *url.URL) bool
	recognise  func(ctx context.Context, h *Handoff, p *Page) bool
	recognises string
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
		name:  "odysee",
		why:   "its pages name no media URL, only an LBRY claim to resolve",
		shape: odyseeShape,
		recognise: func(_ context.Context, _ *Handoff, p *Page) bool {
			// A claim's page names the LBRY embed player for it; a channel
			// is the app shell around its bundle.
			if !odyseeShape(p.Final) || !p.Named("Odysee") {
				return false
			}
			return p.playerMeta(odyseeEmbed) || odyseeChannel(p.Final) && p.Script(odyseeBundle)
		},
		recognises: "claims (`/@<channel>/<claim>`) whose page, named Odysee, offers the LBRY embed player " +
			"(`/$/embed/`), and channels (`/@<channel>`) served by its app bundle",
	},
	{
		name:  "dailymotion",
		why:   "its manifest is refused to anything but a browser, and it offers no plain file",
		shape: dailymotionShape,
		recognise: func(_ context.Context, _ *Handoff, p *Page) bool {
			// The watch page is rendered by the site's own server-side app,
			// and a short link redirects to it.
			return dailymotionShape(p.Final) && p.Script(dailymotionBundle) && p.Named("Dailymotion")
		},
		recognises: "videos and playlists (`/video/<xid>`, `/playlist/<xid>`) rendered by the " +
			"Dailymotion server-side app (its `neon` bundle), short links included",
	},
	{
		name:  "bilibili",
		why:   "its playback call is signed per session and answers with video and audio apart",
		shape: bilibiliShape,
		recognise: func(_ context.Context, _ *Handoff, p *Page) bool {
			// The video page's state, and its play information or the
			// player it offers for embedding, are what yt-dlp reads.
			return bilibiliShape(p.Final) && p.Has(bilibiliState) &&
				(p.Has(bilibiliPlayInfo) || p.playerMeta(bilibiliPlayer)) &&
				p.Named("bilibili", "哔哩哔哩")
		},
		recognises: "videos (`/video/BV<id>`) and bangumi episodes whose page carries the bilibili " +
			"player state (`window.__INITIAL_STATE__` with its play information), short links included",
	},
	{
		name:  "soundcloud",
		why:   "its API needs a client key scraped from a script bundle that rotates",
		shape: soundcloudShape,
		recognise: func(_ context.Context, _ *Handoff, p *Page) bool {
			// A track or a set is a two-segment path, far too common to
			// single out by form, so the page must be the web client's,
			// hydrated with the sound and offering its widget player.
			return soundcloudPage(p.Final) && p.Has(soundcloudHydration) &&
				p.playerMeta(soundcloudWidget) && p.Named("SoundCloud")
		},
		recognises: "tracks and sets (`/<artist>/<track>`, `/<artist>/sets/<set>`) whose page is the " +
			"SoundCloud web client hydrated with the sound (`__sc_hydration`) and offering its widget player",
	},
	{
		name:  "mixcloud",
		why:   "its stream URLs are obfuscated rather than published",
		shape: mixcloudShape,
		recognise: func(_ context.Context, _ *Handoff, p *Page) bool {
			// A show is /<user>/<show>/, as common a form as any, so the
			// page must carry the app's query for that show.
			return mixcloudPage(p.Final) && p.Has(mixcloudQuery) && p.Named("Mixcloud")
		},
		recognises: "shows (`/<user>/<show>/`) whose page, named Mixcloud, carries the app's " +
			"`cloudcastQuery` for the show",
	},
	{
		name:   "rumble",
		why:    "its media endpoint sits behind a browser challenge",
		oembed: rumbleOEmbed,
		shape:  rumbleShape,
		recognise: func(ctx context.Context, h *Handoff, p *Page) bool {
			// The site's pages are usually behind a browser challenge, so
			// they say nothing. Its oEmbed endpoint is not, and is what this
			// extractor names the media from: the URL's own origin is asked
			// about it, once the form has matched.
			return rumbleShape(p.Final) && h.rumbleKnows(ctx, p)
		},
		recognises: "videos (`/v<id>-<slug>.html`, `/embed/v<id>`) that the origin's Rumble oEmbed " +
			"endpoint describes",
	},
}

// The forms and markers the handoff sites are recognised by.
var (
	odyseeShape   = pathShape(`^/@[^/]+(?:/[^/]+)?/?$`, `^/(?:%24|\$)/embed/[^/]+`, `^/[^/@$][^/]*:[0-9a-f]+/?$`)
	odyseeChannel = pathShape(`^/@[^/]+/?$`)
	odyseeEmbed   = regexp.MustCompile(`^/(?:%24|\$)/embed/`)

	dailymotionShape = pathShape(`^/(?:video|playlist)/x[0-9a-z]+/?$`, `^/embed/video/x[0-9a-z]+/?$`)

	bilibiliShape  = pathShape(`^(?:/en)?/video/(?:BV[0-9A-Za-z]{10}|av[0-9]+)/?$`, `^/bangumi/play/(?:ep|ss)[0-9]+/?$`)
	bilibiliPlayer = regexp.MustCompile(`^/player\.html\?(?:.*&)?(?:bvid|aid)=`)

	soundcloudShape  = pathShape(`^/[^/]+/sets/[^/]+/?$`)
	soundcloudPage   = pathShape(`^/[^/]+/[^/]+/?$`, `^/[^/]+/sets/[^/]+/?$`)
	soundcloudWidget = regexp.MustCompile(`^/player/`)

	mixcloudShape = func(*url.URL) bool { return false }
	mixcloudPage  = pathShape(`^/[^/]+/[^/]+/$`)

	rumbleShape = pathShape(`^/v[0-9a-z]{4,}-[^/]*\.html$`, `^/embed/v[0-9a-z]{4,}/?$`)
)

const (
	odyseeBundle        = "/public/assets/"
	dailymotionBundle   = "/neon-user-ssr/"
	bilibiliState       = "window.__INITIAL_STATE__"
	bilibiliPlayInfo    = "window.__playinfo__"
	soundcloudHydration = "window.__sc_hydration"
	mixcloudQuery       = "cloudcastQuery"
	// rumbleOEmbedPath is where a Rumble origin's oEmbed endpoint is.
	rumbleOEmbedPath = "/api/Media/oembed.json"
)

// rumbleKnows asks the page's own origin whether its oEmbed endpoint
// describes the page as one of its videos. Every recogniser that would ask
// the same shares the one answer.
func (h *Handoff) rumbleKnows(ctx context.Context, p *Page) bool {
	if h.client == nil {
		return false
	}
	endpoint := util.Origin(p.Final) + rumbleOEmbedPath + "?" + url.Values{"url": {p.Final.String()}}.Encode()
	return p.Memo(endpoint, func() any {
		var out struct {
			Type         string `json:"type"`
			ProviderName string `json:"provider_name"`
			HTML         string `json:"html"`
		}
		return platformJSON(ctx, h.client, endpoint, nil, &out) &&
			strings.HasPrefix(strings.ToLower(out.ProviderName), "rumble") && out.HTML != ""
	}).(bool)
}

// Recognises is the inventory's description.
func (h *Handoff) Recognises() string { return h.site.recognises }

// Shape reports the site's URL forms worth a probe.
func (h *Handoff) Shape(u *url.URL) bool { return h.site.shape != nil && h.site.shape(u) }

// Recognise reports whether the probed page is the site's.
func (h *Handoff) Recognise(ctx context.Context, p *Page) bool {
	return h.site.recognise != nil && h.site.recognise(ctx, h, p)
}

// NewHandoffs builds one extractor per host, so each is recognised by its
// own pages and named for itself in the UI exactly as a hand-written host is.
func NewHandoffs(client *httpx.Client) []Extractor {
	out := make([]Extractor, 0, len(handoffSites))
	for _, site := range handoffSites {
		out = append(out, &Handoff{client: client, site: site})
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
