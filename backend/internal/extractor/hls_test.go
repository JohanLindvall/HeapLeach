// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A master playlist in the shape that made this worth fixing: the variants
// advertise both a video and an audio codec, but the audio lives in a group
// of its own with its own playlist. Concatenating such a variant's segments
// yields video and no sound.
const demuxedMaster = `#EXTM3U
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-high",NAME="English",DEFAULT=YES,LANGUAGE="en",URI="audio/high/media.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",URI="sub/140662/media.m3u8"
#EXT-X-STREAM-INF:SUBTITLES="subs",BANDWIDTH=3063040,RESOLUTION=1280x720,CODECS="avc1.64001F,mp4a.40.2",AUDIO="audio-high"
video/720/media.m3u8
#EXT-X-STREAM-INF:SUBTITLES="subs",BANDWIDTH=921925,RESOLUTION=640x360,CODECS="avc1.64001F,mp4a.40.2",AUDIO="audio-high"
video/360/media.m3u8
`

// The same shape, except the audio rendition has no URI of its own, which is
// how a playlist says the audio is already inside the variant's segments.
const muxedMaster = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="English",DEFAULT=YES,LANGUAGE="en"
#EXT-X-STREAM-INF:BANDWIDTH=3063040,RESOLUTION=1280x720,CODECS="avc1.64001F,mp4a.40.2",AUDIO="audio"
video/720/media.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=921925,RESOLUTION=640x360,CODECS="avc1.64001F,mp4a.40.2"
video/360/media.m3u8
`

// TestMuxedIgnoresCodecsWhenAudioIsElsewhere is the regression guard. CODECS
// describes the whole presentation, so a variant naming a separate audio
// group advertises audio it does not itself carry; taking that at face value
// produced a silent file that looked like a finished download.
func TestMuxedIgnoresCodecsWhenAudioIsElsewhere(t *testing.T) {
	base := mustParse(t, "https://cdn.example.test/master.m3u8")

	variants := parseMasterPlaylist(demuxedMaster, base)
	if len(variants) != 2 {
		t.Fatalf("parsed %d variants, want 2", len(variants))
	}
	for _, v := range variants {
		if v.muxed() {
			t.Errorf("%s reads as self-contained, but its audio is in a group of its own", v.Resolution)
		}
	}
}

func TestMuxedAcceptsAudioWithNoPlaylistOfItsOwn(t *testing.T) {
	base := mustParse(t, "https://cdn.example.test/master.m3u8")

	variants := parseMasterPlaylist(muxedMaster, base)
	if len(variants) != 2 {
		t.Fatalf("parsed %d variants, want 2", len(variants))
	}
	for _, v := range variants {
		if !v.muxed() {
			t.Errorf("%s reads as video only, but nothing in the playlist puts its audio elsewhere", v.Resolution)
		}
	}

	best, ok := bestVariant(variants)
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if best.height() != 720 {
		t.Errorf("chose %s, want the largest", best.Resolution)
	}
}

// TestBestVariantPrefersSelfContained covers the ordering the fix restores:
// a smaller self-contained rendition beats a larger one whose audio is
// somewhere else, because only the first joins into a playable file.
// Byte ranges of one file are segments like any other, each carrying its
// range: the stated offset, or where the previous part of the same file
// ended, and a map's range from the start of the file when it states none.
func TestMediaPlaylistByteRanges(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.test/v/clip/index.m3u8")
	doc := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n" + `#EXT-X-MAP:URI="clip.mp4",BYTERANGE="700"` + "\n" +
		"#EXTINF:6,\n#EXT-X-BYTERANGE:1000@700\nclip.mp4\n" +
		"#EXTINF:6,\n#EXT-X-BYTERANGE:1200\nclip.mp4\n" +
		"#EXTINF:6,\n#EXT-X-BYTERANGE:300@5000\nother.mp4\n" +
		"#EXTINF:6,\nwhole.mp4\n#EXT-X-ENDLIST\n"
	want := []string{
		"https://cdn.example.test/v/clip/clip.mp4#range=0-699",
		"https://cdn.example.test/v/clip/clip.mp4#range=700-1699",
		"https://cdn.example.test/v/clip/clip.mp4#range=1700-2899",
		"https://cdn.example.test/v/clip/other.mp4#range=5000-5299",
		"https://cdn.example.test/v/clip/whole.mp4",
	}
	if got := parseMediaPlaylist(doc, base); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("segments:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if key, err := mediaPlaylistKey(doc, base); err != nil || key != nil {
		t.Errorf("a ranged playlist in the clear: key %+v, err %v", key, err)
	}

	for segment, want := range map[string]struct {
		target      string
		first, last int64
		ranged      bool
	}{
		"https://cdn.example.test/a.mp4#range=700-1699": {"https://cdn.example.test/a.mp4", 700, 1699, true},
		"https://cdn.example.test/a.mp4":                {"https://cdn.example.test/a.mp4", 0, 0, false},
		"https://cdn.example.test/a.mp4#range=9-3":      {"https://cdn.example.test/a.mp4#range=9-3", 0, 0, false},
	} {
		target, first, last, ranged := SegmentRange(segment)
		if target != want.target || first != want.first || last != want.last || ranged != want.ranged {
			t.Errorf("SegmentRange(%q) = %q %d-%d %v", segment, target, first, last, ranged)
		}
	}
}

