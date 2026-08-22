package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// ruvMasterPlaylist is RÚV's own shape, and the claim the whole television
// half of this extractor rests on.
//
// The trap is the EXT-X-MEDIA line. It has a URI, so anything that merely
// looks for one would conclude the variants carry no audio of their own and
// refuse them — but its TYPE is SUBTITLES, and there is no audio group in the
// document at all. Every variant holds video and audio in the same MPEG-TS
// segments, which is what makes concatenation produce something playable.
const ruvMasterPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-MEDIA:TYPE=SUBTITLES,URI="888/index.m3u8",GROUP-ID="vtt",LANGUAGE="ice",NAME="Subtitle0",DEFAULT=NO,AUTOSELECT=YES
#EXT-X-STREAM-INF:BANDWIDTH=1004637,CODECS="avc1.64001e,mp4a.40.2",RESOLUTION=640x360,FRAME-RATE=25.000,SUBTITLES="vtt"
800/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3648132,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=25.000,SUBTITLES="vtt"
3600/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2530004,CODECS="avc1.64001f,mp4a.40.2",RESOLUTION=1280x720,FRAME-RATE=25.000,SUBTITLES="vtt"
2400/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1390132,CODECS="avc1.64001e,mp4a.40.2",RESOLUTION=852x480,FRAME-RATE=25.000,SUBTITLES="vtt"
1200/index.m3u8
`

// ruvVariantPlaylist is what one rendition looks like: plain VOD, MPEG-TS,
// and no EXT-X-KEY anywhere, which is the other half of "no DRM here".
const ruvVariantPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:VOD
#EXTINF:6.096
segment0.ts
#EXTINF:6.000
segment1.ts
#EXTINF:0.320
segment2.ts
#EXT-X-ENDLIST
`

// TestRUVMasterPlaylistIsSelfContained pins the deciding constraint. RÚV
// declares subtitles as a separate rendition and audio not at all, so the
// best variant simply wins and its segments join into a playable file.
func TestRUVMasterPlaylistIsSelfContained(t *testing.T) {
	base, err := ParseURL("https://cdn.example.test/opid/1234A0/1234A0.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	// The document does carry an EXT-X-MEDIA entry with a URI of its own.
	// That it names no audio group is the whole point.
	if groups := parseAudioGroups(ruvMasterPlaylist); len(groups) != 0 {
		t.Errorf("a SUBTITLES rendition was read as an audio group: %v", groups)
	}

	variants := parseMasterPlaylist(ruvMasterPlaylist, base)
	if len(variants) != 4 {
		t.Fatalf("parsed %d variants, want 4", len(variants))
	}
	for _, v := range variants {
		if !v.muxed() {
			t.Errorf("%s reads as video only, but RÚV declares no audio group at all", v.Resolution)
		}
	}

	best, ok := bestVariant(variants)
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if best.Resolution != "1920x1080" {
		t.Errorf("chose %s, want the largest rendition", best.Resolution)
	}
	// Neither the variant URL nor the segments behind it name cmaf or mp4.
	if got := playlistExtension(best); got != ".ts" {
		t.Errorf("extension %q, want .ts", got)
	}
}

// TestRUVSeriesIsOneRequest is the reason this extractor is short: the
// listing already carries every episode's stream URL, so nothing has to be
// looked up per episode. The fixture is the answer to the one query made.
func TestRUVSeriesIsOneRequest(t *testing.T) {
	prog := ruvDecode(t, ruvSeries(ruvExampleCDN))

	if prog.name() != "Talað yfir 20 og eitthvað" {
		t.Errorf("name = %q", prog.name())
	}
	if len(prog.Episodes) != 6 {
		t.Fatalf("got %d episodes, want 6", len(prog.Episodes))
	}
	for _, ep := range prog.Episodes {
		if ep.File == "" {
			t.Errorf("episode %s carries no file, so a second request would be needed", ep.ID)
		}
	}
}

// TestRUVOrderedHonoursThePlayersFlag covers the ordering rule, and the fact
// that decided it: the API answers newest-first whatever the programme, so
// reverse_episode_order is an instruction about the list rather than a
// description of it.
func TestRUVOrderedHonoursThePlayersFlag(t *testing.T) {
	prog := ruvDecode(t, ruvSeries(ruvExampleCDN))
	if !prog.ReverseEpisodeOrder {
		t.Fatal("the fixture is meant to carry reverse_episode_order")
	}

	var titles []string
	for _, ep := range prog.ordered() {
		titles = append(titles, ep.Title)
	}
	want := []string{
		"Þáttur 1 af 6", "Þáttur 2 af 6", "Þáttur 3 af 6",
		"Þáttur 4 af 6", "Þáttur 5 af 6", "Þáttur 6 af 6",
	}
	for i, title := range want {
		if titles[i] != title {
			t.Errorf("episode %d = %q, want %q", i, titles[i], title)
		}
	}

	// The flag is what makes it happen. Cleared, the newest-first order the
	// API sent is kept, which is what a daily strand wants.
	prog.ReverseEpisodeOrder = false
	if got := prog.ordered()[0].Title; got != "Þáttur 6 af 6" {
		t.Errorf("without the flag the list starts at %q, want the API's own order", got)
	}
	// ordered must not rearrange the listing under whoever else reads it.
	if got := prog.Episodes[0].Title; got != "Þáttur 6 af 6" {
		t.Errorf("ordered reversed the programme in place: %q", got)
	}
}

// TestRUVNamesDisambiguateRepeatedTitles is the naming trap, and it is a real
// one: a news strand runs to over a thousand instalments of which a couple of
// dozen share the title "Fréttir og veður". Two files with one name have one
// destination, so the second would be written over the first.
func TestRUVNamesDisambiguateRepeatedTitles(t *testing.T) {
	prog := ruvDecode(t, ruvNewsResponse)
	names := ruvNames(prog.ordered())

	want := []string{
		"bbbb04 Fréttir og veður",
		"21.08.2026",
		"bbbb02 Fréttir og veður",
		"bbbb01",
	}
	if len(names) != len(want) {
		t.Fatalf("got %d names, want %d", len(names), len(want))
	}
	for i, name := range want {
		if names[i] != name {
			t.Errorf("name %d = %q, want %q", i, names[i], name)
		}
	}

	// Nothing may end up sharing a destination.
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			t.Errorf("two episodes are both called %q", name)
		}
		seen[name] = true
	}
}

