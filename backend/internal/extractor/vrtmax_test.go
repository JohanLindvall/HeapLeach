package extractor

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// vrtMasterPlaylist is VRT's master in the shape its packager writes one, and
// it is the fixture the whole extractor rests on. Three details are
// deliberate.
//
// The AUDIO group carries no URI, which is HLS's way of saying the audio is
// already inside each variant's segments — the reason these can be joined by
// concatenation at all. The SUBTITLES group beside it *does* carry one, which
// is the trap: a rule that looked for any EXT-X-MEDIA with a URI would
// conclude the variants were video only and refuse a host that works. And the
// I-frame entries at the bottom carry a URI too, in a tag that is not a
// variant and must not be walked as one.
const vrtMasterPlaylist = `#EXTM3U
#EXT-X-VERSION:4
## Created with Unified Streaming Platform (version=1.11.14-26090)

# AUDIO groups
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-aacl-96",NAME="audio",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"

# SUBTITLES groups
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="textstream",LANGUAGE="nl",NAME="Nederlands (CC)",DEFAULT=NO,AUTOSELECT=YES,URI="https://cdn.example.test/vod/show.ism/show-textstream_dut=1000.m3u8"

# variants
#EXT-X-STREAM-INF:BANDWIDTH=684000,CODECS="mp4a.40.2,avc1.4D401F",RESOLUTION=480x270,FRAME-RATE=25,AUDIO="audio-aacl-96",SUBTITLES="textstream",CLOSED-CAPTIONS=NONE
https://cdn.example.test/vod/show.ism/show-audio=96000-video=548000.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2428000,CODECS="mp4a.40.2,avc1.4D401F",RESOLUTION=960x540,FRAME-RATE=25,AUDIO="audio-aacl-96",SUBTITLES="textstream",CLOSED-CAPTIONS=NONE
https://cdn.example.test/vod/show.ism/show-audio=96000-video=2193000.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5405000,CODECS="mp4a.40.2,avc1.64002A",RESOLUTION=1920x1080,FRAME-RATE=50,AUDIO="audio-aacl-96",SUBTITLES="textstream",CLOSED-CAPTIONS=NONE
https://cdn.example.test/vod/show.ism/show-audio=96000-video=5002000.m3u8

# keyframes
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=663000,CODECS="avc1.64002A",RESOLUTION=1920x1080,URI="https://cdn.example.test/vod/show.ism/keyframes/show-video=5002000.m3u8"
`

// vrtDemuxedPlaylist is the same presentation packaged the other way, with
// the audio moved into a rendition of its own. Nothing here serves it today;
// it is the fixture for the day something does, because the failure it causes
// is silent in both senses — the download finishes, and the file has no
// sound.
const vrtDemuxedPlaylist = `#EXTM3U
#EXT-X-VERSION:4
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-aacl-96",NAME="audio",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2",URI="https://cdn.example.test/vod/show.ism/show-audio=96000.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=5405000,CODECS="mp4a.40.2,avc1.64002A",RESOLUTION=1920x1080,FRAME-RATE=50,AUDIO="audio-aacl-96",CLOSED-CAPTIONS=NONE
https://cdn.example.test/vod/show.ism/show-video=5002000.m3u8
`

