package extractor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

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
	base, err := ParseURL("https://cdn.example.test/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}

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
	base, err := ParseURL("https://cdn.example.test/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}

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
func TestBestVariantPrefersSelfContained(t *testing.T) {
	mixed := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-high",NAME="English",URI="audio/high/media.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=3063040,RESOLUTION=1920x1080,CODECS="avc1.64001F,mp4a.40.2",AUDIO="audio-high"
video/1080/media.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=921925,RESOLUTION=640x360,CODECS="avc1.64001F,mp4a.40.2"
video/360/media.m3u8
`
	base, err := ParseURL("https://cdn.example.test/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	best, ok := bestVariant(parseMasterPlaylist(mixed, base))
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if best.height() != 360 {
		t.Errorf("chose the %s rendition; want the 640x360 one, which is the only playable one",
			best.Resolution)
	}
}

func TestParseAudioGroups(t *testing.T) {
	groups := parseAudioGroups(demuxedMaster)
	if !groups["audio-high"] {
		t.Error("an audio group with its own playlist was not recorded")
	}
	if groups["subs"] {
		t.Error("a subtitle group was taken for an audio one")
	}
	if len(parseAudioGroups(muxedMaster)) != 0 {
		t.Error("an audio rendition with no URI was recorded as living elsewhere")
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

	// The same key repeated before every segment is one key.
	repeated := head + `#EXT-X-KEY:METHOD=AES-128,URI="k",IV=0x000102030405060708090a0b0c0d0e0f` +
		"\n#EXTINF:6,\na.ts\n" + `#EXT-X-KEY:METHOD=AES-128,URI="k",IV=0x000102030405060708090a0b0c0d0e0f` + "\n#EXTINF:6,\nb.ts\n"
	key, err = mediaPlaylistKey(repeated, base)
	if err != nil || len(key.IV) != 16 || key.IV[15] != 0x0f {
		t.Errorf("repeated key with IV: %+v, %v", key, err)
	}

	for name, doc := range map[string]string{
		"key changes":        head + `#EXT-X-KEY:METHOD=AES-128,URI="k1"` + "\n#EXTINF:6,\na.ts\n" + `#EXT-X-KEY:METHOD=AES-128,URI="k2"` + "\n#EXTINF:6,\nb.ts\n",
		"clear then key":     head + "#EXTINF:6,\na.ts\n" + `#EXT-X-KEY:METHOD=AES-128,URI="k"` + "\n#EXTINF:6,\nb.ts\n",
		"key then clear":     head + `#EXT-X-KEY:METHOD=AES-128,URI="k"` + "\n#EXTINF:6,\na.ts\n#EXT-X-KEY:METHOD=NONE\n#EXTINF:6,\nb.ts\n",
		"sample-aes":         head + `#EXT-X-KEY:METHOD=SAMPLE-AES,URI="k"` + "\n#EXTINF:6,\na.ts\n",
		"drm key format":     head + `#EXT-X-KEY:METHOD=AES-128,URI="skd://k",KEYFORMAT="com.apple.streamingkeydelivery"` + "\n#EXTINF:6,\na.ts\n",
		"encrypted fmp4":     head + `#EXT-X-KEY:METHOD=AES-128,URI="k"` + "\n" + `#EXT-X-MAP:URI="init.mp4"` + "\n#EXTINF:6,\na.m4s\n",
		"unreadable iv":      head + `#EXT-X-KEY:METHOD=AES-128,URI="k",IV=0x1234` + "\n#EXTINF:6,\na.ts\n",
		"key without a uri":  head + "#EXT-X-KEY:METHOD=AES-128\n#EXTINF:6,\na.ts\n",
		"byte ranges (kept)": head + "#EXT-X-BYTERANGE:100@0\n#EXTINF:6,\na.ts\n",
	} {
		if _, err := mediaPlaylistKey(doc, base); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A caller that hands back segments alone has nowhere to carry a key, so an
// encrypted playlist is refused to it rather than joined as ciphertext —
// while the full resolver hands the key over.
func TestEncryptedPlaylistsReachOnlyCallersThatCarryTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n"+`#EXT-X-KEY:METHOD=AES-128,URI="key"`+
			"\n#EXTINF:6,\na.ts\n#EXT-X-ENDLIST\n")
	}))
	defer srv.Close()
	client := httpx.New("test-agent", "en-US", 0, 5*time.Second)

	if _, _, err := resolvePlaylist(context.Background(), client, srv.URL+"/index.m3u8", nil); !errors.Is(err, errEncryptedPlaylist) {
		t.Errorf("resolvePlaylist: %v, want the encrypted refusal", err)
	}
	media, err := resolveMediaPlaylist(context.Background(), client, srv.URL+"/index.m3u8", nil)
	if err != nil || media.Key == nil || media.Key.URI != srv.URL+"/key" {
		t.Errorf("resolveMediaPlaylist: %+v, %v", media, err)
	}
}