// TestRUVNamesLeaveGoodTitlesAlone is the other half of that rule. Only the
// titles that actually repeat are prefixed, because spoiling a thousand
// readable names to separate eighteen would be a poor trade.
func TestRUVNamesLeaveGoodTitlesAlone(t *testing.T) {
	prog := ruvDecode(t, ruvSeries(ruvExampleCDN))
	for i, name := range ruvNames(prog.ordered()) {
		if !strings.HasPrefix(name, "Þáttur ") {
			t.Errorf("name %d = %q, want the episode title untouched", i, name)
		}
	}
}

// TestRUVRadioIsAPlainFile covers the better of the two cases RÚV serves.
// Radio is an MP3 with a length and byte ranges, so it must arrive as a URL —
// dressing it up as a stream would cost the segmented engine, resume and the
// skip check for no reason at all.
func TestRUVRadioIsAPlainFile(t *testing.T) {
	prog := ruvDecode(t, ruvRadioResponse)
	session := &ruvSession{client: nil} // a plain file needs no request

	file, err := session.file(context.Background(), prog.Episodes[0])
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if file.URL != "https://radio.example.test/opid/1000001D0.mp3" {
		t.Errorf("URL = %q, want the file itself", file.URL)
	}
	if file.Segments != nil {
		t.Errorf("a plain MP3 was turned into %d segments", len(file.Segments))
	}
	if file.Name != ".mp3" {
		t.Errorf("extension %q, want .mp3", file.Name)
	}
	// RÚV publishes duration in seconds and no byte count anywhere, so
	// nothing here may claim to know the length.
	if file.Size != -1 {
		t.Errorf("Size = %d, want -1: the schema carries no byte count", file.Size)
	}
	if file.Headers[httpx.HeaderReferer] == "" {
		t.Error("no Referer was set")
	}
}

