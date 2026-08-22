package extractor

import (
	"encoding/json"
	"strings"
	"testing"
)

// rtveVideoDocument is one video's own document, in the shape the catalogue
// serves it. Four of its fields are traps, and every one is set here the way
// the live API sets it on assets that download and play perfectly well as
// H.264 and AAC: hasDRM and requireLogged are true across the whole
// catalogue, notDownloadable and paidContent across whole series. Anything
// that consulted them would refuse this.
const rtveVideoDocument = `{
  "page": {
    "items": [
      {
        "id": "1000001",
        "title": "Something",
        "shortTitle": "Something",
        "longTitle": "A Programme - T2 - Episodio 7: Something",
        "temporadaShortTitle": "T2",
        "episode": 7,
        "contentType": "video",
        "hasDRM": true,
        "requireLogged": true,
        "notDownloadable": true,
        "paidContent": true,
        "geolocalizado": false,
        "allowedInCountry": true,
        "country": "ES",
        "programInfo": { "id": "9000", "title": "A Programme" },
        "qualities": [
          { "identifier": 1, "preset": "HD_FULL",  "filesize": 2147000000, "type": "application/mp4", "width": 1920, "height": 1080, "bitRate": 4500 },
          { "identifier": 2, "preset": "HD_READY", "filesize": 1310000000, "type": "application/mp4", "width": 1280, "height": 720,  "bitRate": 2750 },
          { "identifier": 3, "preset": "HQ",       "filesize":  880000000, "type": "application/mp4", "width": 1024, "height": 576,  "bitRate": 1850 },
          { "identifier": 4, "preset": "HIGH",     "filesize":  500000000, "type": "application/mp4", "width":  640, "height": 360,  "bitRate": 1000 }
        ]
      }
    ],
    "number": 1,
    "total": 1,
    "totalPages": 1
  }
}`

// rtveSeasonListing is a season's episodes, and carries the split this host
// turns on. One entry is licensed by territory and one is not, exactly as the
// live listing reports — but both also claim allowedInCountry true and place
// the caller in "ES", because a listing document answers availability for
// Spain whoever asked for it. The second entry's own document, below, tells
// the truth about the same id.
const rtveSeasonListing = `{
  "page": {
    "items": [
      {
        "id": "1000001",
        "longTitle": "A Programme - T2 - Episodio 7: Something",
        "temporadaShortTitle": "T2",
        "geolocalizado": false,
        "allowedInCountry": true,
        "country": "ES",
        "programInfo": { "id": "9000", "title": "A Programme" },
        "qualities": [
          { "preset": "HD_FULL", "filesize": 2147000000, "type": "application/mp4", "width": 1920, "height": 1080 }
        ]
      },
      {
        "id": "1000002",
        "longTitle": "A Programme - T2 - Episodio 8: Something else",
        "temporadaShortTitle": "T2",
        "geolocalizado": true,
        "allowedInCountry": true,
        "country": "ES",
        "programInfo": { "id": "9000", "title": "A Programme" },
        "qualities": [
          { "preset": "HD_FULL", "filesize": 2011000000, "type": "application/mp4", "width": 1920, "height": 1080 }
        ]
      }
    ],
    "number": 1,
    "total": 2,
    "totalPages": 1
  }
}`

// rtveRestrictedVideo is that second listing entry asked about on its own.
// The country is the caller's now rather than Spain, and the availability
// answer has flipped. Nothing else about the document changed.
const rtveRestrictedVideo = `{
  "page": {
    "items": [
      {
        "id": "1000002",
        "longTitle": "A Programme - T2 - Episodio 8: Something else",
        "temporadaShortTitle": "T2",
        "geolocalizado": true,
        "allowedInCountry": false,
        "country": "SE",
        "programInfo": { "id": "9000", "title": "A Programme" }
      }
    ],
    "number": 1,
    "total": 1,
    "totalPages": 1
  }
}`

