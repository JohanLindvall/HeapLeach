// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Minimal HLS support: enough to turn a master playlist into an ordered list
// of segment URLs.
//
// Adaptive streams have no single file to fetch, so a playlist-backed file
// carries its segments instead of a URL and the downloader concatenates
// them. Variants that carry audio and video together are preferred, because
// concatenating those alone yields a playable file — separate renditions
// would have to be muxed back together afterwards.
//
// A host whose master playlist offers nothing self-contained cannot be
// served by this at all: the best variant would still be video only. Such a
// host belongs on the external downloader, which has ffmpeg to mux with.

// hlsVariant is one entry of a master playlist.
type hlsVariant struct {
	URL        string
	Bandwidth  int
	Resolution string
	Codecs     string
	// audioElsewhere records that the variant named an audio group whose
	// renditions have playlists of their own, which means the variant's own
	// segments carry video and nothing else.
	audioElsewhere bool
}

// muxed reports whether this variant carries its own audio, which makes it
// usable without a separate audio rendition.
func (v hlsVariant) muxed() bool {
	// CODECS describes the whole presentation — the video plus whatever
	// audio group the variant names — so on its own it says nothing about
	// what the variant's segments contain. Vimeo advertises "avc1,mp4a" on
	// renditions that are video only, and believing it would produce a
	// silent file that looks like a finished download.
	if v.audioElsewhere {
		return false
	}
	codecs := strings.ToLower(v.Codecs)
	if codecs == "" {
		return false
	}
	hasVideo := strings.Contains(codecs, "avc") || strings.Contains(codecs, "hvc") ||
		strings.Contains(codecs, "hev") || strings.Contains(codecs, "vp")
	hasAudio := strings.Contains(codecs, "mp4a") || strings.Contains(codecs, "ac-3") ||
		strings.Contains(codecs, "opus")
	return hasVideo && hasAudio
}

// height parses the vertical resolution, for choosing the best variant.
func (v hlsVariant) height() int {
	_, h, ok := strings.Cut(v.Resolution, "x")
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(h)
	return n
}

// parseMasterPlaylist reads the variants out of a master playlist. It
// returns nil when the document is a media playlist instead.
func parseMasterPlaylist(doc string, base *url.URL) []hlsVariant {
	separate := parseAudioGroups(doc)
	var (
		variants []hlsVariant
		pending  *hlsVariant
	)
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			attrs := parseAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			bandwidth, _ := strconv.Atoi(attrs["BANDWIDTH"])
			pending = &hlsVariant{
				Bandwidth:      bandwidth,
				Resolution:     attrs["RESOLUTION"],
				Codecs:         attrs["CODECS"],
				audioElsewhere: separate[attrs["AUDIO"]],
			}
		case strings.HasPrefix(line, "#"):
			continue
		default:
			if pending != nil {
				pending.URL = resolveRef(base, line)
				variants = append(variants, *pending)
				pending = nil
			}
		}
	}
	return variants
}

// parseAudioGroups finds the audio groups whose renditions live in their own
// playlists, which is what makes the variants naming them video only.
//
// An EXT-X-MEDIA entry without a URI describes audio that is already inside
// the variant's segments, so only the ones that have a URI count.
func parseAudioGroups(doc string) map[string]bool {
	groups := make(map[string]bool)
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		attrs := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
		if !strings.EqualFold(attrs["TYPE"], "AUDIO") || attrs["URI"] == "" {
			continue
		}
		if id := attrs["GROUP-ID"]; id != "" {
			groups[id] = true
		}
	}
	return groups
}

// bestVariant picks the highest-quality rendition, preferring ones that
// carry audio so the result needs no muxing.
func bestVariant(variants []hlsVariant) (hlsVariant, bool) {
	if len(variants) == 0 {
		return hlsVariant{}, false
	}
	ranked := append([]hlsVariant(nil), variants...)
	slices.SortStableFunc(ranked, func(a, b hlsVariant) int {
		return cmp.Or(
			cmpBool(a.audioElsewhere, b.audioElsewhere),
			cmpBool(b.muxed(), a.muxed()), // self-contained first
			cmp.Compare(b.height(), a.height()),
			cmp.Compare(b.Bandwidth, a.Bandwidth),
		)
	})
	return ranked[0], true
}

// parseMediaPlaylist reads the ordered segment URLs. An initialisation
// segment, where the playlist declares one, is returned first because it
// must lead the concatenated output.
func parseMediaPlaylist(doc string, base *url.URL) []string {
	var segments []string
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			if uri := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MAP:"))["URI"]; uri != "" {
				segments = append(segments, resolveRef(base, uri))
			}
		case strings.HasPrefix(line, "#"):
			continue
		default:
			segments = append(segments, resolveRef(base, line))
		}
	}
	return segments
}

