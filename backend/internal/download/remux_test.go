package download

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Conversion must never re-encode. These build real transport streams with
// ffmpeg and check that what comes out carries the same codecs that went in.
func TestRemuxNeverReEncodes(t *testing.T) {
	ffmpeg := requireFFmpeg(t)

	tests := []struct {
		name        string
		audioArgs   []string
		extraArgs   []string
		wantConvert bool
		why         string
	}{
		{
			name:        "aac audio",
			audioArgs:   []string{"-c:a", "aac"},
			wantConvert: true,
			why:         "AAC in TS is exactly what MP4 wants",
		},
		{
			name:        "mp2 audio",
			audioArgs:   []string{"-c:a", "mp2"},
			wantConvert: true,
			why:         "MP4 accepts MP2 on a stream copy",
		},
		{
			name:        "no subtitle track",
			audioArgs:   []string{"-c:a", "aac"},
			wantConvert: true,
			why:         "the subtitle map is optional, so its absence must not fail",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "clip.ts")
			buildTS(t, ffmpeg, source, tc.audioArgs, tc.extraArgs)

			before := probeStreams(t, ffmpeg, source)
			manager := &Manager{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			result := manager.remuxToMP4(context.Background(), source)

			if !tc.wantConvert {
				if result != source {
					t.Fatalf("converted a stream that cannot go into MP4 losslessly (%s)", tc.why)
				}
				if _, err := os.Stat(source); err != nil {
					t.Error("the original was removed even though nothing was produced")
				}
				return
			}

			if result == source {
				t.Fatalf("did not convert, but should have (%s)", tc.why)
			}
			if filepath.Ext(result) != ".mp4" {
				t.Fatalf("produced %q, want an .mp4", filepath.Base(result))
			}
			if _, err := os.Stat(source); !os.IsNotExist(err) {
				t.Error("the transport stream was left behind after a successful conversion")
			}

			after := probeStreams(t, ffmpeg, result)
			if len(after) != len(before) {
				t.Fatalf("stream count changed: %v -> %v", before, after)
			}
			for i := range before {
				if !sameCodec(before[i], after[i]) {
					t.Errorf("stream %d was re-encoded: %s -> %s", i, before[i], after[i])
				}
			}
			t.Logf("streams preserved: %v", after)
		})
	}
}

// Only the best of each kind is carried over, so a second audio track is
// deliberately left behind rather than bloating the result.
func TestRemuxKeepsBestStreams(t *testing.T) {
	ffmpeg := requireFFmpeg(t)

	dir := t.TempDir()
	source := filepath.Join(dir, "multi.ts")

	// One video and two audio tracks.
	run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=1",
		"-map", "0:v", "-map", "1:a", "-map", "2:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac",
		"-f", "mpegts", source)

	before := probeStreams(t, ffmpeg, source)
	if len(before) != 3 {
		t.Fatalf("fixture has %d streams, want 3", len(before))
	}

	manager := &Manager{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	result := manager.remuxToMP4(context.Background(), source)
	if result == source {
		t.Fatal("did not convert a stream set MP4 can hold")
	}

	after := probeStreams(t, ffmpeg, result)
	if len(after) != 2 {
		t.Fatalf("kept %v, want one video and one audio track", after)
	}
	var video, audio int
	for _, stream := range after {
		switch {
		case strings.HasPrefix(stream, "video:"):
			video++
		case strings.HasPrefix(stream, "audio:"):
			audio++
		}
	}
	if video != 1 || audio != 1 {
		t.Errorf("kept %d video and %d audio streams, want one of each: %v", video, audio, after)
	}
	t.Logf("best streams kept: %v (from %v)", after, before)
}

func TestRemuxLeavesNonTransportStreamsAlone(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"movie.mp4", "clip.webm", "image.jpg"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("not a transport stream"), 0o644); err != nil {
			t.Fatal(err)
		}
		manager := &Manager{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if got := manager.remuxToMP4(context.Background(), path); got != path {
			t.Errorf("%s was touched, want it left alone", name)
		}
	}
}