// rtveMasterDocument is an asset whose renditions include ORIGINAL, the
// delivery master. It is listed at 1080p with a believable size and the
// player is never handed it: an asset offering exactly these three is served
// the HIGH file. Ranking by resolution would take ORIGINAL and announce a
// length seven times the one that arrives.
const rtveMasterDocument = `{
  "page": { "items": [ {
    "id": "1000003",
    "longTitle": "A Programme - A clip",
    "geolocalizado": false,
    "allowedInCountry": true,
    "country": "ES",
    "qualities": [
      { "preset": "ORIGINAL", "filesize": 36217767, "type": "application/mp4", "width": 1920, "height": 1080 },
      { "preset": "HIGH",     "filesize":  4800439, "type": "application/mp4", "width":  640, "height": 360 },
      { "preset": "MED",      "filesize":  3576852, "type": "application/mp4", "width":  480, "height": 270 }
    ]
  } ], "totalPages": 1 }
}`

// rtveIncompleteQualities is the other way a size record goes wrong: the
// rendition that will arrive carries no filesize at all, while a smaller one
// does. Nulls in this list are common enough to expect.
const rtveIncompleteQualities = `{
  "page": { "items": [ {
    "id": "1000004",
    "longTitle": "A Programme - Another clip",
    "geolocalizado": false,
    "allowedInCountry": true,
    "qualities": [
      { "preset": "HD_FULL",  "filesize": null,      "type": "application/mp4", "width": 1920, "height": 1080 },
      { "preset": "HD_READY", "filesize": 500000000, "type": "application/mp4", "width": 1280, "height": 720 }
    ]
  } ], "totalPages": 1 }
}`

// rtveMasterPlaylist is RTVE's own shape, and it is what makes the playlist
// usable as a fallback at all: there is no EXT-X-MEDIA entry anywhere in the
// document, so the variant's own segments carry video and audio together. The
// variant's name says the same thing — Unified Streaming writes
// "-audio=…-video=…" for one stream holding both.
const rtveMasterPlaylist = `#EXTM3U
#EXT-X-VERSION:3
## Created with Unified Streaming Platform (version=1.15.13-32113)

# variants
#EXT-X-STREAM-INF:BANDWIDTH=4975000,CODECS="mp4a.40.2,avc1.640029",RESOLUTION=1920x1080,FRAME-RATE=25,VIDEO-RANGE=SDR
1000001-audio=193356-video=4499707.m3u8?idasset=1000001&hls_client_manifest_version=3
`

// rtveMediaListDocument is a media list in the encoding the live host answers
// with: base64 text rather than image bytes, one URL per PNG tEXt chunk, each
// chunk carrying the alphabet its own digits index. It was produced by the
// same algorithm the host uses — written from a captured response, checked
// against it, then run backwards over URLs that name nothing real.
//
// Three properties of the real thing are reproduced deliberately. The chunks
// follow an IHDR that must be passed over and precede an IEND that must stop
// the walk; the playlist sits inside a directory named after the progressive
// file, so ".mp4" occurs in the path of the entry that is not the file; and
// that entry comes first, so anything picking the earliest match picks wrong.
const rtveMediaListDocument = `
iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAAAAADhZOFXAAAA7nRFWHQwN293VDIxVi4/PU
ktSUR1ZWhtYjh4JmQ1alNhekAAZ0xtb3k3bXQvM2h2NEtTL0pxLmxpPWtaLnZhSm82NUVz
d2VwPXlqMWFCfjZEeEl3VV90Si5vdmNid1ZlZjRkcFM6eXhIOSMxNjkyNjMyMzQyMjc5Nj
IyNTk1MzgxNzgxMDQ4MjM3ODIzMzMxNTM0NzAyMjc2ODEzMjA2MTcyOTMyMTI1MzMzODIz
MzY1MjIwMjUzMjE3ODIxNDczNzk5MzEzMjQ2MTM5NzE2MjI4MjIyOTAzMjc3MzA2MDUwMT
MyMzE3MzMxNDQyNTA2MDc4eRDx/wAAANJ0RVh0eDAmb2xuRTZ4ODYvM0xNazAxUjVlL2du
XzdhTnNqADlVOmQ/aUBGbkAwN19AZC5hOFhxPUt+dFJkMWx4Zm0/MUZhP3g6Jmw0X3NCNm
M6X29oLTcvRXdvbXQ/MmE2Yj1ncDkmVHQjMzY0NDEzOTM1OTM4MDgxMjA2MzMzMzAzMjIy
MzE1ODA4MDgwMDAzMzQ2Mjk3MTQzODI1NzAwMzgxNjQ1ODM5MDk4MjEyOTMzMTY5MzMyOT
UxNTU4MjUwMDQzODI1ODIzNzYxNTQwNDIwMjQ1QW8TrgAAAABJRU5ErkJggg==
`