// parseAttributes reads a comma-separated HLS attribute list, honouring the
// quoting rules so a comma inside CODECS does not split the list.
func parseAttributes(line string) map[string]string {
	attrs := make(map[string]string)
	var (
		key, value strings.Builder
		inKey      = true
		quoted     bool
	)
	flush := func() {
		if k := strings.TrimSpace(key.String()); k != "" {
			attrs[k] = strings.Trim(strings.TrimSpace(value.String()), `"`)
		}
		key.Reset()
		value.Reset()
		inKey = true
	}
	for _, r := range line {
		switch {
		case r == '"':
			quoted = !quoted
			value.WriteRune(r)
		case r == '=' && inKey && !quoted:
			inKey = false
		case r == ',' && !quoted:
			flush()
		case inKey:
			key.WriteRune(r)
		default:
			value.WriteRune(r)
		}
	}
	flush()
	return attrs
}

// playlistExtension names the container the joined segments will be in, read
// off the variant's own URL.
func playlistExtension(v hlsVariant) string {
	if strings.Contains(strings.ToLower(v.URL), "cmaf") || strings.Contains(v.URL, ".mp4") {
		return ".mp4"
	}
	return ".ts"
}

// segmentsExtension names the container the joined segments will be in,
// asking the segments themselves before falling back to the variant's URL.
//
// The fallback alone is not to be trusted, because several packagers build
// the manifest path out of the *source* file's name: Akamai's ".mp4.csmil/"
// and nginx-vod-module's urlsets both put ".mp4" in the path of a stream
// whose parts are MPEG-TS, and a .ts file named .mp4 is one players refuse
// and remux.go never converts. The segments are what actually get written,
// so they are what the file is named after.
//
// The last segment is read rather than the first, which on a fragmented
// stream is the initialisation segment named by EXT-X-MAP — though either
// answers the same here, since an init segment is ".mp4" and the parts it
// leads are ".m4s", and both name the same container.
func segmentsExtension(segments []string, variant hlsVariant) string {
	if len(segments) > 0 {
		if u, err := url.Parse(segments[len(segments)-1]); err == nil {
			switch strings.ToLower(path.Ext(u.Path)) {
			case ".ts":
				return ".ts"
			case ".mp4", ".m4s":
				return ".mp4"
			}
		}
	}
	return playlistExtension(variant)
}

// hlsMedia is a manifest followed down to its media playlist.
type hlsMedia struct {
	Segments []string
	// Variant is the rendition chosen out of a master playlist, or — when
	// the manifest was already a media playlist — the manifest itself, with
	// nothing but its URL filled in. Comparing that URL with the one asked
	// for is how a caller tells the two apart.
	Variant hlsVariant
	// Doc is the media playlist's own text, for the caller that needs to
	// read more off it than the segment list — whether it has ended, say.
	Doc string
	// Key is the AES-128 key the segments are encrypted under, or nil for a
	// playlist in the clear. A caller that cannot carry it onto its File
	// must refuse the playlist: joined as they are, the segments make a
	// file of ciphertext that looks like a finished download.
	Key *SegmentKey
}

// resolveMediaPlaylist follows a master playlist down to its media playlist.
func resolveMediaPlaylist(ctx context.Context, client *httpx.Client, manifestURL string, headers httpx.Header) (*hlsMedia, error) {
	doc, base, err := fetchPlaylist(ctx, client, manifestURL, headers)
	if err != nil {
		return nil, fmt.Errorf("fetch playlist: %w", err)
	}

	variant := hlsVariant{URL: manifestURL}
	if variants := parseMasterPlaylist(doc, base); len(variants) > 0 {
		best, ok := bestVariant(variants)
		if !ok {
			return nil, fmt.Errorf("no usable variant in playlist")
		}
		variant = best
		if best.audioElsewhere {
			return nil, fmt.Errorf("no rendition that carries its own audio: each keeps its audio in a playlist of its own, so joining video alone would have no sound; use the external downloader (yt-dlp)")
		}
		if doc, base, err = fetchPlaylist(ctx, client, best.URL, headers); err != nil {
			return nil, fmt.Errorf("fetch variant playlist: %w", err)
		}
	}
	key, err := mediaPlaylistKey(doc, base)
	if err != nil {
		return nil, err
	}

	segments := parseMediaPlaylist(doc, base)
	if len(segments) == 0 {
		return nil, fmt.Errorf("playlist lists no segments")
	}
	return &hlsMedia{Segments: segments, Variant: variant, Doc: doc, Key: key}, nil
}