// The MPEG audio layers share an identifier in MP4, so a copied MP2 stream
// is reported as mp3 on the way out. The bitstream is byte-for-byte the
// same, so these count as the same codec rather than as a re-encode.
func sameCodec(before, after string) bool {
	if before == after {
		return true
	}
	equivalent := map[string]string{"audio:mp2": "audio:mp3", "audio:mp3": "audio:mp2"}
	return equivalent[before] == after
}

// A recording carrying several languages should stay usable in all of them,
// while the duplicate encodes within one language are left behind.
func TestRemuxKeepsBestOfEachLanguage(t *testing.T) {
	ffmpeg := requireFFmpeg(t)

	dir := t.TempDir()
	source := filepath.Join(dir, "multilang.ts")

	// English twice (stereo and mono) plus Swedish once. The stereo English
	// track should win its language; Swedish should survive on its own.
	run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=660:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=1",
		"-map", "0:v", "-map", "1:a", "-map", "2:a", "-map", "3:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac",
		"-ac:a:0", "2", "-ac:a:1", "1", "-ac:a:2", "2",
		"-metadata:s:a:0", "language=eng",
		"-metadata:s:a:1", "language=eng",
		"-metadata:s:a:2", "language=swe",
		"-f", "mpegts", source)

	manager := &Manager{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	result := manager.remuxToMP4(context.Background(), source)
	if result == source {
		t.Fatal("did not convert")
	}

	languages := probeLanguages(t, ffmpeg, result, "audio")
	if len(languages) != 2 {
		t.Fatalf("kept %d audio tracks %v, want one per language (eng, swe)", len(languages), languages)
	}
	seen := map[string]bool{}
	for _, lang := range languages {
		if seen[lang] {
			t.Errorf("kept two tracks for %q; only the best of each language should survive", lang)
		}
		seen[lang] = true
	}
	if !seen["eng"] || !seen["swe"] {
		t.Errorf("languages kept = %v, want eng and swe", languages)
	}
	t.Logf("one track per language kept: %v (from 3 audio tracks)", languages)
}

// probeLanguages lists the language tag of each stream of a kind.
func probeLanguages(t *testing.T, ffmpeg, path, kind string) []string {
	t.Helper()
	ffprobe := filepath.Join(filepath.Dir(ffmpeg), "ffprobe")
	if _, err := os.Stat(ffprobe); err != nil {
		ffprobe = "ffprobe"
	}
	out, err := exec.Command(ffprobe, "-v", "error", "-show_streams",
		"-print_format", "json", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Tags      struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatal(err)
	}
	var languages []string
	for _, s := range probe.Streams {
		if s.CodecType == kind {
			languages = append(languages, s.Tags.Language)
		}
	}
	return languages
}

// --- helpers ---------------------------------------------------------------

func requireFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	return path
}

func buildTS(t *testing.T, ffmpeg, target string, audioArgs, extraArgs []string) {
	t.Helper()
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast"}
	args = append(args, audioArgs...)
	args = append(args, extraArgs...)
	args = append(args, "-f", "mpegts", target)
	run(t, ffmpeg, args...)
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", filepath.Base(name), err, out)
	}
}

// probeStreams lists each stream's codec, so re-encoding would show up.
func probeStreams(t *testing.T, ffmpeg, path string) []string {
	t.Helper()
	ffprobe := filepath.Join(filepath.Dir(ffmpeg), "ffprobe")
	if _, err := os.Stat(ffprobe); err != nil {
		ffprobe = "ffprobe"
	}
	out, err := exec.Command(ffprobe, "-v", "error", "-show_streams",
		"-print_format", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", filepath.Base(path), err)
	}
	var probe struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatal(err)
	}
	var streams []string
	for _, s := range probe.Streams {
		streams = append(streams, strings.Join([]string{s.CodecType, s.CodecName}, ":"))
	}
	return streams
}
