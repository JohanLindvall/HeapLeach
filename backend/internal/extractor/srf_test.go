package extractor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// The fixtures below are written by hand in the shape the Integration Layer
// answers in, with its traps reproduced deliberately. Nothing here names a
// real programme.

// srfProgressive is the ordinary case, and carries the choice that matters:
// four ways to fetch the same chapter, of which the plain HD file is the one
// worth taking. The HLS entries come first, as they do in the real document,
// so anything that merely takes the first playable resource takes the wrong
// one. The progressive links arrive as http, which is not how a
// multi-gigabyte file should be fetched.
const srfProgressive = `{
  "chapterUrn": "urn:srf:video:11111111-1111-1111-1111-111111111111",
  "show": { "id": "aaaa", "urn": "urn:srf:show:tv:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "title": "A Magazine" },
  "episode": { "id": "bbbb", "title": "An Instalment", "publishedDate": "2026-08-19T13:00:00+02:00" },
  "chapterList": [
    {
      "id": "11111111-1111-1111-1111-111111111111",
      "urn": "urn:srf:video:11111111-1111-1111-1111-111111111111",
      "mediaType": "VIDEO",
      "type": "EPISODE",
      "title": "An Instalment",
      "duration": 1213800,
      "playableAbroad": true,
      "podcastSdUrl": "https://media.example.test/world/video/other/2026/08/a_20260828_q30.mp4",
      "podcastHdUrl": "https://media.example.test/world/video/other/2026/08/a_20260828_q30.mp4",
      "resourceList": [
        { "url": "https://cdn.example.test/hls/a_,q40,q50,q60,.mp4.csmil/master.m3u8", "quality": "HD",
          "protocol": "HLS", "streaming": "HLS", "mimeType": "application/x-mpegURL", "tokenType": "NONE" },
        { "url": "https://cdn.example.test/hls/a_,q40,q50,.mp4.csmil/master.m3u8", "quality": "SD",
          "protocol": "HLS", "streaming": "HLS", "mimeType": "application/x-mpegURL", "tokenType": "NONE" },
        { "url": "http://download.example.test/world/video/a_20260819_q60.mp4", "quality": "HD",
          "protocol": "HTTP", "streaming": "PROGRESSIVE", "mimeType": "video/mp4", "tokenType": "NONE" },
        { "url": "http://download.example.test/world/video/a_20260819_q50.mp4", "quality": "SD",
          "protocol": "HTTP", "streaming": "PROGRESSIVE", "mimeType": "video/mp4", "tokenType": "NONE" }
      ]
    }
  ]
}`

// srfBlockedChapter is what roughly one title in eight answers with from
// outside Switzerland. resourceList is not empty — it is absent, which is why
// blockReason has to be read before anything is chosen rather than after
// nothing was found.
const srfBlockedChapter = `{
  "chapterUrn": "urn:srf:video:22222222-2222-2222-2222-222222222222",
  "show": { "title": "A Drama" },
  "episode": { "title": "An Episode (Staffel 1, Folge 1)", "seasonNumber": 1, "number": 1 },
  "chapterList": [
    {
      "urn": "urn:srf:video:22222222-2222-2222-2222-222222222222",
      "mediaType": "VIDEO",
      "type": "EPISODE",
      "title": "An Episode (Staffel 1, Folge 1)",
      "blockReason": "GEOBLOCK",
      "playableAbroad": false,
      "displayable": true
    }
  ]
}`

// srfClipRequest is the composition returned for a *segment* urn: the
// document is about the whole programme, and the thing that was asked for
// appears only in segmentList, as a pair of time offsets with no resources of
// its own. Nothing here can cut a range out of a file, so the programme is
// what a clip link has to resolve to.
const srfClipRequest = `{
  "chapterUrn": "urn:srf:video:33333333-3333-3333-3333-333333333333",
  "show": { "title": "A News Strand" },
  "episode": { "title": "A News Strand vom 21.08.2026" },
  "chapterList": [
    {
      "urn": "urn:srf:video:33333333-3333-3333-3333-333333333333",
      "type": "EPISODE",
      "title": "A News Strand vom 21.08.2026",
      "segmentList": [
        { "urn": "urn:srf:video:44444444-4444-4444-4444-444444444444", "title": "A single item",
          "markIn": 53480, "markOut": 326000, "resourceList": [] }
      ],
      "resourceList": [
        { "url": "http://download.example.test/world/video/n_20260821_q50.mp4", "quality": "SD",
          "protocol": "HTTP", "streaming": "PROGRESSIVE", "tokenType": "NONE" }
      ]
    }
  ]
}`

// srfUnfetchable has resources and none that can be taken: one behind DRM,
// one signed with an Akamai token this does not mint, and one speaking a
// protocol with no code behind it.
const srfUnfetchable = `{
  "chapterUrn": "urn:srf:video:55555555-5555-5555-5555-555555555555",
  "show": { "title": "A Film Strand" },
  "episode": { "title": "A Film" },
  "chapterList": [
    {
      "urn": "urn:srf:video:55555555-5555-5555-5555-555555555555",
      "title": "A Film",
      "resourceList": [
        { "url": "https://cdn.example.test/dash/a.mpd", "quality": "HD", "protocol": "DASH",
          "tokenType": "NONE", "drmList": [ { "type": "WIDEVINE", "licenseUrl": "https://drm.example.test/l" } ] },
        { "url": "https://cdn.example.test/hls/a/master.m3u8", "quality": "HD", "protocol": "HLS",
          "tokenType": "AKAMAI" },
        { "url": "rtmp://cdn.example.test/a", "quality": "SD", "protocol": "RTMP", "tokenType": "NONE" }
      ]
    }
  ]
}`

