// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
