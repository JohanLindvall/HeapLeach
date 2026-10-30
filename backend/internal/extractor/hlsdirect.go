// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// A bare adaptive manifest, pasted straight into the queue.
//
// An .m3u8 is a text file listing somebody else's media, and the fallback
// extractor treats any URL as a file — so pasting one saved a few kilobytes
// of playlist under the video's name and reported a finished download.
// Nothing about that is visible until the file is opened. The machinery to do
// it properly was already here in resolveMediaPlaylist and the segment
// transfer behind it; what was missing was anything to call it for a URL no
// host extractor claimed. A master whose every variant keeps its audio in a
// rendition of its own comes with those renditions, which the downloader
// joins to the video.
//
// Two shapes are refused by name rather than half-handled, because each
// would otherwise produce a file that looks finished and is not:
//
//   - MPEG-DASH (.mpd). A DASH representation is usually video with no audio
//     of its own, and its segments are generated from a template rather than
//     listed, so there is nothing here to concatenate and what came out would
//     be silent. That is the same trap hlsVariant.muxed exists to catch, and
//     it is matched only so the refusal can say so: left to the fallback it
//     downloads as a small XML file that looks like a video.
//   - A playlist still being written. Without #EXT-X-ENDLIST there is no last
//     segment, so what would be saved is however much of a live stream had
//     been produced by the time the queue reached it.
//
// Each refusal names the external downloader, which can do what this
// deliberately does not.
type HLSDirect struct {
	client *httpx.Client
}

// Tags read straight off a playlist. Only these three carry a decision; the
// rest of the parsing lives in hls.go.
const (
	hlsPlaylistTag     = "#EXTM3U"
	hlsEndListTag      = "#EXT-X-ENDLIST"
	hlsPlaylistTypeTag = "#EXT-X-PLAYLIST-TYPE:"
)

// NewHLSDirect builds the bare-manifest extractor.
func NewHLSDirect(client *httpx.Client) *HLSDirect { return &HLSDirect{client: client} }

func (h *HLSDirect) Name() string { return "hls" }

// Recognises is the inventory's description.
func (h *HLSDirect) Recognises() string {
	return "bare HLS manifests: a response served as a playlist type or opening `#EXTM3U`, at any " +
		"URL, or a `.m3u8` link; `.mpd` links are taken in order to refuse them with the reason"
}

// Shape claims a manifest by the extension on its path, which is read
// before the query: these URLs nearly always carry a token or a signature
// after the '?', and none of that is part of the name.
//
// DASH is matched only in order to be refused. A refusal that explains itself
// is worth more than the silent handover to the fallback that produced the
// defect this file exists to fix.
func (h *HLSDirect) Shape(u *url.URL) bool {
	switch strings.ToLower(path.Ext(u.Path)) {
	case ".m3u8", ".mpd":
		return true
	}
	return false
}

// Recognise takes what answered as a playlist — by its declared type, or by
// its first line — whatever the URL looks like, and a link that names a
// manifest by its extension even where the probe could not read it: the
// form is unmistakable, and the fallback would save the manifest as a file.
func (h *HLSDirect) Recognise(_ context.Context, p *Page) bool {
	if hlsManifestType(p.MediaType()) {
		return true
	}
	if p.OK() && hlsManifestBody(p.Body[:min(len(p.Body), config.ManifestSniffBytes)]) {
		return true
	}
	return h.Shape(p.URL) || h.Shape(p.Final)
}

// Extract resolves the manifest into the one file its segments join into.
func (h *HLSDirect) Extract(ctx context.Context, u *url.URL, _ Options) (*Result, error) {
	if strings.EqualFold(path.Ext(u.Path), ".mpd") {
		return nil, fmt.Errorf("hls: %s is an MPEG-DASH manifest, which cannot be joined the way an HLS "+
			"one can: a DASH representation is usually video carrying no audio of its own, and its segments "+
			"are described by a template rather than listed, so concatenating them would give a silent file. "+
			"Queue the page the manifest belongs to instead — that goes to the external downloader (yt-dlp), "+
			"which has ffmpeg to mux with", u.Redacted())
	}
	return hlsPlaylistResult(ctx, h.client, u)
}

