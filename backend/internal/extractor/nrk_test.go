package extractor

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// nrkSequentialCatalogue is a drama series in the shape the catalogue serves
// one: every season's episodes are embedded, so the whole listing arrives in
// a single request. Two traps are reproduced deliberately.
//
// The first is the series name, which sits under a key named after the
// series type rather than under a field of its own. The second is the
// episode names: NRK numbers its drama episodes and calls them nothing else,
// so the same "1. episode" appears in every season and the season folder is
// the only thing telling two of them apart.
const nrkSequentialCatalogue = `{
  "seriesType": "sequential",
  "sequential": { "titles": { "title": "A Drama", "subtitle": "Something long and descriptive." } },
  "_embedded": {
    "seasons": [
      {
        "sequenceNumber": 1,
        "titles": { "title": "Sesong 1", "subtitle": null },
        "_embedded": { "episodes": [
          { "id": "aaaa", "prfId": "ABCD10000101", "titles": { "title": "1. episode", "subtitle": "– a line of dialogue." }, "sequenceNumber": 1 },
          { "id": "bbbb", "prfId": "ABCD10000201", "titles": { "title": "2. episode", "subtitle": null }, "sequenceNumber": 2 }
        ] }
      },
      {
        "sequenceNumber": 2,
        "titles": { "title": "Sesong 2", "subtitle": null },
        "_embedded": { "episodes": [
          { "id": "cccc", "prfId": "ABCD10000102", "titles": { "title": "1. episode", "subtitle": null }, "sequenceNumber": 1 }
        ] }
      }
    ]
  }
}`

// nrkStandardCatalogue is the other shape: a magazine or news series, whose
// seasons are years or months and run to the hundreds. Its seasons embed no
// episodes at all — only the one the site itself displays is embedded, and
// it hangs off _embedded.instalments as an object wrapping the same list a
// season carries under a different name.
const nrkStandardCatalogue = `{
  "seriesType": "standard",
  "standard": { "titles": { "title": "A Magazine", "subtitle": "Weekly." } },
  "_embedded": {
    "seasons": [
      { "titles": { "title": "2026", "subtitle": null }, "hasAvailableInstalments": true, "_links": { "self": { "href": "/tv/catalog/series/a-magazine/seasons/2026", "name": "2026" } } },
      { "titles": { "title": "2025", "subtitle": null }, "hasAvailableInstalments": true, "_links": { "self": { "href": "/tv/catalog/series/a-magazine/seasons/2025", "name": "2025" } } }
    ],
    "instalments": {
      "_links": { "self": { "href": "/tv/catalog/series/a-magazine/instalments?page=2026" } },
      "seriesType": "standard",
      "_embedded": { "instalments": [
        { "id": "dddd", "prfId": "EFGH20000126", "titles": { "title": "Something happened", "subtitle": "A paragraph of description that is not a name." } },
        { "id": "eeee", "prfId": "EFGH20000226", "titles": { "title": "Something else happened", "subtitle": null } }
      ] }
    }
  }
}`

// nrkSeasonDocument is one season fetched on its own, which is the only
// listing shape the catalogue serves the same way for every kind of series.
const nrkSeasonDocument = `{
  "sequenceNumber": 2,
  "titles": { "title": "Sesong 2", "subtitle": null },
  "_links": {
    "self": { "href": "/tv/catalog/series/a-drama/seasons/2" },
    "series": { "href": "/tv/catalog/series/a-drama", "name": "sequential", "title": "A Drama" }
  },
  "_embedded": { "episodes": [
    { "prfId": "ABCD10000102", "titles": { "title": "1. episode" }, "sequenceNumber": 1 },
    { "prfId": "ABCD10000202", "titles": { "title": "2. episode" }, "sequenceNumber": 2 }
  ] }
}`

