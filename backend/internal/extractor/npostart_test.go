package extractor

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// npoSeasonsListing is what series-seasons answers with, and it carries the
// two things about a season that cannot be assumed.
//
// The first is that `label` — the only field written for a person — is null
// for a good part of the catalogue, so a folder named from it alone would be
// named "". The second is that `seasonKey` is the site's own numbering and
// counts differently from one series to the next: this series calls its 2026
// season "2026" and its 2025 one "25", in the same document.
const npoSeasonsListing = `[
  {"guid":"11111111-1111-1111-1111-111111111111","seasonKey":"2026","slug":"seizoen-2026","label":null,"images":[]},
  {"guid":"22222222-2222-2222-2222-222222222222","seasonKey":"25","slug":"seizoen-25","label":"Seizoen 2025","images":[]}
]`

// npoDailyStrandSeason is one season of a nightly current-affairs programme,
// as programs-by-season serves it. Every episode is titled with the
// programme's own name, which is the trap: named from the title alone, a
// season downloads as one file written over and over.
const npoDailyStrandSeason = `[
  {
    "guid":"aaaaaaaa-0001-0000-0000-000000000000",
    "title":"Het Journaal",
    "slug":"het-journaal_3",
    "productId":"WWWWW_0000003",
    "programKey":"3",
    "durationInSeconds":1800,
    "publishedDateTime":1735862400,
    "season":{"guid":"11111111-1111-1111-1111-111111111111","slug":"seizoen-2026","seasonKey":"2026"},
    "series":{"guid":"99999999-9999-9999-9999-999999999999","slug":"het-journaal","title":"Het Journaal","type":"timebound_series"},
    "restrictions":[{"available":{"from":946684800,"till":4102444800},"isStreamReady":true,"subscriptionType":"free"}]
  },
  {
    "guid":"aaaaaaaa-0002-0000-0000-000000000000",
    "title":"Het Journaal",
    "slug":"het-journaal_2",
    "productId":"WWWWW_0000002",
    "programKey":"2",
    "durationInSeconds":1800,
    "publishedDateTime":1735776000,
    "season":{"guid":"11111111-1111-1111-1111-111111111111","slug":"seizoen-2026","seasonKey":"2026"},
    "series":{"guid":"99999999-9999-9999-9999-999999999999","slug":"het-journaal","title":"Het Journaal","type":"timebound_series"},
    "restrictions":[{"available":{"from":946684800,"till":4102444800},"isStreamReady":true,"subscriptionType":"free"}]
  },
  {
    "guid":"aaaaaaaa-0003-0000-0000-000000000000",
    "title":"Het Journaal",
    "slug":"het-journaal_1",
    "productId":"WWWWW_0000001",
    "programKey":"1",
    "durationInSeconds":1800,
    "publishedDateTime":1735689600,
    "season":{"guid":"11111111-1111-1111-1111-111111111111","slug":"seizoen-2026","seasonKey":"2026"},
    "series":{"guid":"99999999-9999-9999-9999-999999999999","slug":"het-journaal","title":"Het Journaal","type":"timebound_series"},
    "restrictions":[{"available":{"from":946684800,"till":4102444800},"isStreamReady":true,"subscriptionType":"free"}]
  },
  {
    "guid":"aaaaaaaa-0004-0000-0000-000000000000",
    "title":"Een fragment zonder product",
    "slug":"het-journaal_0",
    "productId":"",
    "publishedDateTime":1735689600,
    "series":{"guid":"99999999-9999-9999-9999-999999999999","slug":"het-journaal","title":"Het Journaal"}
  }
]`

