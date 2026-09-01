package extractor

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Vimeo resolves public and unlisted videos.
//
// Two facts about this host decide the shape of everything below.
//
// The first is that the watch page is the wrong door. vimeo.com/<id> sits
// behind a bot check that answers anything but a browser with a 404, and the
// player's JSON configuration endpoint answers 403 outright. The player page
// — player.vimeo.com/video/<id>, the one an iframe on somebody else's site
// loads — serves the whole configuration to anyone: no session, no token,
// and no referer needed. Everything here therefore addresses that URL, and
// that alone is the difference between "requires an account" and "works".
//
// The second is that a vimeo stream arrives demuxed. Its master playlist
// offers video renditions and names a separate audio group, so concatenating
// one rendition's segments — which is all the playlist engine here does —
// would produce a silent video. Modern uploads no longer carry progressive
// MP4s to fall back on either, so there is nothing this program can fetch
// directly and end up with a playable file. Joining the two streams needs
// ffmpeg, which is exactly what the external downloader is for, so the
// player URL is handed to yt-dlp.
type Vimeo struct{ hostSet }

// NewVimeo builds the vimeo extractor.
func NewVimeo() *Vimeo { return &Vimeo{hostSet: hostSet{"vimeo.com", "player.vimeo.com"}} }

func (v *Vimeo) Name() string { return "vimeo" }

// Extract names the video and leaves the download to the helper script.
func (v *Vimeo) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id, hash := vimeoVideoID(u)
	if id == "" {
		return nil, fmt.Errorf("vimeo: no video id in %s "+
			"(a link to a channel or a profile names no single video)", u.Redacted())
	}

	ytdlp, ok := tools.Find(ytdlpBinary)
	if !ok {
		return nil, fmt.Errorf("vimeo: yt-dlp is not installed — run `make dependencies` "+
			"to fetch it into the folder holding heapleach, or put it on PATH (%s)", u.Redacted())
	}

	player := vimeoPlayerURL(id, hash)
	title, err := v.probe(ctx, ytdlp, player)
	if err != nil {
		return nil, err
	}
	return &Result{Title: title, Files: []File{{
		Name:     title + ".mp4",
		Size:     -1,
		External: player,
	}}}, nil
}

// probe asks yt-dlp what the player URL holds, without downloading anything.
func (v *Vimeo) probe(ctx context.Context, ytdlp, player string) (string, error) {
	title, err := ytdlpTitle(ctx, ytdlp, player)
	if err != nil {
		return "", vimeoError(player, err)
	}
	return util.FirstNonEmpty(title, "vimeo"), nil
}

// vimeoError explains the one failure a user can act on. A video that is
// private, or restricted to the sites its owner allows, cannot be opened by
// anyone not signed in, and saying so is more use than yt-dlp's advice about
// passing cookies to a program the user is not running.
func vimeoError(player string, err error) error {
	reported := ytdlpError(err)
	if text := strings.ToLower(reported.Error()); strings.Contains(text, "logged-in") ||
		strings.Contains(text, "log in") || strings.Contains(text, "password") ||
		strings.Contains(text, "private") {
		return fmt.Errorf("vimeo: this video is not public — it is private, "+
			"password protected, or embeddable only from its owner's own site, "+
			"so it cannot be fetched without an account (%s)", player)
	}
	return fmt.Errorf("vimeo: yt-dlp could not read %s: %w", player, reported)
}

// vimeoPlayerURL builds the embed URL, carrying the unlisted hash when the
// link had one.
func vimeoPlayerURL(id, hash string) string {
	player := "https://player.vimeo.com/video/" + url.PathEscape(id)
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