// nrkGeoBlocked is what almost every programme answers with from outside
// Norway. The message is the whole point: it is written for a viewer, it
// names the country, and nothing invented here would be as useful.
const nrkGeoBlocked = `{
  "id": "ABCD10000101",
  "playability": "nonPlayable",
  "playable": null,
  "nonPlayable": {
    "reason": "blocked",
    "messageType": "ProgramIsGeoblocked",
    "endUserMessage": "Ikke tilgjengelig utenfor Norge",
    "endUserMessageSupplement": null,
    "helpUrl": "https://info.nrk.no/tv/"
  }
}`

// nrkPlayable is a manifest offering several assets, with the traps a real
// one carries: the encrypted asset comes first and would be taken by
// anything that merely looks for a URL, and the plain MP4 beside it is a
// thumbnail track rather than the programme.
const nrkPlayable = `{
  "id": "ABCD10000101",
  "playability": "playable",
  "playable": {
    "duration": "PT43M48.44S",
    "assets": [
      { "url": "https://cdn.example.test/open/drm/index.mpd", "format": "DASH", "mimeType": "application/dash+xml", "encrypted": true, "encryptionScheme": "widevine" },
      { "url": "https://cdn.example.test/open/thumbs/preview.mp4", "format": "MP4", "mimeType": "video/mp4", "encrypted": false, "encryptionScheme": "none" },
      { "url": "https://cdn.example.test/open/a/muxed.m3u8?adap=small", "format": "HLS", "mimeType": "application/vnd.apple.mpegurl", "encrypted": false, "encryptionScheme": "none" }
    ],
    "subtitles": []
  },
  "nonPlayable": null
}`

// nrkMasterPlaylist is NRK's own, and the reason this host needs no special
// handling: there is no EXT-X-MEDIA entry anywhere in it, so every variant
// carries its video and its audio in the same segments.
const nrkMasterPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-INDEPENDENT-SEGMENTS

# Variants
#EXT-X-STREAM-INF:BANDWIDTH=2198671,AVERAGE-BANDWIDTH=2179918,CODECS="avc1.64001f,mp4a.40.2",RESOLUTION=1280x720,FRAME-RATE=25.000
sc-abcdef/m31_index.m3u8?adap=small
#EXT-X-STREAM-INF:BANDWIDTH=647203,AVERAGE-BANDWIDTH=643523,CODECS="avc1.4d401e,mp4a.40.2",RESOLUTION=640x360,FRAME-RATE=25.000
sc-abcdef/m10_index.m3u8?adap=small
#EXT-X-STREAM-INF:BANDWIDTH=1305615,AVERAGE-BANDWIDTH=1297182,CODECS="avc1.4d401f,mp4a.40.2",RESOLUTION=960x540,FRAME-RATE=25.000
sc-abcdef/m21_index.m3u8?adap=small
`

// TestNRKMasterPlaylistIsSelfContained pins the claim the whole extractor
// rests on. NRK names the file "muxed" and means it: with no audio group
// declared, every variant is usable on its own and the segments join into
// something that plays.
func TestNRKMasterPlaylistIsSelfContained(t *testing.T) {
	base, err := ParseURL("https://cdn.example.test/open/a/muxed.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(nrkMasterPlaylist, base)
	if len(variants) != 3 {
		t.Fatalf("parsed %d variants, want 3", len(variants))
	}
	for _, v := range variants {
		if !v.muxed() {
			t.Errorf("%s reads as video only, but NRK declares no audio group at all", v.Resolution)
		}
	}

	best, ok := bestVariant(variants)
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if best.Resolution != "1280x720" {
		t.Errorf("chose %s, want the largest rendition", best.Resolution)
	}
	// The variant URLs name neither cmaf nor mp4, and the segments behind
	// them are MPEG-TS.
	if got := playlistExtension(best); got != ".ts" {
		t.Errorf("extension %q, want .ts", got)
	}
}

// TestNRKStreamSkipsEncryptedAssets is the regression guard for the asset
// walk. NRK publishes DRM-protected renditions alongside the rest instead of
// withholding them, so taking the first URL on offer downloads a file that
// finishes and then plays as noise.
func TestNRKStreamSkipsEncryptedAssets(t *testing.T) {
	var doc nrkManifest
	if err := json.Unmarshal([]byte(nrkPlayable), &doc); err != nil {
		t.Fatal(err)
	}

	stream, err := doc.stream()
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if stream != "https://cdn.example.test/open/a/muxed.m3u8?adap=small" {
		t.Errorf("chose %q, want the unencrypted playlist", stream)
	}
}

func TestNRKStreamRejectsWhatCannotBePlayed(t *testing.T) {
	tests := map[string]string{
		"nothing at all": `{"playability":"playable","playable":{"assets":[]}}`,
		"only encrypted": `{"playability":"playable","playable":{"assets":[
			{"url":"https://cdn.example.test/a/index.mpd","format":"DASH","encrypted":true}]}}`,
		"a playlist with no url": `{"playability":"playable","playable":{"assets":[
			{"url":"","format":"HLS","encrypted":false}]}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			var doc nrkManifest
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatal(err)
			}
			if stream, err := doc.stream(); err == nil {
				t.Errorf("accepted %q", stream)
			}
		})
	}
}