// npoProgramDetail is one episode of a documentary strand, where the title
// is the episode's own and the series is named beside it — so a single
// episode can be filed under its programme without a second lookup.
const npoProgramDetail = `{
  "guid":"bbbbbbbb-0001-0000-0000-000000000000",
  "title":"Een aflevering met een eigen naam",
  "slug":"een-serie_12",
  "productId":"XX_000000012",
  "programKey":"12",
  "durationInSeconds":2400,
  "publishedDateTime":1735689600,
  "season":{"guid":"22222222-2222-2222-2222-222222222222","slug":"seizoen-25","seasonKey":"25"},
  "series":{"guid":"88888888-8888-8888-8888-888888888888","slug":"een-serie","title":"Een Serie","type":"timebound_series"},
  "restrictions":[{"available":{"from":946684800,"till":4102444800},"isStreamReady":true,"subscriptionType":"free"}]
}`

// npoClearStream is stream-link's answer for something served in the clear.
// Note what is absent: there is no drmType key at all, rather than one set
// to null, which is why its absence cannot be the only thing checked.
const npoClearStream = `{
  "stream":{
    "streamURL":"https://cdn.example.test/token/vod/npo/usp/npoplus/hls_unencrypted/XX_000000012/XX_000000012_v1.ism/playlist.m3u8",
    "streamProfile":"hls",
    "sourceProfile":"-",
    "drm":null,
    "avType":"vod",
    "isLiveStream":false
  },
  "user":{"type":"anonymous"}
}`

// npoFairPlayStream is the commonest answer of all: three quarters of the
// catalogue comes back like this. The CDN path agrees with the field —
// /npoplus/hls/ rather than /npoplus/hls_unencrypted/ — but the field is
// what is read, because it is the half the site guarantees.
const npoFairPlayStream = `{
  "stream":{
    "streamURL":"https://cdn.example.test/token/vod/npo/fps/TEST/npoplus/hls/YY_000000034_v1/playlist.m3u8",
    "streamProfile":"hls",
    "drmToken":"eyJhbGciOiJIUzI1NiJ9.e30.signature",
    "drmType":"fairplay",
    "drmExpirationInSeconds":1735689600,
    "drm":{
      "token":"eyJhbGciOiJIUzI1NiJ9.e30.signature",
      "certificateUrl":"https://fairplay.example.test/certificate/fairplay.cer",
      "licenseUrl":"https://drm.example.test/authentication",
      "type":"fairplay"
    }
  },
  "user":{"type":"anonymous"}
}`

// npoUnnamedDRMStream is the case the second half of the check exists for: a
// licence server is handed over but the scheme is not named. Reading drmType
// alone would take this for clear content and download ciphertext to the
// end.
const npoUnnamedDRMStream = `{
  "stream":{
    "streamURL":"https://cdn.example.test/token/vod/npo/fps/TEST/npoplus/hls/ZZ_000000056_v1/playlist.m3u8",
    "streamProfile":"hls",
    "drm":{"licenseUrl":"https://drm.example.test/authentication"}
  }
}`

// npoMasterPlaylist is what Unified Streaming serves for a clear item, and
// it is the reason this host needs no special handling: there is no
// EXT-X-MEDIA entry anywhere in it, so each video variant carries its audio
// in the same segments. The audio-only variant at the end is the one thing
// to get right, and it needs no rule of its own — it declares no video
// codec, so it reads as unmuxed and sorts last.
const npoMasterPlaylist = `#EXTM3U
#EXT-X-VERSION:1
## Created with Unified Streaming Platform  (version=1.12.1-28247)

# variants
#EXT-X-STREAM-INF:BANDWIDTH=645000,CODECS="mp4a.40.2,avc1.66.30",RESOLUTION=640x360,VIDEO-RANGE=SDR
XX_000000012_v1-audio=128000-video=480000.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1691000,CODECS="mp4a.40.2,avc1.77.31",RESOLUTION=1280x720,VIDEO-RANGE=SDR
XX_000000012_v1-audio=128000-video=1467000.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=4358000,CODECS="mp4a.40.2,avc1.100.41",RESOLUTION=1920x1080,VIDEO-RANGE=SDR
XX_000000012_v1-audio=128000-video=3983000.m3u8

# variants
#EXT-X-STREAM-INF:BANDWIDTH=136000,CODECS="mp4a.40.2"
XX_000000012_v1-audio=128000.m3u8
`

