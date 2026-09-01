package download

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
)

// Choosing what to carry into an MP4 needs to know what is in the source, so
// the container is probed first. Without a probe the conversion still works;
// it just falls back to letting ffmpeg pick one video and one audio track.

// mediaStream is one stream of a container.
type mediaStream struct {
	Index    int
	Kind     string
	Codec    string
	Language string
	Channels int
	Bitrate  int64
	Pixels   int64
	Default  bool
}

// probeMedia lists the streams of a file.
func probeMedia(ctx context.Context, ffprobe, path string) ([]mediaStream, error) {
	output, err := exec.CommandContext(ctx, ffprobe,
		"-v", "error", "-show_streams", "-print_format", "json", path).Output()
	if err != nil {
		return nil, fmt.Errorf("probe %s: %w", path, err)
	}

	var probe struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Channels  int    `json:"channels"`
			BitRate   string `json:"bit_rate"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Tags      struct {
				Language string `json:"language"`
			} `json:"tags"`
			Disposition struct {
				Default int `json:"default"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(output, &probe); err != nil {
		return nil, fmt.Errorf("read probe of %s: %w", path, err)
	}

	streams := make([]mediaStream, 0, len(probe.Streams))
	for _, s := range probe.Streams {
		bitrate, _ := strconv.ParseInt(s.BitRate, 10, 64)
		language := s.Tags.Language
		if language == "" {
			language = "und" // untagged streams form their own group
		}
		streams = append(streams, mediaStream{
			Index:    s.Index,
			Kind:     s.CodecType,
			Codec:    s.CodecName,
			Language: language,
			Channels: s.Channels,
			Bitrate:  bitrate,
			Pixels:   int64(s.Width) * int64(s.Height),
			Default:  s.Disposition.Default == 1,
		})
	}
	return streams, nil
}

// selectStreams picks what to carry over: the best video, and the best audio
// and subtitle track of each language. Keeping one per language means a
// multi-language recording stays usable in every language it shipped with,
// without also dragging along the duplicate encodes of each.
func selectStreams(streams []mediaStream) (video []int, audio []int, subtitles []int) {
	best := func(a, b mediaStream) mediaStream {
		if betterStream(a, b) {
			return a
		}
		return b
	}

	var topVideo *mediaStream
	byLanguage := map[string]mediaStream{}
	subsByLanguage := map[string]mediaStream{}

	for _, s := range streams {
		switch s.Kind {
		case "video":
			// Cover art and thumbnails masquerade as video with no size.
			if s.Pixels == 0 {
				continue
			}
			if topVideo == nil || betterStream(s, *topVideo) {
				chosen := s
				topVideo = &chosen
			}
		case "audio":
			if existing, ok := byLanguage[s.Language]; ok {
				byLanguage[s.Language] = best(s, existing)
			} else {
				byLanguage[s.Language] = s
			}
		case "subtitle":
			if existing, ok := subsByLanguage[s.Language]; ok {
				subsByLanguage[s.Language] = best(s, existing)
			} else {
				subsByLanguage[s.Language] = s
			}
		}
	}

	if topVideo != nil {
		video = append(video, topVideo.Index)
	}
	audio = indexesOf(byLanguage)
	subtitles = indexesOf(subsByLanguage)
	return video, audio, subtitles
}

// betterStream ranks two streams of the same kind.
func betterStream(a, b mediaStream) bool {
	switch {
	case a.Pixels != b.Pixels:
		return a.Pixels > b.Pixels
	case a.Channels != b.Channels:
		return a.Channels > b.Channels
	case a.Bitrate != b.Bitrate:
		return a.Bitrate > b.Bitrate
	case a.Default != b.Default:
		return a.Default
	default:
		return a.Index < b.Index
	}
}

// indexesOf returns the chosen stream indexes in file order, so the output
// keeps the ordering the source had.
func indexesOf(chosen map[string]mediaStream) []int {
	out := make([]int, 0, len(chosen))
	for _, s := range chosen {
		out = append(out, s.Index)
	}
	slices.Sort(out)
	return out
}