// What rtveMediaListDocument decodes to, in the order its chunks carry it.
const (
	rtveFixturePlaylist = "https://v.example.test/1.mp4/video.m3u8"
	rtveFixtureFile     = "http://f.example.test/1.mp4?i=1"
)

// TestRTVEMediaListDecodes is the cipher end to end, and nothing else here
// can work without it: the catalogue names neither a manifest nor a file, so
// this response is the only route to either.
func TestRTVEMediaListDecodes(t *testing.T) {
	links, err := rtveMediaList(rtveMediaListDocument)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{rtveFixturePlaylist, rtveFixtureFile}
	if len(links) != len(want) {
		t.Fatalf("decoded %d entries (%v), want %d", len(links), links, len(want))
	}
	for i, link := range links {
		if link != want[i] {
			t.Errorf("entry %d = %q, want %q", i, link, want[i])
		}
	}
}

// TestRTVEMediaListRefusesWhatItCannotRead pins the failure mode, which is
// what makes this obfuscation tolerable at all: every way of misreading it
// ends in a parse error rather than in a plausible wrong URL that would
// download something and record it as finished.
func TestRTVEMediaListRefusesWhatItCannotRead(t *testing.T) {
	tests := map[string]string{
		"not base64 at all":         "this is not a media list!",
		"base64 of something else":  "aGVsbG8gd29ybGQgYW5kIHNvbWUgbW9yZSB0ZXh0",
		"a body cut short":          strings.TrimSpace(rtveMediaListDocument)[:120],
		"a PNG carrying no entries": "iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAAAAADhZOFXAAAAAElFTkSuQmCC",
		"nothing at all":            "",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if links, err := rtveMediaList(body); err == nil {
				t.Errorf("accepted it and returned %v", links)
			}
		})
	}
}

// TestRTVEDecodeChunkRefusesMalformedEntries covers the cipher's own guards.
// The index bound is the one that matters: without it a changed encoding is a
// panic rather than an error, and a changed encoding is what this host has
// already done once.
func TestRTVEDecodeChunkRefusesMalformedEntries(t *testing.T) {
	entry := func(keyword, text string) []byte {
		return append([]byte(keyword), append([]byte{0}, text...)...)
	}
	tests := map[string][]byte{
		"no keyword separator":       []byte("abcdef#0123"),
		"no cipher separator":        entry("abcdef", "ghijkl"),
		"a letter among the digits":  entry("abcdefghijklmnop", "qrstuvwx#12x4"),
		"an index past the alphabet": entry("ab", "cd#900009"),
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if got, err := rtveDecodeChunk(payload); err == nil {
				t.Errorf("accepted it and returned %q", got)
			}
		})
	}
}

