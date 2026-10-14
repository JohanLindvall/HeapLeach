// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// A playlist that keeps its audio apart from its video, as CMAF's do, is
// fetched as several part files, the video's own and one per audio track
// (playlist.go), and joined here into one MP4 by stream copy before the file
// is finished. Unlike remux.go's rewrap this is no convenience: the video
// alone is silent. So without ffmpeg such a transfer fails before it
// fetches anything (errNoFFmpegToJoin), rather than finishing without sound.

// errNoFFmpegToJoin is the refusal of a video whose audio cannot be joined
// to it.
func errNoFFmpegToJoin() error {
	return fmt.Errorf("this video keeps its audio apart from it, and joining the two needs ffmpeg: %s",
		tools.NotInstalled(tools.FFmpeg))
}

// joinedPart names the file a playlist's tracks are joined into, beside
// their parts.
func joinedPart(part string) string {
	return strings.TrimSuffix(part, ".part") + ".joined.part"
}

// joinTracks joins every audio track to the video, each labelled with its
// language and name, and returns the joined file. Removing the parts is left
// to the caller, once that file is in place: a join that fails keeps them,
// and the next attempt joins them again without fetching anything.
func (m *Manager) joinTracks(ctx context.Context, it *Item, tracks []playlistTrack, files transferFiles) (string, error) {
	ffmpeg, ok := tools.Find(tools.FFmpeg)
	if !ok {
		return "", errNoFFmpegToJoin()
	}
	out := joinedPart(tracks[0].part)
	// Created through the root first, so the path ffmpeg is handed is known
	// to stay inside it.
	f, err := files.open(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("join the audio to the video: %w", err)
	}
	_ = f.Close()

	apart := 0
	for _, t := range tracks[1:] {
		if t.part != "" {
			apart++
		}
	}
	m.note(it, "joining "+plural(apart, "audio track")+" to the video")
	defer m.note(it, "")

	run, cancel := context.WithTimeout(ctx, config.RemuxTimeout)
	defer cancel()
	cmd := tools.CommandContext(run, ffmpeg, joinArgs(tracks, m.audioInVideo(run, tracks), out)...)
	diagnostic := &util.BoundedBuffer{Limit: config.ErrorBodySample}
	cmd.Stdout, cmd.Stderr = diagnostic, diagnostic
	err = cmd.Run()
	if err == nil {
		if info, statErr := files.stat(out); statErr != nil || info.Size() == 0 {
			err = errors.New("it wrote nothing")
		}
	}
	if err != nil {
		_ = files.remove(out)
		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case run.Err() != nil:
			return "", fmt.Errorf("joining the audio to the video took longer than %s", config.RemuxTimeout)
		}
		return "", fmt.Errorf("join the audio to the video with ffmpeg: %w: %s", err,
			util.Truncate(strings.TrimSpace(string(diagnostic.Bytes())), 300))
	}
	m.log.Debug("joined the audio to the video", "item", it.ID, "tracks", apart)
	return out, nil
}

// audioInVideo counts the audio streams the video's own segments carry, when
// the playlist says they carry any of its renditions, and -1 when it says
// none or the count could not be read (taking the playlist at its word).
func (m *Manager) audioInVideo(ctx context.Context, tracks []playlistTrack) int {
	if !slices.ContainsFunc(tracks[1:], func(t playlistTrack) bool { return t.part == "" }) {
		return -1
	}
	ffprobe, ok := tools.Find(tools.FFprobe)
	if !ok {
		return -1
	}
	streams, err := probeMedia(ctx, ffprobe, tracks[0].part)
	if err != nil {
		return -1
	}
	n := 0
	for _, s := range streams {
		if s.Kind == "audio" {
			n++
		}
	}
	return n
}

// joinArgs is the ffmpeg command line that joins the tracks into out.
//
// The audio is exactly the renditions the playlist names, in its order, each
// from where it says it is: a part file of its own, or the video's own
// segments, of which embedded says how many audio streams they carry (-1 for
// unknown). Audio in the video's segments that the playlist does not name is
// left out. Akamai mixes the default track into the video and publishes it
// as a rendition of its own as well, and keeping both would list it twice.
//
// Two more things in it are easy to get wrong. Unless told otherwise, ffmpeg
// starts every input at zero on its own, which drops whatever offset the
// renditions had between them: -copyts keeps their timestamps, which all
// count on the playlist's one timeline, and make_zero then moves the output
// as a whole to start at zero. And an MP4 keeps a track's language only as
// three letters (mp4Language) and its label only as the handler name, and
// it marks every track flagged default to play, which for several audio
// tracks means all at once. So exactly one is: the playlist's own default,
// which the extractor lists first.
func joinArgs(tracks []playlistTrack, embedded int, out string) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-copyts", "-i", tracks[0].part}
	maps := []string{"-map", "0:v?"}
	var labels []string
	input, inVideo, stream := 1, 0, 0
	for _, t := range tracks[1:] {
		switch {
		case t.part != "":
			args = append(args, "-i", t.part)
			maps = append(maps, "-map", strconv.Itoa(input)+":a:0")
			input++
		case embedded < 0 || inVideo < embedded:
			maps = append(maps, "-map", "0:a:"+strconv.Itoa(inVideo))
			inVideo++
		default:
			inVideo++ // named as carried in the video, which it is not
			continue
		}
		index := strconv.Itoa(stream)
		disposition := "0"
		if stream == 0 {
			disposition = "default"
		}
		labels = append(labels, "-disposition:a:"+index, disposition)
		if language := mp4Language(t.audio.Language); language != "" {
			labels = append(labels, "-metadata:s:a:"+index, "language="+language)
		}
		if t.audio.Name != "" {
			labels = append(labels, "-metadata:s:a:"+index, "handler_name="+t.audio.Name)
		}
		stream++
	}
	args = append(args, maps...)
	args = append(args, "-c", "copy")
	args = append(args, labels...)
	return append(args, "-avoid_negative_ts", "make_zero", "-f", "mp4", "-movflags", "+faststart", out)
}