// At the same height H.264 is taken over HEVC, whatever its bitrate; a
// taller HEVC rendition still wins.
func TestBestVariantPrefersH264AtTheSameHeight(t *testing.T) {
	variants := []hlsVariant{
		{URL: "hevc-1080.m3u8", Resolution: "1920x1080", Codecs: "hev1.1.6.L120.B0,mp4a.40.2", Bandwidth: 4647248},
		{URL: "avc-1080.m3u8", Resolution: "1920x1080", Codecs: "avc1.640028,mp4a.40.2", Bandwidth: 4581328},
		{URL: "avc-720.m3u8", Resolution: "1280x720", Codecs: "avc1.64001F,mp4a.40.2", Bandwidth: 2923376},
	}
	if best, _ := bestVariant(variants); best.URL != "avc-1080.m3u8" {
		t.Errorf("chose %s, want the H.264 rendition at 1080p", best.URL)
	}
	if best, _ := bestVariant([]hlsVariant{variants[0], variants[2]}); best.URL != "hevc-1080.m3u8" {
		t.Errorf("chose %s, want the taller rendition", best.URL)
	}
}

func TestBestVariantPrefersSelfContained(t *testing.T) {
	mixed := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-high",NAME="English",URI="audio/high/media.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=3063040,RESOLUTION=1920x1080,CODECS="avc1.64001F,mp4a.40.2",AUDIO="audio-high"
video/1080/media.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=921925,RESOLUTION=640x360,CODECS="avc1.64001F,mp4a.40.2"
video/360/media.m3u8
`
	base := mustParse(t, "https://cdn.example.test/master.m3u8")
	best, ok := bestVariant(parseMasterPlaylist(mixed, base))
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if best.height() != 360 {
		t.Errorf("chose the %s rendition; want the 640x360 one, which is the only playable one",
			best.Resolution)
	}
}

func TestParseAudioRenditions(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.test/stream/master.m3u8")
	groups := parseAudioRenditions(demuxedMaster, base)
	if len(groups["audio-high"]) == 0 {
		t.Error("an audio group with its own playlist was not recorded")
	}
	if len(groups["subs"]) != 0 {
		t.Error("a subtitle group was taken for an audio one")
	}
	for group, renditions := range parseAudioRenditions(muxedMaster, base) {
		for _, r := range renditions {
			if r.separate() {
				t.Errorf("%s: an audio rendition with no URI was recorded as living elsewhere: %+v", group, r)
			}
		}
	}
}

func TestMediaPlaylistKey(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.test/v/clip/index.m3u8")
	const head = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n"

	key, err := mediaPlaylistKey(head+"#EXTINF:6,\na.ts\n#EXT-X-ENDLIST\n", base)
	if err != nil || key != nil {
		t.Fatalf("clear playlist: key %+v, err %v", key, err)
	}

	key, err = mediaPlaylistKey(head+"#EXT-X-MEDIA-SEQUENCE:7\n"+
		`#EXT-X-KEY:METHOD=AES-128,URI="key.bin"`+"\n#EXTINF:6,\na.ts\n#EXTINF:6,\nb.ts\n", base)
	if err != nil || key == nil {
		t.Fatalf("one key: %+v, %v", key, err)
	}
	if key.URI != "https://cdn.example.test/v/clip/key.bin" || key.IV != nil || key.Sequence != 7 {
		t.Errorf("key = %+v, want the URI resolved, no IV, sequence 7", key)
	}

	// Over fragmented MP4, an initialisation segment named before the key is
	// in the clear, and the sequence an absent IV is counted from starts after
	// it; one named after the key is encrypted with the key's own IV.
	for _, tc := range []struct {
		name, doc string
		clear     int
	}{
		{"init in the clear", head + "#EXT-X-MEDIA-SEQUENCE:1\n" + `#EXT-X-MAP:URI="init.mp4"` + "\n" +
			`#EXT-X-KEY:METHOD=AES-128,URI="k"` + "\n#EXTINF:6,\na.m4s\n", 1},
		{"init in the clear, IV named", head + `#EXT-X-MAP:URI="init.mp4"` + "\n" +
			`#EXT-X-KEY:METHOD=AES-128,URI="k",IV=0x28CD62225B4E1B736063663FA69385D2` + "\n#EXTINF:6,\na.m4s\n", 1},
		{"init encrypted, IV named", head + `#EXT-X-KEY:METHOD=AES-128,URI="k",IV=0x000102030405060708090a0b0c0d0e0f` + "\n" +
			`#EXT-X-MAP:URI="init.mp4"` + "\n#EXTINF:6,\na.m4s\n", 0},
	} {
		key, err := mediaPlaylistKey(tc.doc, base)
		if err != nil || key == nil || key.Clear != tc.clear {
			t.Errorf("%s: key %+v, err %v; want %d segment in the clear", tc.name, key, err, tc.clear)
		}
	}

	// The same key repeated before every segment is one key.
	repeated := head + `#EXT-X-KEY:METHOD=AES-128,URI="k",IV=0x000102030405060708090a0b0c0d0e0f` +
		"\n#EXTINF:6,\na.ts\n" + `#EXT-X-KEY:METHOD=AES-128,URI="k",IV=0x000102030405060708090a0b0c0d0e0f` + "\n#EXTINF:6,\nb.ts\n"
	key, err = mediaPlaylistKey(repeated, base)
	if err != nil || len(key.IV) != 16 || key.IV[15] != 0x0f {
		t.Errorf("repeated key with IV: %+v, %v", key, err)
	}

	for name, doc := range map[string]string{
		"key changes":           head + `#EXT-X-KEY:METHOD=AES-128,URI="k1"` + "\n#EXTINF:6,\na.ts\n" + `#EXT-X-KEY:METHOD=AES-128,URI="k2"` + "\n#EXTINF:6,\nb.ts\n",
		"clear then key":        head + "#EXTINF:6,\na.ts\n" + `#EXT-X-KEY:METHOD=AES-128,URI="k"` + "\n#EXTINF:6,\nb.ts\n",
		"key then clear":        head + `#EXT-X-KEY:METHOD=AES-128,URI="k"` + "\n#EXTINF:6,\na.ts\n#EXT-X-KEY:METHOD=NONE\n#EXTINF:6,\nb.ts\n",
		"sample-aes":            head + `#EXT-X-KEY:METHOD=SAMPLE-AES,URI="k"` + "\n#EXTINF:6,\na.ts\n",
		"drm key format":        head + `#EXT-X-KEY:METHOD=AES-128,URI="skd://k",KEYFORMAT="com.apple.streamingkeydelivery"` + "\n#EXTINF:6,\na.ts\n",
		"encrypted init, no IV": head + `#EXT-X-KEY:METHOD=AES-128,URI="k"` + "\n" + `#EXT-X-MAP:URI="init.mp4"` + "\n#EXTINF:6,\na.m4s\n",
		"unreadable iv":         head + `#EXT-X-KEY:METHOD=AES-128,URI="k",IV=0x1234` + "\n#EXTINF:6,\na.ts\n",
		"key without a uri":     head + "#EXT-X-KEY:METHOD=AES-128\n#EXTINF:6,\na.ts\n",
	} {
		if _, err := mediaPlaylistKey(doc, base); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A master can carry its main audio in the variants' own segments, naming it
// with no URI, beside an alternate that has a playlist of its own. The
// alternate is fetched, and the main one listed in the group's order with no
// segments, so that the joined file labels it and keeps it first.
func TestAudioCarriedInTheVideoIsListedBesideTheTracksApart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/master.m3u8") {
			_, _ = io.WriteString(w, "#EXTM3U\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",LANGUAGE="de",NAME="Deutsch",DEFAULT=NO,URI="german.m3u8"`+"\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",LANGUAGE="en",NAME="English",DEFAULT=YES`+"\n"+
				`#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="aac"`+"\nvideo.m3u8\n")
			return
		}
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\na.ts\n#EXT-X-ENDLIST\n")
	}))
	defer srv.Close()
	client := testClient()

	media, err := resolveMediaPlaylist(context.Background(), client, srv.URL+"/master.m3u8", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(media.Audio) != 2 {
		t.Fatalf("audio = %+v; want the main track and the alternate", media.Audio)
	}
	if a := media.Audio[0]; a.Name != "English" || a.Language != "en" || len(a.Segments) != 0 {
		t.Errorf("first = %+v; want the default, carried in the video, with no segments of its own", a)
	}
	if a := media.Audio[1]; a.Name != "Deutsch" || len(a.Segments) != 1 || !strings.HasSuffix(a.Segments[0], "/a.ts") {
		t.Errorf("second = %+v; want the alternate, read from its own playlist", a)
	}

	// A group whose every rendition is in the variant is the plain muxed
	// case: nothing apart, nothing to join.
	base, _ := url.Parse(srv.URL + "/master.m3u8")
	variants := parseMasterPlaylist("#EXTM3U\n"+`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",NAME="English",DEFAULT=YES`+"\n"+
		`#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="aac"`+"\nvideo.m3u8\n", base)
	if len(variants) != 1 || variants[0].audioElsewhere || !variants[0].muxed() {
		t.Errorf("variants = %+v; want one carrying its own audio", variants)
	}
}

