package extractor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// zdfProgramme is one programme in the shape futura serves one. Every trap
// this extractor exists to get right is reproduced.
//
//   - The audio description is published at every quality, "ad" beside
//     "main", and at the top quality it is listed first. It differs from the
//     programme in nothing a ranking by quality can see.
//   - An encrypted rendition leads the list, at the best quality of all, so
//     that skipping it has to be deliberate.
//   - The same rendition appears on two media hosts, the second of which the
//     player marks user-agent-restricted.
//   - A rendition arrives under a type this does not know.
//   - downloadFilesize is printed and is a lie about the file taken here: it
//     is the length of the 1628k rendition, and what wins is the 6628k one.
const zdfProgramme = `{
  "document": {
    "id": "a-programme-100",
    "type": "video",
    "contentType": "episode",
    "titel": "A Programme",
    "beschreibung": "One sentence about it.",
    "seasonNumber": "2026",
    "episodeNumber": "22",
    "geoLocation": "none",
    "length": 2673,
    "downloadFilesize": "408 MB",
    "isDownloadAllowed": true,
    "formitaeten": [
      { "type": "h264_aac_mp4_http_na_na", "quality": "fhd", "hd": true, "mimeType": "video/mp4",
        "language": "deu", "class": "main", "hasVideoDrm": true,
        "url": "https://cdn.example.test/none/a-programme_6628k_p61v17_drm.mp4" },
      { "type": "vp9_opus_webm_http_na_na", "quality": "hd", "hd": true, "mimeType": "video/webm",
        "language": "deu", "class": "main",
        "url": "https://cdn.example.test/none/a-programme_2128k_p18v15.webm" },
      { "type": "h264_aac_ts_http_m3u8_http", "quality": "auto", "hd": true,
        "mimeType": "application/x-mpegURL", "language": "deu", "class": "main",
        "url": "https://vod.example.test/i/mp4/none/a-programme,_508k_p9,_1628k_p13,_6628k_p61,v17.mp4.csmil/master.m3u8?audiotrack=0:deu:TV%20Ton,1:deu:Audiodeskription" },
      { "type": "h264_aac_ts_http_m3u8_http", "quality": "auto", "hd": true,
        "mimeType": "application/x-mpegURL", "language": "deu", "class": "ad",
        "url": "https://vod.example.test/i/mp4/none/a-programme,_508k_p9,_1628k_p13,_6628k_p61,v17.mp4.csmil/master.m3u8?audiotrack=0:deu:Audiodeskription,1:deu:TV%20Ton" },
      { "type": "h264_aac_mp4_http_na_na", "quality": "fhd", "hd": true, "mimeType": "video/mp4",
        "language": "deu", "class": "ad",
        "url": "https://cdn.example.test/none/a-programme_a3a4_6628k_p61v17.mp4" },
      { "type": "h264_aac_mp4_http_na_na", "quality": "fhd", "hd": true, "mimeType": "video/mp4",
        "language": "deu", "class": "main",
        "url": "https://cdn.example.test/none/a-programme_a1a2_6628k_p61v17.mp4" },
      { "type": "h264_aac_mp4_http_na_na", "quality": "hd", "hd": true, "mimeType": "video/mp4",
        "language": "deu", "class": "main",
        "url": "https://cdn.example.test/none/a-programme_a1a2_3328k_p15v17.mp4" },
      { "type": "h264_aac_mp4_http_na_na", "quality": "veryhigh", "hd": false, "mimeType": "video/mp4",
        "language": "deu", "class": "main",
        "url": "https://cdn.example.test/none/a-programme_a1a2_1628k_p13v17.mp4" },
      { "type": "h264_aac_mp4_http_na_na", "quality": "veryhigh", "hd": false, "mimeType": "video/mp4",
        "language": "deu", "class": "main",
        "url": "https://legacy.example.test/none/a-programme_a1a2_1628k_p13v17.mp4" }
    ]
  },
  "cluster": []
}`

