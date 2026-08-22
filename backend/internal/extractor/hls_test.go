package extractor

import "testing"

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
