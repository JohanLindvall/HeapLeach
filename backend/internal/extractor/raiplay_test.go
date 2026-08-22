package extractor

import (
	"encoding/json"
	"errors"
	"testing"
)

// raiAkamaiPlaylist is RaiPlay's commonest master playlist, and the reason
// this extractor cannot use hlsVariant.muxed() as its gate: there is no CODECS
// attribute anywhere in it. The segments behind these variants carry H.264 and
// AAC together, but muxed() reads a missing CODECS as "no" and would reject
// every rendition of a perfectly good stream.
const raiAkamaiPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=1758000,NAME="Italiano",RESOLUTION=1024x576
chunklist_b1758000_slita_t64MTgwMA==.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2344000,NAME="Italiano",RESOLUTION=1280x720
chunklist_b2344000_slita_t64MjQwMA==.m3u8
`

// raiMultiaudioPlaylist is the one packaging that genuinely splits the two,
// and it announces itself twice over: the variants name an AUDIO group, and
// the group's renditions have playlists of their own. Rai's own naming says
// the same thing — "vo" is video only, "ao" is audio only — but the URI on the
// EXT-X-MEDIA line is what decides it, since a name is not a contract.
const raiMultiaudioPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=1758000,NAME="Italiano",RESOLUTION=1024x576,AUDIO="aac"
chunklist_b1758000_vo_slita_t64MTgwMA==.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2344000,NAME="Italiano",RESOLUTION=1280x720,AUDIO="aac"
chunklist_b2344000_vo_slita_t64MjQwMA==.m3u8
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",LANGUAGE="ita",NAME="Italiano",DEFAULT=YES,AUTOSELECT=YES,URI="chunklist_b192400_ao_slita_t64aXRhX2F1ZGlv.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aac",LANGUAGE="des",NAME="Audiodescrizione",DEFAULT=NO,AUTOSELECT=YES,URI="chunklist_b192400_ao_sldes_t64ZGVzX2F1ZGlv.m3u8"
`

// raiCsmilPlaylist is the other muxed packaging, which does declare CODECS.
// Every path under it contains ".mp4" because the publishing point is named
// after the mezzanine file, while the segments it serves are MPEG-TS.
const raiCsmilPlaylist = `#EXTM3U
#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=1928124,RESOLUTION=1024x576,FRAME-RATE=25.000,CODECS="avc1.4d401f,mp4a.40.2",VIDEO-RANGE=SDR
chunklist-f1-v1-a1.m3u8
#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=2592188,RESOLUTION=1280x720,FRAME-RATE=25.000,CODECS="avc1.4d4028,mp4a.40.2",VIDEO-RANGE=SDR
chunklist-f2-v1-a1.m3u8
`

// raiRefusedAsset is how Rai declines: 200, an empty container, and a real
// fetchable MP4 that plays a caption saying the video is not available.
// Everything that looks like it should explain the refusal misleads —
// geoprotection reads "N" while refusing, and the licence map is empty — so
// only the container and the substituted clip are believed.
const raiRefusedAsset = `{"video":["https://download.example.test/video_no_available.mp4"],
 "ct":"","bitrate":"0","description":"video non disponibile",
 "geoprotection":"N","licence_server_map":{}}`

// raiPlayableAsset is the same document for something Rai will serve.
const raiPlayableAsset = `{"video":["https://vod.example.test/podcastcdn/Rai/A_PROGRAMME/12345_,1800,2400/playlist.m3u8?hdnea=st=1000~exp=1150~acl=/*~hmac=deadbeef"],
 "playlist":[{"type":"bumper","url":""},{"type":"main","url":"https://vod.example.test/podcastcdn/Rai/A_PROGRAMME/12345_,1800,2400/playlist.m3u8"}],
 "ct":"m3u8","bitrate":"1800","is_live":"N","description":"Una puntata",
 "geoprotection":"N","licence_server_map":{}}`

