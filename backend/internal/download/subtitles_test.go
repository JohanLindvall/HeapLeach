// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
)

// hideFFmpeg hides ffmpeg and ffprobe from the tool lookup for the rest of
// the test.
func hideFFmpeg(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	tools.Reset()
	t.Cleanup(tools.Reset)
	if _, ok := tools.Find(tools.FFmpeg); ok {
		t.Skip("ffmpeg sits beside the test binary, where it cannot be hidden")
	}
}

func TestMergeWebVTT(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  []string
		want   string
		offset float64
		mapped bool
		err    string
	}{{
		name: "one file, as most hosts serve it",
		files: []string{"\ufeffWEBVTT - An Invented Title\r\nKind: captions\r\n\r\nSTYLE\r\n::cue { color: #ffffff; }\r\n\r\n" +
			"NOTE a comment\r\n\r\n1\r\n00:00:01.000 --> 00:00:02.500 align:start\r\n<i>First</i> line\r\n\r\n" +
			"02:03.250 --> 02:04.000\r\nSecond line\r\n"},
		want: "WEBVTT\n\n00:00:01.000 --> 00:00:02.500 align:start\n<i>First</i> line\n\n" +
			"00:02:03.250 --> 00:02:04.000\nSecond line\n\n",
	}, {
		// Each file maps its own cue times onto the media's clock. The first
		// two share one mapping, and repeat the cue that spans their
		// boundary; the third counts its cues from its own start.
		name: "segments, each with its own timestamp map",
		files: []string{
			"WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000\n\n00:00:01.000 --> 00:00:03.000\nA\n\n" +
				"00:00:05.500 --> 00:00:06.500\nB\n",
			"WEBVTT\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:900000\n\n00:00:05.500 --> 00:00:06.500\nB\n\n" +
				"00:00:07.000 --> 00:00:08.000\nC\n",
			"WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:1800000,LOCAL:00:00:00.000\n\n00:00:00.500 --> 00:00:01.500\nD\n",
		},
		want: "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nA\n\n00:00:05.500 --> 00:00:06.500\nB\n\n" +
			"00:00:07.000 --> 00:00:08.000\nC\n\n00:00:10.500 --> 00:00:11.500\nD\n\n",
		offset: 10, mapped: true,
	}, {
		// The 90 kHz counter wraps every 26½ hours; a file past the wrap is
		// two seconds on from the one before it, not a day back.
		name: "a timestamp map past the counter's wrap",
		files: []string{
			fmt.Sprintf("WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:00:00:00.000\n\n00:00:00.000 --> 00:00:01.000\nA\n", mpegTSWrap-90000),
			"WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:90000,LOCAL:00:00:00.000\n\n00:00:00.000 --> 00:00:01.000\nB\n",
		},
		want:   "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nA\n\n00:00:02.000 --> 00:00:03.000\nB\n\n",
		offset: float64(mpegTSWrap-90000) / mpegTSClock, mapped: true,
	}, {
		name:  "a page where a WebVTT file should be",
		files: []string{"<html><body>Not found</body></html>"},
		err:   errNotWebVTT.Error(),
	}, {
		name:  "a file with nothing to show",
		files: []string{"WEBVTT\n\nNOTE nothing here\n"},
		err:   "no cues",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var files [][]byte
			for _, f := range tc.files {
				files = append(files, []byte(f))
			}
			got, err := mergeWebVTT(files)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got.data) != tc.want {
				t.Errorf("merged:\n%q\nwant:\n%q", got.data, tc.want)
			}
			if got.offset != tc.offset || got.mapped != tc.mapped {
				t.Errorf("offset %v, mapped %v; want %v, %v", got.offset, got.mapped, tc.offset, tc.mapped)
			}
		})
	}
}