// TestRUVResolvesAPlaylistToSegments walks the television case end to end
// against a server laid out the way the CDN lays one out.
func TestRUVResolvesAPlaylistToSegments(t *testing.T) {
	srv, _ := ruvCDN(t, nil)
	prog := ruvDecode(t, ruvSeries(srv.URL))

	session := &ruvSession{client: ruvTestClient()}
	file, err := session.file(context.Background(), prog.Episodes[0])
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if len(file.Segments) != 3 {
		t.Fatalf("got %d segments, want 3", len(file.Segments))
	}
	// The chosen variant is the 1080p one, and its segments are resolved
	// against the variant playlist rather than the master.
	if want := srv.URL + "/opid/1/3600/segment0.ts"; file.Segments[0] != want {
		t.Errorf("first segment = %q, want %q", file.Segments[0], want)
	}
	if file.Name != ".ts" {
		t.Errorf("extension %q, want .ts", file.Name)
	}
	if file.URL != "" {
		t.Errorf("a playlist-backed file also carries a URL: %q", file.URL)
	}
}

// TestRUVDomesticIsRefusedInWords is the geo-blocking contract. RÚV publishes
// a category rather than a sentence, so there is nothing to pass through the
// way NRK's endUserMessage is passed through — and an Akamai "Access Denied"
// page is not an answer. The refusal has to be written here.
func TestRUVDomesticIsRefusedInWords(t *testing.T) {
	srv, seen := ruvCDN(t, map[string]bool{"/lokad/": true})
	prog := ruvDecode(t, ruvDomestic(srv.URL))

	session := &ruvSession{client: ruvTestClient()}
	_, err := session.file(context.Background(), prog.Episodes[0])
	if err == nil {
		t.Fatal("an Iceland-only episode resolved from outside Iceland")
	}
	var refusal *ruvRefused
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %v, want a refusal this run can recognise", err)
	}
	if !strings.Contains(refusal.Error(), "Iceland only") {
		t.Errorf("refusal = %q, want RÚV's licence stated plainly", refusal.Error())
	}
	if strings.Contains(refusal.Error(), "403") {
		t.Errorf("refusal = %q, want words rather than a status code", refusal.Error())
	}

	// One refusal proves where the caller is, not something about that one
	// episode, so the rest of the programme must not be asked for again.
	before := seen.Load()
	for _, ep := range prog.Episodes[1:] {
		if _, err := session.file(context.Background(), ep); err == nil {
			t.Errorf("episode %s resolved after the CDN had already refused this address", ep.ID)
		}
	}
	if after := seen.Load(); after != before {
		t.Errorf("%d further requests were made after the answer was already known", after-before)
	}
}

// TestRUVGlobalFailureIsNotAGeoBlock guards the other direction. Scope is a
// label on the episode, and a Global episode that fails failed for some other
// reason — reporting a licence that does not apply would send the caller
// looking in the wrong place, and worse, would latch and skip the rest.
func TestRUVGlobalFailureIsNotAGeoBlock(t *testing.T) {
	srv, _ := ruvCDN(t, map[string]bool{"/opid/": true})
	prog := ruvDecode(t, ruvSeries(srv.URL))

	session := &ruvSession{client: ruvTestClient()}
	_, err := session.file(context.Background(), prog.Episodes[0])
	if err == nil {
		t.Fatal("a refused playlist resolved anyway")
	}
	var refusal *ruvRefused
	if errors.As(err, &refusal) {
		t.Errorf("a Global episode was reported as Iceland-only: %v", err)
	}
	if session.outsideIceland {
		t.Error("a Global failure concluded the caller is outside Iceland")
	}
}

// TestRUVDomesticIsTriedRatherThanPreJudged is why scope is not read as a
// veto. It is a fixed label on the episode — it says "Domestic" to a viewer
// in Reykjavík too, and there the file plays. Refusing on the field alone
// would refuse the one audience this host has.
func TestRUVDomesticIsTriedRatherThanPreJudged(t *testing.T) {
	srv, seen := ruvCDN(t, nil) // a CDN that serves everyone, i.e. Iceland
	prog := ruvDecode(t, ruvDomestic(srv.URL))

	session := &ruvSession{client: ruvTestClient()}
	file, err := session.file(context.Background(), prog.Episodes[0])
	if err != nil {
		t.Fatalf("an Iceland-only episode was refused from inside Iceland: %v", err)
	}
	if len(file.Segments) == 0 {
		t.Error("no segments were resolved")
	}
	if seen.Load() == 0 {
		t.Error("nothing was fetched, so the scope was read as an answer")
	}
}