// TestNPOMasterPlaylistIsSelfContained pins the claim the whole extractor
// rests on. Were a variant to name an audio group with a playlist of its
// own, concatenating its segments would yield a silent video that looks like
// a finished download, and this host would belong on the external
// downloader instead.
func TestNPOMasterPlaylistIsSelfContained(t *testing.T) {
	base, err := ParseURL("https://cdn.example.test/token/vod/npo/usp/npoplus/hls_unencrypted/XX_000000012/XX_000000012_v1.ism/playlist.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(npoMasterPlaylist, base)
	if len(variants) != 4 {
		t.Fatalf("parsed %d variants, want 4", len(variants))
	}
	for _, v := range variants[:3] {
		if !v.muxed() {
			t.Errorf("%s reads as video only, but NPO declares no audio group at all", v.Resolution)
		}
	}
	// The audio-only rendition must never pass as self-contained: it is the
	// one entry here that would download to the end and play as sound over a
	// blank screen.
	if variants[3].muxed() {
		t.Error("the audio-only variant reads as muxed")
	}

	best, ok := bestVariant(variants)
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if best.Resolution != "1920x1080" {
		t.Errorf("chose %q, want the largest rendition", best.Resolution)
	}
	if got := playlistExtension(best); got != ".ts" {
		t.Errorf("extension %q, want .ts", got)
	}
}

// TestNPOStreamRefusesEncryptedContent is the gate that matters. Both halves
// of the check are covered, because each fails on its own: a renamed
// drmType would read as clear, and a scheme named with no licence object
// beside it would too.
func TestNPOStreamRefusesEncryptedContent(t *testing.T) {
	tests := []struct {
		name, body, scheme string
		want               bool
	}{
		{"clear", npoClearStream, "", false},
		{"fairplay", npoFairPlayStream, "fairplay", true},
		{"a licence server and no scheme named", npoUnnamedDRMStream, "a scheme it does not name", true},
		{"widevine, which is what the dash profile answers with",
			`{"stream":{"streamURL":"https://cdn.example.test/a/manifest.mpd","drmType":"widevine","drm":{"type":"widevine"}}}`,
			"widevine", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var link npoStreamLink
			if err := json.Unmarshal([]byte(tc.body), &link); err != nil {
				t.Fatal(err)
			}
			scheme, got := link.Stream.encrypted()
			if got != tc.want {
				t.Fatalf("encrypted() = %v, want %v", got, tc.want)
			}
			if scheme != tc.scheme {
				t.Errorf("scheme = %q, want %q", scheme, tc.scheme)
			}
		})
	}
}

// TestNPOClearStreamIsTakenWhole covers the other side of the same
// response: nothing else about a clear answer may be misread.
func TestNPOClearStreamIsTakenWhole(t *testing.T) {
	var link npoStreamLink
	if err := json.Unmarshal([]byte(npoClearStream), &link); err != nil {
		t.Fatal(err)
	}
	if link.Stream.URL == "" {
		t.Fatal("no stream URL was read")
	}
	if link.Stream.Profile != "hls" {
		t.Errorf("profile = %q, want hls", link.Stream.Profile)
	}
}

