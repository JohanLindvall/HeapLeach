// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
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

func TestWithoutPackedAudioTag(t *testing.T) {
	frames := []byte{0x0b, 0x77, 0x01, 0x02, 0x03}
	for _, version := range []byte{3, 4} {
		if got := withoutPackedAudioTag(append(id3Timestamp(version, 90000), frames...)); !bytes.Equal(got, frames) {
			t.Errorf("ID3v2.%d: %x; want the frames alone", version, got)
		}
	}
	// ID3v2.4 lets a tag end with a footer, which goes with it.
	tag := id3Timestamp(4, 90000)
	tag[5] |= 0x10
	footed := append(append(tag, append([]byte("3DI"), tag[3:10]...)...), frames...)
	if got := withoutPackedAudioTag(footed); !bytes.Equal(got, frames) {
		t.Errorf("with a footer: %x; want the frames alone", got)
	}
	for name, segment := range map[string][]byte{
		"MPEG-TS":         {0x47, 0x40, 0x11, 0x10, 0, 0, 0, 0, 0, 0, 0, 0},
		"bare AC-3":       frames,
		"a tag cut short": id3Timestamp(4, 90000)[:40],
	} {
		if got := withoutPackedAudioTag(segment); !bytes.Equal(got, segment) {
			t.Errorf("%s came back as %x", name, got)
		}
	}
}

// A packed audio track keeps its first segment's tag alone. Each later one
// would sit between two frames, where ffmpeg's AC-3 reader takes it for the
// end of the frame before, and its decoder then drops that frame whole: the
// sound would break off for 32 ms at every segment. The first tag stays, as
// the track's start is read from it.
func TestPackedAudioKeepsOnlyItsFirstTag(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t), requireFFprobe(t)
	origin := t.TempDir()
	srv := testServer(t, http.FileServer(http.Dir(origin)))
	video := cmafRendition(t, ffmpeg, origin, srv.URL, "video",
		append([]string{"-f", "lavfi", "-i", testPicture, "-an"}, testH264...)...)

	// AC-3 at 192 kbit/s and 48 kHz is frames of 768 bytes; 32 of them make
	// a segment, each opening with its tag, the first half a second in.
	const frame, perSegment = 768, 32
	tone := filepath.Join(t.TempDir(), "tone.ac3")
	run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", fmt.Sprintf(testTone, 440),
		"-ar", "48000", "-c:a", "ac3", "-b:a", "192k", "-f", "ac3", tone)
	frames, err := os.ReadFile(tone)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames)%frame != 0 {
		t.Fatalf("%d bytes of AC-3, not a whole number of %d-byte frames", len(frames), frame)
	}
	var sound []string
	for i := 0; i*frame*perSegment < len(frames); i++ {
		name := fmt.Sprintf("sound-%d.ac3", i)
		ticks := uint64(45000 + i*perSegment*1536*90000/48000)
		body := frames[i*frame*perSegment : min(len(frames), (i+1)*frame*perSegment)]
		if err := os.WriteFile(filepath.Join(origin, name), append(id3Timestamp(4, ticks), body...), 0o644); err != nil {
			t.Fatal(err)
		}
		sound = append(sound, srv.URL+"/"+name)
	}

	m := busyManager(t)
	m.streams = 4
	it := &Item{ID: newID(), Name: "Show.mp4", URL: srv.URL + "/master.m3u8", Size: -1, Segments: video,
		Audio: []extractor.Track{{Segments: sound, Language: "sv", Name: "Svenska"}}}
	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	joined := filepath.Join(m.cfg.DownloadDir, "Show.mp4")

	if out, err := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", joined,
		"-map", "0:a", "-f", "null", "-").CombinedOutput(); err != nil || len(out) > 0 {
		t.Errorf("decoding the joined sound: %v %s", err, out)
	}
	samples := strings.Fields(run(t, ffprobe, "-v", "error", "-select_streams", "a:0", "-show_entries", "packet=size",
		"-of", "csv=p=0", joined))
	if len(samples) != len(frames)/frame {
		t.Errorf("%d samples, want a frame each: %d", len(samples), len(frames)/frame)
	}
	for i, size := range samples {
		if size != strconv.Itoa(frame) {
			t.Errorf("sample %d holds %s bytes, want one frame's %d", i, size, frame)
		}
	}

	starts := startsByKind(t, joined)
	if diff := starts["audio"] - starts["video"]; diff < 0.45 || diff > 0.55 {
		t.Errorf("audio starts %.3fs after the video (%v); want the 0.5s its first tag says", diff, starts)
	}
}

// A packed audio track is joined where its tag places it, not at zero,
// beside a TS video that keeps its own timestamps.
func TestJoiningPlacesPackedAudioOnTheVideosTimeline(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	requireFFprobe(t)
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

	// The muxer starts the picture a little after the offset asked for.
	want := 10.5 - startsByKind(t, video)["video"]

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
	got := startsByKind(t, joined)
	if diff := got["audio"] - got["video"]; diff < want-0.05 || diff > want+0.05 {
		t.Errorf("audio starts %.3fs after the video (%v); want the %.3fs its tag says", diff, got, want)
	}
}

// startsByKind is when a file's last stream of each kind starts.
func startsByKind(t *testing.T, path string) map[string]float64 {
	t.Helper()
	starts := map[string]float64{}
	for _, s := range ffprobeStreams(t, path) {
		starts[s.CodecType] = s.StartTime
	}
	return starts
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
