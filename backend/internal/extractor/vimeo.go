// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Vimeo resolves public and unlisted videos.
//
// Three facts about this host decide the shape of everything below.
//
// The first is that the watch page is the wrong door. vimeo.com/<id> sits
// behind a bot check that answers anything but a browser with a 404, and the
// player's JSON configuration endpoint answers 403 outright. The player page
// — player.vimeo.com/video/<id>, the one an iframe on somebody else's site
// loads — carries the whole configuration (window.playerConfig) to anyone
// with a browser's TLS: no session, no token, and no referer needed. curl's
// own handshake is turned away from it ("We couldn't verify the security of
// your connection"), which the client's Chrome-shaped one is not.
//
// The second is that a vimeo stream arrives demuxed. Its HLS master offers
// video renditions, names a separate audio group, and lists its subtitles in
// playlists of their own, and the downloader joins them all (download/mux.go).
// It used to be handed to yt-dlp for that. Progressive MP4s, which modern
// uploads mostly no longer carry, are taken only when the HLS cannot be read.
// Every link is signed for an hour, so the page is read again when the
// transfer starts.
//
// The third is that some videos are DRM-protected in every form they are
// offered in, FairPlay for HLS and Widevine for DASH, which nothing can
// decrypt: yt-dlp fails on them too. The configuration says so (request.drm),
// and those are refused in so many words.
type Vimeo struct {
	client *httpx.Client
	// player is where the player pages are, a field so that tests can stand
	// in for it.
	player string
}

// vimeoPlayerRoot is where the player pages are.
const vimeoPlayerRoot = "https://player.vimeo.com/video/"

// vimeoConfigMarker opens the player page's configuration.
const vimeoConfigMarker = "window.playerConfig = "

// NewVimeo builds the vimeo extractor.
func NewVimeo(client *httpx.Client) *Vimeo {
	return &Vimeo{client: client, player: vimeoPlayerRoot}
}

func (v *Vimeo) Name() string { return "vimeo" }

var (
	// vimeoWatchShape is a video's own page in each of the places it is
	// listed: /<id>, /<id>/<hash>, a channel's, a group's, an album's.
	vimeoWatchShape = pathShape(
		`^/[0-9]{3,}(?:/[0-9a-f]{6,})?/?$`,
		`^/channels/[^/]+/[0-9]{3,}/?$`,
		`^/groups/[^/]+/videos/[0-9]{3,}/?$`,
		`^/album/[0-9]+/video/[0-9]{3,}/?$`,
	)
	// vimeoPlayerShape is the player page, /video/<id>.
	vimeoPlayerShape = pathShape(`^/video/[0-9]{3,}/?$`)
	// vimeoPlayerMeta is the player a watch page offers for embedding.
	vimeoPlayerMeta = pathShape(`^/video/[0-9]+`)
)

// Recognises is the inventory's description.
func (v *Vimeo) Recognises() string {
	return "player pages (`/video/<id>`) carrying `window.playerConfig`, and watch pages " +
		"(`/<id>`, `/<id>/<hash>`, channel, group and album videos) of a site named Vimeo " +
		"that offer that player for embedding"
}

// Shape reports a watch page or a player page.
func (v *Vimeo) Shape(u *url.URL) bool { return vimeoWatchShape(u) || vimeoPlayerShape(u) }

// Recognise takes the player page by the configuration this extractor
// reads, and a watch page by the player it names in its meta tags — the
// page this extractor goes on to read. A watch page answers anything but a
// browser with a bot check, but that check still carries the meta tags.
func (v *Vimeo) Recognise(_ context.Context, p *Page) bool {
	if vimeoPlayerShape(p.Final) {
		return p.Has(vimeoConfigMarker)
	}
	return vimeoWatchShape(p.Final) && p.playerMeta(vimeoPlayerMeta) && p.Named("Vimeo")
}