// A variant's subtitles come with it: each WebVTT rendition of the group it
// names, its flags kept. One wrapped in fragmented MP4, which the downloader
// does not join, and one that cannot be fetched are left out, without
// failing the video.
func TestSubtitlesComeWithTheVariantThatNamesThem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n"+
				`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",LANGUAGE="fr",NAME="Français",URI="wrapped.m3u8"`+"\n"+
				`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",LANGUAGE="de",NAME="Deutsch",URI="missing.m3u8"`+"\n"+
				`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",LANGUAGE="en",NAME="English",DEFAULT=YES,FORCED=YES,URI="english.m3u8"`+"\n"+
				`#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS="avc1.4d401f,mp4a.40.2",SUBTITLES="subs"`+"\nvideo.m3u8\n")
		case "/english.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:60\n#EXTINF:60,\nenglish.vtt\n#EXT-X-ENDLIST\n")
		case "/wrapped.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n"+`#EXT-X-MAP:URI="init.mp4"`+
				"\n#EXTINF:6,\nsub-1.m4s\n#EXT-X-ENDLIST\n")
		case "/video.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\na.ts\n#EXT-X-ENDLIST\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := testClient()

	media, err := resolveMediaPlaylist(context.Background(), client, srv.URL+"/master.m3u8", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(media.Subtitles) != 1 {
		t.Fatalf("subtitles = %+v; want the one WebVTT rendition that could be read", media.Subtitles)
	}
	if s := media.Subtitles[0]; s.Name != "English" || s.Language != "en" || !s.Default || !s.Forced ||
		len(s.Segments) != 1 || s.Segments[0] != srv.URL+"/english.vtt" {
		t.Errorf("subtitle = %+v", s)
	}
	if len(media.Audio) != 0 || media.extension() != ".ts" {
		t.Errorf("audio %+v, extension %s: subtitles alone change neither", media.Audio, media.extension())
	}
}