// TestNRKStreamAcceptsAPlaylistByItsMimeType covers a manifest that labels
// its format some other way. The media type is unambiguous either way.
func TestNRKStreamAcceptsAPlaylistByItsMimeType(t *testing.T) {
	var doc nrkManifest
	body := `{"playability":"playable","playable":{"assets":[
		{"url":"https://cdn.example.test/a/muxed.m3u8","format":"hls_ts","mimeType":"application/vnd.apple.mpegurl","encrypted":false}]}}`
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.stream(); err != nil {
		t.Errorf("stream: %v", err)
	}
}

// TestNRKRefusalIsPassedThroughVerbatim is why nonPlayable is read at all.
// The site is geo-restricted and says so in a sentence meant for a person;
// replacing that with a guess of our own would lose the only fact the caller
// can act on.
func TestNRKRefusalIsPassedThroughVerbatim(t *testing.T) {
	var doc nrkManifest
	if err := json.Unmarshal([]byte(nrkGeoBlocked), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Playability == "playable" {
		t.Fatal("a refused manifest read as playable")
	}
	if got := doc.refusal(); got != "Ikke tilgjengelig utenfor Norge" {
		t.Errorf("refusal = %q, want NRK's own words", got)
	}
}

func TestNRKRefusalForms(t *testing.T) {
	tests := map[string]string{
		`{"playability":"nonPlayable","nonPlayable":{"endUserMessage":"Ikke tilgjengelig utenfor Norge","endUserMessageSupplement":"Prøv igjen senere."}}`: "Ikke tilgjengelig utenfor Norge Prøv igjen senere.",
		`{"playability":"nonPlayable","nonPlayable":{"endUserMessage":"","reason":"rightsNotAvailable"}}`:                                                  "rightsNotAvailable",
		// Nothing to say to a viewer and no reason either: the state itself
		// is still more use than an empty message.
		`{"playability":"notAvailable","nonPlayable":{}}`: "notAvailable",
		`{"playability":""}`:                              "not playable",
	}
	for body, want := range tests {
		var doc nrkManifest
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatal(err)
		}
		if got := doc.refusal(); got != want {
			t.Errorf("refusal of %s = %q, want %q", body, got, want)
		}
	}
}

// TestNRKRefusedErrorCarriesTheMessage covers what the series expansion
// leans on: hundreds of episodes refused for one reason should be reported
// as that one reason, not as hundreds of failures.
func TestNRKRefusedErrorCarriesTheMessage(t *testing.T) {
	err := &nrkRefused{id: "ABCD10000101", message: "Ikke tilgjengelig utenfor Norge"}
	if got := err.Error(); got != "nrk: ABCD10000101: Ikke tilgjengelig utenfor Norge" {
		t.Errorf("Error() = %q", got)
	}
}