// Extract reads the player page into the one file it plays, read again
// before each transfer for links freshly signed.
func (v *Vimeo) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id, hash := vimeoVideoID(u)
	if id == "" {
		return nil, fmt.Errorf("vimeo: no video id in %s "+
			"(a link to a channel or a profile names no single video)", u.Redacted())
	}
	player := vimeoPlayerURL(v.player, id, hash)
	video, err := v.read(ctx, player, "")
	if err != nil {
		return nil, err
	}
	file := video.file
	kind := video.kind
	file.Resolve = func(ctx context.Context) (*Target, error) {
		fresh, err := v.read(ctx, player, kind)
		if err != nil {
			return nil, err
		}
		f := fresh.file
		return &Target{URL: f.URL, Name: f.Name, Size: -1, Headers: f.Headers,
			Segments: f.Segments, SegmentKey: f.SegmentKey, Audio: f.Audio, Subtitles: f.Subtitles}, nil
	}
	return &Result{Title: video.title, Files: []File{file}}, nil
}

// vimeoConfig is the part of the player's configuration read here.
type vimeoConfig struct {
	// View is 1 for a video the page will play; a private or password
	// protected one comes back otherwise, with nothing to play.
	View    int `json:"view"`
	Request struct {
		Files struct {
			HLS struct {
				DefaultCDN string `json:"default_cdn"`
				CDNs       map[string]struct {
					URL    string `json:"url"`
					AVCURL string `json:"avc_url"`
				} `json:"cdns"`
			} `json:"hls"`
			Progressive []struct {
				URL    string `json:"url"`
				Height int    `json:"height"`
			} `json:"progressive"`
		} `json:"files"`
		DRM json.RawMessage `json:"drm"`
	} `json:"request"`
	Video struct {
		Title     string          `json:"title"`
		LiveEvent json.RawMessage `json:"live_event"`
	} `json:"video"`
}

// vimeoVideo is one reading of a player page: the file it plays, which of
// the two kinds of stream that file is, and the video's title.
type vimeoVideo struct {
	file  File
	kind  string
	title string
}

// The two kinds of stream a video is fetched as.
const (
	vimeoHLS         = "hls"
	vimeoProgressive = "progressive"
)

// errVimeoNotPublic is the refusal of a video only its owner, or a site its
// owner allows, can play.
var errVimeoNotPublic = errors.New("this video is not public — it is private, password protected, " +
	"or embeddable only on its owner's own site, so it cannot be fetched without an account")

// read fetches the player page and makes the file it plays. kind, when set,
// holds a reading to the kind of stream an earlier one chose, so that a
// transfer resumed with fresh links resumes the same stream.
func (v *Vimeo) read(ctx context.Context, player, kind string) (*vimeoVideo, error) {
	cfg, err := v.config(ctx, player)
	if err != nil {
		return nil, err
	}
	title := cmp.Or(strings.TrimSpace(cfg.Video.Title), "vimeo")
	headers := httpx.Referer("https://player.vimeo.com/")

	var hlsErr error
	if kind != vimeoProgressive {
		file, err := v.hls(ctx, cfg, title, headers)
		if err == nil {
			return &vimeoVideo{file: *file, kind: vimeoHLS, title: title}, nil
		}
		if kind == vimeoHLS || ctx.Err() != nil {
			return nil, fmt.Errorf("vimeo: %s: %w", player, err)
		}
		hlsErr = err
	}
	if file, ok := vimeoProgressiveFile(cfg, title, headers); ok {
		return &vimeoVideo{file: file, kind: vimeoProgressive, title: title}, nil
	}
	if vimeoDRM(cfg) {
		return nil, fmt.Errorf("vimeo: %s is DRM-protected in every form it is offered in, "+
			"which nothing can decrypt (%s)", title, player)
	}
	return nil, fmt.Errorf("vimeo: %s offers nothing to fetch: %w", player, cmp.Or(hlsErr, errors.New("no stream")))
}

// config fetches the player page and reads its configuration.
func (v *Vimeo) config(ctx context.Context, player string) (*vimeoConfig, error) {
	playerURL, err := url.Parse(player)
	if err != nil {
		return nil, fmt.Errorf("vimeo: %s: %w", player, err)
	}
	// The player page is often the page submitted, already probed.
	page, _, err := fetchDocument(ctx, v.client, playerURL, httpx.Referer("https://vimeo.com/"))
	if err != nil {
		if httpx.HasStatus(err, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound) {
			return nil, fmt.Errorf("vimeo: %w (%s)", errVimeoNotPublic, player)
		}
		return nil, fmt.Errorf("vimeo: %s: %w", player, err)
	}
	i := strings.Index(page, vimeoConfigMarker)
	if i < 0 {
		return nil, fmt.Errorf("vimeo: %w (%s)", errVimeoNotPublic, player)
	}
	var cfg vimeoConfig
	if err := json.NewDecoder(strings.NewReader(page[i+len(vimeoConfigMarker):])).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("vimeo: read the player configuration of %s: %w", player, err)
	}
	if cfg.View != 1 {
		return nil, fmt.Errorf("vimeo: %w (%s)", errVimeoNotPublic, player)
	}
	return &cfg, nil
}

