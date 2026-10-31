// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Minimal HLS support: enough to turn a master playlist into an ordered list
// of segment URLs.
//
// Adaptive streams have no single file to fetch, so a playlist-backed file
// carries its segments instead of a URL and the downloader concatenates
// them. Variants that carry audio and video together are preferred, because
// concatenating those alone yields a playable file. One that keeps its audio
// apart comes with every audio rendition, and its subtitles with any
// variant, each a Track the downloader fetches beside the video and joins to
// it with ffmpeg.

// hlsVariant is one entry of a master playlist.
type hlsVariant struct {
	URL        string
	Bandwidth  int
	Resolution string
	Codecs     string
	// audioElsewhere records that the variant named an audio group with a
	// rendition in a playlist of its own, which means the variant's own
	// segments are not the whole of its audio, and when every rendition is,
	// carry video and nothing else. audioGroup is that group.
	audioElsewhere bool
	audioGroup     string
	// subtitleGroup is the subtitle group the variant names, if any.
	subtitleGroup string
}

// hlsRendition is one audio or subtitle rendition of a master playlist's
// EXT-X-MEDIA. URL is empty for one carried in the variant's own segments.
type hlsRendition struct {
	URL, Language, Name string
	Default, Forced     bool
	// description marks an audio description.
	description bool
}

// separate reports a rendition with a playlist of its own.
func (r hlsRendition) separate() bool { return r.URL != "" }

// avc reports a variant whose video is H.264.
func (v hlsVariant) avc() bool {
	codecs := strings.ToLower(v.Codecs)
	return strings.Contains(codecs, "avc1") || strings.Contains(codecs, "avc3")
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
	renditions := parseRenditions(doc, base, "AUDIO")
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
				audioElsewhere: slices.ContainsFunc(renditions[attrs["AUDIO"]], hlsRendition.separate),
				audioGroup:     attrs["AUDIO"],
				subtitleGroup:  attrs["SUBTITLES"],
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

// parseAudioRenditions lists each audio group's renditions, in the master's
// order with the default first. One with a URI has a playlist of its own; one
// without describes audio already inside the segments of the variants that
// name the group, and has no URL here. A group with any of the first kind is
// what makes those variants' own segments not the whole of their audio.
func parseAudioRenditions(doc string, base *url.URL) map[string][]hlsRendition {
	return parseRenditions(doc, base, "AUDIO")
}

// parseSubtitleRenditions lists each subtitle group's renditions the same
// way.
func parseSubtitleRenditions(doc string, base *url.URL) map[string][]hlsRendition {
	return parseRenditions(doc, base, "SUBTITLES")
}

// parseRenditions lists the renditions of one EXT-X-MEDIA type by group.
func parseRenditions(doc string, base *url.URL, kind string) map[string][]hlsRendition {
	groups := make(map[string][]hlsRendition)
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		attrs := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
		if !strings.EqualFold(attrs["TYPE"], kind) || attrs["GROUP-ID"] == "" {
			continue
		}
		groups[attrs["GROUP-ID"]] = append(groups[attrs["GROUP-ID"]], hlsRendition{
			URL: resolveRef(base, attrs["URI"]), Language: attrs["LANGUAGE"], Name: strings.TrimSpace(attrs["NAME"]),
			Default:     strings.EqualFold(attrs["DEFAULT"], "YES"),
			Forced:      strings.EqualFold(attrs["FORCED"], "YES"),
			description: kind == "AUDIO" && strings.Contains(attrs["CHARACTERISTICS"], "public.accessibility.describes-video"),
		})
	}
	// An audio description is told apart only by its characteristics where
	// its name says nothing: SRF names it as it names the main track, but
	// for a trailing space. Named so where two would read alike, and only
	// there, since most hosts name it for what it is.
	for _, renditions := range groups {
		for i, r := range renditions {
			if r.description && slices.ContainsFunc(renditions, func(o hlsRendition) bool { return o != r && o.Name == r.Name }) {
				renditions[i].Name = strings.TrimSpace(r.Name + " (audio description)")
			}
		}
	}
	for _, renditions := range groups {
		slices.SortStableFunc(renditions, func(a, b hlsRendition) int { return cmpBool(b.Default, a.Default) })
	}
	return groups
}

