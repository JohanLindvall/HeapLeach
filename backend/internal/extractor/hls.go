package extractor

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

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
	for _, line := range strings.Split(doc, "\n") {
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
	for _, line := range strings.Split(doc, "\n") {
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
	sort.SliceStable(ranked, func(i, j int) bool {
		if a, b := ranked[i].muxed(), ranked[j].muxed(); a != b {
			return a // self-contained first
		}
		if a, b := ranked[i].height(), ranked[j].height(); a != b {
			return a > b
		}
		return ranked[i].Bandwidth > ranked[j].Bandwidth
	})
	return ranked[0], true
}

// parseMediaPlaylist reads the ordered segment URLs. An initialisation
// segment, where the playlist declares one, is returned first because it
// must lead the concatenated output.
func parseMediaPlaylist(doc string, base *url.URL) []string {
	var segments []string
	for _, line := range strings.Split(doc, "\n") {
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

// resolvePlaylist follows a master playlist down to its segment list.
func resolvePlaylist(ctx context.Context, client *httpx.Client, manifestURL string, headers httpx.Header) ([]string, hlsVariant, error) {
	base, err := ParseURL(manifestURL)
	if err != nil {
		return nil, hlsVariant{}, err
	}
	doc, err := client.GetString(ctx, manifestURL, headers)
	if err != nil {
		return nil, hlsVariant{}, fmt.Errorf("fetch playlist: %w", err)
	}

	variant := hlsVariant{URL: manifestURL}
	if variants := parseMasterPlaylist(doc, base); len(variants) > 0 {
		best, ok := bestVariant(variants)
		if !ok {
			return nil, hlsVariant{}, fmt.Errorf("no usable variant in playlist")
		}
		variant = best
		if base, err = ParseURL(best.URL); err != nil {
			return nil, hlsVariant{}, err
		}
		if doc, err = client.GetString(ctx, best.URL, headers); err != nil {
			return nil, hlsVariant{}, fmt.Errorf("fetch variant playlist: %w", err)
		}
	}

	segments := parseMediaPlaylist(doc, base)
	if len(segments) == 0 {
		return nil, hlsVariant{}, fmt.Errorf("playlist lists no segments")
	}
	return segments, variant, nil
}