// raiVideoDocument is a video page. The two fields that decide anything are
// the empty DRM slot — Rai writes an empty object rather than omitting it —
// and the relinker URL, which already carries a query of its own.
const raiVideoDocument = `{
  "id": "ContentItem-00000000-0000-0000-0000-000000000000",
  "type": "RaiPlay Video Item",
  "path_id": "/video/2026/08/Un-Programma-S1E1-Episodio-1-00000000-0000-0000-0000-000000000000.json",
  "name": "Un Programma S1E1 - Episodio 1",
  "subtitle": "St 1 Ep 1 - Un Programma",
  "season": "1",
  "episode": "1",
  "login_required": false,
  "is_live": false,
  "video": {
    "content_url": "https://mediapolisvod.example.test/relinker/relinkerServlet.htm?cont=AAAAAAAA",
    "duration": "00:51:12"
  },
  "rights_management": {
    "rights": {
      "offline": { "Web": true },
      "drm": {},
      "geoprotection": { "Fuori dall'Italia": true, "Fuori dall'Europa": true }
    }
  },
  "program_info": { "name": "Un Programma", "path_id": "/programmi/unprogramma.json" }
}`

// raiNewsDocument is the other naming case: a clip whose own name says nothing
// about the bulletin it came from.
const raiNewsDocument = `{
  "name": "Una notizia del giorno",
  "is_live": false,
  "video": { "content_url": "https://mediapolisvod.example.test/relinker/relinkerServlet.htm?cont=BBBBBBBB" },
  "rights_management": { "rights": { "drm": {} } },
  "program_info": { "name": "Telegiornale" }
}`

// raiProgrammeDocument reproduces the trap in Rai's programme layout: which of
// the two nesting levels names the season is not fixed. Here the blocks are
// the seasons and the sets are the kind of material, so a folder named after
// the set alone would drop two years of "Puntate" into one place. The last
// block is the common shape where both levels share a name.
const raiProgrammeDocument = `{
  "name": "Un Programma",
  "path_id": "/programmi/unprogramma.json",
  "blocks": [
    {
      "name": "Stagione 2026",
      "type": "RaiPlay Multimedia Block",
      "sets": [
        { "name": "Puntate", "type": "RaiPlay Multimedia Set", "path_id": "/programmi/unprogramma/ContentSet-0001.json" },
        { "name": "Clip", "type": "RaiPlay Content Set", "path_id": "/programmi/unprogramma/ContentSet-0002.json" }
      ]
    },
    {
      "name": "Stagione 2025",
      "type": "RaiPlay Multimedia Block",
      "sets": [
        { "name": "Puntate", "type": "RaiPlay Multimedia Set", "path_id": "/programmi/unprogramma/ContentSet-0003.json" }
      ]
    },
    {
      "name": "Speciali",
      "type": "RaiPlay Multimedia Block",
      "sets": [
        { "name": "Speciali", "type": "RaiPlay Multimedia Set", "path_id": "/programmi/unprogramma/ContentSet-0004.json" },
        { "name": "Senza percorso", "type": "RaiPlay Multimedia Set", "path_id": "" }
      ]
    }
  ]
}`

// raiFilmDocument is what a film looks like: a programme with one block, one
// set and one item in it. Nothing about it says "film" — the shape is the
// only difference — so it must come out as a single file in no folder.
const raiFilmDocument = `{
  "name": "Un Film",
  "blocks": [
    { "name": "Un Film", "sets": [
      { "name": "Un Film", "path_id": "/programmi/unfilm/ContentSet-0009.json" } ] }
  ]
}`