// bestVariant picks the highest-quality rendition, preferring ones that
// carry audio so the result needs no muxing. At the same height H.264 comes
// before HEVC and AV1, whatever their bitrates: every player decodes it, and
// HEVC in an MP4 tagged hev1, as packagers mostly tag it, plays in nothing
// of Apple's.
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
			cmpBool(b.avc(), a.avc()),
			cmp.Compare(b.Bandwidth, a.Bandwidth),
		)
	})
	return ranked[0], true
}

// parseMediaPlaylist reads the ordered segment URLs. An initialisation
// segment, where the playlist declares one, is returned first because it
// must lead the concatenated output. A segment that is a byte range of a
// larger file (EXT-X-BYTERANGE, or a map's BYTERANGE) carries its range
// (rangeSegment); one whose range names no offset starts where the
// previous segment of the same file ended.
func parseMediaPlaylist(doc string, base *url.URL) []string {
	var (
		segments []string
		pending  *byteRange
		lastRef  string
		lastEnd  int64
	)
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			attrs := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MAP:"))
			if attrs["URI"] == "" {
				continue
			}
			ref := resolveRef(base, attrs["URI"])
			if r, ok := parseByteRange(attrs["BYTERANGE"]); ok {
				// A map's range with no offset starts at the beginning.
				segments = append(segments, rangeSegment(ref, r.offset, r.length))
			} else {
				segments = append(segments, ref)
			}
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			if r, ok := parseByteRange(strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")); ok {
				pending = &r
			}
		case strings.HasPrefix(line, "#"):
			continue
		default:
			ref := resolveRef(base, line)
			if pending == nil {
				segments = append(segments, ref)
				continue
			}
			offset := pending.offset
			if !pending.stated && ref == lastRef {
				offset = lastEnd
			}
			segments = append(segments, rangeSegment(ref, offset, pending.length))
			lastRef, lastEnd, pending = ref, offset+pending.length, nil
		}
	}
	return segments
}

// byteRange is an EXT-X-BYTERANGE: length bytes from offset, and whether the
// offset was stated.
type byteRange struct {
	length, offset int64
	stated         bool
}

// parseByteRange reads "<length>[@<offset>]".
func parseByteRange(text string) (byteRange, bool) {
	text = strings.Trim(strings.TrimSpace(text), `"`)
	lengthText, offsetText, stated := strings.Cut(text, "@")
	length, err := strconv.ParseInt(lengthText, 10, 64)
	if err != nil || length <= 0 {
		return byteRange{}, false
	}
	var offset int64
	if stated {
		if offset, err = strconv.ParseInt(offsetText, 10, 64); err != nil || offset < 0 {
			return byteRange{}, false
		}
	}
	return byteRange{length: length, offset: offset, stated: stated}, true
}

// segmentRangeMark opens a byte range carried in a segment's fragment.
const segmentRangeMark = "#range="

// rangeSegment is a segment that is length bytes of the file at ref from
// offset: the URL, with the range in its fragment, which no request ever
// sends. That keeps File.Segments a list of strings for every extractor
// that never meets a range; the downloader reads it back (SegmentRange).
func rangeSegment(ref string, offset, length int64) string {
	ref, _, _ = strings.Cut(ref, "#")
	return ref + segmentRangeMark + strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(offset+length-1, 10)
}

// SegmentRange splits a segment into the URL to ask and the bytes of it to
// ask for, first and last inclusive, as a Range header counts them. ranged
// is false for a segment that is a whole file.
func SegmentRange(segment string) (target string, first, last int64, ranged bool) {
	i := strings.LastIndex(segment, segmentRangeMark)
	if i < 0 {
		return segment, 0, 0, false
	}
	firstText, lastText, ok := strings.Cut(segment[i+len(segmentRangeMark):], "-")
	first, errFirst := strconv.ParseInt(firstText, 10, 64)
	last, errLast := strconv.ParseInt(lastText, 10, 64)
	if !ok || errFirst != nil || errLast != nil || first < 0 || last < first {
		return segment, 0, 0, false
	}
	return segment[:i], first, last, true
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
	// Audio is the soundtrack when the variant keeps it apart: every
	// rendition of the audio group the chosen variant names, in the group's
	// order (resolveAudio).
	Audio []Track
	// Subtitles are the renditions of the subtitle group the variant names,
	// those that could be read (resolveSubtitles).
	Subtitles []Track
}