// Relative playlist references belong to the response URL after redirects.
func fetchPlaylist(ctx context.Context, client *httpx.Client, rawURL string, headers httpx.Header) (string, *url.URL, error) {
	req, err := client.NewRequest(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, &httpx.StatusError{Code: resp.StatusCode, Status: resp.Status, URL: req.URL.Redacted()}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, config.MaxResponseBytes+1))
	if err != nil {
		return "", nil, err
	}
	if len(body) > config.MaxResponseBytes {
		return "", nil, fmt.Errorf("playlist exceeds %d bytes", config.MaxResponseBytes)
	}
	doc := string(body)
	if !hlsManifestBody(doc) {
		return "", nil, fmt.Errorf("response is not an HLS playlist")
	}
	return doc, resp.Request.URL, nil
}

// errEncryptedPlaylist refuses an encrypted playlist to a caller that has
// nowhere to carry its key.
var errEncryptedPlaylist = errors.New("encrypted HLS playlists require the external downloader (yt-dlp)")

// mediaPlaylistKey reads how a media playlist's segments are encrypted, and
// refuses what the native assembler cannot honour before a byte of it can
// be reported as a completed download.
//
// One AES-128 key over every segment is the common case and is supported:
// the downloader fetches the key once and decrypts each segment as it
// lands. A playlist that repeats that same key line before every segment —
// some packagers do — is the same key. Anything else is refused: SAMPLE-AES
// and its relatives encrypt inside the stream rather than around each
// segment; a key that changes partway, or segments in the clear before it,
// would need a key per segment; a key format other than the plain one is
// DRM; and encryption over fragmented MP4 puts the initialisation segment
// ahead of the numbering an absent IV is counted from.
func mediaPlaylistKey(doc string, base *url.URL) (*SegmentKey, error) {
	var (
		key      *SegmentKey
		sequence int64
		segments int
		mapped   bool
	)
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			if n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")), 10, 64); err == nil {
				sequence = n
			}
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			attrs := parseAttributes(strings.TrimPrefix(line, "#EXT-X-KEY:"))
			switch method := attrs["METHOD"]; method {
			case "NONE":
				if key != nil {
					return nil, fmt.Errorf("HLS playlists that stop encrypting partway are not supported")
				}
			case "AES-128":
				if format := attrs["KEYFORMAT"]; format != "" && format != "identity" {
					return nil, fmt.Errorf("HLS segments under the %q key format are DRM-protected", format)
				}
				if attrs["URI"] == "" {
					return nil, fmt.Errorf("HLS key names no URI")
				}
				iv, err := parseKeyIV(attrs["IV"])
				if err != nil {
					return nil, err
				}
				next := &SegmentKey{URI: resolveRef(base, attrs["URI"]), IV: iv}
				switch {
				case key != nil && (key.URI != next.URI || !bytes.Equal(key.IV, next.IV)):
					return nil, fmt.Errorf("HLS playlists whose key changes partway are not supported")
				case key == nil && segments > 0:
					return nil, fmt.Errorf("HLS playlists that start encrypting partway are not supported")
				}
				key = next
			default:
				return nil, fmt.Errorf("HLS segments encrypted with %s cannot be decrypted here", method)
			}
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			return nil, fmt.Errorf("HLS byte ranges require the external downloader (yt-dlp)")
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			if _, ok := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MAP:"))["BYTERANGE"]; ok {
				return nil, fmt.Errorf("HLS initialization byte ranges require the external downloader (yt-dlp)")
			}
			mapped = true
		case strings.HasPrefix(line, "#"):
		default:
			segments++
		}
	}
	if key != nil {
		if mapped {
			return nil, fmt.Errorf("encrypted fragmented-MP4 HLS is not supported")
		}
		key.Sequence = sequence
	}
	return key, nil
}

// parseKeyIV reads an EXT-X-KEY IV: sixteen bytes written as 0x and
// thirty-two hexadecimal digits. Absent is fine — the segments' sequence
// numbers stand in.
func parseKeyIV(raw string) ([]byte, error) {
	if raw == "" {
		return nil, nil
	}
	digits := strings.TrimPrefix(strings.TrimPrefix(raw, "0x"), "0X")
	iv, err := hex.DecodeString(digits)
	if err != nil || len(iv) != 16 {
		return nil, fmt.Errorf("HLS key has an unreadable IV %q", raw)
	}
	return iv, nil
}

// resolvePlaylist follows a master playlist down to its segment list, which
// is all that nearly every caller wants of it.
func resolvePlaylist(ctx context.Context, client *httpx.Client, manifestURL string, headers httpx.Header) ([]string, hlsVariant, error) {
	media, err := resolveMediaPlaylist(ctx, client, manifestURL, headers)
	if err != nil {
		return nil, hlsVariant{}, err
	}
	// These callers hand back segments alone, with nowhere to carry a key.
	if media.Key != nil {
		return nil, hlsVariant{}, errEncryptedPlaylist
	}
	return media.Segments, media.Variant, nil
}