// hls resolves the configuration's HLS master into the file: its video, and
// every audio and subtitle track beside it. The AVC master is preferred, the
// one every player decodes.
func (v *Vimeo) hls(ctx context.Context, cfg *vimeoConfig, title string, headers httpx.Header) (*File, error) {
	if vimeoLive(cfg) {
		return nil, errors.New("this is a live event, which has no end to download to")
	}
	hls := cfg.Request.Files.HLS
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range hls.CDNs {
			if !yield(name) {
				return
			}
		}
	})
	if i := slices.Index(names, hls.DefaultCDN); i > 0 {
		names[0], names[i] = names[i], names[0]
	}
	var master string
	for _, name := range names {
		if cdn := hls.CDNs[name]; cmp.Or(cdn.AVCURL, cdn.URL) != "" {
			master = cmp.Or(cdn.AVCURL, cdn.URL)
			break
		}
	}
	if master == "" {
		return nil, errors.New("the configuration names no HLS stream")
	}
	media, err := resolveMediaPlaylist(ctx, v.client, master, headers)
	if err != nil {
		return nil, err
	}
	file := media.target(master, title+media.extension(), headers).file("")
	return &file, nil
}

// vimeoProgressiveFile is the tallest progressive MP4 the configuration
// offers, one file carrying audio and video together.
func vimeoProgressiveFile(cfg *vimeoConfig, title string, headers httpx.Header) (File, bool) {
	files := cfg.Request.Files.Progressive
	best := -1
	for i, f := range files {
		if f.URL != "" && (best < 0 || f.Height > files[best].Height) {
			best = i
		}
	}
	if best < 0 {
		return File{}, false
	}
	return File{Name: title + ".mp4", URL: files[best].URL, Size: -1, Headers: headers}, true
}

// vimeoDRM reports a configuration that names DRM for the video.
func vimeoDRM(cfg *vimeoConfig) bool {
	raw := bytes.TrimSpace(cfg.Request.DRM)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// vimeoLive reports a live event.
func vimeoLive(cfg *vimeoConfig) bool {
	raw := bytes.TrimSpace(cfg.Video.LiveEvent)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

// vimeoPlayerURL builds the embed URL under root (vimeoPlayerRoot, or a
// test's server), carrying the unlisted hash when the link had one.
func vimeoPlayerURL(root, id, hash string) string {
	player := root + url.PathEscape(id)
	if hash != "" {
		player += "?h=" + url.QueryEscape(hash)
	}
	return player
}

// vimeoVideoID reads the id, and any unlisted hash, out of the link shapes
// vimeo uses: /<id>, /<id>/<hash>, /channels/<name>/<id>,
// /groups/<name>/videos/<id>, /album/<id>/video/<id>, and the player's own
// /video/<id>?h=<hash>.
//
// The id is the last numeric segment rather than a fixed position, which is
// what covers all of those without a pattern for each. A segment following
// it is the hash an unlisted link carries.
func vimeoVideoID(u *url.URL) (id, hash string) {
	segs := util.PathSegments(u)
	for i, seg := range slices.Backward(segs) {
		if !vimeoIsID(seg) {
			continue
		}
		id = seg
		if i+1 < len(segs) && !vimeoIsID(segs[i+1]) {
			hash = segs[i+1]
		}
		break
	}
	if hash == "" {
		hash = u.Query().Get("h")
	}
	return id, hash
}

// vimeoIsID reports whether a path segment is a video id.
func vimeoIsID(seg string) bool {
	if seg == "" {
		return false
	}
	_, err := strconv.ParseUint(seg, 10, 64)
	return err == nil
}