// TestRTVEAlphabetWalk pins the pattern the alphabet is built with: take a
// character, then drop a run that grows 1, 2, 3, 0 and repeats. That is why a
// hundred and eighty-odd characters of material yield seventy-odd entries.
func TestRTVEAlphabetWalk(t *testing.T) {
	// Capitals are kept, lower-case x is dropped.
	const source = "AxBxxCxxxDExFxxG"
	if got := string(rtveAlphabet([]byte(source), nil)); got != "ABCDEFG" {
		t.Errorf("alphabet = %q, want %q", got, "ABCDEFG")
	}
	// The keyword and the preamble are one run delivered in two pieces, so
	// the walk must not restart at the boundary between them.
	if got := string(rtveAlphabet([]byte(source[:6]), []byte(source[6:]))); got != "ABCDEFG" {
		t.Errorf("split across the NUL the alphabet came out %q", got)
	}
}

// TestRTVEByExtensionReadsThePath is the trap the media list sets. RTVE's
// playlist lives in a directory named after the progressive file, so ".mp4"
// occurs in the manifest's URL as well as the file's — and the manifest comes
// first.
func TestRTVEByExtensionReadsThePath(t *testing.T) {
	links, err := rtveMediaList(rtveMediaListDocument)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(links[0], ".mp4") {
		t.Fatal("the fixture no longer reproduces the trap: its playlist URL carries no .mp4")
	}
	if got := rtveByExtension(links, ".mp4"); got != rtveFixtureFile {
		t.Errorf("picked %q as the file, want %q", got, rtveFixtureFile)
	}
	if got := rtveByExtension(links, ".m3u8"); got != rtveFixturePlaylist {
		t.Errorf("picked %q as the playlist, want %q", got, rtveFixturePlaylist)
	}
	if got := rtveByExtension(links, ".mpd"); got != "" {
		t.Errorf("found a DASH manifest that is not there: %q", got)
	}
}

// TestRTVEExtensionAsksTheSegments covers that same directory naming reaching
// the other side. playlistExtension reads the variant's URL and would call
// this MPEG-TS stream an MP4; a .ts file named .mp4 is one players refuse and
// remux.go never converts.
func TestRTVEExtensionAsksTheSegments(t *testing.T) {
	variant := hlsVariant{URL: "https://v.example.test/1.mp4/1-audio=1-video=2.m3u8?i=1"}
	segments := []string{
		"https://v.example.test/1.mp4/1-audio=1-video=2-1.ts?i=1",
		"https://v.example.test/1.mp4/1-audio=1-video=2-2.ts?i=1",
	}
	if got := playlistExtension(variant); got != ".mp4" {
		t.Fatalf("the trap has gone: playlistExtension = %q, and it was expected to be misled", got)
	}
	if got := rtveExtension(segments, variant); got != ".ts" {
		t.Errorf("rtveExtension = %q, want .ts", got)
	}
	// With no segments to ask, and for a stream whose parts really are
	// fragmented MP4, the general rule stands.
	if got := rtveExtension(nil, variant); got != ".mp4" {
		t.Errorf("rtveExtension with nothing to ask = %q, want the general answer", got)
	}
}

