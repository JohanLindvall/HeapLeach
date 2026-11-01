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
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
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

// joinTracks joins every audio track and subtitle to the video, each
// labelled with its language and name, and returns the joined file. Removing
// the parts is left to the caller, once that file is in place: a join that
// fails keeps them, and the next attempt joins them again without fetching
// anything.
func (m *Manager) joinTracks(ctx context.Context, it *Item, tracks []playlistTrack, subtitles []subtitleFile,
	files transferFiles) (string, error) {
	ffmpeg, ok := tools.Find(tools.FFmpeg)
	if !ok {
		return "", errNoFFmpegToJoin()
	}
	out := joinedPart(tracks[0].part)
	// Created through the root first, so the path ffmpeg is handed is known
	// to stay inside it.
	f, err := files.open(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("join the tracks to the video: %w", err)
	}
	_ = f.Close()

	apart := 0
	for _, t := range tracks[1:] {
		if t.part != "" {
			apart++
		}
	}
	var what []string
	if apart > 0 {
		what = append(what, plural(apart, "audio track"))
	}
	if len(subtitles) > 0 {
		what = append(what, plural(len(subtitles), "subtitle track"))
	}
	m.note(it, "joining "+strings.Join(what, " and ")+" to the video")
	defer m.note(it, "")

	tracks = slices.Clone(tracks)
	for i := 1; i < len(tracks); i++ {
		if tracks[i].part != "" {
			tracks[i].start = files.packedAudioStart(tracks[i].part)
		}
	}
	run, cancel := context.WithTimeout(ctx, config.RemuxTimeout)
	defer cancel()
	diagnostic, err := runFFmpeg(run, ffmpeg, joinArgs(tracks, subtitles, m.probeVideo(run, tracks, subtitles), out)...)
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
			return "", fmt.Errorf("joining the tracks to the video took longer than %s", config.RemuxTimeout)
		}
		return "", fmt.Errorf("join the tracks to the video with ffmpeg: %w: %s", err,
			util.Truncate(strings.TrimSpace(string(diagnostic)), 300))
	}
	m.log.Debug("joined the tracks to the video", "item", it.ID, "audio", apart, "subtitles", len(subtitles))
	return out, nil
}

// runFFmpeg runs ffmpeg and returns what it said, the start of it, which is
// where it puts the reason a run failed.
func runFFmpeg(ctx context.Context, ffmpeg string, args ...string) ([]byte, error) {
	cmd := tools.CommandContext(ctx, ffmpeg, args...)
	diagnostic := &util.BoundedBuffer{Limit: config.ErrorBodySample}
	cmd.Stdout, cmd.Stderr = diagnostic, diagnostic
	err := cmd.Run()
	return diagnostic.Bytes(), err
}

// videoProbe is what a join needs to know of the video's own part: how many
// audio streams its segments carry (-1 for unknown), when its picture
// starts, and whether its frames are stamped to be shown before they are
// decoded (presentedEarly).
type videoProbe struct {
	audio          int
	start          float64
	presentedEarly bool
}

// probeVideo asks ffprobe what the join needs to know: whether the video's
// frames are presented early, which takes a look at its first packets, and
// the rest only when it needs it: how many audio streams the video's own
// segments carry, when the playlist says they carry any of its renditions,
// and when the picture starts, when a subtitle track's cues count from it.
// Unread, the playlist is taken at its word and the picture at zero.
func (m *Manager) probeVideo(ctx context.Context, tracks []playlistTrack, subtitles []subtitleFile) videoProbe {
	probe := videoProbe{audio: -1}
	ffprobe, ok := tools.Find(tools.FFprobe)
	if !ok {
		return probe
	}
	if packets, err := tools.Probe(ctx, ffprobe, "-v", "error", "-select_streams", "v:0", "-read_intervals", "%+#60",
		"-show_entries", "packet=pts,dts", "-of", "csv=p=0", tracks[0].part); err == nil {
		probe.presentedEarly = presentedEarly(string(packets))
	}
	inVideo := slices.ContainsFunc(tracks[1:], func(t playlistTrack) bool { return t.part == "" })
	unmapped := slices.ContainsFunc(subtitles, func(s subtitleFile) bool { return !s.mapped })
	if !inVideo && !unmapped {
		return probe
	}
	streams, err := probeMedia(ctx, ffprobe, tracks[0].part)
	if err != nil {
		return probe
	}
	audio, picture := 0, false
	for _, s := range streams {
		switch s.Kind {
		case "audio":
			audio++
		case "video":
			if !picture && s.HasStart {
				probe.start, picture = s.Start, true
			}
		}
	}
	if inVideo {
		probe.audio = audio
	}
	return probe
}

