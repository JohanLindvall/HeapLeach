// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// Each audio track a playlist keeps apart from its video is joined into a
// part file of its own beside the video's, resuming from its own checkpoint,
// which holds only for the track that wrote it: a playlist's tracks commonly
// have the same number of segments.
func TestPlaylistJoinsEachTrackIntoItsOwnPart(t *testing.T) {
	const parts = 6
	body := func(track string, i int) []byte { return fmt.Appendf(nil, "%s segment %d;", track, i) }
	joined := func(track string) []byte {
		var all []byte
		for i := range parts {
			all = append(all, body(track, i)...)
		}
		return all
	}
	var (
		mu        sync.Mutex
		requested = map[string]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		track, index, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		i, err := strconv.Atoi(index)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		requested[r.URL.Path]++
		mu.Unlock()
		_, _ = w.Write(body(track, i))
	}))
	defer srv.Close()
	list := func(track string) []string {
		var segments []string
		for i := range parts {
			segments = append(segments, fmt.Sprintf("%s/%s/%d", srv.URL, track, i))
		}
		return segments
	}

	m := busyManager(t)
	m.streams = 3
	it := &Item{ID: newID(), Name: "show.mp4", URL: srv.URL, Size: -1, Segments: list("video"),
		Audio: []extractor.Track{
			{Segments: list("swedish"), Language: "sv", Name: "Svenska"},
			{Segments: list("clear"), Language: "sv-x-tydligaretal", Name: "Tydligare tal"},
		}}
	part := filepath.Join(m.cfg.DownloadDir, "show.mp4.test.part")

	// The first track got two segments in before an earlier attempt ended.
	// The second's part holds a checkpoint that another track wrote.
	first, second := audioPart(part, 0), audioPart(part, 1)
	start := append(body("swedish", 0), body("swedish", 1)...)
	if err := os.WriteFile(first, start, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := savePlaylistState(first, playlistState{Index: 2, Bytes: int64(len(start)), Total: parts, Track: "sv/Svenska"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("another track's bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := savePlaylistState(second, playlistState{Index: 3, Bytes: 10, Total: parts, Track: "en/English"}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.transferPlaylist(context.Background(), it, part, "show.mp4"); err != nil {
		t.Fatal(err)
	}

	for path, track := range map[string]string{part: "video", first: "swedish", second: "clear"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if want := joined(track); !bytes.Equal(got, want) {
			t.Errorf("%s holds %q, want %q", filepath.Base(path), got, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range 2 {
		if n := requested[fmt.Sprintf("/swedish/%d", i)]; n != 0 {
			t.Errorf("the resumed track fetched its segment %d again, %d times", i+1, n)
		}
	}
	for i := range parts {
		if n := requested[fmt.Sprintf("/clear/%d", i)]; n != 1 {
			t.Errorf("the track whose checkpoint was another's fetched segment %d %d times, want once", i+1, n)
		}
	}
	clear := playlistTrack{part: second, segments: it.Audio[1].Segments, audio: &it.Audio[1]}
	if st := clear.state(); st == nil || st.Index != parts {
		t.Errorf("checkpoint = %+v; want the track's own, complete", st)
	}
	m.mu.Lock()
	done, total := it.SegmentsDone, it.SegmentsTotal
	m.mu.Unlock()
	if done != 3*parts || total != 3*parts {
		t.Errorf("segment progress = %d/%d, want every track's: %d/%d", done, total, 3*parts, 3*parts)
	}
}

// A playlist whose audio is kept apart comes out as one MP4 holding the
// video and exactly the audio renditions the playlist names, in its order,
// each labelled with its language and name and only the first played by
// default, with nothing left beside it. A rendition can be a playlist of its
// own or, named with no URI, carried in the video's own segments.
func TestPlaylistJoinsAudioKeptApart(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}

	origin := t.TempDir()
	srv := httptest.NewServer(http.FileServer(http.Dir(origin)))
	defer srv.Close()
	hls := func(name string, input ...string) []string {
		return cmafRendition(t, ffmpeg, origin, srv.URL, name, input...)
	}
	video := hls("video", append([]string{"-f", "lavfi", "-i", testPicture, "-an"}, testH264...)...)
	muxed := hls("muxed", append([]string{"-f", "lavfi", "-i", testPicture, "-f", "lavfi", "-i", fmt.Sprintf(testTone, 330),
		"-c:a", "aac"}, testH264...)...)
	swedish := hls("swedish", "-f", "lavfi", "-i", fmt.Sprintf(testTone, 440), "-c:a", "aac")
	clear := hls("clear", "-f", "lavfi", "-i", fmt.Sprintf(testTone, 880), "-c:a", "aac")

	const picked = `video und "VideoHandler" default=1`
	for _, tc := range []struct {
		name     string
		segments []string
		audio    []extractor.Track
		want     []string
	}{{
		name:     "every track apart from a video with none",
		segments: video,
		audio: []extractor.Track{
			{Segments: swedish, Language: "sv", Name: "Svenska"},
			{Segments: clear, Language: "sv-x-tydligaretal", Name: "Tydligare tal"},
		},
		want: []string{picked, `audio swe "Svenska" default=1`, `audio swe "Tydligare tal" default=0`},
	}, {
		// Akamai mixes the default track into the video as well as
		// publishing it apart. The copy the playlist does not name is left
		// out, or the file would list it twice.
		name:     "audio in the video that the playlist does not name",
		segments: muxed,
		audio: []extractor.Track{
			{Segments: swedish, Language: "de", Name: "Deutsch"},
			{Segments: clear, Language: "de", Name: "Deutsch (audio description)"},
		},
		want: []string{picked, `audio deu "Deutsch" default=1`, `audio deu "Deutsch (audio description)" default=0`},
	}, {
		name:     "the main track in the video and an alternate apart",
		segments: muxed,
		audio: []extractor.Track{
			{Language: "en", Name: "English"},
			{Segments: swedish, Language: "sv", Name: "Svenska"},
		},
		want: []string{picked, `audio eng "English" default=1`, `audio swe "Svenska" default=0`},
	}, {
		name:     "a track named as in the video that is not there",
		segments: video,
		audio: []extractor.Track{
			{Language: "en", Name: "English"},
			{Segments: swedish, Language: "sv", Name: "Svenska"},
		},
		want: []string{picked, `audio swe "Svenska" default=1`},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			m := busyManager(t)
			m.streams = 4
			it := &Item{ID: newID(), Name: "Show.mp4", URL: srv.URL + "/master.m3u8", Size: -1,
				Segments: tc.segments, Audio: tc.audio}
			if err := m.transfer(context.Background(), it); err != nil {
				t.Fatal(err)
			}

			entries, err := os.ReadDir(m.cfg.DownloadDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "Show.mp4" {
				t.Errorf("directory holds %v, want only the joined file", entries)
			}
			m.mu.Lock()
			path := it.Path
			m.mu.Unlock()
			if path != "Show.mp4" {
				t.Errorf("recorded path = %q", path)
			}

			out, err := exec.Command(ffprobe, "-v", "error", "-show_streams", "-print_format", "json",
				filepath.Join(m.cfg.DownloadDir, "Show.mp4")).Output()
			if err != nil {
				t.Fatal(err)
			}
			var probe struct {
				Streams []struct {
					CodecType   string  `json:"codec_type"`
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
			var got []string
			for _, s := range probe.Streams {
				got = append(got, fmt.Sprintf("%s %s %q default=%d", s.CodecType, s.Tags.Language,
					s.Tags.HandlerName, s.Disposition.Default))
				// Every input starts at zero here, so all of the output should.
				if s.StartTime > 0.2 {
					t.Errorf("%s stream starts at %.3fs, want the start of the file", s.CodecType, s.StartTime)
				}
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("streams:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// The sources the packaged renditions are made from: three seconds of a test
// picture and of a tone.
const (
	testPicture = "testsrc=size=160x120:rate=10:duration=3"
	testTone    = "sine=frequency=%d:duration=3"
)

var testH264 = []string{"-c:v", "libx264", "-preset", "ultrafast", "-g", "10"}

// cmafRendition packages a rendition into origin the way CMAF does, and lists
// its segments as a media playlist reads, from base: the initialisation
// segment first.
func cmafRendition(t *testing.T, ffmpeg, origin, base, name string, input ...string) []string {
	t.Helper()
	args := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, input...)
	args = append(args, "-f", "hls", "-hls_time", "1", "-hls_playlist_type", "vod",
		"-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", name+"-init.mp4",
		"-hls_segment_filename", name+"-%d.m4s", name+".m3u8")
	cmd := exec.Command(ffmpeg, args...)
	cmd.Dir = origin
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package %s: %v\n%s", name, err, out)
	}
	playlist, err := os.ReadFile(filepath.Join(origin, name+".m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	var segments []string
	for line := range strings.Lines(string(playlist)) {
		line = strings.TrimSpace(line)
		if init, ok := strings.CutPrefix(line, `#EXT-X-MAP:URI="`); ok {
			segments = append([]string{base + "/" + strings.TrimSuffix(init, `"`)}, segments...)
		} else if line != "" && !strings.HasPrefix(line, "#") {
			segments = append(segments, base+"/"+line)
		}
	}
	return segments
}

// Renditions keep their timestamps on the playlist's one timeline, and the
// offset between them is what keeps sound and picture together. ffmpeg
// starts each input at zero on its own unless told not to, which would drop
// it; the joined file keeps it, and starts at zero as a whole.
func TestJoiningKeepsTheOffsetBetweenTracks(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	video, audio := filepath.Join(dir, "clip.part"), filepath.Join(dir, "clip.audio1.part")
	run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=25:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-output_ts_offset", "10", "-f", "mpegts", video)
	run(t, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:a", "aac", "-output_ts_offset", "10.5", "-f", "mpegts", audio)
	starts := func(path string) []float64 {
		out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=start_time",
			"-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		var times []float64
		for field := range strings.FieldsSeq(string(out)) {
			value, err := strconv.ParseFloat(strings.TrimSuffix(field, ","), 64)
			if err != nil {
				t.Fatalf("start time %q: %v", field, err)
			}
			times = append(times, value)
		}
		return times
	}
	offset := starts(audio)[0] - starts(video)[0]

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "clip.mp4"}
	tracks := []playlistTrack{
		{part: video},
		{part: audio, audio: &extractor.Track{Language: "en", Name: "English"}},
	}
	joined, err := m.joinTracks(context.Background(), it, tracks, nil, transferFiles{})
	if err != nil {
		t.Fatal(err)
	}
	got := starts(joined)
	if len(got) != 2 {
		t.Fatalf("joined file has %d streams, want 2", len(got))
	}
	if got[0] > 0.2 {
		t.Errorf("video starts at %.3fs; want the file to start at zero", got[0])
	}
	if diff := got[1] - got[0]; diff < offset-0.01 || diff > offset+0.01 {
		t.Errorf("audio starts %.3fs after the video; want the %.3fs it did before joining", diff, offset)
	}
}

// Without ffmpeg there is nothing to join a soundtrack kept apart from its
// video with, and the video alone is silent. The transfer says so before it
// fetches a byte, rather than once the whole video has arrived.
func TestPlaylistWithAudioKeptApartNeedsFFmpegFirst(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	hideFFmpeg(t)

	m := busyManager(t)
	it := &Item{ID: newID(), Name: "silent.mp4", URL: srv.URL, Size: -1, Segments: []string{srv.URL + "/v0"},
		Audio: []extractor.Track{{Segments: []string{srv.URL + "/a0"}, Language: "en", Name: "English"}}}
	err := m.transfer(context.Background(), it)
	if err == nil || !strings.Contains(err.Error(), "needs ffmpeg") {
		t.Fatalf("err = %v; want one saying the join needs ffmpeg", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("fetched %d segments before saying ffmpeg was missing", n)
	}
}

func TestMP4Language(t *testing.T) {
	for tag, want := range map[string]string{
		"sv":                "swe",
		"sv-x-tydligaretal": "swe",
		"en-US":             "eng",
		"DE":                "deu",
		"zh-Hant":           "zho",
		"pt_BR":             "por",
		"deu":               "deu",
		"gsw":               "gsw",
		"":                  "",
		"x-private":         "",
		"qq":                "",
		"d3u":               "",
	} {
		if got := mp4Language(tag); got != want {
			t.Errorf("mp4Language(%q) = %q, want %q", tag, got, want)
		}
	}
}