// A playlist's subtitles end up in the MP4 as text tracks beside its audio,
// labelled and flagged as the playlist has them, their cues where the WebVTT
// put them. One that cannot be fetched is left out, and the video comes
// through without it. And a muxed video whose only tracks apart are its
// subtitles is joined to them, keeping its own audio.
func TestPlaylistJoinsItsSubtitles(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}

	origin := t.TempDir()
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(origin)))
	vtt := map[string]string{
		// A style block, which left ffmpeg reading no cues at all.
		"/english.vtt": "WEBVTT\n\nSTYLE\n::cue(.speaker) { color: #00ffff; }\n\n" +
			"00:00:01.000 --> 00:00:02.000 align:left position:20%\n<c.speaker>First line</c>\n",
		"/swedish-1.vtt": "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n" +
			"00:00:00.500 --> 00:00:01.200\nFörsta raden\n",
		"/swedish-2.vtt": "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:90000,LOCAL:00:00:00.000\n\n" +
			"00:00:01.000 --> 00:00:01.800\nAndra raden\n",
	}
	for path, body := range vtt {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/vtt")
			_, _ = w.Write([]byte(body))
		})
	}
	mux.HandleFunc("/missing.vtt", http.NotFound)
	srv := testServer(t, mux)
	hls := func(name string, input ...string) []string {
		return cmafRendition(t, ffmpeg, origin, srv.URL, name, input...)
	}
	video := hls("video", append([]string{"-f", "lavfi", "-i", testPicture, "-an"}, testH264...)...)
	muxed := hls("muxed", append([]string{"-f", "lavfi", "-i", testPicture, "-f", "lavfi", "-i", fmt.Sprintf(testTone, 330),
		"-c:a", "aac"}, testH264...)...)
	audio := hls("audio", "-f", "lavfi", "-i", fmt.Sprintf(testTone, 440), "-c:a", "aac")
	subtitles := []extractor.Track{
		{Segments: []string{srv.URL + "/english.vtt"}, Language: "en", Name: "English", Default: true},
		{Segments: []string{srv.URL + "/swedish-1.vtt", srv.URL + "/swedish-2.vtt"}, Language: "sv", Name: "Svenska"},
		{Segments: []string{srv.URL + "/missing.vtt"}, Language: "de", Name: "Deutsch"},
	}

	for _, tc := range []struct {
		name, file string
		segments   []string
		audio      []extractor.Track
		want       []string
	}{{
		name: "beside audio kept apart", file: "Show.mp4", segments: video,
		audio: []extractor.Track{{Segments: audio, Language: "en", Name: "English"}},
		want: []string{`video und "VideoHandler" default=1`, `audio eng "English" default=1`,
			`subtitle eng "English" default=1`, `subtitle swe "Svenska" default=0`},
	}, {
		name: "alone beside a muxed video", file: "Show.ts", segments: muxed,
		want: []string{`video und "VideoHandler" default=1`, `audio und "SoundHandler" default=1`,
			`subtitle eng "English" default=1`, `subtitle swe "Svenska" default=0`},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			m := busyManager(t)
			m.streams = 4
			it := &Item{ID: newID(), Name: tc.file, URL: srv.URL + "/master.m3u8", Size: -1,
				Segments: tc.segments, Audio: tc.audio, Subtitles: subtitles}
			if err := m.transfer(context.Background(), it); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(m.cfg.DownloadDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "Show.mp4" {
				t.Fatalf("directory holds %v, want only the joined MP4", entries)
			}
			joined := filepath.Join(m.cfg.DownloadDir, "Show.mp4")

			out, err := exec.Command(ffprobe, "-v", "error", "-show_streams", "-print_format", "json", joined).Output()
			if err != nil {
				t.Fatal(err)
			}
			var probe struct {
				Streams []struct {
					CodecType   string  `json:"codec_type"`
					CodecName   string  `json:"codec_name"`
					StartTime   float64 `json:"start_time,string"`
					Disposition struct {
						Default int `json:"default"`
					} `json:"disposition"`
					Tags struct {
						Language    string `json:"language"`
						HandlerName string `json:"handler_name"`
					} `json:"tags"`
				} `json:"streams"`
			}
			if err := json.Unmarshal(out, &probe); err != nil {
				t.Fatal(err)
			}
			var (
				got     []string
				picture float64
			)
			for _, s := range probe.Streams {
				got = append(got, fmt.Sprintf("%s %s %q default=%d", s.CodecType, s.Tags.Language,
					s.Tags.HandlerName, s.Disposition.Default))
				if s.CodecType == "subtitle" && s.CodecName != "mov_text" {
					t.Errorf("subtitle codec %s, want mov_text", s.CodecName)
				}
				if s.CodecType == "video" {
					picture = s.StartTime
				}
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("streams:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}

			// The cues are where the WebVTT put them. One that says nothing
			// of where it sits counts from the start of the picture; one with
			// a timestamp map sits on the media's own timeline, where the
			// picture of the muxed video starts after its audio's priming.
			// The whole file then moves by however far its earliest stream
			// starts ahead of zero. The second Swedish file's map moves its
			// line one second on.
			shift := picture - renditionStart(t, ffprobe, origin, srv.URL, tc.segments)
			for _, cue := range []struct {
				stream, text string
				at, from     float64
			}{{"0:s:0", "First line", 1.0, picture}, {"0:s:1", "Andra raden", 2.0, shift}} {
				srt, err := exec.Command(ffmpeg, "-v", "error", "-i", joined, "-map", cue.stream, "-f", "srt", "-").Output()
				if err != nil {
					t.Fatal(err)
				}
				at, ok := srtCueStart(string(srt), cue.text)
				if !ok || at-cue.from < cue.at-0.002 || at-cue.from > cue.at+0.002 {
					t.Errorf("%s: %q starts at %.3fs, %.3fs on from %.3fs; want %.3fs on:\n%s",
						cue.stream, cue.text, at, at-cue.from, cue.from, cue.at, srt)
				}
			}
		})
	}
}

// renditionStart is when the picture of a packaged rendition starts on its
// own timeline: its segments, from origin, joined and probed.
func renditionStart(t *testing.T, ffprobe, origin, base string, segments []string) float64 {
	t.Helper()
	var joined []byte
	for _, segment := range segments {
		data, err := os.ReadFile(filepath.Join(origin, strings.TrimPrefix(segment, base+"/")))
		if err != nil {
			t.Fatal(err)
		}
		joined = append(joined, data...)
	}
	path := filepath.Join(t.TempDir(), "rendition.mp4")
	if err := os.WriteFile(path, joined, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=start_time",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	start, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("start time %q: %v", out, err)
	}
	return start
}

// srtCueStart finds the cue holding text in an SRT document and reads its
// start, in seconds.
func srtCueStart(srt, text string) (float64, bool) {
	for block := range strings.SplitSeq(strings.ReplaceAll(srt, "\r\n", "\n"), "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) < 3 || !strings.Contains(strings.Join(lines[2:], "\n"), text) {
			continue
		}
		start, _, _ := strings.Cut(lines[1], " --> ")
		return parseCueTime(strings.Replace(start, ",", ".", 1))
	}
	return 0, false
}

// Without ffmpeg there is nothing to join subtitles with: they are not
// fetched, and the video comes through as it would have without them.
func TestSubtitlesAreNotFetchedWithoutFFmpeg(t *testing.T) {
	var asked atomic.Bool
	srv := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".vtt") {
			asked.Store(true)
		}
		_, _ = w.Write([]byte("data"))
	}))
	hideFFmpeg(t)

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.ts", URL: srv.URL, Size: -1, Segments: []string{srv.URL + "/v0.ts"},
		Subtitles: []extractor.Track{{Segments: []string{srv.URL + "/en.vtt"}, Language: "en"}}}
	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	if asked.Load() {
		t.Error("subtitles were fetched with nothing to join them")
	}
	if _, err := os.Stat(filepath.Join(m.cfg.DownloadDir, "clip.ts")); errors.Is(err, os.ErrNotExist) {
		t.Error("the video did not come through")
	}
}