// TestNPOUnavailableNamesTheProgramme covers what a user is actually told,
// since for most of the catalogue a refusal is the entire outcome of a
// paste.
func TestNPOUnavailableNamesTheProgramme(t *testing.T) {
	err := &npoUnavailable{productID: "YY_000000034", reason: "DRM protected — NPO serves it under fairplay"}
	want := "npo: YY_000000034: DRM protected — NPO serves it under fairplay"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// npoGeoBlocked is what a programme licensed for the Netherlands alone
// answers with, body and status together. The sentence is the whole point:
// it is written for a viewer, it says where the refusal came from, and
// nothing invented here would be as useful.
const npoGeoBlocked = `{"body":"Dit programma mag niet bekeken worden vanaf jouw locatie.","status":451,"context":{}}`

// npoOutsideWindow is the other refusal, and it carries a trap of its own:
// the sentence says the programme may "not yet, or no longer" be watched and
// will not say which, so the dates beside it are the only thing that settles
// it.
const npoOutsideWindow = `{"body":"Dit programma mag nog niet of niet meer bekeken worden.","status":412,` +
	`"context":{"programAvailableFrom":"2024-11-08T08:00:00.000Z","programAvailableUntil":"2025-02-07T22:59:00.000Z"}}`

// TestNPORefusalIsPassedThroughVerbatim is why the body of a failed response
// is read at all. Left alone, a geo-block would reach the user as a bare 451
// wrapped around a blob of JSON.
func TestNPORefusalIsPassedThroughVerbatim(t *testing.T) {
	tests := map[string]string{
		npoGeoBlocked:    "Dit programma mag niet bekeken worden vanaf jouw locatie.",
		npoOutsideWindow: "Dit programma mag nog niet of niet meer bekeken worden. (available from 2024-11-08 to 2025-02-07)",
		// A window with only one end named still says more than the
		// sentence alone.
		`{"body":"Nog niet beschikbaar.","context":{"programAvailableFrom":"2026-01-01T00:00:00.000Z"}}`:   "Nog niet beschikbaar. (available from 2026-01-01)",
		`{"body":"Niet meer beschikbaar.","context":{"programAvailableUntil":"2026-01-01T00:00:00.000Z"}}`: "Niet meer beschikbaar. (available until 2026-01-01)",
		// The other envelope: a rejected token puts its sentence under
		// "message" rather than "body", and reading only "body" would leave
		// this looking like an edge that had stopped answering.
		`{"message":"Autorisatie van de speler mislukt, probeer het opnieuw","code":401}`: "Autorisatie van de speler mislukt, probeer het opnieuw",
		// A refusal with nothing to say to a viewer must produce nothing, so
		// the caller falls back to reporting the status it already has.
		`{"status":500,"context":{}}`: "",
	}
	for body, want := range tests {
		var refusal npoRefusal
		if err := json.Unmarshal([]byte(body), &refusal); err != nil {
			t.Fatal(err)
		}
		if got := refusal.message(); got != want {
			t.Errorf("message of %s = %q, want %q", body, got, want)
		}
	}
}

// TestNPOReasonReadsAStatusError covers the join between the HTTP client and
// the refusal: the sentence arrives inside the error's body, not beside it.
func TestNPOReasonReadsAStatusError(t *testing.T) {
	err := &httpx.StatusError{Code: 451, Status: "451 Unavailable For Legal Reasons",
		URL: npoStreamAPI, Body: npoGeoBlocked}
	if got := npoReason(fmt.Errorf("stream: %w", err)); got != "Dit programma mag niet bekeken worden vanaf jouw locatie." {
		t.Errorf("npoReason = %q", got)
	}
	// Anything that is not a refusal must fall through to being reported as
	// itself rather than swallowed.
	for _, other := range []error{
		errors.New("dial tcp: connection refused"),
		&httpx.StatusError{Code: 502, Status: "502 Bad Gateway", Body: "<html>gateway</html>"},
	} {
		if got := npoReason(other); got != "" {
			t.Errorf("npoReason(%v) = %q, want empty", other, got)
		}
	}
}

// npoEdgeBlock is what npo.nl answers with once a caller has asked too
// often: not NPO's JSON at all, but the error page of the CDN in front of
// it.
const npoEdgeBlock = `<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 4.01 Transitional//EN">
<HTML><HEAD><TITLE>ERROR: The request could not be satisfied</TITLE></HEAD>
<BODY><H1>ERROR</H1></BODY></HTML>`

// TestNPOEdgeBlockIsNotARefusal covers the distinction a series expansion
// turns on, since both arrive as a 403.
//
// The player refuses one programme and says why in a sentence. The edge in
// front of npo.nl refuses everything, for a few minutes, once a caller has
// asked around two hundred times — and expanding a series needs one request
// per episode, so it is reachable. Read as a refusal, that would report a
// throttle as though the catalogue had declined, and quietly return however
// many episodes were asked before the shutter came down.
func TestNPOEdgeBlockIsNotARefusal(t *testing.T) {
	edge := &httpx.StatusError{Code: 403, Status: "403 Forbidden",
		URL: npoDomainAPI + npoTokenEndpoint, Body: npoEdgeBlock}
	if got := npoReason(edge); got != "" {
		t.Errorf("an edge error page was read as a refusal: %q", got)
	}
	if !npoThrottling(edge) {
		t.Error("the edge block was not recognised as throttling")
	}

	// The player's own 403 is a refusal of one programme, says so, and must
	// never be waited out as though the whole site had shut.
	player := &httpx.StatusError{Code: 403, Status: "403 Forbidden", URL: npoStreamAPI,
		Body: `{"message":"Autorisatie van de speler mislukt, probeer het opnieuw","code":401}`}
	if npoThrottling(player) {
		t.Error("a rejected token reads as throttling")
	}
	if npoReason(player) == "" {
		t.Error("a rejected token carries no reason")
	}

	// Nothing else is a throttle, whatever its status.
	for _, other := range []error{
		errors.New("dial tcp: connection refused"),
		&httpx.StatusError{Code: 451, Status: "451 Unavailable For Legal Reasons", Body: npoGeoBlocked},
		&npoUnavailable{productID: "XX_000000012", reason: "geblokkeerd"},
	} {
		if npoThrottling(other) {
			t.Errorf("%v reads as throttling", other)
		}
	}

	// The throttle has to survive being wrapped, since that is how it
	// reaches the series expansion — through a token fetch and a stream.
	wrapped := fmt.Errorf("npo: player token for XX_000000012: %w", errNPOThrottled)
	if !errors.Is(wrapped, errNPOThrottled) {
		t.Error("a wrapped throttle is not recognisable")
	}
}

// TestNPOEmptyDocumentIsNotFound covers the site's way of saying it has
// never heard of a slug: HTTP 200, and the JSON string "" where the document
// should be. Decoded straight into a struct that is a type error about a
// string, which is true and no use to anybody.
func TestNPOEmptyDocumentIsNotFound(t *testing.T) {
	empty := []string{`""`, `null`, ``, `  `, "\n"}
	for _, body := range empty {
		if !npoEmpty(json.RawMessage(body)) {
			t.Errorf("%q was not recognised as an empty document", body)
		}
	}
	for _, body := range []string{`{}`, `[]`, `{"guid":"x"}`, `"something"`} {
		if npoEmpty(json.RawMessage(body)) {
			t.Errorf("%q was mistaken for an empty document", body)
		}
	}
}

// TestNPORouteReadsEveryURLShape covers the addresses the site serves. All
// of them are in circulation, because NPO answers the older ones with a
// redirect rather than retiring them — and a redirect is no help to
// something that decides where to go before it fetches anything.
func TestNPORouteReadsEveryURLShape(t *testing.T) {
	tests := map[string]npoTarget{
		"https://npo.nl/start/afspelen/een-serie_12":                            {episode: "een-serie_12"},
		"https://npo.nl/start/serie/een-serie/seizoen-25/een-serie_12/afspelen": {episode: "een-serie_12"},
		"https://npo.nl/start/serie/een-serie":                                  {series: "een-serie"},
		"https://npo.nl/start/serie/een-serie/afleveringen":                     {series: "een-serie"},
		"https://npo.nl/start/serie/een-serie/afleveringen/seizoen-25":          {series: "een-serie", season: "seizoen-25"},
		"https://npo.nl/start/serie/een-serie/seizoen-25":                       {series: "een-serie", season: "seizoen-25"},

		// Nothing is read out of these, and that is the answer: a
		// pre-relaunch npostart.nl link names a product id and no slug at
		// all, and a collection is not a series. Both are handed on to the
		// page itself to say where they land.
		"https://www.npostart.nl/een-serie/XX_000000012":   {},
		"https://npo.nl/start/collectie/iets-samengesteld": {},
		"https://npo.nl/start":                             {},
		"https://npo.nl/start/afspelen":                    {},
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := npoRoute(u); got != want {
			t.Errorf("npoRoute(%s) = %+v, want %+v", raw, got, want)
		}
	}
}

// TestNPORouteTreatsATabAsASeasonCandidate documents the deliberate
// vagueness: a tab and a season sit in the same path position, so routing
// cannot tell them apart and does not try. npoPick settles it against the
// season list, which is fetched anyway.
func TestNPORouteTreatsATabAsASeasonCandidate(t *testing.T) {
	u, err := ParseURL("https://npo.nl/start/serie/een-serie/fragmenten")
	if err != nil {
		t.Fatal(err)
	}
	target := npoRoute(u)
	if target.series != "een-serie" || target.season != "fragmenten" {
		t.Fatalf("npoRoute = %+v", target)
	}

	var seasons []npoSeason
	if err := json.Unmarshal([]byte(npoSeasonsListing), &seasons); err != nil {
		t.Fatal(err)
	}
	if _, ok := npoPick(seasons, target.season); ok {
		t.Error("a tab was matched as a season, which would download the wrong list")
	}
	if season, ok := npoPick(seasons, "seizoen-25"); !ok || season.GUID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("npoPick(seizoen-25) = %+v, %v", season, ok)
	}
}