// presentedEarly reports, from ffprobe's packet times (pts,dts a line), a
// video whose frames are stamped to be shown before they are decoded: a
// presentation time earlier than the decode time. One broadcaster's packager
// stamps its B-frames so, with the first frame's decode time at its
// presentation time and no room left for the reordering. A transport stream
// reader takes that in its stride; copied into an MP4, the frames come out
// with times that repeat, which a player shows by dropping some. The join
// then has ffmpeg work the decode times out from the presentation times
// (+igndts), as the stream should have stated them. The first packets tell,
// since a packager stamps every frame alike.
func presentedEarly(packets string) bool {
	for line := range strings.Lines(packets) {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 2 {
			continue
		}
		pts, perr := strconv.ParseInt(fields[0], 10, 64)
		dts, derr := strconv.ParseInt(fields[1], 10, 64)
		if perr == nil && derr == nil && pts < dts {
			return true
		}
	}
	return false
}

// joinArgs is the ffmpeg command line that joins the tracks into out.
//
// The audio is exactly the renditions the playlist names, in its order, each
// from where it says it is: a part file of its own, or the video's own
// segments, of which probe.audio says how many audio streams they carry (-1
// for unknown). Audio in the video's segments that the playlist does not
// name is left out. Akamai mixes the default track into the video and
// publishes it as a rendition of its own as well, and keeping both would
// list it twice. A playlist that names no audio at all, joined only for its
// subtitles, carries it all in the video, and all of it comes along.
//
// Each subtitle is a WebVTT file, made an MP4 text track (mov_text) at
// the time mergeWebVTT put its cues on, or counted from the start of the
// picture where the WebVTT said nothing of where they sit.
//
// Two more things in it are easy to get wrong. Unless told otherwise, ffmpeg
// starts every input at zero on its own, which drops whatever offset the
// renditions had between them: -copyts keeps their timestamps, which all
// count on the playlist's one timeline, and make_zero then moves the output
// as a whole to start at zero. And an MP4 keeps a track's language only as
// three letters (mp4Language) and its label only as the handler name, and
// it marks every track flagged default to play, which for several audio
// tracks means all at once. So exactly one is: the playlist's own default,
// which the extractor lists first. A subtitle keeps the playlist's flags.
func joinArgs(tracks []playlistTrack, subtitles []subtitleFile, probe videoProbe, out string) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-copyts"}
	if probe.presentedEarly {
		args = append(args, "-fflags", "+igndts")
	}
	args = append(args, "-i", tracks[0].part)
	maps := []string{"-map", "0:v?"}
	var labels []string
	input, inVideo, stream := 1, 0, 0
	for _, t := range tracks[1:] {
		switch {
		case t.part != "":
			if t.start != 0 {
				args = append(args, "-itsoffset", strconv.FormatFloat(t.start, 'f', 6, 64))
			}
			args = append(args, "-i", t.part)
			maps = append(maps, "-map", strconv.Itoa(input)+":a:0")
			input++
		case probe.audio < 0 || inVideo < probe.audio:
			maps = append(maps, "-map", "0:a:"+strconv.Itoa(inVideo))
			inVideo++
		default:
			inVideo++ // named as carried in the video, which it is not
			continue
		}
		disposition := "0"
		if stream == 0 {
			disposition = "default"
		}
		labels = append(labels, trackLabels("a", stream, disposition, *t.audio)...)
		stream++
	}
	if len(tracks) == 1 {
		maps = append(maps, "-map", "0:a?")
	}
	for i, s := range subtitles {
		offset := s.offset
		if !s.mapped {
			offset += probe.start
		}
		args = append(args, "-itsoffset", strconv.FormatFloat(offset, 'f', 6, 64), "-f", "webvtt", "-i", s.path)
		maps = append(maps, "-map", strconv.Itoa(input)+":s:0")
		input++
		labels = append(labels, trackLabels("s", i, subtitleDisposition(s.track), s.track)...)
	}
	args = append(args, maps...)
	args = append(args, "-c", "copy")
	if len(subtitles) > 0 {
		args = append(args, "-c:s", "mov_text")
	}
	args = append(args, labels...)
	return append(args, "-avoid_negative_ts", "make_zero", "-f", "mp4", "-movflags", "+faststart", out)
}

// trackLabels are the arguments that label output stream index of a kind
// ("a", "s"): its disposition, language and name.
func trackLabels(kind string, index int, disposition string, track extractor.Track) []string {
	spec := kind + ":" + strconv.Itoa(index)
	labels := []string{"-disposition:" + spec, disposition}
	if language := mp4Language(track.Language); language != "" {
		labels = append(labels, "-metadata:s:"+spec, "language="+language)
	}
	if track.Name != "" {
		labels = append(labels, "-metadata:s:"+spec, "handler_name="+track.Name)
	}
	return labels
}

// subtitleDisposition is a subtitle's flags, as ffmpeg spells them.
func subtitleDisposition(track extractor.Track) string {
	switch {
	case track.Default && track.Forced:
		return "default+forced"
	case track.Default:
		return "default"
	case track.Forced:
		return "forced"
	}
	return "0"
}