// The resolver hands an encrypted playlist's key over with its segments,
// which every caller carries onto its File: joined as they are, they would
// be ciphertext.
func TestEncryptedPlaylistsComeWithTheirKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n"+`#EXT-X-KEY:METHOD=AES-128,URI="key"`+
			"\n#EXTINF:6,\na.ts\n#EXT-X-ENDLIST\n")
	}))
	defer srv.Close()
	client := testClient()

	media, err := resolveMediaPlaylist(context.Background(), client, srv.URL+"/index.m3u8", nil)
	if err != nil || media.Key == nil || media.Key.URI != srv.URL+"/key" {
		t.Errorf("resolveMediaPlaylist: %+v, %v", media, err)
	}
}

// A finished VOD media playlist. The segments are relative, which is how
// hosts write them and what makes the base URL matter.
const hlsCompleteMedia = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:6.000,
seg-1.ts
#EXTINF:6.000,
seg-2.ts
#EXTINF:3.480,
seg-3.ts
#EXT-X-ENDLIST
`

// The same playlist while it is still being produced: a sliding window with
// no last segment. Downloading this saves whatever existed at the moment the
// queue reached it, under a name that claims to be the whole stream.
const hlsLiveMedia = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:2481
#EXTINF:6.000,
seg-2481.ts
#EXTINF:6.000,
seg-2482.ts
`

// An event that is still running. It says so twice — the type and the missing
// end tag — and the message is expected to name the first, because that is
// what tells the user to come back later.
const hlsEventMedia = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-PLAYLIST-TYPE:EVENT
#EXT-X-TARGETDURATION:6
#EXTINF:6.000,
seg-1.ts
`

// A master whose largest rendition is the unusable one: both variants
// advertise a video and an audio codec, but the 1080 one names an audio group
// that has a playlist of its own, so its segments carry no sound. The 720 one
// names nothing and is self-contained. Taking the biggest would be wrong.
const hlsMixedMaster = `#EXTM3U
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="stereo",NAME="English",DEFAULT=YES,LANGUAGE="en",URI="audio/media.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=5600000,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",AUDIO="stereo"
video/1080/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1900000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2"
video/720/index.m3u8
`

// A master offering nothing self-contained: every variant leaves its audio in
// the group. Joining any one of them yields a silent file.
const hlsDemuxedOnlyMaster = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="stereo",NAME="English",DEFAULT=YES,LANGUAGE="en",URI="audio/media.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=5600000,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",AUDIO="stereo"
video/1080/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1900000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2",AUDIO="stereo"
video/720/index.m3u8
`

// hlsServer serves a fixed set of documents by path, so a master and the
// variant it points at can be laid out the way a host lays them out. The
// count is the number of documents it served, which is what the sniff guard
// is measured with: other recognisers may ask the origin about a page, which
// is not this extractor's cost.
func hlsServer(t *testing.T, contentType string, pages map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	srv, hits := fakeSite(t, sitePages(contentType, pages))
	return srv, &hits.served
}

const hlsManifestContentType = "application/vnd.apple.mpegurl"