// vrtProgramDocument is a programme page with every awkwardness a real one
// has, and none of them is incidental.
//
// The tab holding the episodes is called "Alle seizoenen" rather than
// "Afleveringen": VRT labels it differently from one programme to the next,
// so anything matching on that word finds the episodes on some programmes and
// not on others. Only the season the site itself displays arrives with its
// tiles; the one below it is a LazyTileList naming a list to fetch. The
// season titles carry a count that moves whenever an episode is published or
// expires. The tab above the seasons repeats an episode the season already
// listed. The podcast tab holds tiles that are not episodes, and the last tab
// holds no list at all.
const vrtProgramDocument = `{
  "__typename": "ProgramPage",
  "title": "A Programme",
  "components": [
    {
      "__typename": "ContainerNavigation",
      "items": [
        {
          "title": "Vandaag in A Programme",
          "components": [
            { "__typename": "PaginatedTileList", "items": [
              { "__typename": "EpisodeTile", "title": "The newest one", "action": { "link": "/vrtmax/a-z/a-programme/2026/ep-260821/" } }
            ] }
          ]
        },
        {
          "title": "Alle seizoenen",
          "components": [
            {
              "__typename": "ContainerNavigation",
              "items": [
                {
                  "title": "Seizoen 2026 (2 van 262 afl.)",
                  "components": [
                    { "__typename": "PaginatedTileList", "items": [
                      { "__typename": "EpisodeTile", "title": "The newest one", "action": { "link": "/vrtmax/a-z/a-programme/2026/ep-260821/" } },
                      { "__typename": "EpisodeTile", "title": "The one before", "action": { "link": "/vrtmax/a-z/a-programme/2026/ep-260820/" } }
                    ] }
                  ]
                },
                {
                  "title": "Seizoen 2025 (1 van 262 afl.)",
                  "components": [
                    { "__typename": "LazyTileList", "listId": "$c2Vhc29uLTIwMjU=" }
                  ]
                }
              ]
            }
          ]
        },
        {
          "title": "Podcast: A Programme",
          "components": [
            { "__typename": "PaginatedTileList", "items": [
              { "__typename": "PodcastProgramTile", "title": "The podcast", "action": { "link": "/vrtmax/podcasts/a-programme/" } }
            ] }
          ]
        },
        {
          "title": "Bloopers",
          "components": [
            { "__typename": "PaginatedTileList", "items": [
              { "__typename": "EpisodeTile", "title": "Outtakes", "action": { "link": "/vrtmax/a-z/a-programme/bloopers/ep-b1/" } }
            ] }
          ]
        },
        {
          "title": "Meer info",
          "components": [
            { "__typename": "Text" },
            { "__typename": "TagsList" }
          ]
        }
      ]
    }
  ]
}`

// vrtLazySeasonDocument is what the list query answers with for the season
// the programme page named but did not inline.
const vrtLazySeasonDocument = `{
  "data": {
    "list": {
      "__typename": "PaginatedTileList",
      "items": [
        { "__typename": "EpisodeTile", "title": "Last year's finale", "action": { "link": "/vrtmax/a-z/a-programme/2025/ep-251231/" } }
      ]
    }
  }
}`

// vrtEpisodeDocument is an episode page. The first playback mode names no
// stream, which is the sort of placeholder a player carries for a route it
// does not offer, and taking modes[0] blindly would resolve nothing.
const vrtEpisodeDocument = `{
  "__typename": "EpisodePage",
  "title": "The newest one",
  "player": {
    "title": "The newest one",
    "modes": [
      { "streamId": "" },
      { "streamId": "pbs-pub-00000000-0000-0000-0000-000000000000$vid-11111111-1111-1111-1111-111111111111" }
    ]
  }
}`

// vrtMediaDocument is a playable media document. The DASH locator sits beside
// the HLS one and would be taken by anything that merely looks for a URL, and
// the HLS locator carries a query the packager needs, which is why it is
// passed on whole rather than rebuilt.
const vrtMediaDocument = `{
  "duration": 414240,
  "skinType": "vod",
  "title": "A Programme S2026 E167",
  "drm": null,
  "drmExpired": null,
  "videoRobustness": null,
  "targetUrls": [
    { "type": "mpeg_dash", "url": "https://cdn.example.test/vod/show.ism/.mpd?filter=x" },
    { "type": "hls", "url": "https://cdn.example.test/vod/show.ism/.m3u8?filter=%28%21%28type%3D%3D%22audio%22%26%26FourCC%21%3D%22AACL%22%29%29" }
  ]
}`