// errAudioElsewhere is a variant whose audio is said to be kept apart from
// it, with no playlist named for any of it: the video alone would join into
// a silent file.
var errAudioElsewhere = errors.New("the audio is kept apart from the video, and no playlist for it is named: joining the video alone would make a silent file")

// resolveMediaPlaylist follows a master playlist down to its media playlist,
// and to the audio renditions' when the variant keeps its audio apart.
//
// A caller must carry all of what it returns onto its File, the key and the
// audio as well as the segments: joined as they are, the segments of an
// encrypted playlist are ciphertext, and those of a variant whose audio is
// kept apart are silent, and either looks like a finished download.
func resolveMediaPlaylist(ctx context.Context, client *httpx.Client, manifestURL string, headers httpx.Header) (*hlsMedia, error) {
	doc, base, err := fetchPlaylist(ctx, client, manifestURL, headers)
	if err != nil {
		return nil, fmt.Errorf("fetch playlist: %w", err)
	}

	variant := hlsVariant{URL: manifestURL}
	var audio, subtitles []Track
	if variants := parseMasterPlaylist(doc, base); len(variants) > 0 {
		best, ok := bestVariant(variants)
		if !ok {
			return nil, fmt.Errorf("no usable variant in playlist")
		}
		variant = best
		if best.audioElsewhere {
			if audio, err = resolveAudio(ctx, client, parseAudioRenditions(doc, base)[best.audioGroup], headers); err != nil {
				return nil, err
			}
		}
		if best.subtitleGroup != "" {
			subtitles = resolveSubtitles(ctx, client, parseSubtitleRenditions(doc, base)[best.subtitleGroup], headers)
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
	return &hlsMedia{Segments: segments, Variant: variant, Doc: doc, Key: key, Audio: audio, Subtitles: subtitles}, nil
}

// resolveSubtitles reads each subtitle rendition's media playlist into a
// Track. Subtitles are a bonus to the video, never a condition of it, so a
// rendition that cannot be read is left out rather than failing the video,
// as is one packaged in a form the downloader does not join: WebVTT wrapped
// in fragmented MP4, or encrypted.
func resolveSubtitles(ctx context.Context, client *httpx.Client, renditions []hlsRendition, headers httpx.Header) []Track {
	var tracks []Track
	seen := make(map[string]bool, len(renditions))
	for _, r := range renditions {
		if !r.separate() || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		doc, base, err := fetchPlaylist(ctx, client, r.URL, headers)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		if key, err := mediaPlaylistKey(doc, base); err != nil || key != nil || strings.Contains(doc, "#EXT-X-MAP:") {
			continue
		}
		if segments := parseMediaPlaylist(doc, base); len(segments) > 0 {
			tracks = append(tracks, Track{Segments: segments, Language: r.Language, Name: r.Name,
				Default: r.Default, Forced: r.Forced})
		}
	}
	return tracks
}

// file is the playlist as a File named name: its segments, key, audio and
// subtitles all carried over, as resolveMediaPlaylist requires. A playlist
// has no length until every part is fetched, so Size is unknown.
func (m *hlsMedia) file(name string, headers httpx.Header) File {
	return File{
		Name: name, Size: -1, Headers: headers,
		Segments: m.Segments, SegmentKey: m.Key, Audio: m.Audio, Subtitles: m.Subtitles,
	}
}

// target is the playlist as a freshly resolved Target, for a resolver that
// reads a signed playlist again at transfer time.
func (m *hlsMedia) target(link, name string, headers httpx.Header) *Target {
	return &Target{
		URL: link, Name: name, Size: -1, Headers: headers,
		Segments: m.Segments, SegmentKey: m.Key, Audio: m.Audio, Subtitles: m.Subtitles,
	}
}

// extension names the container the downloaded file will be in: an MP4 when
// the audio is joined to the video, which ffmpeg writes, and otherwise
// whatever the segments are (segmentsExtension).
func (m *hlsMedia) extension() string {
	return m.extensionOr(segmentsExtension(m.Segments, m.Variant))
}

// extensionOr is ext, the container a caller names the segments as, unless
// the audio is joined to them.
func (m *hlsMedia) extensionOr(ext string) string {
	if len(m.Audio) > 0 {
		return ".mp4"
	}
	return ext
}

// resolveAudio reads each audio rendition's media playlist into a Track, and
// lists one carried in the video's own segments as a Track with none, in the
// group's order. A group with no playlist to read leaves the video silent,
// which is refused as before.
func resolveAudio(ctx context.Context, client *httpx.Client, renditions []hlsRendition, headers httpx.Header) ([]Track, error) {
	if !slices.ContainsFunc(renditions, hlsRendition.separate) {
		return nil, errAudioElsewhere
	}
	seen := make(map[string]bool, len(renditions))
	tracks := make([]Track, 0, len(renditions))
	for _, r := range renditions {
		if !r.separate() {
			tracks = append(tracks, Track{Language: r.Language, Name: r.Name})
			continue
		}
		if seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		label := r.Name
		if label == "" {
			label = r.Language
		}
		doc, base, err := fetchPlaylist(ctx, client, r.URL, headers)
		if err != nil {
			return nil, fmt.Errorf("fetch the %q audio playlist: %w", label, err)
		}
		key, err := mediaPlaylistKey(doc, base)
		if err != nil {
			return nil, err
		}
		segments := parseMediaPlaylist(doc, base)
		if len(segments) == 0 {
			return nil, fmt.Errorf("the %q audio playlist lists no segments", label)
		}
		tracks = append(tracks, Track{Segments: segments, Key: key, Language: r.Language, Name: r.Name})
	}
	return tracks, nil
}

// Relative playlist references belong to the response URL after redirects.
func fetchPlaylist(ctx context.Context, client *httpx.Client, rawURL string, headers httpx.Header) (string, *url.URL, error) {
	body, resp, err := client.Fetch(ctx, rawURL, headers)
	if err != nil {
		return "", nil, err
	}
	doc := string(body)
	if !hlsManifestBody(doc) {
		return "", nil, fmt.Errorf("response is not an HLS playlist")
	}
	return doc, resp.Request.URL, nil
}

// mediaPlaylistKey reads how a media playlist's segments are encrypted, and
// refuses what the native assembler cannot honour before a byte of it can
// be reported as a completed download.
//
// One AES-128 key over every segment is the common case and is supported:
// the downloader fetches the key once and decrypts each segment as it
// lands. A playlist that repeats that same key line before every segment —
// some packagers do — is the same key. Over fragmented MP4 the
// initialisation segment comes first: named before the key, it is in the
// clear (SegmentKey.Clear); named after it, it is encrypted too, and the key
// must state its IV, since the initialisation segment has no sequence number
// to count one from. Anything else is refused: SAMPLE-AES and its relatives
// encrypt inside the stream rather than around each segment; a key that
// changes partway, or segments in the clear before it, would need a key per
// segment; and a key format other than the plain one is DRM.
func mediaPlaylistKey(doc string, base *url.URL) (*SegmentKey, error) {
	var (
		key      *SegmentKey
		sequence int64
		segments int
		mapped   bool // an initialisation segment is named
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
				case key == nil && mapped:
					next.Clear = 1 // the initialisation segment, named before the key
				case key != nil:
					next.Clear = key.Clear
				}
				key = next
			default:
				return nil, fmt.Errorf("HLS segments encrypted with %s cannot be decrypted here", method)
			}
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			mapped = true
		case strings.HasPrefix(line, "#"):
		default:
			segments++
		}
	}
	if key != nil {
		if mapped && key.Clear == 0 && key.IV == nil {
			return nil, fmt.Errorf("HLS encrypts its initialisation segment without naming the IV to decrypt it with")
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
		seg := util.Stem(seg)
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