// zdfDescribedOnlyAtTheTop is the case the class filter exists for, rather
// than a class preference: the audio description is the only rendition at
// the best quality, so anything that ranks by quality first and settles the
// class afterwards downloads the narrated soundtrack in 1080p.
const zdfDescribedOnlyAtTheTop = `{
  "document": {
    "type": "video",
    "titel": "A Described Programme",
    "geoLocation": "none",
    "formitaeten": [
      { "type": "h264_aac_mp4_http_na_na", "quality": "fhd", "class": "ad",
        "url": "https://cdn.example.test/none/described_a3a4_6628k_p61v17.mp4" },
      { "type": "h264_aac_mp4_http_na_na", "quality": "hd", "class": "main",
        "url": "https://cdn.example.test/none/described_a1a2_3328k_p15v17.mp4" }
    ]
  }
}`

// zdfAnnounced is a programme that has not been broadcast yet. It carries no
// renditions at all and one sentence saying when it will, which is the only
// thing anybody can usefully be told about it.
const zdfAnnounced = `{
  "document": {
    "type": "video",
    "contentType": "episode",
    "titel": "An Event",
    "currentVideoType": "novideo",
    "label": "Livestream",
    "videoInfo": "Im Livestream: 23.08.2026, 14:00 - 18:55",
    "availabilityInfo": null,
    "geoLocation": null,
    "formitaeten": []
  },
  "cluster": []
}`

// zdfWatershed is the other document with no renditions, and the reason
// videoInfo cannot simply be quoted. Its own explanation is in
// availabilityInfo — the programme is behind a watershed, so ZDF publishes
// nothing for it outside the small hours — while videoInfo holds the listing
// line printed under a thumbnail, which says nothing at all and would read
// as though it were the reason.
const zdfWatershed = `{
  "document": {
    "type": "video",
    "contentType": "episode",
    "titel": "A Late Programme",
    "geoLocation": "de",
    "videoInfo": "43 min | 17.07.2026 | UT",
    "availabilityInfo": "Video verfügbar bis 25.07.2028, in Deutschland, von 22 bis 6 Uhr",
    "timetolive": "25.07.2028 23:59",
    "formitaeten": []
  },
  "cluster": []
}`

// zdfRegionLocked is a licensed programme. The restriction is a code in a
// field rather than a sentence for the viewer, and it says which region the
// licence covers — not whether the caller is in it.
const zdfRegionLocked = `{
  "document": {
    "type": "video",
    "titel": "A Licensed Film",
    "geoLocation": "dach",
    "formitaeten": [
      { "type": "h264_aac_mp4_http_na_na", "quality": "hd", "class": "main",
        "url": "%s/dach/a-licensed-film_3328k_p15v15.mp4" }
    ]
  }
}`

// zdfShow is a show page. ZDF's clusters are editorial shelves rather than a
// season table, so three things have to be got right at once: the featured
// entry at the top is a repeat of one further down, a shelf mixes trailers
// in with the episodes, and the last shelf is cross-promotion for other
// pages entirely.
const zdfShow = `{
  "document": {
    "id": "a-show-100",
    "type": "brand",
    "contentType": "brand",
    "titel": "A Show",
    "videoCount": 5
  },
  "cluster": [
    { "name": "", "type": "teaserContent", "teaser": [
      { "type": "video", "id": "third-100", "titel": "Third", "seasonNumber": "2", "episodeNumber": "1", "contentType": "episode", "geoLocation": "none" }
    ] },
    { "name": "A Show - Staffel 2", "type": "teaser", "teaser": [
      { "type": "video", "id": "trailer-100", "titel": "Trailer: A Show", "seasonNumber": null, "episodeNumber": null, "contentType": "clip", "geoLocation": "none" },
      { "type": "video", "id": "third-100", "titel": "Third", "seasonNumber": "2", "episodeNumber": "1", "contentType": "episode", "geoLocation": "none" },
      { "type": "video", "id": "fourth-100", "titel": "Fourth", "seasonNumber": "2", "episodeNumber": "2", "contentType": "episode", "geoLocation": "dach" }
    ] },
    { "name": "Staffel 1", "type": "teaser", "teaser": [
      { "type": "video", "id": "first-100", "titel": "First", "seasonNumber": "1", "episodeNumber": "1", "contentType": "episode", "geoLocation": "none" },
      { "type": "video", "id": "", "titel": "A listing entry with no id", "seasonNumber": "1" },
      { "type": "video", "id": "second-100", "titel": "Second", "seasonNumber": "1", "episodeNumber": "2", "contentType": "episode", "geoLocation": "none" }
    ] },
    { "name": "Mehr aus dieser Welt", "type": "teaser", "teaser": [
      { "type": "brand", "id": "another-show-100", "titel": "Another Show", "seasonNumber": null, "contentType": "brand", "geoLocation": null },
      { "type": "topic", "id": "a-topic-100", "titel": "A Topic", "contentType": "topic" },
      { "type": "category", "id": "a-category-100", "titel": "A Category", "contentType": "category" }
    ] }
  ]
}`