// TestNPOSeasonNameFallsBackToTheSlug covers the folder a season's episodes
// are filed under. The label is null for much of the catalogue, and
// seasonKey cannot stand in for it: the same document numbers one season
// "2026" and the next "25".
func TestNPOSeasonNameFallsBackToTheSlug(t *testing.T) {
	var seasons []npoSeason
	if err := json.Unmarshal([]byte(npoSeasonsListing), &seasons); err != nil {
		t.Fatal(err)
	}
	if len(seasons) != 2 {
		t.Fatalf("got %d seasons, want 2", len(seasons))
	}
	if got := seasons[0].name(); got != "seizoen-2026" {
		t.Errorf("unlabelled season named %q, want its slug", got)
	}
	if got := seasons[1].name(); got != "Seizoen 2025" {
		t.Errorf("labelled season named %q, want its label", got)
	}
	// Two seasons in one series must never land in the same folder, which is
	// what taking seasonKey for a year would have done here.
	if seasons[0].name() == seasons[1].name() {
		t.Error("two seasons share a folder name")
	}
}

// TestNPODailyStrandEpisodesGetDistinctNames is the naming regression guard.
// NPO titles every instalment of a nightly programme with the programme's
// own name, so three episodes here are all called "Het Journaal" — named
// from the title alone they would be one file written three times.
func TestNPODailyStrandEpisodesGetDistinctNames(t *testing.T) {
	var listing []npoProgram
	if err := json.Unmarshal([]byte(npoDailyStrandSeason), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing) != 4 {
		t.Fatalf("got %d entries, want 4", len(listing))
	}

	seen := make(map[string]bool)
	for _, ep := range listing[:3] {
		if ep.Title != ep.Series.Title {
			t.Fatalf("the fixture lost its trap: %q is not %q", ep.Title, ep.Series.Title)
		}
		name := ep.name()
		if seen[name] {
			t.Fatalf("two episodes are both called %q", name)
		}
		seen[name] = true
	}
	if got := listing[0].name(); got != "Het Journaal - 2025-01-03" {
		t.Errorf("name = %q, want the broadcast date in place of the repeated title", got)
	}

	// The last entry names no product, and a season listing carries such
	// rows: there is nothing to ask the player for.
	if listing[3].ProductID != "" {
		t.Error("the fixture lost its entry with no product id")
	}
}

