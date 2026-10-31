// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
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
//   - youtube: playback URLs are withheld until a caller passes BotGuard
//     attestation, on top of the signature and throttling parameters that
//     already required running the player's JavaScript.
//
// A link naming a collection rather than one piece of media — an album, a
// set, a channel — is passed on exactly as it stands, and shows up as one
// item in the queue however many files come out of it, unless the site
// sets expand. Then yt-dlp is asked to list the collection, flat, which
// costs a request a page of it rather than one per entry, and each entry
// becomes an item of its own. That is still yt-dlp's work, not ours: it
// reads the listing exactly as it would on download.
type Handoff struct {
	about
	client *httpx.Client
	site   handoffSite
}

// handoffSite is one host passed over whole.
type handoffSite struct {
	// about's shape is the URL forms the site's media and collections
	// have, for the link scanner.
	about
	// recognise says whether a probed page is the site's, by what the page
	// carries that yt-dlp goes on to read.
	recognise func(ctx context.Context, h *Handoff, p *Page) bool
	// why completes the sentence explaining what yt-dlp is needed for. It
	// only ever reaches the user when yt-dlp is missing, which is the one
	// moment the reason is something they can act on.
	why string
	// oembed, when set, names the media through the host's own oEmbed
	// endpoint instead of through yt-dlp. Worth doing only where it reaches
	// something yt-dlp cannot, which on rumble is the whole point: the
	// endpoint that would answer yt-dlp is challenged and this one is not.
	oembed string
	// expand lists a playlist or a channel one item per entry, bounded by
	// the file cap, where without it the collection is one item. Set only
	// where the site has a collection form and yt-dlp lists it flat.
	expand bool
	// watch, when set, builds an entry's page from its id, for a listing
	// entry that names no page of its own.
	watch string
}

// rumbleOEmbed is the one endpoint on rumble that is served to anyone.
const rumbleOEmbed = "https://rumble.com/api/Media/oembed.json"