// zdfFilmPage is a film's own page. Futura describes it as a page, so it
// carries no renditions, and the clusters list nothing — the one video it
// plays lives at an id the URL never mentions and only the markup names.
//
// The recommendations beside it are why the middle path segment has to
// match: they are ordinary /video/ links, indistinguishable by shape, and
// following them would download somebody else's film. The page names its own
// video twice, once as an anchor and once inside an escaped payload.
const zdfFilmPage = `<!doctype html><html><head><title>A Film - ZDFmediathek</title></head><body>
<a href="/filme/a-film-movie-100">A Film</a>
<a href="/video/filme/a-film-movie-100/a-film-100">Ganzen Film ansehen</a>
<script>self.__next_f.push([1,"{\"sharingUrl\":\"https://www.zdf.de/video/filme/a-film-movie-100/a-film-100\",\"ptmdTemplate\":\"/tmd/2/{playerId}/vod/ptmd/mediathek/000000_a_film/8\"}"])</script>
<a href="/video/filme/another-film-movie-100/another-film-100">Another Film</a>
<a href="/video/dokus/a-documentary-100/a-documentary-102">A Documentary</a>
</body></html>`

// zdfMasterPlaylist is what ZDF serves for a programme with one soundtrack.
// There is no EXT-X-MEDIA line of any kind, so every variant carries its
// video and its audio in the same segments and concatenation is enough.
const zdfMasterPlaylist = `#EXTM3U
#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=1710048,RESOLUTION=960x540,FRAME-RATE=25.000,CODECS="avc1.4d401f,mp4a.40.2",AVERAGE-BANDWIDTH=1200936,VIDEO-RANGE=SDR,CLOSED-CAPTIONS=NONE
index-f1-v1-a1.m3u8
#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=7024056,RESOLUTION=1920x1080,FRAME-RATE=50.000,CODECS="avc1.64002a,mp4a.40.2",AVERAGE-BANDWIDTH=5203992,VIDEO-RANGE=SDR,CLOSED-CAPTIONS=NONE
index-f3-v1-a1.m3u8

#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=346457,RESOLUTION=1920x1080,CODECS="avc1.64002a",URI="iframes-f3-v1-a1.m3u8",VIDEO-RANGE=SDR
`

// zdfDescribedMasterPlaylist is the same programme once an audio description
// exists for it, and it is the reason the chosen variant is asked rather than
// trusted. The audio has moved into EXT-X-MEDIA renditions with playlists of
// their own, so the variants below carry video and nothing else — while
// still advertising mp4a in CODECS, which describes the presentation rather
// than the segments. Concatenated, they make a silent file of the right
// length that looks like a finished download.
const zdfDescribedMasterPlaylist = `#EXTM3U

#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",NAME="TV Ton",LANGUAGE="de",AUTOSELECT=YES,DEFAULT=YES,CHANNELS="2",URI="index-f7-a1.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",NAME="Audiodeskription",LANGUAGE="de",AUTOSELECT=NO,DEFAULT=NO,CHANNELS="2",URI="index-f8-a1.m3u8"

#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=1609656,RESOLUTION=960x540,FRAME-RATE=25.000,CODECS="avc1.4d401f,mp4a.40.2",AVERAGE-BANDWIDTH=1227736,VIDEO-RANGE=SDR,AUDIO="audio0",CLOSED-CAPTIONS=NONE
index-f1-v1-a1.m3u8
#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=6966904,RESOLUTION=1920x1080,FRAME-RATE=50.000,CODECS="avc1.64002a,mp4a.40.2",AVERAGE-BANDWIDTH=5025816,VIDEO-RANGE=SDR,AUDIO="audio0",CLOSED-CAPTIONS=NONE
index-f3-v1-a1.m3u8
`

const zdfMediaPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-PLAYLIST-TYPE:VOD
#EXTINF:10.000,
segment-1.ts
#EXTINF:10.000,
segment-2.ts
#EXT-X-ENDLIST
`

func zdfTestClient() *httpx.Client {
	return httpx.New("test-agent", "de-DE", 0, 5*time.Second)
}

func zdfParse(t *testing.T, body string) *zdfResponse {
	t.Helper()
	var res zdfResponse
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	return &res
}

// TestZDFFileTakesTheLargestOrdinaryRendition is the whole selection in one
// assertion: over the type, over the DRM flag, over the audio class, and
// over the quality ladder, on a list that punishes getting any of them
// wrong.
func TestZDFFileTakesTheLargestOrdinaryRendition(t *testing.T) {
	doc := zdfParse(t, zdfProgramme)

	file, err := (&ZDF{}).file(context.Background(), &doc.Document, newZDFReach(nil))
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if want := "https://cdn.example.test/none/a-programme_a1a2_6628k_p61v17.mp4"; file.URL != want {
		t.Errorf("chose %q, want the unencrypted 1080p file with the ordinary soundtrack (%q)", file.URL, want)
	}
	if len(file.Segments) != 0 {
		t.Errorf("took %d segments, want the progressive file rather than the playlist", len(file.Segments))
	}
	if file.Name != ".mp4" {
		t.Errorf("extension %q, want .mp4", file.Name)
	}

	// downloadFilesize is printed on this document and describes a different
	// rendition. A length that is merely close is worse than none at all: the
	// downloader skips a file whose length matches what is on disk.
	if file.Size != -1 {
		t.Errorf("Size %d, want -1 — the only figure ZDF prints belongs to another rendition", file.Size)
	}
	if file.SizeApprox {
		t.Error("SizeApprox is set, but the printed length is not an approximation of this file")
	}
}

// TestZDFAudioDescriptionIsExcludedNotDemoted pins the reason the class is
// filtered rather than ranked. ZDF does not always describe every quality,
// and the narrated soundtrack at 1080p must lose to the ordinary one at 720p
// — it is a different programme, not a better copy of this one.
func TestZDFAudioDescriptionIsExcludedNotDemoted(t *testing.T) {
	doc := zdfParse(t, zdfDescribedOnlyAtTheTop)

	best, ok := zdfPick(doc.Document.Formitaeten)
	if !ok {
		t.Fatal("nothing was chosen")
	}
	if want := "https://cdn.example.test/none/described_a1a2_3328k_p15v17.mp4"; best.URL != want {
		t.Errorf("chose %q, want the ordinary soundtrack at a lower quality (%q)", best.URL, want)
	}
}

// TestZDFPickFallsBackToThePlaylist covers the programme with no progressive
// file. The playlist is the second choice for a reason — it is not rangeable
// — so it must never be taken while a file is on offer.
func TestZDFPickFallsBackToThePlaylist(t *testing.T) {
	doc := zdfParse(t, zdfProgramme)

	var playlistsOnly []zdfFormat
	for _, f := range doc.Document.Formitaeten {
		if f.Type != zdfProgressive {
			playlistsOnly = append(playlistsOnly, f)
		}
	}
	best, ok := zdfPick(playlistsOnly)
	if !ok {
		t.Fatal("nothing was chosen")
	}
	if best.Type != zdfPlaylist {
		t.Errorf("chose a %q rendition, want the playlist", best.Type)
	}
	if !strings.Contains(best.URL, "audiotrack=0:deu:TV%20Ton") {
		t.Errorf("chose %q, want the manifest whose first audio track is the ordinary one", best.URL)
	}
	// The webm in that list is neither a file this fetches nor a playlist,
	// and an unrecognised type is passed over rather than attempted.
	if strings.Contains(best.URL, ".webm") {
		t.Error("chose the webm rendition, which arrives under a type this does not accept")
	}
}

// TestZDFMasterPlaylistIsSelfContained is the ordinary case, and the reason
// the playlist fallback is worth having at all.
func TestZDFMasterPlaylistIsSelfContained(t *testing.T) {
	base, err := ParseURL("https://vod.example.test/i/mp4/none/a.csmil/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(zdfMasterPlaylist, base)
	if len(variants) != 2 {
		t.Fatalf("parsed %d variants, want 2 (the I-frame entry is not one)", len(variants))
	}
	for _, v := range variants {
		if !v.muxed() {
			t.Errorf("%s reads as video only, but this playlist declares no audio group", v.Resolution)
		}
	}

	best, ok := bestVariant(variants)
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if best.Resolution != "1920x1080" {
		t.Errorf("chose %s, want the largest rendition", best.Resolution)
	}
	if got := playlistExtension(best); got != ".ts" {
		t.Errorf("extension %q, want .ts", got)
	}
}

// TestZDFDescribedPlaylistIsRefused is the deciding constraint, end to end.
// Nothing in this playlist can be joined by concatenation, and the failure it
// guards against is silent: a file of the right length that plays no sound.
func TestZDFDescribedPlaylistIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "application/x-mpegURL")
		if strings.HasSuffix(r.URL.Path, "/master.m3u8") {
			_, _ = w.Write([]byte(zdfDescribedMasterPlaylist))
			return
		}
		_, _ = w.Write([]byte(zdfMediaPlaylist))
	}))
	defer srv.Close()

	doc := &zdfDocument{
		Type:  zdfVideo,
		Titel: "A Described Programme",
		Formitaeten: []zdfFormat{{
			Type: zdfPlaylist, Quality: "auto", Class: zdfMainAudio,
			URL: srv.URL + "/i/mp4/none/a.csmil/master.m3u8",
		}},
	}

	_, err := (&ZDF{client: zdfTestClient()}).file(context.Background(), doc, newZDFReach(nil))
	if err == nil {
		t.Fatal("accepted a playlist whose audio lives in renditions of its own")
	}
	if !strings.Contains(err.Error(), "audio in a playlist of its own") {
		t.Errorf("refused with %q, want the reason named", err)
	}
}

// TestZDFPlaylistResolvesToSegments is the same path when the playlist is
// usable, so the refusal above is known to be about the audio rather than
// about playlists in general.
func TestZDFPlaylistResolvesToSegments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "application/x-mpegURL")
		if strings.HasSuffix(r.URL.Path, "/master.m3u8") {
			_, _ = w.Write([]byte(zdfMasterPlaylist))
			return
		}
		_, _ = w.Write([]byte(zdfMediaPlaylist))
	}))
	defer srv.Close()

	doc := &zdfDocument{
		Type:  zdfVideo,
		Titel: "A Programme",
		Formitaeten: []zdfFormat{{
			Type: zdfPlaylist, Quality: "auto", Class: zdfMainAudio,
			URL: srv.URL + "/i/mp4/none/a.csmil/master.m3u8",
		}},
	}

	file, err := (&ZDF{client: zdfTestClient()}).file(context.Background(), doc, newZDFReach(nil))
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if len(file.Segments) != 2 {
		t.Fatalf("got %d segments, want 2", len(file.Segments))
	}
	if file.URL != "" {
		t.Errorf("URL %q is set as well as the segments", file.URL)
	}
	if file.Name != ".ts" {
		t.Errorf("extension %q, want .ts", file.Name)
	}
}

// TestZDFAnnouncedProgrammeQuotesTheBroadcaster covers the document with
// nothing to fetch. It is the only thing that knows why, and it says so in a
// sentence written for a viewer.
func TestZDFAnnouncedProgrammeQuotesTheBroadcaster(t *testing.T) {
	doc := zdfParse(t, zdfAnnounced)

	_, err := (&ZDF{}).file(context.Background(), &doc.Document, newZDFReach(nil))
	if err == nil {
		t.Fatal("a document with no renditions was accepted")
	}
	if !strings.Contains(err.Error(), "Im Livestream: 23.08.2026, 14:00 - 18:55") {
		t.Errorf("refused with %q, want ZDF's own note passed through", err)
	}
	// The refusal is written to follow the programme's name, since that is
	// where both callers put it.
	if !strings.HasPrefix(err.Error(), "offers no stream") {
		t.Errorf("refusal %q does not read as a sentence about a programme", err)
	}
}

// TestZDFWatershedQuotesTheRightField is why videoInfo cannot simply be
// passed through: the field holds an explanation for one kind of document
// and a listing line for another, and the listing line would read as the
// reason while saying nothing.
func TestZDFWatershedQuotesTheRightField(t *testing.T) {
	doc := zdfParse(t, zdfWatershed)

	_, err := (&ZDF{}).file(context.Background(), &doc.Document, newZDFReach(nil))
	if err == nil {
		t.Fatal("a document with no renditions was accepted")
	}
	if !strings.Contains(err.Error(), "von 22 bis 6 Uhr") {
		t.Errorf("refused with %q, want the availability sentence", err)
	}
	if strings.Contains(err.Error(), "43 min") {
		t.Errorf("refused with %q, which quotes the listing line rather than a reason", err)
	}
}

// TestZDFRegionIsProbedOnceAndNamed is the geo-block, and both halves matter.
//
// A restriction code is not a refusal — most of this catalogue is licensed
// for Germany and it is people in Germany who will mostly be downloading it
// — so what the code does is decide that a question is worth asking. The
// answer is then reused for everything licensed the same way, which is what
// keeps a season of thirty from costing thirty probes.
func TestZDFRegionIsProbedOnceAndNamed(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			probes.Add(1)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	var res zdfResponse
	if err := json.Unmarshal([]byte(strings.Replace(zdfRegionLocked, "%s", srv.URL, 1)), &res); err != nil {
		t.Fatal(err)
	}

	z := &ZDF{client: zdfTestClient()}
	reach := newZDFReach(z.client)
	for i := 0; i < 3; i++ {
		_, err := z.file(context.Background(), &res.Document, reach)
		if err == nil {
			t.Fatal("a programme the media host refuses was accepted")
		}
		if want := "is licensed for Germany, Austria and Switzerland only"; !strings.Contains(err.Error(), want) {
			t.Errorf("refused with %q, want the licence named (%q)", err, want)
		}
	}
	if got := probes.Load(); got != 1 {
		t.Errorf("asked the media host %d times, want 1 — the answer is about the address, not the file", got)
	}
}

// TestZDFRegionWithinReachIsNotRefused is the other half, and the bug worth
// guarding against: refusing on the field alone turns away the audience the
// broadcaster made this for.
func TestZDFRegionWithinReachIsNotRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "video/mp4")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var res zdfResponse
	if err := json.Unmarshal([]byte(strings.Replace(zdfRegionLocked, "%s", srv.URL, 1)), &res); err != nil {
		t.Fatal(err)
	}

	z := &ZDF{client: zdfTestClient()}
	file, err := z.file(context.Background(), &res.Document, newZDFReach(z.client))
	if err != nil {
		t.Fatalf("a programme this address can reach was refused: %v", err)
	}
	if !strings.HasSuffix(file.URL, "_3328k_p15v15.mp4") {
		t.Errorf("chose %q, want the licensed programme's own file", file.URL)
	}
}

// TestZDFBlockedNamesWhatItCan spells out the codes and passes on the ones it
// does not know rather than inventing a region for them.
func TestZDFBlockedNamesWhatItCan(t *testing.T) {
	tests := map[string]string{
		"de":      "is licensed for Germany only, and this address is outside that region",
		"dach":    "is licensed for Germany, Austria and Switzerland only, and this address is outside that region",
		"ebu":     "is licensed for the European Broadcasting Union's member countries only, and this address is outside that region",
		"nordics": "is licensed for the region ZDF calls nordics only, and this address is outside that region",
	}
	for region, want := range tests {
		t.Run(region, func(t *testing.T) {
			if got := (&zdfBlocked{region: region}).Error(); got != want {
				t.Errorf("Error() = %q, want %q", got, want)
			}
		})
	}
}

func TestZDFRestricted(t *testing.T) {
	tests := map[string]bool{
		"none": false,
		"NONE": false,
		"":     false,
		" ":    false,
		"de":   true,
		"dach": true,
		"ebu":  true,
	}
	for region, want := range tests {
		if got := zdfRestricted(region); got != want {
			t.Errorf("zdfRestricted(%q) = %v, want %v", region, got, want)
		}
	}
}

// TestZDFShowListsEveryVideoOnce covers the shelves. The featured entry
// repeats one further down, the shelves mix trailers in with the episodes,
// and the last shelf is other pages entirely.
func TestZDFShowListsEveryVideoOnce(t *testing.T) {
	res := zdfParse(t, zdfShow)

	refs := res.videos()
	want := []zdfRef{
		{id: "third-100", name: "Third", season: "2"},
		{id: "trailer-100", name: "Trailer: A Show", season: ""},
		{id: "fourth-100", name: "Fourth", season: "2"},
		{id: "first-100", name: "First", season: "1"},
		{id: "second-100", name: "Second", season: "1"},
	}
	if len(refs) != len(want) {
		t.Fatalf("listed %d videos, want %d: %+v", len(refs), len(want), refs)
	}
	for i, ref := range refs {
		if ref != want[i] {
			t.Errorf("video %d is %+v, want %+v", i, ref, want[i])
		}
	}
}

// TestZDFSeasonFolder pins the naming. ZDF numbers a drama's seasons 1, 2, 3
// and a magazine's by the calendar year, so the two would otherwise sit side
// by side as "5" and "2023" meaning different things.
func TestZDFSeasonFolder(t *testing.T) {
	tests := map[string]string{
		"1":    "Staffel 1",
		"5":    "Staffel 5",
		"12":   "Staffel 12",
		"2023": "2023",
		"2026": "2026",
	}
	for season, want := range tests {
		if got := zdfSeasonFolder(season); got != want {
			t.Errorf("zdfSeasonFolder(%q) = %q, want %q", season, got, want)
		}
	}
}

// TestZDFVideoLinksKeepOnlyThePagesOwn covers the film page. Following a
// recommendation instead would download somebody else's film under this
// page's name, and the two are indistinguishable by shape.
func TestZDFVideoLinksKeepOnlyThePagesOwn(t *testing.T) {
	ids := zdfVideoLinks(zdfFilmPage, "a-film-movie-100")
	if len(ids) != 1 {
		t.Fatalf("found %v, want just the page's own video", ids)
	}
	if ids[0] != "a-film-100" {
		t.Errorf("found %q, want a-film-100", ids[0])
	}
	if got := zdfVideoLinks(zdfFilmPage, "a-page-that-plays-nothing-100"); len(got) != 0 {
		t.Errorf("found %v for a page that links to no video of its own", got)
	}
}

func TestZDFDocumentID(t *testing.T) {
	tests := map[string]string{
		"https://www.zdf.de/video/dokus/a-show-100/a-programme-100":      "a-programme-100",
		"https://www.zdf.de/dokus/a-show-100":                            "a-show-100",
		"https://www.zdf.de/filme/a-film-movie-100":                      "a-film-movie-100",
		"https://www.zdf.de/serien/a-drama-102/":                         "a-drama-102",
		"https://www.zdf.de/video/dokus/a-show-100/a-programme-100?at=1": "a-programme-100",
		"https://www.zdf.de/":                                            "",
		"https://www.zdf.de":                                             "",
	}
	for raw, want := range tests {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := zdfDocumentID(u); got != want {
			t.Errorf("zdfDocumentID(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestZDFRank(t *testing.T) {
	// Ordered worst to best, so each must rank strictly above the one before.
	ladder := []string{"low", "med", "high", "veryhigh", "hd", "fhd", "auto"}
	for i := 1; i < len(ladder); i++ {
		if zdfRank(ladder[i]) <= zdfRank(ladder[i-1]) {
			t.Errorf("%q does not rank above %q", ladder[i], ladder[i-1])
		}
	}
	// A name ZDF has not used before is usable but never preferred, so a
	// relabelled ladder still yields a download rather than nothing.
	if zdfRank("uhd") >= zdfRank("low") {
		t.Error("an unrecognised quality outranks a known one")
	}
	if _, ok := zdfBestOf([]zdfFormat{{
		Type: zdfProgressive, Quality: "8k", Class: zdfMainAudio,
		URL: "https://cdn.example.test/none/a.mp4",
	}}, zdfProgressive); !ok {
		t.Error("a rendition with an unrecognised quality was refused although it was the only one")
	}
}

func TestZDFMatch(t *testing.T) {
	z := &ZDF{}
	tests := map[string]bool{
		"https://www.zdf.de/dokus/a-show-100":                       true,
		"https://zdf.de/video/dokus/a-show-100/a-programme-100":     true,
		"https://zdf-prod-futura.zdf.de/mediathekV2/document/x-100": true,
		"https://www.zdf.example.test/dokus/a-show-100":             false,
		"https://notzdf.de/dokus/a-show-100":                        false,
		"https://www.ardmediathek.de/video/x":                       false,
	}
	for raw, want := range tests {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := z.Match(u); got != want {
			t.Errorf("Match(%s) = %v, want %v", raw, got, want)
		}
	}
}