// TestNPOProgramDetailNamesItsSeries covers the single-episode case, where
// the file lands in the download directory with no folder to say what it
// belongs to and the name has to carry the series itself.
func TestNPOProgramDetailNamesItsSeries(t *testing.T) {
	var doc npoProgram
	if err := json.Unmarshal([]byte(npoProgramDetail), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ProductID != "XX_000000012" {
		t.Errorf("productId = %q", doc.ProductID)
	}
	if got := doc.name(); got != "Een Serie - Een aflevering met een eigen naam" {
		t.Errorf("name = %q", got)
	}
}

func TestNPOName(t *testing.T) {
	tests := []struct {
		name            string
		series, episode string
		published       int64
		want            string
	}{
		{"an episode with a name of its own", "Een Serie", "Een aflevering", 1735689600, "Een Serie - Een aflevering"},
		{"a daily strand", "Het Journaal", "Het Journaal", 1735689600, "Het Journaal - 2025-01-01"},
		{"the same, cased differently", "Het Journaal", "het journaal", 1735689600, "Het Journaal - 2025-01-01"},
		// Nothing to date it by leaves the series name alone, which is worse
		// but is not a lie: a name is not worth refusing a download over.
		{"a daily strand with no broadcast date", "Het Journaal", "Het Journaal", 0, "Het Journaal"},
		{"an episode belonging to no series", "", "Een documentaire", 1735689600, "Een documentaire"},
		{"an untitled episode", "Een Serie", "", 1735689600, "Een Serie - 2025-01-01"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := npoProgram{Title: tc.episode, Published: tc.published}
			doc.Series.Title = tc.series
			if got := doc.name(); got != tc.want {
				t.Errorf("name = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNPOJoin(t *testing.T) {
	tests := []struct{ series, part, want string }{
		{"Een Serie", "Seizoen 2025", "Een Serie - Seizoen 2025"},
		{"Een Serie", "", "Een Serie"},
		{"", "Seizoen 2025", "Seizoen 2025"},
		{"Een Serie", "een serie", "een serie"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := npoJoin(tc.series, tc.part); got != tc.want {
			t.Errorf("npoJoin(%q, %q) = %q, want %q", tc.series, tc.part, got, tc.want)
		}
	}
}

func TestNPODate(t *testing.T) {
	if got := npoDate(1735689600); got != "2025-01-01" {
		t.Errorf("npoDate = %q", got)
	}
	// A missing timestamp must produce nothing rather than 1970, which would
	// read as a real broadcast date and be the same for every episode.
	for _, unix := range []int64{0, -1} {
		if got := npoDate(unix); got != "" {
			t.Errorf("npoDate(%d) = %q, want empty", unix, got)
		}
	}
}

// TestNPOMatchLeavesTheCorporateSiteAlone covers the one thing Match has to
// get right: npo.nl is the broadcaster's whole web presence, and claiming
// all of it would mean failing on pages the fallback would have handled.
func TestNPOMatchLeavesTheCorporateSiteAlone(t *testing.T) {
	npo := NewNPOStart(nil)
	tests := map[string]bool{
		"https://npo.nl/start":                     true,
		"https://npo.nl/start/serie/een-serie":     true,
		"https://www.npo.nl/start/afspelen/een_12": true,
		"https://www.npostart.nl/een-serie/XX_1":   true,
		"https://npo.nl/":                          false,
		"https://npo.nl/overons/jaarverslag.pdf":   false,
		"https://npo.nl/npo3":                      false,
		"https://npo-example.nl/start/serie/x":     false,
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := npo.Match(u); got != want {
			t.Errorf("Match(%s) = %v, want %v", raw, got, want)
		}
	}
}
