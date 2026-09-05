package extractor

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
// it properly was already here in resolvePlaylist and the segment transfer
// behind it; what was missing was anything to call it for a URL no host
// extractor claimed.
//
// Three shapes are refused by name rather than half-handled, because each
// would otherwise produce a file that looks finished and is not:
//
//   - MPEG-DASH (.mpd). A DASH representation is usually video with no audio
//     of its own, and its segments are generated from a template rather than
//     listed, so there is nothing here to concatenate and what came out would
//     be silent. That is the same trap hlsVariant.muxed exists to catch, and
//     it is matched only so the refusal can say so: left to the fallback it
//     downloads as a small XML file that looks like a video.
//   - A master playlist whose every variant keeps its audio in a rendition of
//     its own, for the same reason.
//   - A playlist still being written. Without #EXT-X-ENDLIST there is no last
//     segment, so what would be saved is however much of a live stream had
//     been produced by the time the queue reached it.
//
// Each refusal names the external downloader, which has ffmpeg and can do
// what this deliberately does not.
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

// Match claims a manifest by the extension on its path, which is read before
// the query: these URLs nearly always carry a token or a signature after the
// '?', and none of that is part of the name.
//
// DASH is matched only in order to be refused. A refusal that explains itself
// is worth more than the silent handover to the fallback that produced the
// defect this file exists to fix.
func (h *HLSDirect) Match(u *url.URL) bool {
	switch strings.ToLower(path.Ext(u.Path)) {
	case ".m3u8", ".mpd":
		return true
	}
	return false
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

// hlsPlaylistResult resolves a manifest URL, refusing the two playlists that
// would otherwise finish as an unplayable file.
func hlsPlaylistResult(ctx context.Context, client *httpx.Client, u *url.URL) (*Result, error) {
	headers := httpx.Referer(util.Origin(u) + "/")

	media, err := resolveMediaPlaylist(ctx, client, u.String(), headers)
	if err != nil {
		return nil, fmt.Errorf("hls: %s: %w", u.Redacted(), err)
	}

	// The resolver reports the variant it settled on, and hands the URL back
	// unchanged when the document was a media playlist rather than a master.
	// That is what tells the two apart here, and the distinction matters: a
	// media playlist declares no codecs at all, so muxed() is false for one
	// and checking it unconditionally would refuse every bare playlist a
	// user pastes.
	variant := media.Variant
	if variant.URL != u.String() && !variant.muxed() {
		return nil, fmt.Errorf("hls: %s offers no rendition that carries its own audio — every one of them "+
			"keeps it in a separate track — so joining any single variant would save video with no sound. "+
			"Queue the page the manifest belongs to instead: that goes to the external downloader (yt-dlp), "+
			"which has ffmpeg to mux the two back together", u.Redacted())
	}

	if live, why := hlsLiveEdge(media.Doc); live {
		return nil, fmt.Errorf("hls: %s is still being written — %s. Downloading it now would save however "+
			"much of the stream exists at this moment, under a name that says it is the whole thing. Wait "+
			"for the stream to end, or hand the page to the external downloader (yt-dlp), which can follow "+
			"a live edge", u.Redacted(), why)
	}

	name := hlsName(u)
	return &Result{Title: name, Files: []File{{
		Name: name + playlistExtension(variant),
		// A playlist has no length until every part has been fetched, and
		// guessing one from the durations would be a number the skip-what-is-
		// already-downloaded check could act on.
		Size:     -1,
		Headers:  headers,
		Segments: media.Segments,
	}}}, nil
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

// hlsSniff resolves a URL that serves a playlist although nothing about the
// URL says so.
//
// The precedent is kvsSniff: a shape worth one request, and nothing lost when
// the guess is wrong — a nil result and a nil error mean "not a manifest",
// and the caller carries on exactly as it would have. Here the shape is a
// path that names no file at all: a URL ending in a filename has said what it
// is serving and is taken at its word, while one that does not has said
// nothing, and a manifest handed out from such a path is precisely what would
// otherwise be saved as a few kilobytes of text. Keeping the guard that
// narrow matters because some hosts sign a link for a single use, and
// sniffing every direct download would spend that signature before the
// transfer got to it.
//
// It departs from kvsSniff in one place, and deliberately. Once the response
// has identified itself as a playlist, a failure to resolve it is reported
// rather than swallowed: falling through would hand the manifest back to the
// fallback and save it as a file, which is the very failure this file exists
// to stop. Only the "is this a manifest at all" question falls through.
func hlsSniff(ctx context.Context, client *httpx.Client, u *url.URL) (*Result, error) {
	if !hlsSniffable(u) || !hlsServesManifest(ctx, client, u) {
		return nil, nil
	}
	return hlsPlaylistResult(ctx, client, u)
}

// hlsSniffable reports whether a URL is worth one bounded request before it
// is treated as a file: its path names no extension, so the host has not said
// what is behind it.
func hlsSniffable(u *url.URL) bool {
	segs := util.PathSegments(u)
	return len(segs) > 0 && path.Ext(segs[len(segs)-1]) == ""
}

// hlsServesManifest asks for the opening bytes of a URL and reports whether
// what comes back is a playlist.
//
// A stated type is believed, and the body is read only when there is none
// worth believing: origins serving media out of object storage routinely
// label everything text/plain or application/octet-stream, and a playlist
// announces itself on its first line anyway. The request asks for a range so
// that being wrong costs a few hundred bytes rather than the opening of a
// video, and it never retries — a host that will not answer at once is not
// worth delaying a download this may have nothing to do with.
func hlsServesManifest(ctx context.Context, client *httpx.Client, u *url.URL) bool {
	req, err := client.NewRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	req.Header.Set(httpx.HeaderAccept, httpx.AcceptAny)
	req.Header.Set(httpx.HeaderReferer, util.Origin(u)+"/")
	req.Header.Set(httpx.HeaderRange, fmt.Sprintf("bytes=0-%d", config.ManifestSniffBytes-1))

	resp, err := client.DoOnce(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return false
	}
	if hlsManifestType(resp.Header.Get(httpx.HeaderContentType)) {
		return true
	}
	prefix, err := io.ReadAll(io.LimitReader(resp.Body, config.ManifestSniffBytes))
	return err == nil && hlsManifestBody(string(prefix))
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
	return strings.HasPrefix(strings.TrimLeft(prefix, " \t\r\n"), hlsPlaylistTag)
}