// srfMuxedMaster is a title with no audio description: no EXT-X-MEDIA AUDIO
// entry at all, so every variant carries its video and its audio in the same
// segments and joining one of them yields something that plays.
const srfMuxedMaster = `#EXTM3U
#EXT-X-VERSION:4
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="WebVTT",LANGUAGE="de",NAME="Deutsch",DEFAULT=NO,AUTOSELECT=YES,FORCED=NO,URI="subs.m3u8"
#EXT-X-STREAM-INF:SUBTITLES="WebVTT",PROGRAM-ID=1,BANDWIDTH=2991604,RESOLUTION=1280x720,FRAME-RATE=25.000,CODECS="avc1.640020,mp4a.40.2",VIDEO-RANGE=SDR,CLOSED-CAPTIONS=NONE
index-f4-v1.m3u8
#EXT-X-STREAM-INF:SUBTITLES="WebVTT",PROGRAM-ID=1,BANDWIDTH=658405,RESOLUTION=640x360,FRAME-RATE=25.000,CODECS="avc1.4d401f,mp4a.40.2",VIDEO-RANGE=SDR,CLOSED-CAPTIONS=NONE
index-f2-v1.m3u8
`

// srfDemuxedMaster is the same host's other shape, and it is exactly the
// titles that ship an audio-description track: an audio group with two
// renditions, the second described as describing video, both with URIs of
// their own, and every variant naming that group. By RFC 8216 that says the
// variants' segments carry no audio, which is what muxed() reads it as.
//
// SRF's segments do in fact carry it — Akamai mixes the default track in as
// well as publishing it separately — so the manifest is non-conformant and
// the refusal that follows is conservative for this host. It is kept because
// nothing in a manifest separates this case from vimeo's genuine split, where
// the same declaration is true and believing it yields a silent file.
const srfDemuxedMaster = `#EXTM3U
#EXT-X-VERSION:4
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="WebVTT",LANGUAGE="de",NAME="Deutsch",DEFAULT=NO,AUTOSELECT=YES,FORCED=NO,URI="subs.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",NAME="Deutsch",LANGUAGE="de",AUTOSELECT=YES,DEFAULT=YES,CHANNELS="2",URI="index-f1-a1.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",NAME="Deutsch ",LANGUAGE="de",AUTOSELECT=YES,DEFAULT=NO,CHARACTERISTICS="public.accessibility.describes-video",CHANNELS="2",URI="index-f1-a2.m3u8"
#EXT-X-STREAM-INF:SUBTITLES="WebVTT",PROGRAM-ID=1,BANDWIDTH=2128524,RESOLUTION=960x540,FRAME-RATE=25.000,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="audio0",CLOSED-CAPTIONS=NONE
index-f1-v1.m3u8
#EXT-X-STREAM-INF:SUBTITLES="WebVTT",PROGRAM-ID=1,BANDWIDTH=378133,RESOLUTION=320x180,FRAME-RATE=25.000,CODECS="avc1.42c00c,mp4a.40.2",AUDIO="audio0",CLOSED-CAPTIONS=NONE
index-f0-v1.m3u8
`

// srfMedia is a finished variant playlist, so resolvePlaylist has something
// to reach at the end of a master.
const srfMedia = `#EXTM3U
#EXT-X-VERSION:4
#EXT-X-TARGETDURATION:6
#EXT-X-PLAYLIST-TYPE:VOD
#EXTINF:6.000,
seg-1.ts
#EXTINF:6.000,
seg-2.ts
#EXT-X-ENDLIST
`

// srfShowListing is one page of a show's episodes, and holds the trap that
// decides how a series is read. Each episode's mediaList carries the whole
// programme *and* every segment cut out of it, all typed EPISODE and
// indistinguishable in the listing; taking that list downloads the programme
// once whole and then again in pieces. fullLengthUrn names the one entry that
// is the programme. The seasons run backwards, newest first, which is the
// site's own order.
const srfShowListing = `{
  "show": { "id": "aaaa", "urn": "urn:srf:show:tv:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "title": "A Drama" },
  "episodeList": [
    {
      "id": "e2", "title": "The Second (Staffel 2, Folge 1)", "seasonNumber": 2, "number": 1,
      "fullLengthUrn": "urn:srf:video:aaaaaaa2-0000-0000-0000-000000000001",
      "mediaList": [
        { "urn": "urn:srf:video:aaaaaaa2-0000-0000-0000-000000000001", "type": "EPISODE", "title": "The Second (Staffel 2, Folge 1)" },
        { "urn": "urn:srf:video:aaaaaaa2-0000-0000-0000-00000000000a", "type": "EPISODE", "title": "A clip from it" },
        { "urn": "urn:srf:video:aaaaaaa2-0000-0000-0000-00000000000b", "type": "EPISODE", "title": "Another clip from it" }
      ]
    },
    {
      "id": "e1", "title": "The First (Staffel 1, Folge 1)", "seasonNumber": 1, "number": 1,
      "fullLengthUrn": "urn:srf:video:aaaaaaa1-0000-0000-0000-000000000001",
      "mediaList": [
        { "urn": "urn:srf:video:aaaaaaa1-0000-0000-0000-000000000001", "type": "EPISODE", "title": "The First (Staffel 1, Folge 1)" }
      ]
    }
  ]
}`