// handoffSites are the hosts recognised here. See the type comment for what
// each `why` is short for.
var handoffSites = []handoffSite{
	{
		name:   "youtube",
		why:    "it withholds playback URLs until a caller passes its BotGuard attestation",
		shape:  youtubeShape,
		expand: true,
		watch:  "https://www.youtube.com/watch?v=",
		recognise: func(_ context.Context, _ *Handoff, p *Page) bool {
			// The web client's configuration (ytcfg.set) and the initial
			// player response, page data or embedded player response beside
			// it are what yt-dlp reads in turn. A short link redirects to
			// the watch page, so it is judged there. A visitor from some
			// regions is first redirected to a consent page instead; that
			// page is recognised by its own consent form, and judged by the
			// page it says it will continue to. The forms are settled before
			// the body is searched, so a page of neither costs no scan.
			if next, err := url.Parse(p.Final.Query().Get("continue")); err == nil && youtubeShape(next) &&
				p.Has(youtubeConsent) {
				return true
			}
			return youtubeShape(p.Final) && p.Has("ytcfg.set") &&
				p.HasAny("ytInitialPlayerResponse", "ytInitialData", "embedded_player_response") &&
				!p.Has(youtubeConsent)
		},
		recognises: "videos (`/watch?v=<id>`, `/shorts/<id>`, `/embed/<id>`), playlists and channels whose page " +
			"carries the YouTube web client's configuration (`ytcfg`) beside its player or page data, " +
			"or the consent form that stands in for such a page",
	},
	{
		name:   "odysee",
		expand: true,
		why:    "its pages name no media URL, only an LBRY claim to resolve",
		shape:  odyseeShape,
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
		name:   "dailymotion",
		expand: true,
		why:    "its manifest is refused to anything but a browser, and it offers no plain file",
		shape:  dailymotionShape,
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
		name:   "soundcloud",
		expand: true,
		why:    "its API needs a client key scraped from a script bundle that rotates",
		shape:  soundcloudShape,
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

// youtubeShape is a video, a playlist or a channel by its path and query.
func youtubeShape(u *url.URL) bool {
	switch u.EscapedPath() {
	case "/watch":
		return youtubeID.MatchString(u.Query().Get("v"))
	case "/playlist":
		return u.Query().Get("list") != ""
	}
	return youtubePaths(u)
}

// The forms and markers the handoff sites are recognised by.
var (
	// youtubeID is a video id: eleven characters of the URL-safe alphabet.
	youtubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	// youtubePaths are the forms read by path: a video in each of its
	// players, and a channel's pages.
	youtubePaths = pathShape(
		`^/(?:shorts|embed|live|v)/[A-Za-z0-9_-]{11}/?$`,
		`^/(?:@[^/]+|channel/UC[A-Za-z0-9_-]{22}|c/[^/]+|user/[^/]+)(?:/(?:videos|shorts|streams|playlists|featured))?/?$`,
	)

	odyseeShape   = pathShape(`^/@[^/]+(?:/[^/]+)?/?$`, `^/(?:%24|\$)/embed/[^/]+`, `^/[^/@$][^/]*:[0-9a-f]+/?$`)
	odyseeChannel = pathShape(`^/@[^/]+/?$`)
	odyseeEmbed   = pathShape(`^/(?:%24|\$)/embed/`)

	dailymotionShape = pathShape(`^/(?:video|playlist)/x[0-9a-z]+/?$`, `^/embed/video/x[0-9a-z]+/?$`)

	bilibiliShape  = pathShape(`^(?:/en)?/video/(?:BV[0-9A-Za-z]{10}|av[0-9]+)/?$`, `^/bangumi/play/(?:ep|ss)[0-9]+/?$`)
	bilibiliPlayer = pathQueryShape(`^/player\.html\?(?:.*&)?(?:bvid|aid)=`)

	soundcloudShape  = pathShape(`^/[^/]+/sets/[^/]+/?$`)
	soundcloudPage   = pathShape(`^/[^/]+/[^/]+/?$`, `^/[^/]+/sets/[^/]+/?$`)
	soundcloudWidget = pathShape(`^/player/`)

	mixcloudShape = func(*url.URL) bool { return false }
	mixcloudPage  = pathShape(`^/[^/]+/[^/]+/$`)

	rumbleShape = pathShape(`^/v[0-9a-z]{4,}-[^/]*\.html$`, `^/embed/v[0-9a-z]{4,}/?$`)
)

const (
	// youtubeConsent marks the consent form a visitor may be shown first.
	youtubeConsent      = `name="set_ytc"`
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
	var out struct {
		Type         string `json:"type"`
		ProviderName string `json:"provider_name"`
		HTML         string `json:"html"`
	}
	return pageJSON(ctx, p, h.client, endpoint, nil, &out) &&
		strings.HasPrefix(strings.ToLower(out.ProviderName), "rumble") && out.HTML != ""
}

// Recognise reports whether the probed page is the site's.
func (h *Handoff) Recognise(ctx context.Context, p *Page) bool {
	return h.site.recognise(ctx, h, p)
}

// NewHandoffs builds one extractor per host, so each is recognised by its
// own pages and named for itself in the UI exactly as a hand-written host is.
func NewHandoffs(client *httpx.Client) []Extractor {
	out := make([]Extractor, 0, len(handoffSites))
	for _, site := range handoffSites {
		out = append(out, newHandoff(client, site))
	}
	return out
}

// newHandoff builds the extractor for one host.
func newHandoff(client *httpx.Client, site handoffSite) *Handoff {
	return &Handoff{about: site.about, client: client, site: site}
}

// Extract names the media, or lists the collection where the site expands
// one, and hands each page to the external downloader.
func (h *Handoff) Extract(ctx context.Context, u *url.URL, opts Options) (*Result, error) {
	ytdlp, ok := tools.Find(ytdlpBinary)
	if !ok {
		return nil, fmt.Errorf("%s: %s, and yt-dlp is what fetches from hosts like this; %s (%s)",
			h.name, h.site.why, tools.NotInstalled(ytdlpBinary), u.Redacted())
	}
	if h.site.expand {
		return h.list(ctx, ytdlp, u.String(), opts.maxFiles())
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
		return cmp.Or(title, handoffName(u)), nil
	}

	// Elsewhere the probe is yt-dlp reading exactly what it will read again
	// when the transfer starts, so a refusal here predicts a refusal there.
	// Reporting it now, in the host's own words, beats queueing an item that
	// dies later with the same message and less context.
	title, err := ytdlpTitle(ctx, ytdlp, u.String())
	if err != nil {
		return "", fmt.Errorf("%s: yt-dlp could not read %s: %w", h.name, u.Redacted(), err)
	}
	return cmp.Or(title, handoffName(u)), nil
}

// ytdlpBinary is the tool these hosts depend on, spelled once in the
// package that locates it.
const ytdlpBinary = tools.YtDlp

// What the probes ask yt-dlp for, one JSON object per line: each video, or
// each entry of a flat playlist, as yt-dlp reaches it, and then the playlist
// itself once its entries are done.
//
// Only the fields read are printed. -J dumps everything yt-dlp extracted,
// and for a single video that can be mostly automatic captions: a machine
// translation into each of some 180 languages, in several formats, each
// behind a long signed URL. That came to ten megabytes for one ordinary
// video, past the cap on helper output, so the probe failed before any
// download began.
//
// --simulate has to be spelled out alongside them. --print implies it only
// while every print is at the default stage, and the playlist: one is a
// later stage, which turns it off: the probe then downloads, in its
// working directory, the video it was only asked to name.
const (
	ytdlpVideoFields    = "%(.{_type,id,title,url,webpage_url})j"
	ytdlpPlaylistFields = "playlist:%(.{_type,id,title})j"
)

// ytdlpEntry is one line of that output.
type ytdlpEntry struct {
	Type   string `json:"_type"`
	ID     string `json:"id"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	WebURL string `json:"webpage_url"`
}

// ytdlpEntries reads a probe's lines, in the order yt-dlp printed them.
func ytdlpEntries(output []byte) ([]ytdlpEntry, error) {
	var entries []ytdlpEntry
	for line := range bytes.Lines(output) {
		if line = bytes.TrimSpace(line); len(line) == 0 {
			continue
		}
		var entry ytdlpEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("could not read yt-dlp output: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// list asks yt-dlp what the URL contains, without downloading anything: one
// item for a single video, one per entry for a playlist or a channel.
func (h *Handoff) list(ctx context.Context, ytdlp, target string, limit int) (*Result, error) {
	// --flat-playlist keeps a playlist probe to one request per page rather
	// than resolving every video up front. Ask for one extra entry to detect
	// truncation, and bound the listing at its source instead of reading an
	// entire channel only to discard everything past the cap.
	end := limit
	if end < math.MaxInt {
		end++
	}
	output, err := tools.Probe(ctx, ytdlp,
		"--simulate", "--no-warnings", "--flat-playlist", "--ignore-no-formats-error",
		"--print", ytdlpVideoFields, "--print", ytdlpPlaylistFields,
		"--playlist-end", strconv.Itoa(end), "--", target)
	if err != nil {
		return nil, fmt.Errorf("%s: yt-dlp could not read %s: %w", h.name, target, err)
	}
	lines, err := ytdlpEntries(output)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", h.name, err)
	}

	var playlist *ytdlpEntry
	var entries []ytdlpEntry
	for _, line := range lines {
		if line.Type == "playlist" {
			playlist = &line
		} else {
			entries = append(entries, line)
		}
	}
	if playlist == nil {
		if len(entries) == 0 {
			return nil, fmt.Errorf("%s: yt-dlp said nothing about %s", h.name, target)
		}
		video := entries[0]
		name := cmp.Or(video.Title, video.ID, "video")
		link := cmp.Or(video.WebURL, target)
		return &Result{Title: name, Files: []File{{External: link, Name: name, Size: -1}}}, nil
	}

	res := &Result{Title: cmp.Or(playlist.Title, playlist.ID, "playlist")}
	for _, entry := range entries {
		link := cmp.Or(entry.WebURL, entry.URL)
		if link == "" && entry.ID != "" && h.site.watch != "" {
			link = h.site.watch + entry.ID
		}
		if link == "" {
			continue
		}
		res.Files = append(res.Files, File{
			External: link,
			Name:     cmp.Or(entry.Title, entrySlug(link), entry.ID, "video"),
			Size:     -1,
		})
		if len(res.Files) >= limit {
			break
		}
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("%s: %s lists no videos", h.name, target)
	}
	if len(entries) > limit {
		res.Note = fileLimitNote
	}
	return res, nil
}

// ytdlpTitle asks yt-dlp what a single video is called, without downloading
// anything. A host that leaves the transfer to yt-dlp still wants a name to
// show while the item waits its turn in the queue.
func ytdlpTitle(ctx context.Context, ytdlp, target string) (string, error) {
	output, err := tools.Probe(ctx, ytdlp,
		"--simulate", "--no-warnings", "--no-playlist", "--ignore-no-formats-error",
		"--print", ytdlpVideoFields, "--", target)
	if err != nil {
		return "", err
	}
	entries, err := ytdlpEntries(output)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", nil
	}
	return cmp.Or(entries[0].Title, entries[0].ID), nil
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
				"lifts (%s)", h.name, u.Redacted())
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

// entrySlug names a listed entry that came without a title by its page's
// slug, which reads better than the id behind it. A page named by its query
// (/watch?v=<id>) has no slug worth the name, so it gives none.
func entrySlug(link string) string {
	u, err := url.Parse(link)
	if err != nil || u.RawQuery != "" || len(util.PathSegments(u)) == 0 {
		return ""
	}
	return handoffName(u)
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
	return cmp.Or(util.Stem(last), last,
		strings.ToLower(u.Hostname()))
}