// hlsPlaylistResult resolves a manifest URL, refusing a playlist still being
// written, which would otherwise finish as a fragment named as the whole.
func hlsPlaylistResult(ctx context.Context, client *httpx.Client, u *url.URL) (*Result, error) {
	headers := httpx.Referer(util.Origin(u) + "/")

	media, err := resolveMediaPlaylist(ctx, client, u.String(), headers)
	if err != nil {
		return nil, fmt.Errorf("hls: %s: %w", u.Redacted(), err)
	}

	// A variant that points its audio at a separate group comes with that
	// group's renditions as tracks of their own (media.Audio), which the
	// downloader joins to the video. It used to be refused, sent to the
	// external downloader for the muxing.

	if live, why := hlsLiveEdge(media.Doc); live {
		return nil, fmt.Errorf("hls: %s is still being written — %s. Downloading it now would save however "+
			"much of the stream exists at this moment, under a name that says it is the whole thing. Wait "+
			"for the stream to end, or hand the page to the external downloader (yt-dlp), which can follow "+
			"a live edge", u.Redacted(), why)
	}

	name := hlsName(u)
	return &Result{Title: name, Files: []File{media.file(name+media.extension(), headers)}}, nil
}

// hlsLiveEdge reports whether a media playlist is still being written, and if
// so what said so.
//
// #EXT-X-ENDLIST is the only statement a playlist makes about being complete,
// so its absence — rather than the presence of any particular tag — is what
// decides this. PLAYLIST-TYPE is read only to say why: EVENT means segments
// are being appended as they are produced, which is worth naming because it
// tells the user to come back later rather than to report a broken link. An
// event that has since finished carries an ENDLIST like anything else and is
// downloadable, so the tag alone never refuses one.
func hlsLiveEdge(doc string) (bool, string) {
	var event bool
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == hlsEndListTag:
			return false, ""
		case strings.HasPrefix(line, hlsPlaylistTypeTag):
			event = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, hlsPlaylistTypeTag)), "EVENT")
		}
	}
	if event {
		return true, "it is an ongoing event (#EXT-X-PLAYLIST-TYPE:EVENT) with no #EXT-X-ENDLIST yet"
	}
	return true, "it carries no #EXT-X-ENDLIST"
}

// hlsGenericNames are the names a manifest is given for its role rather than
// for what it holds. Nearly every host uses one of them, so taking the
// basename at face value would file half of these downloads as index.ts.
var hlsGenericNames = map[string]bool{
	"chunklist":  true,
	"hls":        true,
	"index":      true,
	"manifest":   true,
	"master":     true,
	"media":      true,
	"playlist":   true,
	"prog_index": true,
	"stream":     true,
}

// hlsName picks the stem to save under: the deepest path segment that names
// something rather than a role. The host is the last resort, which at least
// distinguishes one such download from the next.
func hlsName(u *url.URL) string {
	segs := util.PathSegments(u)
	for _, seg := range slices.Backward(segs) {
		seg := strings.TrimSuffix(seg, path.Ext(seg))
		if seg != "" && !hlsGenericNames[strings.ToLower(seg)] {
			return seg
		}
	}
	return u.Hostname()
}

// hlsManifestMediaTypes are the spellings a playlist arrives under. The first
// two are what HLS hosts send; the others predate HLS and are still emitted
// by origins that treat an .m3u8 as the audio playlist it grew out of.
var hlsManifestMediaTypes = map[string]bool{
	"application/vnd.apple.mpegurl": true,
	"application/x-mpegurl":         true,
	"application/mpegurl":           true,
	"audio/mpegurl":                 true,
	"audio/x-mpegurl":               true,
}

// hlsManifestType reports whether a Content-Type names a playlist, ignoring
// any charset parameter hung off it.
func hlsManifestType(value string) bool {
	base, _, _ := strings.Cut(value, ";")
	return hlsManifestMediaTypes[strings.ToLower(strings.TrimSpace(base))]
}

// hlsManifestBody reports whether a response opens as a playlist. The tag is
// required to be the document's first line; a byte-order mark in front of it
// is not legal but is common enough from origins that re-encode.
func hlsManifestBody(prefix string) bool {
	prefix = strings.TrimPrefix(prefix, "\ufeff")
	first, _, _ := strings.Cut(strings.TrimLeft(prefix, " \t\r\n"), "\n")
	return strings.TrimSpace(first) == hlsPlaylistTag
}
