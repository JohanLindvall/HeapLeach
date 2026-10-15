// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// id3Timestamp is the tag HLS writes ahead of a packed audio segment: an
// ID3 tag of the given version whose PRIV frame holds the timestamp, after
// a frame of something else.
func id3Timestamp(version byte, ticks uint64) []byte {
	size := func(n int) []byte {
		if version >= 4 {
			return []byte{byte(n>>21) & 0x7f, byte(n>>14) & 0x7f, byte(n>>7) & 0x7f, byte(n) & 0x7f}
		}
		return binary.BigEndian.AppendUint32(nil, uint32(n))
	}
	frame := func(id string, payload []byte) []byte {
		return append(append(append([]byte(id), size(len(payload))...), 0, 0), payload...)
	}
	other := frame("TXXX", []byte("\x03an invented note\x00"))
	priv := frame("PRIV", binary.BigEndian.AppendUint64([]byte(packedAudioOwner+"\x00"), ticks))
	body := append(other, priv...)
	tagSize := []byte{byte(len(body)>>21) & 0x7f, byte(len(body)>>14) & 0x7f, byte(len(body)>>7) & 0x7f, byte(len(body)) & 0x7f}
	return append(append([]byte{'I', 'D', '3', version, 0, 0}, tagSize...), body...)
}

func TestPackedAudioTimestamp(t *testing.T) {
	for _, version := range []byte{3, 4} {
		start, ok := packedAudioTimestamp(append(id3Timestamp(version, 896160), 0xff, 0xf1))
		if !ok || start < 9.9573 || start > 9.9574 {
			t.Errorf("ID3v2.%d: %v %v; want 9.957333 s", version, start, ok)
		}
	}
	// The timestamp is 33 bits: what lies above them is not the clock's.
	if start, ok := packedAudioTimestamp(id3Timestamp(4, 1<<40|90000)); !ok || start != 1 {
		t.Errorf("high bits: %v %v; want 1 s", start, ok)
	}
	for name, head := range map[string][]byte{
		"MPEG-TS":  {0x47, 0x40, 0x11, 0x10, 0, 0, 0, 0, 0, 0, 0, 0},
		"bare AAC": {0xff, 0xf1, 0x50, 0x80, 0, 0x1f, 0xfc, 0, 0, 0},
		"short":    []byte("ID3"),
	} {
		if _, ok := packedAudioTimestamp(head); ok {
			t.Errorf("%s read as packed audio", name)
		}
	}
}

// A packed audio track is joined where its tag places it, not at zero,
// beside a TS video that keeps its own timestamps.
func TestJoiningPlacesPackedAudioOnTheVideosTimeline(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	video, aac, audio := filepath.Join(dir, "clip.part"), filepath.Join(dir, "clip.aac"), filepath.Join(dir, "clip.audio1.part")
	run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=25:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-output_ts_offset", "10", "-f", "mpegts", video)
	run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:a", "aac", "-f", "adts", aac)
	frames, err := os.ReadFile(aac)
	if err != nil {
		t.Fatal(err)
	}
	// The audio's own timeline starts half a second after the one the
	// picture was offset to.
	if err := os.WriteFile(audio, append(id3Timestamp(4, 10_5*9000), frames...), 0o644); err != nil {
		t.Fatal(err)
	}

	starts := func(path string) map[string]float64 {
		t.Helper()
		out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_type,start_time", "-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]float64{}
		for line := range strings.Lines(string(out)) {
			kind, value, _ := strings.Cut(strings.TrimSpace(line), ",")
			if start, err := strconv.ParseFloat(value, 64); err == nil {
				found[kind] = start
			}
		}
		return found
	}
	// The muxer starts the picture a little after the offset asked for.
	want := 10.5 - starts(video)["video"]

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4"}
	tracks := []playlistTrack{
		{part: video},
		{part: audio, audio: &extractor.Track{Language: "fr", Name: "Français"}},
	}
	joined, err := m.joinTracks(context.Background(), it, tracks, nil, transferFiles{})
	if err != nil {
		t.Fatal(err)
	}
	got := starts(joined)
	if diff := got["audio"] - got["video"]; diff < want-0.05 || diff > want+0.05 {
		t.Errorf("audio starts %.3fs after the video (%v); want the %.3fs its tag says", diff, got, want)
	}
}

// A video whose frames are presented before they are decoded has its decode
// times worked out again, on its own input alone; one stamped soundly is
// left as it is.
func TestJoiningRestampsAVideoPresentedEarly(t *testing.T) {
	early := "900000,900000,\n910800,903600,\n903600,907200,\n"
	sound := "907200,900000\n918000,903600\n910800,907200\n"
	if !presentedEarly(early) || presentedEarly(sound) || presentedEarly("N/A,N/A\n") {
		t.Fatal("presentedEarly misreads ffprobe's packet times")
	}
	tracks := []playlistTrack{{part: "video.part"}, {part: "audio.part", audio: &extractor.Track{}}}
	args := strings.Join(joinArgs(tracks, nil, videoProbe{audio: -1, presentedEarly: true}, "out.mp4"), " ")
	if !strings.Contains(args, "-copyts -fflags +igndts -i video.part -i audio.part") {
		t.Errorf("args %q; want the video's input alone restamped", args)
	}
	if args := strings.Join(joinArgs(tracks, nil, videoProbe{audio: -1}, "out.mp4"), " "); strings.Contains(args, "igndts") {
		t.Errorf("args %q; a sound video is left as it is", args)
	}
}