// raiEpisodeSet is one listing. The last item repeats the first, which is what
// a clip filed under both its season and the programme's clip reel looks like.
const raiEpisodeSet = `{
  "name": "Puntate",
  "ID": "ContentSet-0001",
  "items": [
    { "name": "Un Programma S1E1 - Episodio 1", "episode": "1", "season": "1",
      "path_id": "/video/2026/08/Un-Programma-ep1-00000000-0000-0000-0000-000000000001.json" },
    { "name": "Un Programma S1E2 - Episodio 2", "episode": "2", "season": "1",
      "path_id": "/video/2026/08/Un-Programma-ep2-00000000-0000-0000-0000-000000000002.json" },
    { "name": "Una voce senza pagina", "path_id": "" },
    { "name": "Un Programma S1E1 - Episodio 1", "episode": "1", "season": "1",
      "path_id": "/video/2026/08/Un-Programma-ep1-00000000-0000-0000-0000-000000000001.json" }
  ]
}`

// TestRaiAkamaiPlaylistIsTakenDespiteHavingNoCodecs is the claim the whole
// extractor rests on. Rai's commonest packaging declares no CODECS, so
// muxed() calls every rendition unusable — and it is wrong: the audio is in
// the same segments. audioElsewhere is what tells the two packagings apart,
// and it is false here.
func TestRaiAkamaiPlaylistIsTakenDespiteHavingNoCodecs(t *testing.T) {
	base, err := ParseURL("https://vod.example.test/podcastcdn/Rai/A/12345_,1800,2400/playlist.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(raiAkamaiPlaylist, base)
	if len(variants) != 2 {
		t.Fatalf("parsed %d variants, want 2", len(variants))
	}
	for _, v := range variants {
		if v.muxed() {
			t.Errorf("%s reads as self-contained; the fixture declares no CODECS, so "+
				"this test no longer covers what it was written for", v.Resolution)
		}
		if v.audioElsewhere {
			t.Errorf("%s reads as video only, but no audio group is declared", v.Resolution)
		}
	}

	best, err := raiUsable(variants)
	if err != nil {
		t.Fatalf("refused a playlist whose segments carry their own audio: %v", err)
	}
	if best.Resolution != "1280x720" {
		t.Errorf("chose %s, want the largest rendition", best.Resolution)
	}
}

// TestRaiMultiaudioPlaylistIsRefused is the regression this extractor exists
// to prevent. bestVariant only ranks and never declines, so handing it the
// multiaudio packaging unfiltered yields a video-only chunklist that
// downloads to the end and plays silently.
func TestRaiMultiaudioPlaylistIsRefused(t *testing.T) {
	base, err := ParseURL("https://vod.example.test/podcastcdn/raitre/raitre_multiaudio_nogeo/12345_,1800,2400/playlist.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(raiMultiaudioPlaylist, base)
	if len(variants) != 2 {
		t.Fatalf("parsed %d variants, want 2", len(variants))
	}
	for _, v := range variants {
		if !v.audioElsewhere {
			t.Errorf("%s reads as self-contained, but its audio group has a URI", v.Resolution)
		}
	}

	// What would happen without the filter, spelled out so the reason for
	// having one cannot be optimised away.
	if unfiltered, ok := bestVariant(variants); !ok || !unfiltered.audioElsewhere {
		t.Error("bestVariant declined a video-only playlist; it is not supposed to decline anything")
	}

	if _, err := raiUsable(variants); !errors.Is(err, errRaiDemuxed) {
		t.Errorf("raiUsable returned %v, want the demuxed refusal", err)
	}
}

// TestRaiUsablePrefersSelfContainedOverBiggerVideoOnly covers a manifest that
// offers both packagings at once. Filtering before ranking is what keeps the
// larger video-only rendition from winning; checking the winner afterwards
// would have refused an asset that had a usable stream all along.
func TestRaiUsablePrefersSelfContainedOverBiggerVideoOnly(t *testing.T) {
	variants := []hlsVariant{
		{URL: "hd_vo.m3u8", Resolution: "1920x1080", Bandwidth: 5000000, audioElsewhere: true},
		{URL: "sd.m3u8", Resolution: "1024x576", Bandwidth: 1758000},
	}
	best, err := raiUsable(variants)
	if err != nil {
		t.Fatalf("raiUsable: %v", err)
	}
	if best.URL != "sd.m3u8" {
		t.Errorf("chose %q, want the smaller rendition that carries its own audio", best.URL)
	}
}

// TestRaiCsmilPlaylistNamesTheContainerFromItsSegments covers the third trap.
// The .csmil publishing point is named after the mezzanine file, so every path
// under it contains ".mp4" and playlistExtension calls the joined file an MP4.
// The segments say otherwise, and they are the ones being joined.
func TestRaiCsmilPlaylistNamesTheContainerFromItsSegments(t *testing.T) {
	base, err := ParseURL("https://cdn.example.test/dom4/podcastcdn/Rai/prod_nogeo/12345_,1800,2400,.mp4.csmil/playlist.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(raiCsmilPlaylist, base)
	best, err := raiUsable(variants)
	if err != nil {
		t.Fatalf("raiUsable: %v", err)
	}
	if !best.muxed() {
		t.Error("the .csmil packaging declares CODECS naming both streams, so it should read as muxed")
	}
	if got := playlistExtension(best); got != ".mp4" {
		t.Fatalf("playlistExtension = %q; the fixture no longer reproduces the trap", got)
	}

	segments := parseMediaPlaylist("#EXTM3U\n#EXTINF:6,\nseg-1-f2-v1-a1.ts\n#EXTINF:6,\nseg-2-f2-v1-a1.ts\n", base)
	if got := raiExtension(segments); got != ".ts" {
		t.Errorf("raiExtension = %q, want .ts: the segments are MPEG-TS whatever the path says", got)
	}
}

func TestRaiExtension(t *testing.T) {
	tests := map[string]string{
		"https://vod.example.test/a/media_b2344000_slita_t64MjQwMA==_0.ts":    ".ts",
		"https://cdn.example.test/a/12345_,1800,.mp4.csmil/seg-1-f2-v1-a1.ts": ".ts",
		"https://cdn.example.test/a/segment-1.mp4":                            ".mp4",
		"https://cdn.example.test/a/segment-1.m4s?tk2=deadbeef":               ".mp4",
		"https://cdn.example.test/a/chunk-1":                                  ".ts",
	}
	for segment, want := range tests {
		if got := raiExtension([]string{segment}); got != want {
			t.Errorf("raiExtension(%s) = %q, want %q", segment, got, want)
		}
	}
	if got := raiExtension(nil); got != ".ts" {
		t.Errorf("raiExtension(nil) = %q, want .ts", got)
	}
}

// TestRaiRefusalIsRecognisedBeforeAnythingIsQueued is the other failure this
// extractor exists to prevent. Rai answers a refusal with 200 and a real MP4,
// which downloads cleanly, agrees with its own Content-Length and records as a
// finished file — a placeholder clip in the download directory instead of the
// programme.
func TestRaiRefusalIsRecognisedBeforeAnythingIsQueued(t *testing.T) {
	var asset raiAsset
	if err := json.Unmarshal([]byte(raiRefusedAsset), &asset); err != nil {
		t.Fatal(err)
	}
	if !asset.refused() {
		t.Fatal("a substituted placeholder clip read as a playable asset")
	}
	// Neither of the fields that look like they should explain it does.
	if !raiEmpty(asset.LicenceServerMap) {
		t.Error("the fixture no longer has the empty licence map that makes this a refusal rather than DRM")
	}

	refusal := &raiRefused{title: "Un Programma", message: asset.Description}
	if got := refusal.reason(); got[:len("video non disponibile")] != "video non disponibile" {
		t.Errorf("reason = %q, want Rai's own sentence first", got)
	}
	if got := refusal.Error(); got[:len("raiplay: Un Programma: ")] != "raiplay: Un Programma: " {
		t.Errorf("Error = %q", got)
	}
}

// TestRaiPlayableAssetIsNotMistakenForARefusal is the other half: the checks
// must not fire on the ordinary case.
func TestRaiPlayableAssetIsNotMistakenForARefusal(t *testing.T) {
	var asset raiAsset
	if err := json.Unmarshal([]byte(raiPlayableAsset), &asset); err != nil {
		t.Fatal(err)
	}
	if asset.refused() {
		t.Error("a playable asset read as refused")
	}
	if asset.CT != "m3u8" {
		t.Errorf("ct = %q", asset.CT)
	}
	// The playlist is taken from "video", not from the "playlist" array beside
	// it: that one carries an advertising bumper first.
	if asset.link() != asset.Video[0] {
		t.Errorf("link = %q, want the first entry of video", asset.link())
	}
}

// TestRaiRefusedRecognisesEitherTell covers both halves of the check, since a
// change of mind on Rai's part about only one of them should still be caught.
func TestRaiRefusedRecognisesEitherTell(t *testing.T) {
	tests := map[string]struct {
		asset raiAsset
		want  bool
	}{
		"empty container and the placeholder": {
			raiAsset{CT: "", Video: []string{"https://download.example.test/video_no_available.mp4"}}, true},
		"empty container alone": {
			raiAsset{CT: "", Video: []string{"https://vod.example.test/a/playlist.m3u8"}}, true},
		"the placeholder alone": {
			raiAsset{CT: "m3u8", Video: []string{"https://download.example.test/video_no_available.mp4"}}, true},
		"nothing named at all": {
			raiAsset{CT: "", Video: nil}, true},
		"an ordinary asset": {
			raiAsset{CT: "m3u8", Video: []string{"https://vod.example.test/a/playlist.m3u8"}}, false},
	}
	for name, tc := range tests {
		if got := tc.asset.refused(); got != tc.want {
			t.Errorf("%s: refused() = %v, want %v", name, got, tc.want)
		}
	}
}

// TestRaiEmptyReadsRaiSFilledInBlanks covers the DRM slots, which Rai writes
// as empty objects rather than omitting. Reading them as raw JSON means a
// value of some shape nobody expected reads as present — the safe direction,
// since the consequence of getting it wrong is a file of noise.
func TestRaiEmptyReadsRaiSFilledInBlanks(t *testing.T) {
	tests := map[string]bool{
		``:     true,
		`null`: true,
		`{}`:   true,
		` {} `: true,
		`[]`:   true,
		`{"widevine":"https://licence.example.test/"}`: false,
		`["widevine"]`: false,
		`"widevine"`:   false,
	}
	for raw, want := range tests {
		if got := raiEmpty(json.RawMessage(raw)); got != want {
			t.Errorf("raiEmpty(%s) = %v, want %v", raw, got, want)
		}
	}
}

// TestRaiVideoDocumentIsReadForWhatDecides pins the fields the extractor acts
// on, including the empty DRM object that must not read as protection.
func TestRaiVideoDocumentIsReadForWhatDecides(t *testing.T) {
	var doc raiVideo
	if err := json.Unmarshal([]byte(raiVideoDocument), &doc); err != nil {
		t.Fatal(err)
	}
	if !raiEmpty(doc.RightsManagement.Rights.DRM) {
		t.Error("an empty drm object read as DRM protection")
	}
	if doc.IsLive {
		t.Error("is_live read as true")
	}
	if doc.Video.ContentURL == "" {
		t.Fatal("no content_url read")
	}
	// The episode's own name already says which programme it belongs to, so
	// nothing is prefixed to it.
	if got := raiTitle(doc.ProgramInfo.Name, doc.Name); got != "Un Programma S1E1 - Episodio 1" {
		t.Errorf("title = %q", got)
	}
}

func TestRaiTitle(t *testing.T) {
	tests := []struct{ program, name, want string }{
		// A drama episode already opens with its series name.
		{"Un Programma", "Un Programma S1E1 - Episodio 1", "Un Programma S1E1 - Episodio 1"},
		// A news clip does not, and without the bulletin's name the file says
		// nothing about where it came from.
		{"Telegiornale", "Una notizia del giorno", "Telegiornale - Una notizia del giorno"},
		// A film's page names it twice.
		{"Un Film", "Un Film", "Un Film"},
		{"", "Solo un nome", "Solo un nome"},
		{"Solo un programma", "", "Solo un programma"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := raiTitle(tc.program, tc.name); got != tc.want {
			t.Errorf("raiTitle(%q, %q) = %q, want %q", tc.program, tc.name, got, tc.want)
		}
	}
}

// TestRaiNewsDocumentGetsItsBulletinPrefixed is the other naming case, read
// off a document rather than asserted about two strings.
func TestRaiNewsDocumentGetsItsBulletinPrefixed(t *testing.T) {
	var doc raiVideo
	if err := json.Unmarshal([]byte(raiNewsDocument), &doc); err != nil {
		t.Fatal(err)
	}
	if got := raiTitle(doc.ProgramInfo.Name, doc.Name); got != "Telegiornale - Una notizia del giorno" {
		t.Errorf("title = %q", got)
	}
}

// TestRaiListingsKeepBothNestingLevels is the programme-layout trap. Rai puts
// the season on the block for one programme and on the set for the next, so a
// folder named after either level alone collides: here it would drop two
// seasons of "Puntate" into one folder and lose which year each belongs to.
func TestRaiListingsKeepBothNestingLevels(t *testing.T) {
	var doc raiProgramme
	if err := json.Unmarshal([]byte(raiProgrammeDocument), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Name != "Un Programma" {
		t.Errorf("programme name = %q", doc.Name)
	}

	refs := raiListings(doc)
	want := []raiSetRef{
		{label: "Stagione 2026 - Puntate", page: "/programmi/unprogramma/ContentSet-0001.json"},
		{label: "Stagione 2026 - Clip", page: "/programmi/unprogramma/ContentSet-0002.json"},
		{label: "Stagione 2025 - Puntate", page: "/programmi/unprogramma/ContentSet-0003.json"},
		// Both levels named it the same; saying so twice helps nobody.
		{label: "Speciali", page: "/programmi/unprogramma/ContentSet-0004.json"},
	}
	if len(refs) != len(want) {
		t.Fatalf("got %d listings, want %d (%+v)", len(refs), len(want), refs)
	}
	for i, ref := range refs {
		if ref != want[i] {
			t.Errorf("listing %d = %+v, want %+v", i, ref, want[i])
		}
	}
}

func TestRaiLabel(t *testing.T) {
	tests := []struct{ block, set, want string }{
		{"Stagione 2026", "Puntate", "Stagione 2026 - Puntate"},
		{"Puntate", "Stagione 2022", "Puntate - Stagione 2022"},
		{"Speciali", "Speciali", "Speciali"},
		{"Clip", "clip", "Clip"},
		{"", "Puntate", "Puntate"},
		{"Episodi", "", "Episodi"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := raiLabel(tc.block, tc.set); got != tc.want {
			t.Errorf("raiLabel(%q, %q) = %q, want %q", tc.block, tc.set, got, tc.want)
		}
	}
}

// TestRaiItemsSkipWhatIsAlreadyTaken guards the dedupe. A clip listed under
// its season and again under the programme's clip reel would otherwise be
// downloaded twice, into two different folders.
func TestRaiItemsSkipWhatIsAlreadyTaken(t *testing.T) {
	var listing raiSet
	if err := json.Unmarshal([]byte(raiEpisodeSet), &listing); err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool)
	entries := raiItems(listing, "Stagione 2026 - Puntate", seen)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (the repeat and the entry with no path must be dropped): %+v",
			len(entries), entries)
	}
	if entries[0].name != "Un Programma S1E1 - Episodio 1" {
		t.Errorf("first entry = %+v", entries[0])
	}
	if entries[0].label != "Stagione 2026 - Puntate" {
		t.Errorf("first entry filed under %q", entries[0].label)
	}
	if again := raiItems(listing, "Stagione 2026 - Clip", seen); len(again) != 0 {
		t.Errorf("the same listing yielded %d entries a second time", len(again))
	}
}

// TestRaiNestOnlyWhenThereIsSomethingToTellApart covers the folder rule from
// both ends: a film, which is a programme with exactly one listing, must land
// as a bare file rather than inside a folder repeating its own name.
func TestRaiNestOnlyWhenThereIsSomethingToTellApart(t *testing.T) {
	var film raiProgramme
	if err := json.Unmarshal([]byte(raiFilmDocument), &film); err != nil {
		t.Fatal(err)
	}
	single := raiListings(film)
	if len(single) != 1 || single[0].label != "Un Film" {
		t.Fatalf("film listings = %+v", single)
	}
	if raiNest([]raiEntry{{label: "Un Film"}, {label: "Un Film"}}) {
		t.Error("one listing was nested; the folder would only repeat the job's own name")
	}

	if !raiNest([]raiEntry{{label: "Stagione 2026 - Puntate"}, {label: "Stagione 2025 - Puntate"}}) {
		t.Error("two listings were not nested; their episodes would collide")
	}
	// A listing that named nothing is not a listing to tell apart.
	if raiNest([]raiEntry{{label: ""}, {label: "Speciali"}}) {
		t.Error("an unnamed listing counted towards nesting")
	}
}

// TestRaiDocument covers the one piece of URL handling there is: every page
// answers as JSON at its own address, and Rai's listings quote paths that way
// already.
func TestRaiDocument(t *testing.T) {
	tests := map[string]string{
		"/video/2026/08/Un-Programma-ep1-0000.html":   raiRoot + "/video/2026/08/Un-Programma-ep1-0000.json",
		"/video/2026/08/Un-Programma-ep1-0000":        raiRoot + "/video/2026/08/Un-Programma-ep1-0000.json",
		"/video/2026/08/Un-Programma-ep1-0000.json":   raiRoot + "/video/2026/08/Un-Programma-ep1-0000.json",
		"/programmi/unprogramma":                      raiRoot + "/programmi/unprogramma.json",
		"/programmi/unprogramma/":                     raiRoot + "/programmi/unprogramma.json",
		"/programmi/unprogramma/ContentSet-0001.json": raiRoot + "/programmi/unprogramma/ContentSet-0001.json",
	}
	for page, want := range tests {
		if got := raiDocument(page); got != want {
			t.Errorf("raiDocument(%s) = %s, want %s", page, got, want)
		}
	}
}

// TestRaiRelinkerAsksForJSON pins the parameter that turns the servlet's 302
// into the document this reads. Without it there is nothing to inspect and a
// refusal would only be discovered by downloading it.
func TestRaiRelinkerAsksForJSON(t *testing.T) {
	tests := map[string]string{
		"https://mediapolisvod.example.test/relinker/relinkerServlet.htm?cont=AAAA": "https://mediapolisvod.example.test/relinker/relinkerServlet.htm?cont=AAAA&output=62",
		"https://mediapolisvod.example.test/relinker/relinkerServlet.htm":           "https://mediapolisvod.example.test/relinker/relinkerServlet.htm?output=62",
	}
	for in, want := range tests {
		if got := raiRelinker(in); got != want {
			t.Errorf("raiRelinker(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestRaiMatch keeps the extractor to the pages it resolves. The relinker and
// the CDNs are hosts of Rai's, but nothing there is a page to paste.
func TestRaiMatch(t *testing.T) {
	rai := NewRaiPlay(nil)
	for raw, want := range map[string]bool{
		"https://www.raiplay.it/video/2026/08/Un-Programma-ep1-0000.html": true,
		"https://raiplay.it/programmi/unprogramma":                        true,
		"https://mediapolisvod.rai.it/relinker/relinkerServlet.htm":       false,
		"https://www.rai.it/":                  false,
		"https://raiplay.example.test/video/1": false,
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := rai.Match(u); got != want {
			t.Errorf("Match(%s) = %v, want %v", raw, got, want)
		}
	}
}