// TestHLSDirectShapesManifestsByPath pins the forms the extractor claims. The
// query is where these URLs carry their signature, so it must not take part
// in the decision; DASH is claimed only so that Extract can refuse it with a
// reason instead of the fallback saving it as a small XML file.
func TestHLSDirectShapesManifestsByPath(t *testing.T) {
	tests := map[string]bool{
		"https://cdn.example.test/hls/a-clip/master.m3u8":             true,
		"https://cdn.example.test/hls/a-clip/master.m3u8?t=1&sig=abc": true,
		"https://cdn.example.test/hls/a-clip/MASTER.M3U8":             true,
		"https://cdn.example.test/dash/a-clip/manifest.mpd":           true,
		"https://cdn.example.test/hls/a-clip/video.mp4":               false,
		"https://cdn.example.test/hls/a-clip/":                        false,
		"https://cdn.example.test/watch?src=video.m3u8":               false,
	}
	h := NewHLSDirect(nil)
	for raw, want := range tests {
		expectShape(t, h, raw, want)
	}
}

// TestHLSDirectRecognise: a playlist by its type or its first line, at any
// URL; anything else only by a manifest's extension.
func TestHLSDirectRecognise(t *testing.T) {
	h := NewHLSDirect(nil)
	expectRecognised(t, h, fixturePage(t, "https://cdn.example.test/stream/a-clip", http.StatusOK, "text/plain", hlsCompleteMedia), true)
	expectRecognised(t, h, fixturePage(t, "https://cdn.example.test/stream/a-clip", http.StatusOK, hlsManifestContentType, ""), true)
	expectRecognised(t, h, fixturePage(t, "https://cdn.example.test/hls/master.m3u8", http.StatusForbidden, "text/html", "denied"), true)
	expectRecognised(t, h, htmlPage(t, "https://cdn.example.test/watch/a-clip", "<html><body>#EXTM3U</body></html>"), false)
	expectRecognised(t, h, fixturePage(t, "https://cdn.example.test/notes.txt", http.StatusOK, "text/plain", "notes\n#EXTM3U"), false)
}

// TestHLSDirectRefusesDASH covers the one thing that must never be joined.
// The nil client is part of the test: the refusal is reached before anything
// is fetched, so a DASH URL costs no request at all.
func TestHLSDirectRefusesDASH(t *testing.T) {
	u := mustParse(t, "https://cdn.example.test/dash/a-clip/manifest.mpd")
	_, err := NewHLSDirect(nil).Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a DASH manifest was accepted; joining one would give a silent file")
	}
	for _, want := range []string{"DASH", "silent", "yt-dlp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestHLSLiveEdge(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		live bool
		why  string
	}{
		{"a finished VOD is complete", hlsCompleteMedia, false, ""},
		{"a sliding window has no last segment", hlsLiveMedia, true, "#EXT-X-ENDLIST"},
		{"an ongoing event says which it is", hlsEventMedia, true, "EVENT"},
		{
			// An event that has ended carries the end tag like anything
			// else, and refusing it on the type alone would turn away a
			// recording that is complete and downloadable.
			name: "a finished event is complete",
			doc:  hlsEventMedia + "#EXT-X-ENDLIST\n",
			live: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			live, why := hlsLiveEdge(tc.doc)
			if live != tc.live {
				t.Fatalf("live = %v, want %v (%q)", live, tc.live, why)
			}
			if tc.why != "" && !strings.Contains(why, tc.why) {
				t.Errorf("the reason %q does not mention %q", why, tc.why)
			}
		})
	}
}

// TestHLSDirectEmitsOneSegmentedFile is the whole point of the extractor: a
// manifest becomes one file with an ordered part list instead of a few
// kilobytes of text. It also pins the choice of rendition end to end — the
// master's largest variant is the one whose audio lives elsewhere.
func TestHLSDirectEmitsOneSegmentedFile(t *testing.T) {
	srv, _ := hlsServer(t, hlsManifestContentType, map[string]string{
		"/hls/a-clip/master.m3u8":          hlsMixedMaster,
		"/hls/a-clip/video/720/index.m3u8": hlsCompleteMedia,
	})

	res, err := hlsExtractFrom(t, srv.URL+"/hls/a-clip/master.m3u8")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want one joined from the segments", len(res.Files))
	}
	file := res.Files[0]

	if res.Title != "a-clip" || file.Name != "a-clip.ts" {
		t.Errorf("named %q / %q, want the path segment that says what this is", res.Title, file.Name)
	}
	if file.URL != "" {
		t.Errorf("URL = %q, want nothing: a playlist has no single resource to fetch", file.URL)
	}
	if file.Size != -1 {
		t.Errorf("Size = %d, want -1: the length is not known until every part is in", file.Size)
	}
	if file.Headers[httpx.HeaderReferer] != srv.URL+"/" {
		t.Errorf("Referer = %q, want the manifest's own origin", file.Headers[httpx.HeaderReferer])
	}

	want := []string{
		srv.URL + "/hls/a-clip/video/720/seg-1.ts",
		srv.URL + "/hls/a-clip/video/720/seg-2.ts",
		srv.URL + "/hls/a-clip/video/720/seg-3.ts",
	}
	if len(file.Segments) != len(want) {
		t.Fatalf("got %d segments, want %d: %v", len(file.Segments), len(want), file.Segments)
	}
	for i, seg := range want {
		if file.Segments[i] != seg {
			t.Errorf("segment %d = %q, want %q", i, file.Segments[i], seg)
		}
	}
}