// TestVRTMaxMasterPlaylistIsSelfContained pins the claim this host is built
// on: VRT's variants carry their own audio, so concatenating one yields
// something that plays.
func TestVRTMaxMasterPlaylistIsSelfContained(t *testing.T) {
	base, err := ParseURL("https://cdn.example.test/vod/show.ism/.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(vrtMasterPlaylist, base)
	if len(variants) != 3 {
		t.Fatalf("parsed %d variants, want the 3 stream entries and neither the "+
			"subtitle rendition nor the I-frame track", len(variants))
	}
	for _, v := range variants {
		if !v.muxed() {
			t.Errorf("%s reads as video only, but its audio group declares no URI of its own", v.Resolution)
		}
		if vrtSilent(v) {
			t.Errorf("%s would be refused as silent", v.Resolution)
		}
	}

	best, ok := bestVariant(variants)
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if best.Resolution != "1920x1080" {
		t.Errorf("chose %s, want the largest rendition", best.Resolution)
	}
	// Unified Streaming names the muxed form "audio=...-video=..." and the
	// parts behind it are MPEG-TS.
	if got := playlistExtension(best); got != ".ts" {
		t.Errorf("extension %q, want .ts", got)
	}
}

// TestVRTMaxRefusesADemuxedPlaylist is the guard for the day VRT repackages.
// Joining a variant whose audio lives elsewhere produces a file that finishes
// and then plays without sound, which nothing downstream can notice, so the
// refusal has to happen here.
func TestVRTMaxRefusesADemuxedPlaylist(t *testing.T) {
	base, err := ParseURL("https://cdn.example.test/vod/show.ism/.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(vrtDemuxedPlaylist, base)
	if len(variants) != 1 {
		t.Fatalf("parsed %d variants, want 1", len(variants))
	}
	if !vrtSilent(variants[0]) {
		t.Error("a variant whose audio group has a URI of its own was accepted; " +
			"downloading it would give a silent video that looks finished")
	}

	// A media playlist reaches the same call with nothing declared, and that
	// is not a refusal: it says nothing either way.
	if vrtSilent(hlsVariant{URL: "https://cdn.example.test/vod/show.ism/index.m3u8"}) {
		t.Error("a playlist that declared no codecs was refused as silent")
	}
}

// TestVRTMaxListsWalkEveryTab covers the programme walk. The tab titles are
// Dutch prose that differs from one programme to the next, so the shape of
// the tree is what decides, and seasons have to come out before the extras.
func TestVRTMaxListsWalkEveryTab(t *testing.T) {
	var page vrtPage
	if err := json.Unmarshal([]byte(vrtProgramDocument), &page); err != nil {
		t.Fatal(err)
	}

	lists := vrtLists(&page)
	want := []vrtList{
		{label: "Seizoen 2026"},
		{label: "Seizoen 2025", listID: "$c2Vhc29uLTIwMjU="},
		{label: "Vandaag in A Programme"},
		{label: "Podcast: A Programme"},
		{label: "Bloopers"},
	}
	if len(lists) != len(want) {
		t.Fatalf("got %d lists %v, want %d", len(lists), lists, len(want))
	}
	for i, list := range lists {
		if list.label != want[i].label {
			t.Errorf("list %d filed under %q, want %q", i, list.label, want[i].label)
		}
		if list.listID != want[i].listID {
			t.Errorf("list %d has listID %q, want %q", i, list.listID, want[i].listID)
		}
	}
	// The seasons lead, so the cap takes the extras rather than the episodes,
	// and a repeat of an episode is dropped rather than the season's copy.
	if lists[0].listID != "" || len(lists[0].tiles) != 2 {
		t.Errorf("the displayed season arrived as %+v, want its tiles inline", lists[0])
	}
	if lists[1].listID == "" {
		t.Error("the older season arrived inline; VRT sends it as a list to fetch")
	}
}

// TestVRTMaxRefsDropDuplicatesAndNonEpisodes covers what the walk's ordering
// is for. A "today" tab repeats what the season already listed, and a podcast
// tab holds tiles that lead to something this cannot download at all.
func TestVRTMaxRefsDropDuplicatesAndNonEpisodes(t *testing.T) {
	var page vrtPage
	if err := json.Unmarshal([]byte(vrtProgramDocument), &page); err != nil {
		t.Fatal(err)
	}

	// Stand in for the fetch of the lazy season, which is the only part of
	// the walk that needs the network.
	lists := vrtLists(&page)
	var lazy vrtResponse
	if err := json.Unmarshal([]byte(vrtLazySeasonDocument), &lazy); err != nil {
		t.Fatal(err)
	}
	lists[1].tiles = lazy.Data.List.Items

	refs := vrtRefs(lists)
	want := []vrtEpisodeRef{
		{id: "/vrtmax/a-z/a-programme/2026/ep-260821/", name: "The newest one", season: "Seizoen 2026"},
		{id: "/vrtmax/a-z/a-programme/2026/ep-260820/", name: "The one before", season: "Seizoen 2026"},
		{id: "/vrtmax/a-z/a-programme/2025/ep-251231/", name: "Last year's finale", season: "Seizoen 2025"},
		{id: "/vrtmax/a-z/a-programme/bloopers/ep-b1/", name: "Outtakes", season: "Bloopers"},
	}
	if len(refs) != len(want) {
		t.Fatalf("got %d episodes %+v, want %d", len(refs), refs, len(want))
	}
	for i, ref := range refs {
		if ref != want[i] {
			t.Errorf("episode %d = %+v, want %+v", i, ref, want[i])
		}
	}
}

// TestVRTMaxSeasonLabelDropsTheCount is why the season titles are not used as
// they arrive. "Seizoen 2026 (166 van 262 afl.)" becomes "Seizoen 2026 (167
// van 262 afl.)" the next day, and a folder named after the first would be
// abandoned in favour of a second copy of the same season.
func TestVRTMaxSeasonLabelDropsTheCount(t *testing.T) {
	tests := map[string]string{
		"Seizoen 2026 (166 van 262 afl.)": "Seizoen 2026",
		"Seizoen 1 (1 aflevering)":        "Seizoen 1",
		"Seizoen 31 (215 afleveringen)":   "Seizoen 31",
		"2026 voorjaar":                   "2026 voorjaar",
		"Bloopers":                        "Bloopers",
		"  Achter de schermen  ":          "Achter de schermen",
		"":                                "",
	}
	for title, want := range tests {
		if got := vrtSeasonLabel(title); got != want {
			t.Errorf("vrtSeasonLabel(%q) = %q, want %q", title, got, want)
		}
	}
}

// TestVRTMaxStreamIDSkipsEmptyModes covers a player that lists a playback
// route it does not offer. Reading modes[0] would hand the media service an
// empty id.
func TestVRTMaxStreamIDSkipsEmptyModes(t *testing.T) {
	var page vrtPage
	if err := json.Unmarshal([]byte(vrtEpisodeDocument), &page); err != nil {
		t.Fatal(err)
	}
	if page.TypeName != vrtEpisodePage {
		t.Fatalf("page read as %q", page.TypeName)
	}
	want := "pbs-pub-00000000-0000-0000-0000-000000000000$vid-11111111-1111-1111-1111-111111111111"
	if got := page.streamID(); got != want {
		t.Errorf("streamID = %q, want %q", got, want)
	}

	var none vrtPage
	if err := json.Unmarshal([]byte(`{"__typename":"EpisodePage","player":{"modes":[]}}`), &none); err != nil {
		t.Fatal(err)
	}
	if got := none.streamID(); got != "" {
		t.Errorf("a player with no modes named %q", got)
	}
}

// TestVRTMaxMediaPicksTheHLSLocatorWhole covers both halves of the choice:
// the DASH locator listed beside it is not something this can join, and the
// query on the HLS one tells the packager which audio codecs to include, so
// rebuilding the URL without it would change what comes back.
func TestVRTMaxMediaPicksTheHLSLocatorWhole(t *testing.T) {
	var media vrtMedia
	if err := json.Unmarshal([]byte(vrtMediaDocument), &media); err != nil {
		t.Fatal(err)
	}
	want := "https://cdn.example.test/vod/show.ism/.m3u8?filter=%28%21%28type%3D%3D%22audio%22%26%26FourCC%21%3D%22AACL%22%29%29"
	if got := media.hls(); got != want {
		t.Errorf("hls() = %q, want %q", got, want)
	}
	if media.Title != "A Programme S2026 E167" {
		t.Errorf("title = %q", media.Title)
	}

	var noHLS vrtMedia
	if err := json.Unmarshal([]byte(`{"targetUrls":[{"type":"mpeg_dash","url":"https://cdn.example.test/a.mpd"}]}`), &noHLS); err != nil {
		t.Fatal(err)
	}
	if got := noHLS.hls(); got != "" {
		t.Errorf("a DASH-only document offered %q", got)
	}
}

// TestVRTMaxProtectedReadsTheFieldNotTheName is the DRM check. The locator
// VRT hands out says "_nodrm_" in its path, and that name is exactly what is
// not trusted: the document beside it has a field for the answer, and a field
// that exists is a field that can one day be filled in.
func TestVRTMaxProtectedReadsTheFieldNotTheName(t *testing.T) {
	tests := map[string]bool{
		vrtMediaDocument:               false,
		`{"drm":null}`:                 false,
		`{}`:                           false,
		`{"drm":""}`:                   true,
		`{"drm":"widevine,playready"}`: true,
		`{"drm":{"widevine":{"licenseUrl":"https://drm.example.test/wv"}}}`: true,
	}
	for body, want := range tests {
		var media vrtMedia
		if err := json.Unmarshal([]byte(body), &media); err != nil {
			t.Fatal(err)
		}
		if got := media.protected(); got != want {
			t.Errorf("protected() of %s = %v, want %v", body, got, want)
		}
	}
}

// TestVRTMaxRefusalCode covers the geo-block, which arrives as a status code
// and a body of one field rather than as a document this ever decodes.
func TestVRTMaxRefusalCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "outside Belgium",
			err:  &httpx.StatusError{Code: http.StatusBadRequest, Body: `{"code":"CONTENT_AVAILABLE_ONLY_FOR_BE_RESIDENTS_AND_EXPATS"}`},
			want: "CONTENT_AVAILABLE_ONLY_FOR_BE_RESIDENTS_AND_EXPATS",
		},
		{
			name: "over sixteen",
			err:  &httpx.StatusError{Code: http.StatusBadRequest, Body: `{"code":"CONTENT_IS_AGE_RESTRICTED"}`},
			want: "CONTENT_IS_AGE_RESTRICTED",
		},
		{
			name: "a code nobody here has seen",
			err:  &httpx.StatusError{Code: http.StatusBadRequest, Body: `{"code":"SOMETHING_NEW"}`},
			want: "SOMETHING_NEW",
		},
		{
			// The endpoint moved once already, and answers the old path with
			// an HTML page. That is a broken extractor, not a refusal.
			name: "the endpoint has moved",
			err:  &httpx.StatusError{Code: http.StatusNotFound, Body: `<html><body>Resource not found</body></html>`},
			want: "",
		},
		{
			name: "a status with nothing in its body",
			err:  &httpx.StatusError{Code: http.StatusBadRequest, Body: ``},
			want: "",
		},
		{
			name: "not a status error at all",
			err:  vrtPlainError,
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := vrtRefusalCode(tc.err); got != tc.want {
				t.Errorf("vrtRefusalCode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVRTMaxReasonKeepsTheCode covers the translation. VRT states its
// refusals as constants rather than as prose meant for a viewer, so they are
// put into words here — and the constant is kept beside them, because a
// translation that has gone stale should be checkable against what VRT
// actually said.
func TestVRTMaxReasonKeepsTheCode(t *testing.T) {
	for _, code := range []string{
		"CONTENT_AVAILABLE_ONLY_FOR_BE_RESIDENTS",
		"CONTENT_AVAILABLE_ONLY_FOR_BE_RESIDENTS_AND_EXPATS",
		"CONTENT_IS_AGE_RESTRICTED",
		"SOMETHING_NOBODY_HAS_SEEN",
	} {
		reason := vrtReason(code)
		if !strings.Contains(reason, code) {
			t.Errorf("vrtReason(%s) = %q, which does not quote the code", code, reason)
		}
		if reason == code {
			t.Errorf("vrtReason(%s) said nothing beyond the code itself", code)
		}
	}
	refusal := &vrtRefused{code: "CONTENT_IS_AGE_RESTRICTED"}
	want := "vrtmax: VRT serves this only to a signed-in account old enough for it (CONTENT_IS_AGE_RESTRICTED)"
	if got := refusal.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestVRTMaxPageID covers the id the API takes, which is the URL's own path.
// vrtmax.be is the same catalogue with /vrtmax cut off the front, and vrt.be
// carries several services this does not resolve.
func TestVRTMaxPageID(t *testing.T) {
	tests := map[string]string{
		"https://www.vrt.be/vrtmax/a-z/a-programme/":       "/vrtmax/a-z/a-programme/",
		"https://www.vrt.be/vrtmax/a-z/a-programme":        "/vrtmax/a-z/a-programme/",
		"https://vrt.be/vrtmax/a-z/a-programme/2026/ep-1/": "/vrtmax/a-z/a-programme/2026/ep-1/",
		"https://www.vrt.be/vrtmax/a-z/a-programme/?x=1":   "/vrtmax/a-z/a-programme/",
		// The service was called VRT NU before, and the API still answers to
		// the old section name, which is what the circulating links carry.
		"https://www.vrt.be/vrtnu/a-z/a-programme/": "/vrtnu/a-z/a-programme/",
		// vrtmax.be redirects to vrt.be with the section put back in front,
		// which is done here rather than spending a request to be told so.
		"https://www.vrtmax.be/a-z/a-programme/":       "/vrtmax/a-z/a-programme/",
		"https://vrtmax.be/a-z/a-programme/2026/ep-1/": "/vrtmax/a-z/a-programme/2026/ep-1/",
		"https://www.vrtmax.be/":                       "/vrtmax/",
		// Everything else on the domain belongs to another service.
		"https://www.vrt.be/vrtnws/nl/2026/01/01/a-story/": "",
		"https://www.vrt.be/":                              "",
		"https://sporza.be/nl/matches/":                    "",
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := vrtPageID(u); got != want {
			t.Errorf("vrtPageID(%s) = %q, want %q", raw, got, want)
		}
	}
}

// TestVRTMaxMatch guards the narrowness of the claim. vrt.be is also the news
// site, the radio and the broadcaster's own pages; claiming the domain whole
// would have the link harvester treat every navigation link on a VRT page as
// something to download.
func TestVRTMaxMatch(t *testing.T) {
	v := NewVRTMax(nil)
	matched := []string{
		"https://www.vrt.be/vrtmax/a-z/a-programme/",
		"https://WWW.VRT.BE/vrtmax/a-z/a-programme/",
		"https://www.vrt.be/vrtnu/a-z/a-programme/",
		"https://www.vrtmax.be/a-z/a-programme/",
		"https://vrtmax.be/",
	}
	for _, raw := range matched {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !v.Match(u) {
			t.Errorf("%s was not matched", raw)
		}
	}

	unmatched := []string{
		"https://www.vrt.be/vrtnws/nl/2026/01/01/a-story/",
		"https://www.vrt.be/",
		"https://radio1.be/",
		"https://sporza.be/nl/",
		"https://notvrt.be.example.test/vrtmax/a-z/a-programme/",
	}
	for _, raw := range unmatched {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if v.Match(u) {
			t.Errorf("%s was matched", raw)
		}
	}
}

// TestVRTMaxTitle covers naming a lone episode, which lands in the download
// directory with no folder above it to say what it belongs to.
func TestVRTMaxTitle(t *testing.T) {
	tests := []struct {
		programme, episode, want string
	}{
		{"A Programme S2026 E167", "The newest one", "A Programme S2026 E167 - The newest one"},
		{"A Documentary", "", "A Documentary"},
		{"", "The newest one", "The newest one"},
		{"A Documentary", "a documentary", "A Documentary"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := vrtTitle(tc.programme, tc.episode); got != tc.want {
			t.Errorf("vrtTitle(%q, %q) = %q, want %q", tc.programme, tc.episode, got, tc.want)
		}
	}
}

// TestVRTMaxSlug covers the last resort for a filename. It is the episode's
// own path segment, which is unique within a programme where a title is not.
func TestVRTMaxSlug(t *testing.T) {
	tests := map[string]string{
		"/vrtmax/a-z/a-programme/2026/ep-260821/": "ep-260821",
		"/vrtmax/a-z/a-programme/":                "a-programme",
		"a-programme":                             "a-programme",
		"":                                        "",
	}
	for id, want := range tests {
		if got := vrtSlug(id); got != want {
			t.Errorf("vrtSlug(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestVRTMaxQueriesNameTheTypesTheyDecode is a cheap guard against the two
// halves drifting apart: the queries are assembled from the same type-name
// constants the walk compares against, and a rename that touched only one
// side would leave every programme page reading as empty.
func TestVRTMaxQueriesNameTheTypesTheyDecode(t *testing.T) {
	for _, name := range []string{vrtEpisodePage, vrtProgramPage, vrtNavigation, vrtPaginatedList, vrtLazyList, vrtEpisodeTile} {
		if !strings.Contains(vrtPageQuery+vrtListQuery+vrtEpisodeQuery, name) {
			t.Errorf("%s is compared against but never asked for", name)
		}
	}
}

// TestVRTMaxAPIHeaderCarriesTheClientName pins the one requirement the
// GraphQL endpoint has. Without it every query, however well formed, is
// answered with the two words "Bad Request." and nothing else.
func TestVRTMaxAPIHeaderCarriesTheClientName(t *testing.T) {
	if got := vrtAPIHeader[vrtClientHeader]; got != vrtClientName {
		t.Errorf("%s = %q, want %q", vrtClientHeader, got, vrtClientName)
	}
}

// vrtPlainError is a failure carrying no status, for the refusal check.
var vrtPlainError = &url.Error{Op: "Post", URL: "https://media.example.test/", Err: http.ErrHandlerTimeout}
