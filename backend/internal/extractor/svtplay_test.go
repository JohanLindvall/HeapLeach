package extractor

import "testing"

// TestSVTBestManifest pins the rendition choice: plain MPEG-TS HLS first,
// because its segments carry audio and video together and join into a
// playable file; the CMAF flavours next; and any HLS at all before nothing.
func TestSVTBestManifest(t *testing.T) {
	ref := func(format, url string) struct {
		Format string `json:"format"`
		URL    string `json:"url"`
	} {
		return struct {
			Format string `json:"format"`
			URL    string `json:"url"`
		}{Format: format, URL: url}
	}

	video := svtVideo{}
	video.VideoReferences = append(video.VideoReferences,
		ref("dash", "https://cdn.example.test/dash.mpd"),
		ref("hls-cmaf-full", "https://cdn.example.test/cmaf.m3u8"),
		ref("hls-ts-full", "https://cdn.example.test/ts.m3u8"),
	)
	if got := video.bestManifest(); got != "https://cdn.example.test/ts.m3u8" {
		t.Errorf("best = %q, want the plain MPEG-TS flavour", got)
	}

	// Without a TS flavour the CMAF one is the best on offer.
	video.VideoReferences = video.VideoReferences[:2]
	if got := video.bestManifest(); got != "https://cdn.example.test/cmaf.m3u8" {
		t.Errorf("best = %q, want the CMAF flavour", got)
	}

	// A flavour nobody here has named still beats returning nothing.
	video.VideoReferences = video.VideoReferences[:0]
	video.VideoReferences = append(video.VideoReferences,
		ref("hls-something-new", "https://cdn.example.test/new.m3u8"))
	if got := video.bestManifest(); got != "https://cdn.example.test/new.m3u8" {
		t.Errorf("best = %q, want the unnamed HLS flavour taken as a fallback", got)
	}

	// DASH alone is nothing this can join.
	video.VideoReferences = video.VideoReferences[:0]
	video.VideoReferences = append(video.VideoReferences,
		ref("dash", "https://cdn.example.test/dash.mpd"))
	if got := video.bestManifest(); got != "" {
		t.Errorf("best = %q, want nothing for DASH alone", got)
	}
}

func TestSVTVideoLinkPattern(t *testing.T) {
	doc := `<a href="/video/abc123DEF/a-slug">one</a>` +
		`<a href="/video/xy/short">too short</a>` +
		`"canonical":"/video/qrs789tuv"`
	var ids []string
	for _, m := range svtVideoLink.FindAllStringSubmatch(doc, -1) {
		ids = append(ids, m[1])
	}
	if len(ids) != 2 || ids[0] != "abc123DEF" || ids[1] != "qrs789tuv" {
		t.Errorf("ids = %v, want the two real ids and not the two-character segment", ids)
	}
}