// TestNRKSequentialCatalogueListsEverySeason pins the one-request case, and
// with it the awkward placement of the series name.
func TestNRKSequentialCatalogueListsEverySeason(t *testing.T) {
	var doc nrkCatalogue
	if err := json.Unmarshal([]byte(nrkSequentialCatalogue), &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.title(); got != "A Drama" {
		t.Errorf("title = %q, want the name filed under the series type", got)
	}

	seen := make(map[string]bool)
	var episodes []nrkEpisodeRef
	for _, season := range doc.Embedded.Seasons {
		episodes = append(episodes, nrkRefs(season, season.Titles.Title, seen)...)
	}
	if len(episodes) != 3 {
		t.Fatalf("got %d episodes, want 3", len(episodes))
	}
	want := []nrkEpisodeRef{
		{id: "ABCD10000101", name: "1. episode", season: "Sesong 1", number: 1},
		{id: "ABCD10000201", name: "2. episode", season: "Sesong 1", number: 2},
		{id: "ABCD10000102", name: "1. episode", season: "Sesong 2", number: 1},
	}
	for i, ep := range episodes {
		if ep != want[i] {
			t.Errorf("episode %d = %+v, want %+v", i, ep, want[i])
		}
	}
}

// TestNRKStandardCatalogueFallsBackToTheDisplayedSeason covers the other
// shape, where the seasons are stubs and the entries hang off a wrapper
// object under a different name.
func TestNRKStandardCatalogueFallsBackToTheDisplayedSeason(t *testing.T) {
	var doc nrkCatalogue
	if err := json.Unmarshal([]byte(nrkStandardCatalogue), &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.title(); got != "A Magazine" {
		t.Errorf("title = %q", got)
	}

	seen := make(map[string]bool)
	var episodes []nrkEpisodeRef
	for _, season := range doc.Embedded.Seasons {
		episodes = append(episodes, nrkRefs(season, season.Titles.Title, seen)...)
	}
	if len(episodes) != 0 {
		t.Fatalf("the season stubs yielded %d episodes; they carry none", len(episodes))
	}

	episodes = nrkRefs(doc.Embedded.Instalments, "", seen)
	if len(episodes) != 2 {
		t.Fatalf("got %d instalments, want 2", len(episodes))
	}
	if episodes[0].id != "EFGH20000126" || episodes[0].name != "Something happened" {
		t.Errorf("first instalment = %+v", episodes[0])
	}
	// Instalments are not numbered, and nothing may pretend otherwise: the
	// older links that name an episode by its position are resolved by
	// counting, and a fabricated number would send them to the wrong one.
	if episodes[0].number != 0 {
		t.Errorf("instalment carries number %d, but the listing numbers nothing", episodes[0].number)
	}
	// Nothing nests: a single season needs no folder.
	if episodes[0].season != "" {
		t.Errorf("instalment filed under %q", episodes[0].season)
	}
}

// TestNRKSeasonDocumentNamesItsSeries covers the season endpoint, whose
// series name lives in a link rather than in the body.
func TestNRKSeasonDocumentNamesItsSeries(t *testing.T) {
	var doc nrkSeason
	if err := json.Unmarshal([]byte(nrkSeasonDocument), &doc); err != nil {
		t.Fatal(err)
	}
	if got := nrkJoin(doc.Links.Series.Title, doc.Titles.Title); got != "A Drama - Sesong 2" {
		t.Errorf("title = %q, want the season named in it", got)
	}
	if got := len(nrkRefs(doc, "", make(map[string]bool))); got != 2 {
		t.Errorf("got %d episodes, want 2", got)
	}
}

// TestNRKRefsSkipWhatIsAlreadyTaken guards against a series that lists one
// episode in two places, which would otherwise be downloaded twice.
func TestNRKRefsSkipWhatIsAlreadyTaken(t *testing.T) {
	var doc nrkCatalogue
	if err := json.Unmarshal([]byte(nrkSequentialCatalogue), &doc); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	first := nrkRefs(doc.Embedded.Seasons[0], "Sesong 1", seen)
	again := nrkRefs(doc.Embedded.Seasons[0], "Sesong 1", seen)
	if len(first) != 2 || len(again) != 0 {
		t.Errorf("got %d then %d episodes, want 2 then 0", len(first), len(again))
	}
}

// TestNRKRefsIgnoreEntriesWithNoProgrammeID covers the listing entry's own
// id, which identifies the row rather than the programme and is no use to
// the player API.
func TestNRKRefsIgnoreEntriesWithNoProgrammeID(t *testing.T) {
	var season nrkSeason
	body := `{"_embedded":{"episodes":[
		{"id":"ffff","titles":{"title":"A trailer with no programme behind it"}},
		{"id":"gggg","prfId":"ABCD10000101","titles":{"title":"1. episode"},"sequenceNumber":1}]}}`
	if err := json.Unmarshal([]byte(body), &season); err != nil {
		t.Fatal(err)
	}
	refs := nrkRefs(season, "", make(map[string]bool))
	if len(refs) != 1 || refs[0].id != "ABCD10000101" {
		t.Errorf("got %+v, want only the entry with a programme id", refs)
	}
}

// TestNRKJoin covers the naming rule. A standalone programme has no series
// to prefix, and an episode that repeats its series' name should not say it
// twice.
func TestNRKJoin(t *testing.T) {
	tests := []struct {
		series, episode, want string
	}{
		{"A Drama", "1. episode", "A Drama - 1. episode"},
		{"", "A Documentary", "A Documentary"},
		{"A Documentary", "", "A Documentary"},
		{"A Drama", "a drama", "a drama"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := nrkJoin(tc.series, tc.episode); got != tc.want {
			t.Errorf("nrkJoin(%q, %q) = %q, want %q", tc.series, tc.episode, got, tc.want)
		}
	}
}

// TestNRKProgramTitles pins which fields a programme document is read from.
// The metadata document the manifest links to is the tempting alternative,
// and this is what rules it out: for a programme belonging to no series its
// subtitle is the synopsis, whereas here both series fields are simply null.
func TestNRKProgramTitles(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{
			name: "an episode of a series",
			body: `{"title":"A Drama","seriesTitle":"A DRAMA","episodeTitle":"1. episode","seriesId":"a-drama"}`,
			want: "A DRAMA - 1. episode",
		},
		{
			name: "a programme belonging to no series",
			body: `{"title":"A Documentary","seriesTitle":null,"episodeTitle":null,"seriesId":null,
				"shortDescription":"A paragraph that is not a name and must never be used as one."}`,
			want: "A Documentary",
		},
		{
			name: "an episode the catalogue gave no episode title",
			body: `{"title":"Part one","seriesTitle":"A Drama","episodeTitle":null}`,
			want: "A Drama - Part one",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var doc nrkProgram
			if err := json.Unmarshal([]byte(tc.body), &doc); err != nil {
				t.Fatal(err)
			}
			got := nrkJoin(doc.SeriesTitle, util.FirstNonEmpty(doc.EpisodeTitle, doc.Title))
			if got != tc.want {
				t.Errorf("title = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNRKTarget covers every link shape the site has used. The older ones
// still matter: the site answers them with a redirect rather than a 404, so
// they go on circulating.
func TestNRKTarget(t *testing.T) {
	tests := map[string]nrkRef{
		"https://tv.nrk.no/serie/a-series":                                        {slug: "a-series"},
		"https://tv.nrk.no/serie/a-series/":                                       {slug: "a-series"},
		"https://tv.nrk.no/serie/a-series/sesong/1":                               {slug: "a-series", season: "1"},
		"https://tv.nrk.no/serie/a-series/sesong/ekstramateriale":                 {slug: "a-series", season: "ekstramateriale"},
		"https://tv.nrk.no/serie/a-series/sesong/1/episode/ABCD1234567":           {program: "ABCD1234567"},
		"https://tv.nrk.no/serie/a-series/sesong/1/episode/ABCD1234567/avspiller": {program: "ABCD1234567"},
		// The number form is a place in the season, not an id, and has to
		// be looked up before anything can be fetched.
		"https://tv.nrk.no/serie/a-series/sesong/2/episode/7": {slug: "a-series", season: "2", number: 7},
		"https://tv.nrk.no/serie/a-series/ABCD12345678":       {program: "ABCD12345678"},
		"https://tv.nrk.no/program/ABCD12345678":              {program: "ABCD12345678"},
		"https://tv.nrk.no/program/ABCD12345678/a-slug":       {program: "ABCD12345678"},
		"https://tv.nrk.no/se?v=ABCD12345678":                 {program: "ABCD12345678"},
		"https://tv.nrk.no/se?s=a-series":                     {slug: "a-series"},
		"https://tv.nrk.no/se?v=ABCD12345678&t=60":            {program: "ABCD12345678"},
		"https://tv.nrk.no/":                                  {},
		"https://tv.nrk.no/direkte/nrk1":                      {},
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := nrkTarget(u); got != want {
			t.Errorf("nrkTarget(%s) = %+v, want %+v", raw, got, want)
		}
	}
}

// TestNRKPageTitleKeepsADashInTheName is why the site suffix is trimmed as
// one string. Cutting at the first " - ", the way most of these hosts are
// handled, beheads every series whose own name contains one.
func TestNRKPageTitleKeepsADashInTheName(t *testing.T) {
	tests := map[string]string{
		"<html><head><title>A Drama - NRK TV</title></head><body></body></html>":          "A Drama",
		"<html><head><title>Here - and there - NRK TV</title></head><body></body></html>": "Here - and there",
		"<html><head><title>A Drama</title></head><body></body></html>":                   "A Drama",
	}
	for doc, want := range tests {
		if got := nrkPageTitle(doc); got != want {
			t.Errorf("nrkPageTitle(%q) = %q, want %q", doc, got, want)
		}
	}
}

// TestNRKEpisodeLinkIgnoresPositions guards the page fallback. The site
// accepts both an id and a number in the same place, and a number resolves
// to nothing when handed to the player API.
func TestNRKEpisodeLinkIgnoresPositions(t *testing.T) {
	page := `<html><body>
	<a href="/serie/a-series/sesong/1/episode/ABCD10000101">1. episode</a>
	<a href="/serie/a-series/sesong/1/episode/ABCD10000201">2. episode</a>
	<a href="/serie/a-series/sesong/1/episode/3">3. episode</a>
	<a href="/serie/a-series/sesong/2">Sesong 2</a>
	</body></html>`

	var ids []string
	for _, match := range nrkEpisodeLink.FindAllStringSubmatch(page, -1) {
		ids = append(ids, match[1])
	}
	if len(ids) != 2 || ids[0] != "ABCD10000101" || ids[1] != "ABCD10000201" {
		t.Errorf("found %v, want the two programme ids and nothing else", ids)
	}
}

func TestNRKMatch(t *testing.T) {
	n := NewNRK(nil)
	for _, host := range []string{"tv.nrk.no", "TV.NRK.NO"} {
		if !n.Match(&url.URL{Scheme: "https", Host: host, Path: "/serie/a-series"}) {
			t.Errorf("%s was not matched", host)
		}
	}
	// Radio and the news site run on the same player API but have a
	// catalogue of their own, which nothing here reads.
	for _, host := range []string{"radio.nrk.no", "www.nrk.no", "nrk.no", "nottv.nrk.no.example.test"} {
		if n.Match(&url.URL{Scheme: "https", Host: host, Path: "/serie/a-series"}) {
			t.Errorf("%s was matched", host)
		}
	}
}