// TestHLSDirectAcceptsABareMediaPlaylist is the guard on the master-versus-
// media test. A media playlist declares no codecs, so it can never look
// self-contained; a demuxed check applied to it would refuse every playlist
// pasted straight in, which is the commonest thing a user does.
func TestHLSDirectAcceptsABareMediaPlaylist(t *testing.T) {
	srv, _ := hlsServer(t, hlsManifestContentType, map[string]string{
		"/stream/a-clip/index.m3u8": hlsCompleteMedia,
	})

	res, err := hlsExtractFrom(t, srv.URL+"/stream/a-clip/index.m3u8")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 1 || len(res.Files[0].Segments) != 3 {
		t.Fatalf("got %+v, want one file of three segments", res.Files)
	}
	if res.Files[0].Name != "a-clip.ts" {
		t.Errorf("Name = %q, want a-clip.ts", res.Files[0].Name)
	}
}

// A master whose every variant keeps its audio in a group of its own comes
// with that group's renditions as tracks, for the downloader to join to the
// video: it used to be refused, and sent to the external downloader.
func TestHLSDirectCarriesAudioKeptApart(t *testing.T) {
	srv, _ := hlsServer(t, hlsManifestContentType, map[string]string{
		"/hls/a-clip/master.m3u8":           hlsDemuxedOnlyMaster,
		"/hls/a-clip/video/1080/index.m3u8": hlsCompleteMedia,
		"/hls/a-clip/audio/media.m3u8":      hlsCompleteMedia,
	})

	res, err := hlsExtractFrom(t, srv.URL+"/hls/a-clip/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	f := res.Files[0]
	if len(f.Segments) == 0 || !strings.HasSuffix(f.Name, ".mp4") {
		t.Fatalf("file = %+v; want the video's segments, named for the MP4 they will be joined into", f)
	}
	if len(f.Audio) != 1 || f.Audio[0].Language != "en" || f.Audio[0].Name != "English" || len(f.Audio[0].Segments) == 0 {
		t.Fatalf("audio = %+v; want the English rendition as a track of its own", f.Audio)
	}
}

func TestHLSDirectRefusesAPlaylistStillBeingWritten(t *testing.T) {
	srv, _ := hlsServer(t, hlsManifestContentType, map[string]string{
		"/live/a-stream/index.m3u8": hlsLiveMedia,
	})

	_, err := hlsExtractFrom(t, srv.URL+"/live/a-stream/index.m3u8")
	if err == nil {
		t.Fatal("a live playlist was accepted; what lands on disk is a fragment named as the whole")
	}
	if !strings.Contains(err.Error(), "#EXT-X-ENDLIST") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// TestHLSSniffRecognisesAManifestServedAsText covers the case the extension
// cannot: a host that serves a playlist from a path naming no file, and
// labels it the way object storage labels everything.
func TestHLSSniffRecognisesAManifestServedAsText(t *testing.T) {
	srv, seen := hlsServer(t, "text/plain; charset=utf-8", map[string]string{
		"/stream/a-clip": hlsCompleteMedia,
	})

	res, err := hlsSniffFrom(t, srv.URL+"/stream/a-clip")
	if err != nil {
		t.Fatalf("hlsSniff: %v", err)
	}
	if res == nil {
		t.Fatal("a playlist opening with #EXTM3U was not recognised")
	}
	if len(res.Files) != 1 || len(res.Files[0].Segments) != 3 {
		t.Fatalf("got %+v, want one file of three segments", res.Files)
	}
	// One request settles what it is, and the resolution reads it properly
	// afterwards — the sniff only ever sees the opening bytes. The live
	// check reads the same document the resolver fetched, not a fresh copy.
	if got := seen.Load(); got != 2 {
		t.Errorf("the host saw %d requests, want the sniff plus one read of the playlist", got)
	}
}

// TestHLSSniffFallsThroughOnAnythingElse is the kvsSniff bargain: a wrong
// guess costs one small request and the caller carries on as though it had
// never happened.
func TestHLSSniffFallsThroughOnAnythingElse(t *testing.T) {
	srv, _ := hlsServer(t, "text/html; charset=utf-8", map[string]string{
		"/watch/a-clip": "<html><head><title>A clip</title></head><body></body></html>",
	})

	res, err := hlsSniffFrom(t, srv.URL+"/watch/a-clip")
	if err != nil {
		t.Fatalf("a page that is not a manifest reported an error: %v", err)
	}
	if res != nil {
		t.Errorf("an HTML page was taken for a playlist: %+v", res)
	}
}

// TestHLSSniffReportsAManifestItCannotDownload is the one place this departs
// from kvsSniff. Once the response has said it is a playlist, falling through
// would hand it back to the fallback and save the text as a file — the very
// failure the extractor exists to stop — so the reason surfaces instead.
func TestHLSSniffReportsAManifestItCannotDownload(t *testing.T) {
	srv, _ := hlsServer(t, hlsManifestContentType, map[string]string{
		"/live/a-stream": hlsLiveMedia,
	})

	res, err := hlsSniffFrom(t, srv.URL+"/live/a-stream")
	if err == nil {
		t.Fatal("a live playlist fell through to the fallback, which would save the manifest itself")
	}
	if res != nil {
		t.Errorf("a result was returned alongside the error: %+v", res)
	}
}

// TestHLSSniffLeavesNamedFilesAlone pins the guard rather than its outcome.
// Some hosts sign a link for a single use, so a sniff of every direct
// download would spend the signature before the transfer got to it.
func TestHLSSniffLeavesNamedFilesAlone(t *testing.T) {
	srv, seen := hlsServer(t, hlsManifestContentType, map[string]string{
		"/media/a-clip.bin": hlsCompleteMedia,
	})

	res, err := hlsSniffFrom(t, srv.URL+"/media/a-clip.bin")
	if res != nil || err != nil {
		t.Fatalf("a URL naming a file was sniffed: %+v, %v", res, err)
	}
	if got := seen.Load(); got != 0 {
		t.Errorf("the host saw %d requests, want none", got)
	}
}

func TestHLSManifestType(t *testing.T) {
	tests := map[string]bool{
		"application/vnd.apple.mpegurl":                true,
		"application/x-mpegURL":                        true,
		"Application/Vnd.Apple.Mpegurl; charset=UTF-8": true,
		"audio/x-mpegurl":                              true,
		"text/plain; charset=utf-8":                    false,
		"video/mp4":                                    false,
		"":                                             false,
	}
	for value, want := range tests {
		if got := hlsManifestType(value); got != want {
			t.Errorf("hlsManifestType(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestHLSManifestBody(t *testing.T) {
	tests := map[string]bool{
		hlsCompleteMedia:                  true,
		"\ufeff#EXTM3U\n#EXT-X-VERSION:3": true, // an origin that re-encoded the file
		"\n  #EXTM3U\n":                   true,
		"<html><body>Not found":           false,
		"#EXTINF:6.000,\nseg-1.ts\n":      false, // a fragment, not a document
		"":                                false,
	}
	for body, want := range tests {
		if got := hlsManifestBody(body); got != want {
			t.Errorf("hlsManifestBody(%.20q) = %v, want %v", body, got, want)
		}
	}
}

// TestHLSName covers the naming trap: a manifest is almost always called
// after its role, so the basename alone would file most of these downloads
// as index.ts.
func TestHLSName(t *testing.T) {
	tests := map[string]string{
		"https://cdn.example.test/hls/a-clip/index.m3u8":      "a-clip",
		"https://cdn.example.test/media/47110815/master.m3u8": "47110815",
		"https://cdn.example.test/a-clip.m3u8":                "a-clip",
		"https://cdn.example.test/a%20clip/playlist.m3u8":     "a clip",
		"https://cdn.example.test/hls/index.m3u8":             "cdn.example.test",
	}
	for raw, want := range tests {
		u := mustParse(t, raw)
		if got := hlsName(u); got != want {
			t.Errorf("hlsName(%s) = %q, want %q", raw, got, want)
		}
	}
}

// hlsExtractFrom runs the extractor against a served manifest.
func hlsExtractFrom(t *testing.T, raw string) (*Result, error) {
	t.Helper()
	u := mustParse(t, raw)
	return NewHLSDirect(testClient()).Extract(context.Background(), u, Options{})
}

// hlsSniffFrom submits a URL to the registry and reports what the manifest
// extractor made of it: nil, nil when the page went elsewhere.
func hlsSniffFrom(t *testing.T, raw string) (*Result, error) {
	t.Helper()
	res, ex, err := NewRegistry(&config.Config{}, testClient()).Extract(context.Background(), raw, Options{})
	if ex == nil || ex.Name() != "hls" {
		return nil, nil
	}
	return res, err
}

func TestHLSRefusesUnsupportedPayloadsBeforeDownloading(t *testing.T) {
	for _, tag := range []string{
		`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="key.bin"`,
		`#EXT-X-KEY:METHOD=AES-128,URI="skd://key",KEYFORMAT="com.apple.streamingkeydelivery"`,
	} {
		t.Run(tag, func(t *testing.T) {
			server, _ := hlsServer(t, "application/vnd.apple.mpegurl", map[string]string{
				"/media.m3u8": "#EXTM3U\n" + tag + "\n#EXTINF:6,\npart.ts\n#EXT-X-ENDLIST\n",
			})
			_, err := extract(t, NewHLSDirect(testClient()), server.URL+"/media.m3u8", Options{})
			if err == nil {
				t.Fatal("unsupported playlist was accepted")
			}
		})
	}
}

// Byte ranges of one file are fetched as the parts they are, each carrying
// its range for the downloader to ask for.
func TestHLSCarriesByteRangesToTheDownloader(t *testing.T) {
	server, _ := hlsServer(t, "application/vnd.apple.mpegurl", map[string]string{
		"/media.m3u8": "#EXTM3U\n" + `#EXT-X-MAP:URI="clip.mp4",BYTERANGE="10@0"` +
			"\n#EXTINF:6,\n#EXT-X-BYTERANGE:20@10\nclip.mp4\n#EXT-X-ENDLIST\n",
	})
	res := extractOK(t, NewHLSDirect(testClient()), server.URL+"/media.m3u8", Options{})
	want := []string{server.URL + "/clip.mp4#range=0-9", server.URL + "/clip.mp4#range=10-29"}
	if got := res.Files[0].Segments; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("segments %v, want %v", got, want)
	}
}

// AES-128 is decrypted rather than refused: the key travels with the file,
// and the downloader decrypts each segment as it lands.
func TestHLSCarriesAnAES128KeyToTheDownloader(t *testing.T) {
	server, _ := hlsServer(t, "application/vnd.apple.mpegurl", map[string]string{
		"/media.m3u8": "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:3\n" + `#EXT-X-KEY:METHOD=AES-128,URI="key.bin"` +
			"\n#EXTINF:6,\npart.ts\n#EXT-X-ENDLIST\n",
	})
	res := extractOK(t, NewHLSDirect(testClient()), server.URL+"/media.m3u8", Options{})
	key := res.Files[0].SegmentKey
	if key == nil || key.URI != server.URL+"/key.bin" || key.Sequence != 3 {
		t.Errorf("key = %+v, want the playlist's own, resolved against it", key)
	}
}

func TestHLSResolvesReferencesAgainstRedirectDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start.m3u8":
			http.Redirect(w, r, "/nested/master.m3u8", http.StatusFound)
		case "/nested/master.m3u8":
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100\nmedia.m3u8\n")
		case "/nested/media.m3u8":
			http.Redirect(w, r, "/final/media.m3u8", http.StatusFound)
		case "/final/media.m3u8":
			_, _ = io.WriteString(w, hlsCompleteMedia)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	media, err := resolveMediaPlaylist(context.Background(), testClient(), server.URL+"/start.m3u8", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := media.Segments[0]; got != server.URL+"/final/seg-1.ts" {
		t.Fatalf("wrong segment base: %s", got)
	}
}

func TestHLSRejectsHTMLAndInvalidHeaders(t *testing.T) {
	for _, body := range []string{"<html>not a playlist</html>", "#EXTM3UX\npart.ts\n#EXT-X-ENDLIST"} {
		server, _ := hlsServer(t, "text/plain", map[string]string{"/media.m3u8": body})
		if _, err := resolveMediaPlaylist(context.Background(), testClient(), server.URL+"/media.m3u8", nil); err == nil {
			t.Errorf("accepted non-playlist %q", body)
		}
	}
}

// TestHLSDirectSniffsAnUnreadTypeAtAnExtensionlessPath: a manifest handed
// out from a path naming no file, under a type the probe does not read
// (object storage's octet-stream, or a segment type), is asked for its
// opening bytes, with the origin as referer, and joined rather than saved as
// a few kilobytes of text. Real binary content at such a path is still a
// file, and a link that names a file is not sniffed.
func TestHLSDirectSniffsAnUnreadTypeAtAnExtensionlessPath(t *testing.T) {
	for _, kind := range []string{"application/octet-stream", "binary/octet-stream", "video/mp2t"} {
		t.Run(kind, func(t *testing.T) {
			var referers []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/stream/a-clip":
					referers = append(referers, r.Header.Get(httpx.HeaderReferer))
					w.Header().Set(httpx.HeaderContentType, kind)
					_, _ = io.WriteString(w, hlsCompleteMedia)
				case "/stream/blob", "/stream/a-clip.bin":
					w.Header().Set(httpx.HeaderContentType, kind)
					_, _ = w.Write([]byte{0x47, 0x40, 0x00, 0x10, 0x00})
				default:
					w.Header().Set(httpx.HeaderContentType, "video/mp2t")
					_, _ = w.Write([]byte{0x47})
				}
			}))
			t.Cleanup(srv.Close)
			res, ex, err := probeRegistry().Extract(context.Background(), srv.URL+"/stream/a-clip", Options{})
			if err != nil || ex.Name() != "hls" || len(res.Files) != 1 || len(res.Files[0].Segments) != 3 {
				t.Fatalf("got %+v from %v: %v, want the joined playlist", res, ex, err)
			}
			if !slices.Contains(referers, srv.URL+"/") {
				t.Errorf("referers %q, want the sniff to send the origin", referers)
			}
			for _, other := range []string{"/stream/blob", "/stream/a-clip.bin"} {
				res, ex, err = probeRegistry().Extract(context.Background(), srv.URL+other, Options{})
				if err != nil || ex.Name() == "hls" || len(res.Files) != 1 {
					t.Errorf("%s: got %+v from %v: %v, want it left a file", other, res, ex, err)
				}
			}
		})
	}
}