// srfUnnumberedListing is the other kind of show: a strand whose instalments
// belong to no season and are named after the strand itself.
const srfUnnumberedListing = `{
  "show": { "title": "A News Strand" },
  "episodeList": [
    { "id": "n2", "title": "A News Strand vom 21.08.2026",
      "fullLengthUrn": "urn:srf:video:bbbbbbb0-0000-0000-0000-000000000002" },
    { "id": "n1", "title": "A News Strand vom 20.08.2026",
      "fullLengthUrn": "urn:srf:video:bbbbbbb0-0000-0000-0000-000000000001" }
  ]
}`

func srfTestClient() *httpx.Client { return httpx.New("test-agent", "en-US", 0, 5*time.Second) }

// TestSRFMasterPlaylistsAreToldApart pins the claim the whole extractor is
// arranged around: SRF serves both shapes, and only one of them can be joined
// by concatenation.
func TestSRFMasterPlaylistsAreToldApart(t *testing.T) {
	base, err := ParseURL("https://cdn.example.test/hls/a/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	muxed := parseMasterPlaylist(srfMuxedMaster, base)
	if len(muxed) != 2 {
		t.Fatalf("parsed %d variants of the self-contained master, want 2", len(muxed))
	}
	for _, v := range muxed {
		if !v.muxed() {
			t.Errorf("%s reads as video only, but the master declares no audio group at all", v.Resolution)
		}
	}
	best, ok := bestVariant(muxed)
	if !ok || best.Resolution != "1280x720" {
		t.Errorf("chose %q, want the largest rendition", best.Resolution)
	}
	if got := playlistExtension(best); got != ".ts" {
		t.Errorf("extension %q, want .ts", got)
	}

	demuxed := parseMasterPlaylist(srfDemuxedMaster, base)
	if len(demuxed) != 2 {
		t.Fatalf("parsed %d variants of the audio-description master, want 2", len(demuxed))
	}
	for _, v := range demuxed {
		if v.muxed() {
			t.Errorf("%s reads as self-contained although it names an audio group whose renditions "+
				"have URIs of their own, which is a declaration that its segments carry no audio",
				v.Resolution)
		}
	}
	// bestVariant still answers, which is exactly why the caller has to look:
	// preferring a self-contained rendition is not the same as refusing when
	// there is none.
	if _, ok := bestVariant(demuxed); !ok {
		t.Error("bestVariant refused the demuxed master by itself, so the extractor's own check is dead code")
	}
}

// TestSRFPicksTheProgressiveResource covers the choice the host is worth
// supporting for: a plain file over a playlist, and the larger of two.
func TestSRFPicksTheProgressiveResource(t *testing.T) {
	var doc srfComposition
	if err := json.Unmarshal([]byte(srfProgressive), &doc); err != nil {
		t.Fatal(err)
	}
	chapter, ok := doc.chapter()
	if !ok {
		t.Fatal("the composition named no chapter")
	}

	best, ok := chapter.pick(srfProtocolHTTP)
	if !ok {
		t.Fatal("no progressive resource was chosen")
	}
	if best.Quality != "HD" {
		t.Errorf("chose the %s file, want HD", best.Quality)
	}
	if got := srfSecure(best.URL); got != "https://download.example.test/world/video/a_20260819_q60.mp4" {
		t.Errorf("URL = %q, want the https form of the HD file", got)
	}
	if got := srfExtension(best.URL); got != ".mp4" {
		t.Errorf("extension = %q, want .mp4", got)
	}

	// The playlist is still there and still HD, which is what makes it a
	// usable fallback for the chapters that have no file.
	if hls, ok := chapter.pick(srfProtocolHLS); !ok || hls.Quality != "HD" {
		t.Errorf("HLS fallback = %+v, want the HD master", hls)
	}
}

// TestSRFPodcastLinksAreNotUsed guards a field that looks like a free second
// source and is not. For most chapters podcastSdUrl and podcastHdUrl repeat
// the resource already listed, on another CDN hostname; for some they name a
// different asset produced days later. A source that is usually right is
// worse than none when being wrong means silently downloading somebody
// else's episode — so a chapter offering nothing but those offers nothing.
func TestSRFPodcastLinksAreNotUsed(t *testing.T) {
	var chapter srfChapter
	if err := json.Unmarshal([]byte(`{
		"urn": "urn:srf:video:1", "title": "An Instalment", "date": "2026-08-10T05:02:00+02:00",
		"podcastSdUrl": "https://media.example.test/world/video/a_20260818_q30.mp4",
		"podcastHdUrl": "https://media.example.test/world/video/a_20260818_q30.mp4",
		"resourceList": []}`), &chapter); err != nil {
		t.Fatal(err)
	}
	if r, ok := chapter.pick(srfProtocolHTTP); ok {
		t.Errorf("took %q, which is a podcast link rather than a listed resource", r.URL)
	}
	if got := chapter.refusal(); got != "offers no stream that can be fetched" {
		t.Errorf("refusal = %q", got)
	}
}

// TestSRFSkipsWhatCannotBeFetched covers the two obstacles the schema can
// carry. Neither has been served here, which is the reason to check for them:
// a DRM resource taken by mistake downloads to the end and then plays as
// noise.
func TestSRFSkipsWhatCannotBeFetched(t *testing.T) {
	var doc srfComposition
	if err := json.Unmarshal([]byte(srfUnfetchable), &doc); err != nil {
		t.Fatal(err)
	}
	chapter, _ := doc.chapter()

	if r, ok := chapter.pick(srfProtocolHTTP); ok {
		t.Errorf("took %q, though nothing here is progressive", r.URL)
	}
	if r, ok := chapter.pick(srfProtocolHLS); ok {
		t.Errorf("took %q, which is signed with a token this does not mint", r.URL)
	}
	// DRM is reported ahead of the token, because it is the one a user can do
	// nothing about.
	if got := chapter.refusal(); got != "is DRM protected" {
		t.Errorf("refusal = %q, want the DRM one", got)
	}

	var tokenOnly srfChapter
	if err := json.Unmarshal([]byte(`{"resourceList":[
		{"url":"https://cdn.example.test/hls/a/master.m3u8","protocol":"HLS","quality":"HD","tokenType":"AKAMAI"}]}`),
		&tokenOnly); err != nil {
		t.Fatal(err)
	}
	if got := tokenOnly.refusal(); !strings.Contains(got, "Akamai token") {
		t.Errorf("refusal = %q, want the token one", got)
	}
}

// TestSRFUnknownQualityIsStillTaken covers a label the host invents. Ranking
// it last is right; refusing it would throw away the only rendition there is.
func TestSRFUnknownQualityIsStillTaken(t *testing.T) {
	var chapter srfChapter
	if err := json.Unmarshal([]byte(`{"resourceList":[
		{"url":"http://download.example.test/a_uhd.mp4","protocol":"HTTP","quality":"UHD","tokenType":"NONE"}]}`),
		&chapter); err != nil {
		t.Fatal(err)
	}
	best, ok := chapter.pick(srfProtocolHTTP)
	if !ok {
		t.Fatal("a rendition with an unrecognised label was refused")
	}
	if best.Quality != "UHD" {
		t.Errorf("chose %q", best.Quality)
	}
	if srfRank("UHD") <= srfRank("SD") {
		t.Error("an unrecognised label outranked SD, so a new name would be preferred over a known one")
	}
}

// TestSRFBlockReasonIsReadBeforeAnythingIsChosen is the geo-block guard. The
// document arrives with no resourceList at all, so a chapter that is not
// checked first reports "offers no stream" — true, unhelpful, and hiding the
// one fact the user can act on.
func TestSRFBlockReasonIsReadBeforeAnythingIsChosen(t *testing.T) {
	var doc srfComposition
	if err := json.Unmarshal([]byte(srfBlockedChapter), &doc); err != nil {
		t.Fatal(err)
	}
	chapter, ok := doc.chapter()
	if !ok {
		t.Fatal("the composition named no chapter")
	}
	if chapter.BlockReason != "GEOBLOCK" {
		t.Fatalf("blockReason = %q", chapter.BlockReason)
	}
	if len(chapter.ResourceList) != 0 {
		t.Errorf("a blocked chapter listed %d resources", len(chapter.ResourceList))
	}
	if got := srfBlockMessage(chapter.BlockReason); !strings.Contains(got, "Switzerland") {
		t.Errorf("message = %q, want one that names the country", got)
	}
}

// TestSRFBlockMessages covers the translation, and the token nobody has seen.
func TestSRFBlockMessages(t *testing.T) {
	tests := map[string]string{
		"GEOBLOCK":  "SRF licenses this for Switzerland only, and serves nothing at all to an address outside it",
		"geoblock":  "SRF licenses this for Switzerland only, and serves nothing at all to an address outside it",
		"STARTDATE": "this has not been published yet",
		"ENDDATE":   "this has passed the end of its availability window",
		// Unrecognised: reported as it stands, because naming what the host
		// said beats flattening it into "unavailable".
		"SOMETHING_NEW": "SRF refused it: SOMETHING_NEW",
	}
	for token, want := range tests {
		if got := srfBlockMessage(token); got != want {
			t.Errorf("srfBlockMessage(%q) = %q, want %q", token, got, want)
		}
	}
}

// TestSRFBlockedErrorCarriesTheReason covers what a whole show's expansion
// leans on: several hundred episodes refused for one reason should be
// reported as that one reason, not as several hundred failures.
func TestSRFBlockedErrorCarriesTheReason(t *testing.T) {
	err := &srfBlocked{urn: "urn:srf:video:1", reason: "a reason"}
	if got := err.Error(); got != "srf: urn:srf:video:1: a reason" {
		t.Errorf("Error() = %q", got)
	}
}

// TestSRFClipResolvesToItsProgramme documents what a segment link can do and
// what it cannot: the composition is about the programme, the requested urn is
// a pair of time offsets inside it, and the file that lands on disk is named
// after the programme because that is what it is.
func TestSRFClipResolvesToItsProgramme(t *testing.T) {
	var doc srfComposition
	if err := json.Unmarshal([]byte(srfClipRequest), &doc); err != nil {
		t.Fatal(err)
	}
	chapter, ok := doc.chapter()
	if !ok {
		t.Fatal("the composition named no chapter")
	}
	if chapter.URN != doc.ChapterURN {
		t.Errorf("chose chapter %q, want the one chapterUrn names (%q)", chapter.URN, doc.ChapterURN)
	}
	if chapter.Title != "A News Strand vom 21.08.2026" {
		t.Errorf("title = %q, want the programme's", chapter.Title)
	}
}

// TestSRFChapterFallsBackToTheFirst covers a document whose chapterUrn names
// nothing in the list. One chapter is all these ever carry, so taking it is
// better than refusing over a field that disagreed with itself.
func TestSRFChapterFallsBackToTheFirst(t *testing.T) {
	var doc srfComposition
	if err := json.Unmarshal([]byte(`{"chapterUrn":"urn:srf:video:elsewhere",
		"chapterList":[{"urn":"urn:srf:video:1","title":"The only one"}]}`), &doc); err != nil {
		t.Fatal(err)
	}
	chapter, ok := doc.chapter()
	if !ok || chapter.Title != "The only one" {
		t.Errorf("chapter = %+v, ok = %v", chapter, ok)
	}

	if err := json.Unmarshal([]byte(`{"chapterUrn":"urn:srf:video:1","chapterList":[]}`), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.chapter(); ok {
		t.Error("an empty chapterList produced a chapter")
	}
}

// TestSRFEpisodeTakesOnlyTheFullLengthURN is the series trap. mediaList holds
// the programme and every clip cut from it, so reading that list queues the
// same programme four times over.
func TestSRFEpisodeTakesOnlyTheFullLengthURN(t *testing.T) {
	var doc srfEpisodeList
	if err := json.Unmarshal([]byte(srfShowListing), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.EpisodeList) != 2 {
		t.Fatalf("parsed %d episodes, want 2", len(doc.EpisodeList))
	}
	first := doc.EpisodeList[0]
	if len(first.MediaList) != 3 {
		t.Fatalf("the fixture lost its clips: mediaList has %d entries", len(first.MediaList))
	}
	if got := first.urn(); got != "urn:srf:video:aaaaaaa2-0000-0000-0000-000000000001" {
		t.Errorf("urn() = %q, want the full-length one rather than a clip", got)
	}
	if got := first.season(); got != "Staffel 2" {
		t.Errorf("season() = %q", got)
	}

	// An episode described by its media list alone still resolves.
	var bare srfEpisode
	if err := json.Unmarshal([]byte(`{"title":"x","mediaList":[
		{"urn":"urn:srf:video:9","type":"EPISODE"},{"urn":"urn:srf:video:8","type":"EPISODE"}]}`),
		&bare); err != nil {
		t.Fatal(err)
	}
	if got := bare.urn(); got != "urn:srf:video:9" {
		t.Errorf("urn() = %q, want the first entry", got)
	}
	if got := bare.season(); got != "" {
		t.Errorf("season() = %q, want nothing to nest by", got)
	}
}

func TestSRFJoin(t *testing.T) {
	tests := []struct {
		show, title, want string
	}{
		{"A Magazine", "An Instalment", "A Magazine - An Instalment"},
		// A daily strand names each instalment after itself, so saying it
		// twice is what the prefix has to avoid.
		{"10 vor 10", "10 vor 10 vom 21.08.2026", "10 vor 10 vom 21.08.2026"},
		{"A Drama", "a drama", "a drama"},
		{"", "A Documentary", "A Documentary"},
		{"A Documentary", "", "A Documentary"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := srfJoin(tc.show, tc.title); got != tc.want {
			t.Errorf("srfJoin(%q, %q) = %q, want %q", tc.show, tc.title, got, tc.want)
		}
	}
}

func TestSRFTarget(t *testing.T) {
	tests := []struct {
		raw  string
		urn  string
		show bool
	}{
		{"https://www.srf.ch/play/tv/a-show/video/a-title?urn=urn:srf:video:11111111-1111-1111-1111-111111111111",
			"urn:srf:video:11111111-1111-1111-1111-111111111111", false},
		// A show page carries the bare id; the transmission is in the path.
		{"https://www.srf.ch/play/tv/sendung/a-show?id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			"urn:srf:show:tv:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", true},
		// A show urn spelled out is recognised as one without the path.
		{"https://www.srf.ch/play/tv/a-show?urn=urn:srf:show:tv:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			"urn:srf:show:tv:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", true},
		// The endpoint answers for every SRG service, so a urn spelled for
		// another one is not refused for its prefix.
		{"https://www.srf.ch/play/tv/x/video/y?urn=urn:rts:video:11111111-1111-1111-1111-111111111111",
			"urn:rts:video:11111111-1111-1111-1111-111111111111", false},
		{"https://www.srf.ch/play/tv/a-show/video/a-title", "", false},
		{"https://www.srf.ch/play/tv/live", "", false},
		{"https://www.srf.ch/play/tv/a-show/video/a-title?urn=not-a-urn", "", false},
		// An id that is not a uuid is something else's query parameter.
		{"https://www.srf.ch/play/tv/sendung/a-show?id=12345", "", false},
	}
	for _, tc := range tests {
		u, err := ParseURL(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		got := srfTarget(u)
		if got.urn != tc.urn || got.show != tc.show {
			t.Errorf("srfTarget(%s) = %+v, want urn %q show %v", tc.raw, got, tc.urn, tc.show)
		}
	}
}

// TestSRFMatch pins the boundary. srf.ch is a news site with a Play section
// bolted on, and claiming the news articles would promise more than this
// resolves — but a link anywhere on the host that names a urn outright is one
// this can answer.
func TestSRFMatch(t *testing.T) {
	tests := map[string]bool{
		"https://www.srf.ch/play/tv/a-show/video/a-title?urn=urn:srf:video:1": true,
		"https://www.srf.ch/play/tv/sendung/a-show?id=a":                      true,
		"https://srf.ch/play/tv":                                              true,
		"https://www.srf.ch/news/a-story?urn=urn:srf:video:1":                 true,
		"https://www.srf.ch/news/a-story":                                     false,
		"https://www.srf.ch/":                                                 false,
		"https://il.srgssr.ch/images/?imageUrl=x":                             false,
		"https://www.rts.ch/play/tv":                                          false,
	}
	ex := NewSRF(nil)
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := ex.Match(u); got != want {
			t.Errorf("Match(%s) = %v, want %v", raw, got, want)
		}
	}
}

// srfServer answers the two endpoints from fixtures, with the playlists
// rewritten to point at itself. compositions is keyed by urn.
func srfServer(t *testing.T, compositions map[string]string, listings []string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	page := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local := func(body string) string { return strings.ReplaceAll(body, "https://cdn.example.test", srv.URL) }
		switch {
		case strings.HasPrefix(r.URL.Path, "/il"+srfCompositionPath):
			urn := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/il"+srfCompositionPath), ".json")
			body, ok := compositions[urn]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			_, _ = w.Write([]byte(local(body)))
		case strings.HasPrefix(r.URL.Path, "/il"+srfEpisodesPath):
			if page >= len(listings) {
				http.NotFound(w, r)
				return
			}
			body := listings[page]
			page++
			if page < len(listings) {
				body = strings.TrimSuffix(strings.TrimSpace(body), "}") +
					`, "next": "` + srv.URL + "/il" + srfEpisodesPath + `x?next=` + string(rune('0'+page)) + `"}`
			}
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/master.m3u8"):
			master := srfMuxedMaster
			if strings.Contains(r.URL.Path, "demuxed") {
				master = srfDemuxedMaster
			}
			_, _ = w.Write([]byte(master))
		case strings.HasSuffix(r.URL.Path, ".m3u8"):
			_, _ = w.Write([]byte(srfMedia))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func srfWith(t *testing.T, srv *httptest.Server) *SRF {
	t.Helper()
	return &SRF{client: srfTestClient(), il: srv.URL + "/il"}
}

// TestSRFExtractProgrammeTakesTheFile is the whole progressive path end to
// end: the file rather than the playlist, https rather than http, and a name
// carrying the show because a lone download has no folder to say what it
// belongs to.
func TestSRFExtractProgrammeTakesTheFile(t *testing.T) {
	const urn = "urn:srf:video:11111111-1111-1111-1111-111111111111"
	srv := srfServer(t, map[string]string{urn: srfProgressive}, nil)

	u, err := ParseURL("https://www.srf.ch/play/tv/a-show/video/a-title?urn=" + urn)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srfWith(t, srv).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	file := res.Files[0]
	if file.URL != "https://download.example.test/world/video/a_20260819_q60.mp4" {
		t.Errorf("URL = %q", file.URL)
	}
	if len(file.Segments) != 0 {
		t.Errorf("a chapter with a plain file was resolved as a playlist")
	}
	if file.Name != "A Magazine - An Instalment.mp4" {
		t.Errorf("Name = %q", file.Name)
	}
	// The Integration Layer publishes no byte count, and a guess would let
	// the skip check decide a part-finished file was already this one.
	if file.Size != -1 {
		t.Errorf("Size = %d, want -1: the host reports no length", file.Size)
	}
	if res.Title != "A Magazine - An Instalment" {
		t.Errorf("Title = %q", res.Title)
	}
}

// TestSRFExtractFallsBackToASelfContainedPlaylist covers the chapters SRF
// publishes with no progressive resource at all.
func TestSRFExtractFallsBackToASelfContainedPlaylist(t *testing.T) {
	const urn = "urn:srf:video:66666666-6666-6666-6666-666666666666"
	composition := `{
	  "chapterUrn": "` + urn + `",
	  "show": { "title": "A Magazine" },
	  "episode": { "title": "An Instalment" },
	  "chapterList": [ { "urn": "` + urn + `", "title": "An Instalment", "resourceList": [
	    { "url": "https://cdn.example.test/hls/a_,q40,q50,q60,.mp4.csmil/master.m3u8", "quality": "HD", "protocol": "HLS", "tokenType": "NONE" },
	    { "url": "https://cdn.example.test/hls/a_,q40,q50,.mp4.csmil/master.m3u8", "quality": "SD", "protocol": "HLS", "tokenType": "NONE" }
	  ] } ]
	}`
	srv := srfServer(t, map[string]string{urn: composition}, nil)

	u, err := ParseURL("https://www.srf.ch/play/tv/a-show/video/a-title?urn=" + urn)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srfWith(t, srv).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	file := res.Files[0]
	if len(file.Segments) != 2 {
		t.Fatalf("got %d segments, want the media playlist's 2", len(file.Segments))
	}
	if file.URL != "" {
		t.Errorf("a playlist-backed file also carries URL %q", file.URL)
	}
	// The variant it came from lives under ".mp4.csmil" and its segments are
	// MPEG-TS, so the name has to come from the segments.
	if file.Name != "A Magazine - An Instalment.ts" {
		t.Errorf("Name = %q, want the container the segments are actually in", file.Name)
	}
}

// TestSRFPlaylistExtensionBelievesTheSegments covers a URL that lies. Akamai
// packages SRF's playlists out of a stored MP4 and names the packaging path
// after it, so the shared rule — which reads the variant's URL — concludes
// ".mp4" for a stream whose parts are all ".ts".
func TestSRFPlaylistExtensionBelievesTheSegments(t *testing.T) {
	variant := hlsVariant{
		URL: "https://cdn.example.test/hls/a_,q40,q60,.mp4.csmil/index-f1-v1-a1.m3u8",
	}
	if got := playlistExtension(variant); got != ".mp4" {
		t.Fatalf("the shared rule returned %q for the csmil path; this test no longer covers anything", got)
	}
	segments := []string{
		"https://cdn.example.test/hls/a_,q40,q60,.mp4.csmil/segment-1-f1-v1-a1.ts",
		"https://cdn.example.test/hls/a_,q40,q60,.mp4.csmil/segment-2-f1-v1-a1.ts",
	}
	if got := segmentsExtension(segments, variant); got != ".ts" {
		t.Errorf("extension = %q, want .ts — the segments are what gets written", got)
	}

	// A fragmented-MP4 playlist leads with its initialisation segment, and
	// naming after that is right for the same reason.
	fmp4 := []string{"https://cdn.example.test/cmaf/init.mp4", "https://cdn.example.test/cmaf/1.m4s"}
	if got := segmentsExtension(fmp4, variant); got != ".mp4" {
		t.Errorf("extension = %q, want .mp4", got)
	}

	// Nothing to read: the shared rule is the fallback rather than a guess.
	if got := segmentsExtension(nil, variant); got != playlistExtension(variant) {
		t.Errorf("extension = %q, want the shared rule's answer", got)
	}
}

// TestSRFExtractRefusesADemuxedPlaylist is the deciding constraint. Nothing
// on this path can mux, and a master that declares its audio elsewhere has to
// be taken at its word: the alternative is a silent file that looks finished.
func TestSRFExtractRefusesADemuxedPlaylist(t *testing.T) {
	const urn = "urn:srf:video:77777777-7777-7777-7777-777777777777"
	composition := `{
	  "chapterUrn": "` + urn + `",
	  "show": { "title": "A Drama" },
	  "episode": { "title": "An Episode" },
	  "chapterList": [ { "urn": "` + urn + `", "title": "An Episode", "resourceList": [
	    { "url": "https://cdn.example.test/hls/demuxed/master.m3u8", "quality": "HD", "protocol": "HLS", "tokenType": "NONE" }
	  ] } ]
	}`
	srv := srfServer(t, map[string]string{urn: composition}, nil)

	u, err := ParseURL("https://www.srf.ch/play/tv/a-show/video/a-title?urn=" + urn)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srfWith(t, srv).Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatalf("accepted a demuxed master and produced %d files", len(res.Files))
	}
	if !strings.Contains(err.Error(), "no sound") || !strings.Contains(err.Error(), "yt-dlp") {
		t.Errorf("error = %q, want one that says what is wrong and what to do instead", err)
	}
}

// TestSRFExtractReportsAGeoBlock covers the refusal a Swiss-only title gives,
// which must never reach the user as a bare failure to find a stream.
func TestSRFExtractReportsAGeoBlock(t *testing.T) {
	const urn = "urn:srf:video:22222222-2222-2222-2222-222222222222"
	srv := srfServer(t, map[string]string{urn: srfBlockedChapter}, nil)

	u, err := ParseURL("https://www.srf.ch/play/tv/a-show/video/a-title?urn=" + urn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srfWith(t, srv).Extract(context.Background(), u, Options{}); err == nil {
		t.Fatal("a geo-blocked chapter resolved")
	} else if !strings.Contains(err.Error(), "Switzerland") {
		t.Errorf("error = %q, want one that names the country", err)
	}
}

// TestSRFExtractSeriesNestsBySeason walks a show end to end: one file per
// episode rather than one per clip, in the listing's own order, and filed by
// season because there is more than one to tell apart.
func TestSRFExtractSeriesNestsBySeason(t *testing.T) {
	episode := func(urn, show, title string) string {
		return `{"chapterUrn":"` + urn + `","show":{"title":"` + show + `"},"episode":{"title":"` + title + `"},
		"chapterList":[{"urn":"` + urn + `","title":"` + title + `","resourceList":[
		  {"url":"http://download.example.test/a_q60.mp4","quality":"HD","protocol":"HTTP","tokenType":"NONE"}]}]}`
	}
	srv := srfServer(t, map[string]string{
		"urn:srf:video:aaaaaaa2-0000-0000-0000-000000000001": episode(
			"urn:srf:video:aaaaaaa2-0000-0000-0000-000000000001", "A Drama", "The Second (Staffel 2, Folge 1)"),
		"urn:srf:video:aaaaaaa1-0000-0000-0000-000000000001": episode(
			"urn:srf:video:aaaaaaa1-0000-0000-0000-000000000001", "A Drama", "The First (Staffel 1, Folge 1)"),
	}, []string{srfShowListing})

	u, err := ParseURL("https://www.srf.ch/play/tv/sendung/a-drama?id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	res, err := srfWith(t, srv).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "A Drama" {
		t.Errorf("Title = %q", res.Title)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want one per episode — the clips in mediaList must not be queued", len(res.Files))
	}
	// FanOut collects by index, so the listing's order survives however the
	// requests interleaved.
	want := []struct{ name, dir string }{
		{"The Second (Staffel 2, Folge 1).mp4", "Staffel 2"},
		{"The First (Staffel 1, Folge 1).mp4", "Staffel 1"},
	}
	for i, file := range res.Files {
		if file.Name != want[i].name || file.Dir != want[i].dir {
			t.Errorf("file %d = %q in %q, want %q in %q", i, file.Name, file.Dir, want[i].name, want[i].dir)
		}
	}
}

// TestSRFExtractSeriesLeavesOneSeasonFlat covers a strand whose instalments
// belong to no season: a folder per season would say nothing, and there is
// none to name anyway.
func TestSRFExtractSeriesLeavesOneSeasonFlat(t *testing.T) {
	episode := func(urn, title string) string {
		return `{"chapterUrn":"` + urn + `","show":{"title":"A News Strand"},"episode":{"title":"` + title + `"},
		"chapterList":[{"urn":"` + urn + `","title":"` + title + `","resourceList":[
		  {"url":"http://download.example.test/n_q50.mp4","quality":"SD","protocol":"HTTP","tokenType":"NONE"}]}]}`
	}
	srv := srfServer(t, map[string]string{
		"urn:srf:video:bbbbbbb0-0000-0000-0000-000000000002": episode(
			"urn:srf:video:bbbbbbb0-0000-0000-0000-000000000002", "A News Strand vom 21.08.2026"),
		"urn:srf:video:bbbbbbb0-0000-0000-0000-000000000001": episode(
			"urn:srf:video:bbbbbbb0-0000-0000-0000-000000000001", "A News Strand vom 20.08.2026"),
	}, []string{srfUnnumberedListing})

	u, err := ParseURL("https://www.srf.ch/play/tv/sendung/a-strand?id=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	res, err := srfWith(t, srv).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(res.Files))
	}
	for _, file := range res.Files {
		if file.Dir != "" {
			t.Errorf("%q was filed under %q, though the show numbers no seasons", file.Name, file.Dir)
		}
		// The instalment is already named after the strand, so the show is
		// not prefixed again.
		if strings.HasPrefix(file.Name, "A News Strand - ") {
			t.Errorf("Name = %q, which says the strand twice", file.Name)
		}
	}
}

// TestSRFExtractSeriesFollowsPaging covers a show longer than one page, and
// the guard that stops a listing which keeps answering with what has already
// been taken.
func TestSRFExtractSeriesFollowsPaging(t *testing.T) {
	episode := func(urn string) string {
		return `{"chapterUrn":"` + urn + `","show":{"title":"A News Strand"},"episode":{"title":"An Instalment"},
		"chapterList":[{"urn":"` + urn + `","title":"An Instalment","resourceList":[
		  {"url":"http://download.example.test/n_q50.mp4","quality":"SD","protocol":"HTTP","tokenType":"NONE"}]}]}`
	}
	const secondPage = `{"show":{"title":"A News Strand"},"episodeList":[
	  {"id":"n0","title":"An older instalment","fullLengthUrn":"urn:srf:video:bbbbbbb0-0000-0000-0000-000000000000"}]}`

	srv := srfServer(t, map[string]string{
		"urn:srf:video:bbbbbbb0-0000-0000-0000-000000000002": episode("urn:srf:video:bbbbbbb0-0000-0000-0000-000000000002"),
		"urn:srf:video:bbbbbbb0-0000-0000-0000-000000000001": episode("urn:srf:video:bbbbbbb0-0000-0000-0000-000000000001"),
		"urn:srf:video:bbbbbbb0-0000-0000-0000-000000000000": episode("urn:srf:video:bbbbbbb0-0000-0000-0000-000000000000"),
		// The repeated page contributes nothing new and must end the walk.
	}, []string{srfUnnumberedListing, secondPage, srfUnnumberedListing})

	u, err := ParseURL("https://www.srf.ch/play/tv/sendung/a-strand?id=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	res, err := srfWith(t, srv).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 3 {
		t.Fatalf("got %d files, want the 3 distinct episodes across both pages", len(res.Files))
	}
}

// TestSRFExtractSeriesReportsWhyNothingIsPlayable covers a whole show behind
// the geo-block, which is how the Swiss-only half of the catalogue looks from
// anywhere else. One reason, once — not several hundred failures.
func TestSRFExtractSeriesReportsWhyNothingIsPlayable(t *testing.T) {
	blocked := func(urn string) string {
		return `{"chapterUrn":"` + urn + `","show":{"title":"A Drama"},"episode":{"title":"An Episode"},
		"chapterList":[{"urn":"` + urn + `","title":"An Episode","blockReason":"GEOBLOCK","playableAbroad":false}]}`
	}
	srv := srfServer(t, map[string]string{
		"urn:srf:video:aaaaaaa2-0000-0000-0000-000000000001": blocked("urn:srf:video:aaaaaaa2-0000-0000-0000-000000000001"),
		"urn:srf:video:aaaaaaa1-0000-0000-0000-000000000001": blocked("urn:srf:video:aaaaaaa1-0000-0000-0000-000000000001"),
	}, []string{srfShowListing})

	u, err := ParseURL("https://www.srf.ch/play/tv/sendung/a-drama?id=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	_, err = srfWith(t, srv).Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a wholly blocked show resolved")
	}
	if !strings.Contains(err.Error(), "Switzerland") {
		t.Errorf("error = %q, want the block reason carried up from the episodes", err)
	}
	if !strings.Contains(err.Error(), "2 episodes") {
		t.Errorf("error = %q, want it to say how many were tried", err)
	}
}

// TestSRFExtractWithoutAURNSaysWhatALinkLooksLike covers the Play section
// pages that name nothing to download.
func TestSRFExtractWithoutAURNSaysWhatALinkLooksLike(t *testing.T) {
	u, err := ParseURL("https://www.srf.ch/play/tv/live")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewSRF(nil).Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a page naming nothing resolved")
	}
	if !strings.Contains(err.Error(), "urn=") {
		t.Errorf("error = %q, want one that shows the link shape", err)
	}
}