// TestRTVEMasterPlaylistIsSelfContained pins the claim the fallback rests on.
// RTVE declares no audio group at all, so the variant's segments carry both
// streams and concatenating them yields something that plays. A host whose
// best variant were video only would belong on the external downloader.
func TestRTVEMasterPlaylistIsSelfContained(t *testing.T) {
	if strings.Contains(rtveMasterPlaylist, "EXT-X-MEDIA") {
		t.Fatal("the fixture has grown an EXT-X-MEDIA entry; RTVE's playlist has none")
	}
	base, err := ParseURL("https://v.example.test/1.mp4/video.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	variants := parseMasterPlaylist(rtveMasterPlaylist, base)
	if len(variants) != 1 {
		t.Fatalf("parsed %d variants, want 1", len(variants))
	}
	if !variants[0].muxed() {
		t.Error("the variant reads as video only, but RTVE declares no audio group")
	}
	if _, ok := bestVariant(variants); !ok {
		t.Error("no variant was chosen")
	}
}

// TestRTVESizeIsTheRenditionThePlayerGets is what makes the pre-connect skip
// work: an exact length, stated before anything is fetched.
func TestRTVESizeIsTheRenditionThePlayerGets(t *testing.T) {
	if got := rtveVideoOf(t, rtveVideoDocument).size(); got != 2147000000 {
		t.Errorf("size = %d, want the largest streaming rendition", got)
	}
}

// TestRTVESizeSkipsTheDeliveryMaster is the trap in the quality list. ORIGINAL
// is the mezzanine: listed at 1080p with a believable size, and never what
// the player is handed.
func TestRTVESizeSkipsTheDeliveryMaster(t *testing.T) {
	if got := rtveVideoOf(t, rtveMasterDocument).size(); got != 4800439 {
		t.Errorf("size = %d, want %d — the file the player is actually given", got, 4800439)
	}
}

// TestRTVESizeIsUnknownWhenTheRecordIsIncomplete covers the other way the
// figures go wrong. A smaller rendition's number would be worse than none at
// all: an exact Size the file can never reach is what the skip check compares
// against for the rest of the run.
func TestRTVESizeIsUnknownWhenTheRecordIsIncomplete(t *testing.T) {
	if got := rtveVideoOf(t, rtveIncompleteQualities).size(); got != -1 {
		t.Errorf("size = %d, want -1", got)
	}
}

// TestRTVEPlayableReadsOnlyTheTerritoryFields is the guard on the four fields
// that lie. Every one is set on the fixture, which is an ordinary
// downloadable video; consulting any would refuse most of the catalogue.
func TestRTVEPlayableReadsOnlyTheTerritoryFields(t *testing.T) {
	var raw struct {
		Page struct {
			Items []map[string]any `json:"items"`
		} `json:"page"`
	}
	if err := json.Unmarshal([]byte(rtveVideoDocument), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"hasDRM", "requireLogged", "notDownloadable", "paidContent"} {
		if raw.Page.Items[0][field] != true {
			t.Fatalf("the fixture no longer sets %s, so it no longer reproduces the trap", field)
		}
	}
	if !rtveVideoOf(t, rtveVideoDocument).playable() {
		t.Error("a downloadable video was refused")
	}
}

// TestRTVEListingAnswersAvailabilityForSpain is why a restricted video is
// asked about twice. A listing entry says allowedInCountry true and puts the
// caller in "ES" whoever asked; the video's own document, for the same id,
// says false and names the country the answer was really evaluated for.
func TestRTVEListingAnswersAvailabilityForSpain(t *testing.T) {
	var listing rtvePage[rtveVideo]
	if err := json.Unmarshal([]byte(rtveSeasonListing), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Page.Items) != 2 {
		t.Fatalf("parsed %d entries, want 2", len(listing.Page.Items))
	}

	entry := listing.Page.Items[1]
	if !entry.Restricted || !entry.Allowed || entry.Country != "ES" {
		t.Fatalf("the fixture no longer reproduces the trap: %+v", entry)
	}
	// Taken at its word the entry would be queued and answer 403, which is
	// exactly why episode re-asks before concluding anything about a video
	// the listing marks as licensed by territory.
	if !entry.playable() {
		t.Error("the listing entry refused itself, so nothing would ever be re-asked")
	}

	own := rtveVideoOf(t, rtveRestrictedVideo)
	if own.ID != entry.ID {
		t.Fatalf("the fixtures describe different videos: %q and %q", own.ID, entry.ID)
	}
	if own.playable() {
		t.Error("the video's own document was read as available")
	}
	if own.Country != "SE" {
		t.Errorf("country = %q, want the one the answer was evaluated for", own.Country)
	}

	// The unrestricted entry needs no second request, and must not be refused
	// over a field it never had reason to set.
	if !listing.Page.Items[0].playable() {
		t.Error("an unrestricted listing entry was refused")
	}
}

// TestRTVERefusedNamesTheCountry covers the wording. RTVE supplies no
// sentence of its own — its origin answers a bare 403 with an empty body — so
// the fact worth carrying is the territory the licence was tested against,
// which the document does supply.
func TestRTVERefusedNamesTheCountry(t *testing.T) {
	err := &rtveRefused{name: "A Programme - Something", country: "SE"}
	got := err.Error()
	if !strings.Contains(got, "SE") || !strings.Contains(got, "A Programme - Something") {
		t.Errorf("Error() = %q", got)
	}
	// A document that named no country still has to say something usable.
	if got := (&rtveRefused{name: "A Programme"}).reason(); !strings.Contains(got, "this country") {
		t.Errorf("reason() = %q", got)
	}
}

// TestRTVEName pins which title a file is called after. RTVE's long title
// names the programme, the season and the episode in one string, which is
// what a lone video needs and what an episode inside a season folder needs
// too: the episode title on its own is a phrase saying nothing about where it
// belongs, and it repeats often enough across a long strand to collide.
func TestRTVEName(t *testing.T) {
	if got := rtveVideoOf(t, rtveVideoDocument).name(); got != "A Programme - T2 - Episodio 7: Something" {
		t.Errorf("name = %q, want the long title", got)
	}
	tests := []struct {
		video rtveVideo
		want  string
	}{
		{rtveVideo{ID: "1", Title: "Something", ShortTitle: "Short"}, "Something"},
		{rtveVideo{ID: "1", ShortTitle: "Short"}, "Short"},
		{rtveVideo{ID: "1"}, "rtve-1"},
	}
	for _, tc := range tests {
		if got := tc.video.name(); got != tc.want {
			t.Errorf("name of %+v = %q, want %q", tc.video, got, tc.want)
		}
	}
}

// TestRTVETarget covers every link shape the site hands out. The slugs before
// the id are decorative — the site answers a wrong one with a redirect rather
// than a 404 — so the id is read as the last numeric segment, and a path with
// none names a programme.
func TestRTVETarget(t *testing.T) {
	tests := map[string]rtveRef{
		"https://www.rtve.es/play/videos/a-programme/an-episode/1000001/":   {video: "1000001"},
		"https://www.rtve.es/play/videos/a-programme/an-episode/1000001":    {video: "1000001"},
		"https://www.rtve.es/play/videos/a-programme/an-episode/1000001/?t": {video: "1000001"},
		"https://www.rtve.es/play/videos/a-programme/1000001/":              {video: "1000001"},
		"https://www.rtve.es/alacarta/videos/a-programme/an-ep/1000001/":    {video: "1000001"},
		"https://www.rtve.es/v/1000001/":                                    {video: "1000001"},
		"https://www.rtve.es/play/videos/a-programme/":                      {series: "a-programme"},
		"https://www.rtve.es/play/videos/a-programme":                       {series: "a-programme"},
		"https://www.rtve.es/play/videos/a-programme/not-an-episode/":       {series: "a-programme"},
		"https://www.rtve.es/alacarta/videos/a-programme/":                  {series: "a-programme"},
		"https://www.rtve.es/play/videos/":                                  {},
		"https://www.rtve.es/play/":                                         {},
		"https://www.rtve.es/":                                              {},
		"https://www.rtve.es/play/internacional/portada/":                   {},
		"https://www.rtve.es/v/":                                            {},
		"https://www.rtve.es/v/a-slug/":                                     {},
		"https://www.rtve.es/play/audios/a-programme/an-episode/1000001/":   {},
		"https://www.rtve.es/noticias/20260822/something/1000001.shtml":     {},
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := rtveTarget(u); got != want {
			t.Errorf("rtveTarget(%s) = %+v, want %+v", raw, got, want)
		}
	}
}

// TestRTVEMatch guards the two ways this Match could be wrong. The site's own
// pages are the whole of it: the catalogue and the media sit on subdomains of
// the same name, and a link to one of those is a plain rangeable file the
// direct extractor serves better than this would.
func TestRTVEMatch(t *testing.T) {
	r := NewRTVE(nil)
	for _, raw := range []string{
		"https://www.rtve.es/play/videos/a-programme/an-episode/1000001/",
		"https://rtve.es/play/videos/a-programme/",
		"https://WWW.RTVE.ES/v/1000001/",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Match(u) {
			t.Errorf("%s was not matched", raw)
		}
	}
	for _, raw := range []string{
		"https://api.rtve.es/api/videos/1000001.json",
		"https://ztnr.rtve.es/ztnr/movil/thumbnail/rtveplayw/videos/1000001.png",
		"https://media.rtve.es/resources/a/b/1.mp4",
		"https://www.rtve.es/noticias/20260822/something/1000001.shtml",
		"https://www.rtve.es/play/audios/a-programme/an-episode/1000001/",
		"https://notrtve.es.example.test/play/videos/a-programme/",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if r.Match(u) {
			t.Errorf("%s was matched", raw)
		}
	}
}

// TestRTVEProgrammeID covers reading a programme's number off its page. The
// page lists no episodes of its own — the browser fetches those afterwards —
// so the number survives only in the URLs of those deferred requests, and two
// independent ones carry it.
func TestRTVEProgrammeID(t *testing.T) {
	tests := map[string]string{
		`<div data-feed="https://www.rtve.es/api/programas/9000/relacionados.json">`: "9000",
		`<a href="/play/videos/modulos/capitulos/9000/">Capítulos</a>`:               "9000",
		`<a href="/play/videos/modulos/noticias/9000/">Noticias</a>`:                 "9000",
	}
	for page, want := range tests {
		match := rtveProgrammeID.FindStringSubmatch(page)
		if match == nil {
			t.Errorf("no programme id found in %s", page)
			continue
		}
		if match[1] != want {
			t.Errorf("found %q in %s, want %q", match[1], page, want)
		}
	}
	if rtveProgrammeID.MatchString(`<a href="/play/videos/a-programme/an-episode/1000001/">`) {
		t.Error("an episode link was read as a programme id")
	}
}

// TestRTVEListingEnvelope covers what every catalogue endpoint answers in,
// including the page count that is the only safe place to stop: asked for a
// page past the end, this API answers with the whole listing again rather
// than with nothing.
func TestRTVEListingEnvelope(t *testing.T) {
	var listing rtvePage[rtveVideo]
	if err := json.Unmarshal([]byte(rtveSeasonListing), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Page.TotalPages != 1 {
		t.Errorf("totalPages = %d, want 1", listing.Page.TotalPages)
	}
	for _, item := range listing.Page.Items {
		// Every entry repeats the season it belongs to, which is where the
		// folder name comes from — no second request names it.
		if item.Season != "T2" {
			t.Errorf("%s is filed under %q, want T2", item.ID, item.Season)
		}
		if item.Program.Title != "A Programme" {
			t.Errorf("%s names its programme %q", item.ID, item.Program.Title)
		}
	}

	var seasons rtvePage[rtveSeason]
	body := `{"page":{"items":[{"id":40034,"shortTitle":"T1","longTitle":"Temporada 1","orden":1},
		{"id":40017,"shortTitle":"T2","longTitle":"Temporada 2","orden":2}],"total":2,"totalPages":1}}`
	if err := json.Unmarshal([]byte(body), &seasons); err != nil {
		t.Fatal(err)
	}
	if len(seasons.Page.Items) != 2 || seasons.Page.Items[0].ID != 40034 {
		t.Errorf("seasons = %+v", seasons.Page.Items)
	}
}

// rtveVideoOf unwraps one video document.
func rtveVideoOf(t *testing.T, body string) rtveVideo {
	t.Helper()
	var doc rtvePage[rtveVideo]
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Page.Items) == 0 {
		t.Fatal("the document carries no items")
	}
	return doc.Page.Items[0]
}