// TestRUVEpisodeWithNoFileIsSkipped covers a listing entry that exists
// without anything behind it.
func TestRUVEpisodeWithNoFileIsSkipped(t *testing.T) {
	session := &ruvSession{}
	if _, err := session.file(context.Background(),
		ruvEpisode{ID: "bbbb04", Title: "Þáttur 1", Scope: "Global"}); err == nil {
		t.Error("an episode naming no file was accepted")
	}
}

// TestRUVResponseRejectsWhatIsNotAProgramme covers the id trap. The site has
// several kinds of id and Program(id:) takes exactly one of them: the number
// in a /spila/ link. Handed the id from a Featured panel or a Category
// listing it answers a null programme with no error beside it, which must not
// read as an empty programme.
func TestRUVResponseRejectsWhatIsNotAProgramme(t *testing.T) {
	tests := map[string]string{
		"an id from somewhere else on the site": `{"data":{"Program":null}}`,
		"a schema that has moved on": `{"data":{"Program":null},"errors":[
			{"message":"Cannot query field \"scope\" on type \"Episode\".","extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			var out ruvResponse
			if err := json.Unmarshal([]byte(body), &out); err != nil {
				t.Fatal(err)
			}
			prog, err := out.program(10001)
			if err == nil {
				t.Fatalf("accepted %+v", prog)
			}
			if !strings.Contains(err.Error(), "10001") {
				t.Errorf("error = %q, want the id that was asked for", err)
			}
		})
	}

	// The message an errors block carries is worth more than anything this
	// could conclude, so it is passed through.
	var out ruvResponse
	if err := json.Unmarshal([]byte(tests["a schema that has moved on"]), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := out.program(1); err == nil || !strings.Contains(err.Error(), "Cannot query field") {
		t.Errorf("error = %v, want the server's own explanation", err)
	}
}

// TestRUVTarget covers the link shapes the site builds. The station in front
// of /spila/ is cosmetic — all five are one catalogue behind one API — and
// the reading is positional because a slug sits between the marker and the
// id.
func TestRUVTarget(t *testing.T) {
	type target struct {
		programme int
		episode   string
		ok        bool
	}
	tests := map[string]target{
		"https://www.ruv.is/sjonvarp/spila/a-programme/10001":         {10001, "", true},
		"https://www.ruv.is/sjonvarp/spila/a-programme/10001/":        {10001, "", true},
		"https://www.ruv.is/sjonvarp/spila/a-programme/10001/eeee01":  {10001, "eeee01", true},
		"https://ruv.is/utvarp/spila/a-programme/10002":               {10002, "", true},
		"https://www.ruv.is/krakkaruv/spila/a-programme/10003/eeee02": {10003, "eeee02", true},
		"https://www.ruv.is/ungruv/spila/a-programme/1/x":             {1, "x", true},
		"https://www.ruv.is/menntaruv/spila/a-programme/1":            {1, "", true},
		"https://www.ruv.is/sjonvarp/spila/a-programme":               {},
		"https://www.ruv.is/sjonvarp/spila":                           {},
		"https://www.ruv.is/sjonvarp":                                 {},
		"https://www.ruv.is/":                                         {},
		"https://www.ruv.is/sjonvarp/spila/a-programme/not-an-id":     {},
		"https://www.ruv.is/sjonvarp/spila/a-programme/10001?t=60":    {10001, "", true},
		"https://www.ruv.is/frett/2026/08/22/a-news-story":            {},
		"https://www.ruv.is/sjonvarp/spila/a-programme/10001/eeee01/": {10001, "eeee01", true},
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		programme, episode, ok := ruvTarget(u)
		got := target{programme, episode, ok}
		if got != want {
			t.Errorf("ruvTarget(%s) = %+v, want %+v", raw, got, want)
		}
	}
}

// TestRUVTargetIgnoresANumericSlug is why the id is read by position rather
// than by finding the first number after /spila/. A programme whose slug is
// all digits would otherwise supply the id and push the real one into the
// episode.
func TestRUVTargetIgnoresANumericSlug(t *testing.T) {
	u, err := ParseURL("https://www.ruv.is/sjonvarp/spila/112/10001/eeee01")
	if err != nil {
		t.Fatal(err)
	}
	programme, episode, ok := ruvTarget(u)
	if !ok || programme != 10001 || episode != "eeee01" {
		t.Errorf("ruvTarget = (%d, %q, %v), want the segment after the slug", programme, episode, ok)
	}
}

// TestRUVJoin covers the naming rule for a lone episode, whose own title is
// routinely no more than "Þáttur 1 af 6". RÚV writes the pair with a colon
// and this does not: a colon cannot survive a filename.
func TestRUVJoin(t *testing.T) {
	tests := []struct {
		programme, episode, want string
	}{
		{"Kastljós", "Þáttur 1 af 6", "Kastljós - Þáttur 1 af 6"},
		{"A Documentary", "", "A Documentary"},
		{"", "An Episode", "An Episode"},
		// A one-off programme is routinely given a single episode named
		// after itself, and saying it twice helps nobody.
		{"Málæði 2025", "Málæði 2025", "Málæði 2025"},
		{"Málæði 2025", "málæði 2025", "málæði 2025"},
		{"Kastljós", "  ", "Kastljós"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := ruvJoin(tc.programme, tc.episode); got != tc.want {
			t.Errorf("ruvJoin(%q, %q) = %q, want %q", tc.programme, tc.episode, got, tc.want)
		}
	}
}

// TestRUVExtension pins how the two kinds of file are told apart, which is
// the one decision standing between a plain MP3 and the playlist engine.
func TestRUVExtension(t *testing.T) {
	tests := map[string]string{
		"https://vod.example.test/opid/1000002T0/1000002T0.m3u8":   ".m3u8",
		"https://vod.example.test/opid/1000002T0/1000002T0.M3U8":   ".m3u8",
		"https://radio.example.test/opid/1000001D0.mp3":            ".mp3",
		"https://radio.example.test/opid/1000001D0.mp3?token=abcd": ".mp3",
		"https://vod.example.test/opid/1000002T0":                  "",
	}
	for raw, want := range tests {
		if got := ruvExtension(raw); got != want {
			t.Errorf("ruvExtension(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestRUVMatch(t *testing.T) {
	r := NewRUV(nil)
	for _, host := range []string{"ruv.is", "www.ruv.is", "RUV.IS", "spilari.nyr.ruv.is"} {
		if !r.Match(&url.URL{Scheme: "https", Host: host, Path: "/sjonvarp/spila/a/1"}) {
			t.Errorf("%s was not matched", host)
		}
	}
	for _, host := range []string{"ruv.example.test", "notruv.is", "ruv.is.example.test"} {
		if r.Match(&url.URL{Scheme: "https", Host: host, Path: "/sjonvarp/spila/a/1"}) {
			t.Errorf("%s was matched", host)
		}
	}
}

// --- fixtures and helpers -------------------------------------------------

// ruvSeriesResponse is a drama in the shape the API answers one: newest
// first, with reverse_episode_order asking for that to be turned round, and
// every episode's stream URL already present. The %s is the CDN, so a test
// can point it at a server of its own.
const ruvSeriesTemplate = `{"data":{"Program":{
  "id": 10001,
  "slug": "a-programme",
  "title": "Talað yfir 20 og eitthvað",
  "reverse_episode_order": true,
  "episodes": [
    {"id":"aaaa06","title":"Þáttur 6 af 6","scope":"Global","file":"%[1]s/opid/1/1.m3u8"},
    {"id":"aaaa05","title":"Þáttur 5 af 6","scope":"Global","file":"%[1]s/opid/2/2.m3u8"},
    {"id":"aaaa04","title":"Þáttur 4 af 6","scope":"Global","file":"%[1]s/opid/3/3.m3u8"},
    {"id":"aaaa03","title":"Þáttur 3 af 6","scope":"Global","file":"%[1]s/opid/4/4.m3u8"},
    {"id":"aaaa02","title":"Þáttur 2 af 6","scope":"Global","file":"%[1]s/opid/5/5.m3u8"},
    {"id":"aaaa01","title":"Þáttur 1 af 6","scope":"Global","file":"%[1]s/opid/6/6.m3u8"}
  ]}}}`

// ruvNewsResponse is the other shape, and the one that repeats itself. A news
// strand names most instalments by their date and some not at all, and the
// same generic title comes round again and again.
const ruvNewsResponse = `{"data":{"Program":{
  "id": 10002,
  "slug": "a-news-strand",
  "title": "Fréttir",
  "reverse_episode_order": false,
  "episodes": [
    {"id":"bbbb04","title":"Fréttir og veður","scope":"Global","file":"https://vod.example.test/opid/1/1.m3u8"},
    {"id":"bbbb03","title":"21.08.2026","scope":"Global","file":"https://vod.example.test/opid/2/2.m3u8"},
    {"id":"bbbb02","title":"Fréttir og veður","scope":"Global","file":"https://vod.example.test/opid/3/3.m3u8"},
    {"id":"bbbb01","title":"","scope":"Global","file":"https://vod.example.test/opid/4/4.m3u8"}
  ]}}}`

// ruvDomesticResponse is a programme licensed for Iceland only, every episode
// of it. The CDN path says the same thing the scope does: /lokad/ rather than
// /opid/.
const ruvDomesticTemplate = `{"data":{"Program":{
  "id": 10003,
  "slug": "a-locked-programme",
  "title": "Smáspæjararnir",
  "reverse_episode_order": true,
  "episodes": [
    {"id":"cccc03","title":"Þáttur 3","scope":"Domestic","file":"%[1]s/lokad/1/1.m3u8"},
    {"id":"cccc02","title":"Þáttur 2","scope":"Domestic","file":"%[1]s/lokad/2/2.m3u8"},
    {"id":"cccc01","title":"Þáttur 1","scope":"Domestic","file":"%[1]s/lokad/3/3.m3u8"}
  ]}}}`

// ruvRadioResponse is the half of the catalogue that is not a stream at all.
const ruvRadioResponse = `{"data":{"Program":{
  "id": 10004,
  "slug": "a-radio-programme",
  "title": "Brot úr Morgunvaktinni",
  "reverse_episode_order": false,
  "episodes": [
    {"id":"dddd01","title":"21.08.2026","scope":"Global","file":"https://radio.example.test/opid/1000001D0.mp3"}
  ]}}}`

// ruvExampleCDN stands in wherever a test does not need a server.
const ruvExampleCDN = "https://vod.example.test"

// ruvSeries and ruvDomestic point a fixture at a CDN.
func ruvSeries(cdn string) string { return fmt.Sprintf(ruvSeriesTemplate, cdn) }

func ruvDomestic(cdn string) string { return fmt.Sprintf(ruvDomesticTemplate, cdn) }

// ruvDecode unwraps a GraphQL answer the way the extractor does.
func ruvDecode(t *testing.T, body string) *ruvProgram {
	t.Helper()
	var out ruvResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	prog, err := out.program(0)
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

// ruvTestClient retries nothing, so a test expecting a refusal gets one at
// once.
func ruvTestClient() *httpx.Client {
	return httpx.New("test-agent", "en-US", 0, 5*time.Second)
}

// ruvCDN serves the master playlist for any /<scope>/<id>/<id>.m3u8 and the
// variant beneath it, and turns away anything whose path starts with one of
// the given prefixes — which is what Akamai does to a foreign address asking
// for a locked file. The counter is how a test proves a request was not made.
func ruvCDN(t *testing.T, forbid map[string]bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		for prefix := range forbid {
			if strings.HasPrefix(r.URL.Path, prefix) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, "<HTML><HEAD>\n<TITLE>Access Denied</TITLE>\n</HEAD></HTML>")
				return
			}
		}
		w.Header().Set(httpx.HeaderContentType, "application/vnd.apple.mpegurl")
		if strings.Contains(r.URL.Path, "/index.m3u8") {
			_, _ = io.WriteString(w, ruvVariantPlaylist)
			return
		}
		_, _ = io.WriteString(w, ruvMasterPlaylist)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}
