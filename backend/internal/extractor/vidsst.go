// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// VidsSt resolves vids.st video pages (/v/<id>, and the embed /e/<id>).
//
// The page states everything in one object, `playerConfig`: the file's
// address, its original name and the thumbnail. The address is usually the
// uploaded file itself — an MP4, an MKV — served under /storage/uploads with
// no token and ranges honoured, so it is downloaded as it is. The player also
// plays HLS when the address is a playlist, and so does this, natively.
//
// The site sends its certificate without the intermediate that links it to
// a trusted root, which a browser completes on its own and nothing else
// does; httpx completes it the same way (aia.go), and nothing here has to
// know.
type VidsSt struct {
	client *httpx.Client
}

// NewVidsSt builds the vids.st extractor.
func NewVidsSt(client *httpx.Client) *VidsSt {
	return &VidsSt{client: client}
}

func (v *VidsSt) Name() string { return "vids.st" }

var (
	// vidsShape is a watch page or a player by the numeric id the site
	// issues, /v/<id> or /e/<id>; vidsPage takes any id of that form.
	vidsShape = pathShape(`^/[ve]/[0-9]+/?$`)
	vidsPage  = pathShape(`^/[ve]/[A-Za-z0-9]+/?$`)
)

// Recognises is the inventory's description.
func (v *VidsSt) Recognises() string {
	return "videos (`/v/<id>`, `/e/<id>`) whose page carries the player's `playerConfig` with its `videoUrl`"
}

// Shape reports a watch page or a player.
func (v *VidsSt) Shape(u *url.URL) bool { return vidsShape(u) }

// Recognise takes a page carrying the player configuration Extract reads.
func (v *VidsSt) Recognise(_ context.Context, p *Page) bool {
	return vidsPage(p.Final) && p.Has(vidsConfigMarker, `"videoUrl"`)
}

// vidsConfigMarker opens the page's player configuration.
const vidsConfigMarker = "playerConfig = "

// vidsConfig is the part of the player configuration worth reading.
type vidsConfig struct {
	VideoURL  string `json:"videoUrl"`
	VideoName string `json:"videoName"`
}

// Extract resolves one video page.
func (v *VidsSt) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	id := vidsID(u)
	if id == "" {
		return nil, fmt.Errorf("vids.st: %s names no video (expected /v/<id>)", u.Redacted())
	}
	page := util.Origin(u) + "/v/" + id
	headers := httpx.Referer(page)

	doc, err := fetchPage(ctx, v.client, page, headers)
	if httpx.HasStatus(err, http.StatusNotFound) {
		return nil, fmt.Errorf("vids.st: video %s is not there — the site says it was not found or has been removed", id)
	}
	if err != nil {
		return nil, fmt.Errorf("vids.st: fetch %s: %w", page, err)
	}
	cfg, err := vidsPlayerConfig(doc)
	if err != nil {
		return nil, fmt.Errorf("vids.st: %s: %w", page, err)
	}

	link := resolveRef(u, cfg.VideoURL)
	name := util.FirstNonEmpty(strings.TrimSpace(cfg.VideoName), util.NameFromURL(link), "vids.st-"+id)
	title := strings.TrimSuffix(name, path.Ext(name))

	if !vidsPlaylist(link) {
		return &Result{Title: title, Files: []File{{Name: name, URL: link, Size: -1, Headers: headers}}}, nil
	}

	// A playlist: followed to its segments, decrypted if it is encrypted,
	// and named after the container the segments actually are.
	media, err := resolveMediaPlaylist(ctx, v.client, link, headers)
	if err != nil {
		return nil, fmt.Errorf("vids.st: %s: %w", page, err)
	}
	return &Result{Title: title, Files: []File{{
		Name:       title + media.extension(),
		URL:        link,
		Size:       -1,
		Headers:    headers,
		Segments:   media.Segments,
		SegmentKey: media.Key,
		Audio:      media.Audio,
		Subtitles:  media.Subtitles,
	}}}, nil
}

// vidsPlayerConfig reads the player configuration out of the page.
func vidsPlayerConfig(doc string) (*vidsConfig, error) {
	at := strings.Index(doc, vidsConfigMarker)
	if at < 0 {
		return nil, fmt.Errorf("no player on the page (the video may have been removed)")
	}
	var cfg vidsConfig
	// The object is followed by the rest of the script; Decode reads the
	// one value and stops.
	if err := json.NewDecoder(strings.NewReader(doc[at+len(vidsConfigMarker):])).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("read the player configuration: %w", err)
	}
	if cfg.VideoURL == "" {
		return nil, fmt.Errorf("the player configuration names no video")
	}
	return &cfg, nil
}

// vidsPlaylist reports whether the player's address is an HLS playlist,
// which it decides the way the player does: by the address's ending.
func vidsPlaylist(link string) bool {
	if u, err := url.Parse(link); err == nil {
		return strings.HasSuffix(strings.ToLower(u.Path), ".m3u8")
	}
	return false
}

// vidsID reads the video id out of /v/<id> or the embed's /e/<id>.
func vidsID(u *url.URL) string {
	segs := util.PathSegments(u)
	if len(segs) >= 2 && (segs[0] == "v" || segs[0] == "e") {
		return segs[1]
	}
	return ""
}
