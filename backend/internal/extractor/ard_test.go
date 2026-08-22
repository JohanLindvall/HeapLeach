package extractor

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// The listing ids below are base64 of a content identifier, which is what
// ARD's own are, and the relationship between them is the point: an
// accessibility copy is the episode's identifier with a segment appended.
//
//	Y3JpZDovL2V4YW1wbGUudGVzdC8xMTExX2dhbnplU2VuZHVuZw
//	  = crid://example.test/1111_ganzeSendung
//	Y3JpZDovL2V4YW1wbGUudGVzdC8xMTExX2dhbnplU2VuZHVuZy9hdWRpb2Rlc2tyaXB0aW9u
//	  = crid://example.test/1111_ganzeSendung/audiodeskription
const (
	ardEpisodeOneID   = "Y3JpZDovL2V4YW1wbGUudGVzdC8xMTExX2dhbnplU2VuZHVuZw"
	ardEpisodeOneAdID = "Y3JpZDovL2V4YW1wbGUudGVzdC8xMTExX2dhbnplU2VuZHVuZy9hdWRpb2Rlc2tyaXB0aW9u"
	ardEpisodeTwoID   = "Y3JpZDovL2V4YW1wbGUudGVzdC8yMjIyX2dhbnplU2VuZHVuZw"
	ardEpisodeTwoAdID = "Y3JpZDovL2V4YW1wbGUudGVzdC8yMjIyX2dhbnplU2VuZHVuZy9hdWRpb2Rlc2tyaXB0aW9u"
	ardTrailerID      = "Y3JpZDovL2V4YW1wbGUudGVzdC90cmFpbGVyLTE"
	ardBonusID        = "Y3JpZDovL2V4YW1wbGUudGVzdC9ib251cy0x"
	ardClipID         = "Y3JpZDovL2V4YW1wbGUudGVzdC9jbGlwLTE"
	ardShowID         = "Y3JpZDovL2V4YW1wbGUudGVzdC9hLWRyYW1h"
)

// ardPlayableItem is one episode as the item endpoint serves it, with the
// trap that decides the whole extractor reproduced faithfully.
//
// The media list holds the same programme three times over, distinguished
// only by audios[].kind, and the ordering is ARD's own: the dialogue-boosted
// remix comes first and the audio description second, so the *first* 1080p
// entry in the list is not the one anybody wants. The plain broadcast mix is
// last.
const ardPlayableItem = `{
  "id": "` + ardEpisodeOneID + `",
  "widgets": [
    {
      "id": "` + ardEpisodeOneID + `",
      "title": "Folge 1 · Staffel 1 | A Drama (S01/E01)",
      "blockedByFsk": false,
      "blockedByLoginOnly": false,
      "availableTo": "2028-08-20T20:50:00Z",
      "geoblocked": false,
      "show": { "id": "` + ardShowID + `", "title": "A Drama" },
      "mediaCollection": {
        "href": "https://api.example.test/mediacollection/1111",
        "embedded": {
          "id": "` + ardEpisodeOneID + `",
          "isGeoBlocked": false,
          "meta": { "title": "Folge 1 · Staffel 1 | A Drama", "seriesTitle": "A Drama", "durationSeconds": 3255 },
          "subtitles": [],
          "streams": [
            {
              "kind": "main",
              "kindName": "Normal",
              "videoLanguageCode": "",
              "media": [
                { "url": "https://cdn.example.test/1111_klaresprache_1920x1080.mp4",
                  "mimeType": "video/mp4", "maxVResolutionPx": 1080, "forcedLabel": "HD 1080p",
                  "audios": [{ "kind": "speech-optimized", "languageCode": "deu" }] },
                { "url": "https://cdn.example.test/1111_audiodeskription_1920x1080.mp4",
                  "mimeType": "video/mp4", "maxVResolutionPx": 1080, "forcedLabel": "HD 1080p",
                  "audios": [{ "kind": "audio-description", "languageCode": "deu" }] },
                { "url": "https://cdn.example.test/ad/index.,1080,720,.mp4.csmil/master.m3u8",
                  "mimeType": "application/vnd.apple.mpegurl", "maxVResolutionPx": 1080, "forcedLabel": "Auto",
                  "audios": [{ "kind": "audio-description", "languageCode": "deu" }] },
                { "url": "https://cdn.example.test/1111_sendeton_1280x720.mp4",
                  "mimeType": "video/mp4", "maxVResolutionPx": 720, "forcedLabel": "HD 720p",
                  "audios": [{ "kind": "standard", "languageCode": "deu" }] },
                { "url": "https://cdn.example.test/1111_sendeton_1920x1080.mp4",
                  "mimeType": "video/mp4", "maxVResolutionPx": 1080, "forcedLabel": "HD 1080p",
                  "audios": [{ "kind": "standard", "languageCode": "deu" }] },
                { "url": "https://cdn.example.test/main/index.,1080,720,.mp4.csmil/master.m3u8",
                  "mimeType": "application/vnd.apple.mpegurl", "maxVResolutionPx": 1080, "forcedLabel": "Auto",
                  "audios": [{ "kind": "standard", "languageCode": "deu" }] }
              ]
            }
          ]
        }
      }
    }
  ]
}`

// ardGeoBlockedItem is the other half of that trap: the widget's own
// `geoblocked` says one thing and the media collection says the opposite, and
// the CDN agrees with the media collection.
const ardGeoBlockedItem = `{
  "widgets": [
    {
      "title": "Folge 2 · Staffel 1 | A Drama (S01/E02)",
      "blockedByFsk": false,
      "blockedByLoginOnly": false,
      "geoblocked": false,
      "show": { "title": "A Drama" },
      "mediaCollection": {
        "embedded": {
          "isGeoBlocked": true,
          "meta": { "title": "Folge 2 · Staffel 1 | A Drama", "seriesTitle": "A Drama" },
          "streams": [
            { "kind": "main", "media": [
              { "url": "https://cdn.example.test/2222_sendeton_1920x1080.mp4",
                "mimeType": "video/mp4", "maxVResolutionPx": 1080,
                "audios": [{ "kind": "standard", "languageCode": "deu" }] }
            ] }
          ]
        }
      }
    }
  ]
}`

// ardAgeRatedItem is what an age-rated title answers with outside the hours
// it may be shown: no media collection at all, and one flag saying why.
const ardAgeRatedItem = `{
  "widgets": [
    {
      "title": "A Late Film",
      "blockedByFsk": true,
      "blockedByLoginOnly": false,
      "geoblocked": false,
      "maturityContentRating": "FSK16",
      "show": { "title": "A Late Film" },
      "mediaCollection": null
    }
  ]
}`

// ardLiveItem is a live relay. It is playable and not blocked; it simply has
// no end, which nothing downstream would notice.
const ardLiveItem = `{
  "widgets": [
    {
      "title": "The Third Stage",
      "show": { "title": "Sport" },
      "mediaCollection": {
        "embedded": {
          "isGeoBlocked": false,
          "live": { "dvrWindowSeconds": 7200, "availFromDateTime": "2026-08-22T14:00:00Z" },
          "streams": [
            { "kind": "main", "media": [
              { "url": "https://cdn.example.test/live/index.m3u8",
                "mimeType": "application/vnd.apple.mpegurl", "maxVResolutionPx": 1080, "audios": [] }
            ] }
          ]
        }
      }
    }
  ]
}`

// ardShowListing is a programme's flat listing, in the shape the asset widget
// serves one. Three things a real listing does are reproduced:
//
//   - every episode is followed by its audio-described copy, whose id is the
//     episode's own with a segment appended;
//   - a trailer, a bonus feature and a clip extract sit among the episodes;
//   - the season list is published on each entry's `show` block rather than
//     once at the top of the document, which is the only place it exists.
const ardShowListing = `{
  "id": "` + ardShowID + `",
  "title": "A Drama",
  "compilationType": "itemsOfShow",
  "pagination": { "pageNumber": 0, "pageSize": 7, "totalElements": 7 },
  "teasers": [
    { "coreAssetType": "EXTRA_BONUS_CONTENT", "shortTitle": "A Drama in Concert",
      "links": { "target": { "id": "` + ardBonusID + `" } },
      "show": { "title": "A Drama", "availableSeasons": ["1", "2"] } },
    { "coreAssetType": "EXTRA_TRAILER", "shortTitle": "Trailer",
      "links": { "target": { "id": "` + ardTrailerID + `" } },
      "show": { "title": "A Drama", "availableSeasons": ["1", "2"] } },
    { "coreAssetType": "EPISODE", "shortTitle": "Folge 1 · Staffel 1 | A Drama (S01/E01)",
      "links": { "target": { "id": "` + ardEpisodeOneID + `" } },
      "show": { "title": "A Drama", "availableSeasons": ["1", "2"] } },
    { "coreAssetType": "EPISODE", "shortTitle": "Folge 1 · Staffel 1 | A Drama (S01/E01) (Audiodeskription)",
      "links": { "target": { "id": "` + ardEpisodeOneAdID + `" } },
      "show": { "title": "A Drama", "availableSeasons": ["1", "2"] } },
    { "coreAssetType": "EPISODE", "shortTitle": "Folge 2 · Staffel 1 | A Drama (S01/E02)",
      "links": { "target": { "id": "` + ardEpisodeTwoID + `" } },
      "show": { "title": "A Drama", "availableSeasons": ["1", "2"] } },
    { "coreAssetType": "EPISODE", "shortTitle": "Folge 2 · Staffel 1 | A Drama (S01/E02) (Audiodeskription)",
      "links": { "target": { "id": "` + ardEpisodeTwoAdID + `" } },
      "show": { "title": "A Drama", "availableSeasons": ["1", "2"] } },
    { "coreAssetType": "SECTION", "shortTitle": "A three minute extract",
      "links": { "target": { "id": "` + ardClipID + `" } },
      "show": { "title": "A Drama", "availableSeasons": ["1", "2"] } }
  ]
}`

// ardSeasonListing is one season fetched on its own. The season endpoint
// names the season rather than the programme, and leaves the audio-described
// copies out of its own accord — which the flat listing above does not.
const ardSeasonListing = `{
  "id": "` + ardShowID + `",
  "title": "Staffel 2",
  "compilationType": "itemsOfSeason",
  "seasonNumber": 2,
  "withAudiodescription": false,
  "pagination": { "pageNumber": 0, "pageSize": 2, "totalElements": 2 },
  "teasers": [
    { "coreAssetType": "EPISODE", "shortTitle": "Folge 1 · Staffel 2 | A Drama (S02/E01)",
      "links": { "target": { "id": "` + ardEpisodeOneID + `" } },
      "show": { "title": "A Drama", "availableSeasons": ["1", "2"] } },
    { "coreAssetType": "EPISODE", "shortTitle": "Folge 2 · Staffel 2 | A Drama (S02/E02)",
      "links": { "target": { "id": "` + ardEpisodeTwoID + `" } },
      "show": { "title": "A Drama", "availableSeasons": ["1", "2"] } }
  ]
}`

// ardMagazineListing is the other listing shape: a strand that publishes
// nothing the catalogue calls an episode. Everything it has must survive the
// filter, or the programme reads as empty.
const ardMagazineListing = `{
  "title": "A Magazine",
  "compilationType": "itemsOfShow",
  "pagination": { "pageNumber": 0, "pageSize": 2, "totalElements": 2 },
  "teasers": [
    { "coreAssetType": "SECTION", "shortTitle": "This week",
      "links": { "target": { "id": "` + ardEpisodeOneID + `" } },
      "show": { "title": "A Magazine", "availableSeasons": null } },
    { "coreAssetType": "SECTION", "shortTitle": "Last week",
      "links": { "target": { "id": "` + ardEpisodeTwoID + `" } },
      "show": { "title": "A Magazine", "availableSeasons": null } }
  ]
}`

// ardLaterPage is what a listing's second page answers with, and the reason
// only totalElements may be read: pageNumber and pageSize come back as an
// offset and a count instead of the values that were asked for.
const ardLaterPage = `{
  "title": "A Drama",
  "pagination": { "pageNumber": 100, "pageSize": 1, "totalElements": 101 },
  "teasers": [
    { "coreAssetType": "EXTRA_BONUS_CONTENT", "shortTitle": "Behind the scenes",
      "links": { "target": { "id": "` + ardBonusID + `" } },
      "show": { "title": "A Drama" } }
  ]
}`

// ardMasterPlaylist is ARD's own master playlist, and carries the two things
// that matter about it. There is no EXT-X-MEDIA entry anywhere, so every
// variant holds its video and its audio in the same segments; and the
// manifest is served from a path built out of the source file's name, so the
// URLs say "mp4" while the segments behind them are MPEG-TS.
const ardMasterPlaylist = `#EXTM3U
#EXT-X-STREAM-INF:PROGRAM-ID=1,RESOLUTION=640x360,FRAME-RATE=50.000,CODECS="avc1.4d401f,mp4a.40.2",BANDWIDTH=1625824,AVERAGE-BANDWIDTH=1274088,VIDEO-RANGE=SDR
index-f1-v1-a1.m3u8
#EXT-X-STREAM-INF:PROGRAM-ID=1,RESOLUTION=1920x1080,FRAME-RATE=50.000,CODECS="avc1.64002a,mp4a.40.2",BANDWIDTH=5488096,AVERAGE-BANDWIDTH=4565880,VIDEO-RANGE=SDR
index-f2-v1-a1.m3u8
#EXT-X-STREAM-INF:PROGRAM-ID=1,RESOLUTION=1280x720,FRAME-RATE=50.000,CODECS="avc1.640020,mp4a.40.2",BANDWIDTH=3923936,AVERAGE-BANDWIDTH=3192592,VIDEO-RANGE=SDR
index-f3-v1-a1.m3u8
#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=128000,CODECS="mp4a.40.2"
index-f6-a1.m3u8
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=469287,RESOLUTION=1920x1080,CODECS="avc1.64002a",URI="iframes-f2-v1-a1.m3u8",VIDEO-RANGE=SDR
`

// ardVariantPlaylist is what the chosen variant answers with.
const ardVariantPlaylist = `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-VERSION:3
#EXT-X-MEDIA-SEQUENCE:1
#EXTINF:2.000,
segment-1-f2-v1-a1.ts
#EXTINF:6.000,
segment-2-f2-v1-a1.ts
`

// TestARDPrefersTheBroadcastSoundtrack is the regression guard for the
// decision the whole extractor turns on. Every rendition of this episode is
// the same programme; three of them are 1080p; and the two listed first are a
// dialogue-boosted remix and an audio description with a narrator over the
// picture. Anything that ranks by resolution downloads one of those, at full
// quality, and reports a clean finish.
func TestARDPrefersTheBroadcastSoundtrack(t *testing.T) {
	var doc ardItem
	if err := json.Unmarshal([]byte(ardPlayableItem), &doc); err != nil {
		t.Fatal(err)
	}
	media := doc.Widgets[0].MediaCollection.Embedded

	best, ok := media.best(ardMimeMP4)
	if !ok {
		t.Fatal("no MP4 was chosen")
	}
	if best.URL != "https://cdn.example.test/1111_sendeton_1920x1080.mp4" {
		t.Errorf("chose %q, want the 1080p broadcast mix", best.URL)
	}

	// The fallback ranks the same way: the described master playlist is
	// listed before the ordinary one and must not win either.
	hls, ok := media.best(ardMimeHLS)
	if !ok {
		t.Fatal("no playlist was chosen")
	}
	if hls.URL != "https://cdn.example.test/main/index.,1080,720,.mp4.csmil/master.m3u8" {
		t.Errorf("chose %q, want the playlist carrying the broadcast mix", hls.URL)
	}
}

// TestARDAudioRankOrdersTheSoundtracks pins the ordering itself, including
// the treatment of a name nobody here has seen: it must outrank the two known
// accessibility tracks, since it is far more likely to be another ordinary
// soundtrack than another of those.
func TestARDAudioRankOrdersTheSoundtracks(t *testing.T) {
	rank := func(kinds ...string) int {
		var m ardMedium
		for _, k := range kinds {
			m.Audios = append(m.Audios, struct {
				Kind string `json:"kind"`
			}{Kind: k})
		}
		return ardAudioRank(m)
	}
	standard, described, remixed := rank("standard"), rank("audio-description"), rank("speech-optimized")
	unknown, absent := rank("something-new"), rank()

	if !(standard > unknown && unknown > remixed && remixed > described) {
		t.Errorf("ranks standard=%d unknown=%d speech-optimized=%d audio-description=%d",
			standard, unknown, remixed, described)
	}
	if absent != unknown {
		t.Errorf("a rendition that names no soundtrack ranks %d, want %d", absent, unknown)
	}
	// Case is the API's business, not ours.
	if rank("STANDARD") != standard {
		t.Error("the soundtrack name was compared case-sensitively")
	}
}

// TestARDBetterPutsSoundBeforePicture states the comparison in the terms that
// matter: a small broadcast mix beats a large audio description, because the
// two are not the same programme.
func TestARDBetterPutsSoundBeforePicture(t *testing.T) {
	described := ardMedium{MaxVResolutionPx: 1080, Audios: []struct {
		Kind string `json:"kind"`
	}{{Kind: "audio-description"}}}
	broadcast := ardMedium{MaxVResolutionPx: 540, Audios: []struct {
		Kind string `json:"kind"`
	}{{Kind: "standard"}}}

	if !ardBetter(broadcast, described) {
		t.Error("a 1080p audio description was preferred to a 540p broadcast mix")
	}
	if ardBetter(described, broadcast) {
		t.Error("the comparison is not antisymmetric")
	}
}

// TestARDRefusalsReadTheRightFlag is the geo-blocking guard. Two fields in
// one document look like they answer the question and only one of them does;
// the widget's own `geoblocked` reads false on material the CDN refuses to
// everybody outside Germany.
func TestARDRefusalsReadTheRightFlag(t *testing.T) {
	var doc ardItem
	if err := json.Unmarshal([]byte(ardGeoBlockedItem), &doc); err != nil {
		t.Fatal(err)
	}
	widget := doc.Widgets[0]
	if widget.Geoblocked {
		t.Fatal("the fixture no longer reproduces the disagreement it is here for")
	}
	if !widget.MediaCollection.Embedded.IsGeoBlocked {
		t.Fatal("the media collection's own flag did not parse")
	}

	reason := widget.refusal()
	if reason == "" {
		t.Fatal("a geo-blocked episode was accepted; it would have failed as a bare 403")
	}
	if !ardSays(reason, "Germany") {
		t.Errorf("refusal = %q, which does not say where it is available", reason)
	}
}

// TestARDRefusalExplainsAnAgeRating covers the ordering inside refusal. An
// age-rated title arrives with no media collection at all, so a check that
// looked for the stream first would report "offers no stream" — true, and no
// help to somebody who only has to come back later.
func TestARDRefusalExplainsAnAgeRating(t *testing.T) {
	var doc ardItem
	if err := json.Unmarshal([]byte(ardAgeRatedItem), &doc); err != nil {
		t.Fatal(err)
	}
	widget := doc.Widgets[0]
	if widget.MediaCollection != nil {
		t.Fatal("the fixture no longer reproduces the missing media collection")
	}
	reason := widget.refusal()
	if !ardSays(reason, "age rating") {
		t.Errorf("refusal = %q, want the age rating named", reason)
	}
}

// TestARDRefusesALiveRelay guards the one case where the payload is entirely
// healthy and the download would still be wrong: a live playlist is a rolling
// window with no end declared, so joining what it lists yields a fragment of
// the broadcast that looks like a finished file.
func TestARDRefusesALiveRelay(t *testing.T) {
	var doc ardItem
	if err := json.Unmarshal([]byte(ardLiveItem), &doc); err != nil {
		t.Fatal(err)
	}
	widget := doc.Widgets[0]
	if widget.MediaCollection.Embedded.Live == nil {
		t.Fatal("the live block did not parse")
	}
	if reason := widget.refusal(); !ardSays(reason, "live") {
		t.Errorf("refusal = %q, want a live relay refused as one", reason)
	}
}

// TestARDPlayableItemIsNotRefused is the other half of those: a perfectly
// ordinary episode must pass every check.
func TestARDPlayableItemIsNotRefused(t *testing.T) {
	var doc ardItem
	if err := json.Unmarshal([]byte(ardPlayableItem), &doc); err != nil {
		t.Fatal(err)
	}
	if reason := doc.Widgets[0].refusal(); reason != "" {
		t.Errorf("a playable episode was refused: %s", reason)
	}
}

// TestARDRefusalReportsAnExpiredLicence covers the remaining shape of an
// absent media collection, where the timestamp beside it says what happened.
func TestARDRefusalReportsAnExpiredLicence(t *testing.T) {
	var widget ardItemWidget
	body := `{"title":"A Drama","availableTo":"2001-01-01T00:00:00Z","mediaCollection":null}`
	if err := json.Unmarshal([]byte(body), &widget); err != nil {
		t.Fatal(err)
	}
	if got := widget.refusal(); !ardSays(got, "2001-01-01") {
		t.Errorf("refusal = %q, want the date its availability ended", got)
	}

	// A licence that has not run out yet explains nothing, and must not be
	// reported as though it had.
	widget.AvailableTo = "2099-01-01T00:00:00Z"
	if got := widget.refusal(); ardSays(got, "2099") {
		t.Errorf("refusal = %q, but the licence is still current", got)
	}
}

// TestARDMasterPlaylistIsSelfContained pins the claim the fallback rests on.
// ARD declares no audio group at all, so every variant carries video and
// audio in the same segments and concatenating one yields something that
// plays. A host whose variants pointed their audio elsewhere would belong on
// the external downloader instead.
func TestARDMasterPlaylistIsSelfContained(t *testing.T) {
	base, err := ParseURL("https://cdn.example.test/main/index.,1080,720,.mp4.csmil/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(ardMasterPlaylist, base)
	if len(variants) != 4 {
		t.Fatalf("parsed %d variants, want 4", len(variants))
	}
	for _, v := range variants {
		if v.Resolution == "" {
			continue // the audio-only variant, which carries no picture
		}
		if !v.muxed() {
			t.Errorf("%s reads as video only, but ARD declares no audio group at all", v.Resolution)
		}
	}

	best, ok := bestVariant(variants)
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if best.Resolution != "1920x1080" {
		t.Errorf("chose %s, want the largest rendition", best.Resolution)
	}
}

// TestARDExtensionReadsTheSegments is the reason this host names its own
// output. ARD builds the manifest path out of the source file's name, so an
// MPEG-TS stream is served from a directory called "....mp4.csmil" and the
// shared rule reads that ".mp4" — naming a concatenation of TS segments as an
// MP4, which is a file players are entitled to refuse.
func TestARDExtensionReadsTheSegments(t *testing.T) {
	base, err := ParseURL("https://cdn.example.test/main/index.,1080,720,.mp4.csmil/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	variant, ok := bestVariant(parseMasterPlaylist(ardMasterPlaylist, base))
	if !ok {
		t.Fatal("no variant was chosen")
	}
	segments := parseMediaPlaylist(ardVariantPlaylist, mustParse(t, variant.URL))
	if len(segments) != 2 {
		t.Fatalf("parsed %d segments, want 2", len(segments))
	}

	if got := playlistExtension(variant); got != ".mp4" {
		t.Fatalf("the shared rule now answers %q for this URL; "+
			"if it reads the segments too, ardExtension can go", got)
	}
	if got := ardExtension(segments, variant); got != ".ts" {
		t.Errorf("extension %q, want .ts — the segments are MPEG-TS", got)
	}
}

// TestARDExtensionFallsBackToTheVariant covers a playlist whose segments are
// named in a way this does not recognise, where the shared rule is still the
// best answer available.
func TestARDExtensionFallsBackToTheVariant(t *testing.T) {
	variant := hlsVariant{URL: "https://cdn.example.test/cmaf/index.m3u8"}
	if got := ardExtension([]string{"https://cdn.example.test/cmaf/chunk?n=1"}, variant); got != ".mp4" {
		t.Errorf("extension %q, want the shared rule's answer for a cmaf URL", got)
	}
	if got := ardExtension(nil, hlsVariant{URL: "https://cdn.example.test/a/index.m3u8"}); got != ".ts" {
		t.Errorf("extension %q, want .ts", got)
	}
	// An initialisation segment leads the list for fragmented MP4.
	if got := ardExtension([]string{"https://cdn.example.test/a/init.mp4"}, hlsVariant{}); got != ".mp4" {
		t.Errorf("extension %q, want .mp4", got)
	}
}

// TestARDRefsDropTheAudioDescribedCopies is the listing guard. A flat listing
// files every episode twice — once plainly and once with a narrator over the
// picture — and the second copy is not marked as anything: it is the same
// content identifier with a segment appended, which is what the ids decode
// to.
func TestARDRefsDropTheAudioDescribedCopies(t *testing.T) {
	var widget ardWidget
	if err := json.Unmarshal([]byte(ardShowListing), &widget); err != nil {
		t.Fatal(err)
	}
	if widget.Title != "A Drama" {
		t.Errorf("title = %q", widget.Title)
	}

	refs := ardRefs(widget.Teasers, "Staffel 1")
	if len(refs) != 2 {
		t.Fatalf("got %d entries, want the two episodes: %+v", len(refs), refs)
	}
	want := []ardEpisodeRef{
		{id: ardEpisodeOneID, name: "Folge 1 · Staffel 1 | A Drama (S01/E01)", season: "Staffel 1"},
		{id: ardEpisodeTwoID, name: "Folge 2 · Staffel 1 | A Drama (S01/E02)", season: "Staffel 1"},
	}
	for i, ref := range refs {
		if ref != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, ref, want[i])
		}
	}
}

// TestARDWithoutVariantsKeepsTheOnlyCopy covers the case that makes the rule
// safe to apply: a listing offering nothing but the described version is not
// offering a duplicate, it is offering that.
func TestARDWithoutVariantsKeepsTheOnlyCopy(t *testing.T) {
	teasers := []ardTeaser{ardTeaserWithID(ardEpisodeOneAdID), ardTeaserWithID(ardEpisodeTwoID)}
	kept := ardWithoutVariants(teasers)
	if len(kept) != 2 {
		t.Fatalf("kept %d of 2 entries", len(kept))
	}
}

// TestARDWithoutVariantsLeavesUnrelatedIDsAlone guards against the filter
// firing on identifiers that merely have several path segments, which is
// ordinary: the broadcasters number their content in namespaces of their own.
func TestARDWithoutVariantsLeavesUnrelatedIDsAlone(t *testing.T) {
	// crid://example.test/sdb/stId/1242 and crid://example.test/sdb/stId/1901
	teasers := []ardTeaser{
		ardTeaserWithID("Y3JpZDovL2V4YW1wbGUudGVzdC9zZGIvc3RJZC8xMjQy"),
		ardTeaserWithID("Y3JpZDovL2V4YW1wbGUudGVzdC9zZGIvc3RJZC8xOTAx"),
		// An id that is not base64 of anything must survive untouched.
		ardTeaserWithID("not-base64-at-all"),
	}
	if kept := ardWithoutVariants(teasers); len(kept) != 3 {
		t.Errorf("kept %d of 3 entries; nothing here is a copy of anything", len(kept))
	}
}

// TestARDRefsKeepEverythingWhenNothingIsAnEpisode covers a strand that
// publishes only extracts. Filtering to episodes would leave it empty, and an
// empty listing is reported as a programme with nothing behind it.
func TestARDRefsKeepEverythingWhenNothingIsAnEpisode(t *testing.T) {
	var widget ardWidget
	if err := json.Unmarshal([]byte(ardMagazineListing), &widget); err != nil {
		t.Fatal(err)
	}
	if got := len(ardRefs(widget.Teasers, "")); got != 2 {
		t.Errorf("got %d entries, want both extracts", got)
	}
}

// TestARDSeasonsAreFoundOnTheEntries pins where the season list lives. It is
// published on every teaser's `show` block and nowhere at the top of the
// document, which is the sort of thing that reads like an oversight until
// somebody looks for it in the obvious place and concludes the series has one
// season.
func TestARDSeasonsAreFoundOnTheEntries(t *testing.T) {
	var widget ardWidget
	if err := json.Unmarshal([]byte(ardShowListing), &widget); err != nil {
		t.Fatal(err)
	}
	seasons := ardSeasons(widget.Teasers)
	if len(seasons) != 2 || seasons[0] != "1" || seasons[1] != "2" {
		t.Errorf("seasons = %v, want [1 2]", seasons)
	}
	if got := ardShowTitle(widget.Teasers); got != "A Drama" {
		t.Errorf("show title = %q", got)
	}

	// A programme with no seasons publishes null, and must not be fetched
	// season by season.
	var magazine ardWidget
	if err := json.Unmarshal([]byte(ardMagazineListing), &magazine); err != nil {
		t.Fatal(err)
	}
	if got := ardSeasons(magazine.Teasers); len(got) != 0 {
		t.Errorf("seasons = %v, want none", got)
	}
}

// TestARDSeasonListingNamesTheSeason covers the other listing endpoint, whose
// title is the season rather than the programme — and which, unlike the flat
// one, has already left the audio-described copies out.
func TestARDSeasonListingNamesTheSeason(t *testing.T) {
	var widget ardWidget
	if err := json.Unmarshal([]byte(ardSeasonListing), &widget); err != nil {
		t.Fatal(err)
	}
	if got := ardJoin(ardShowTitle(widget.Teasers), widget.Title); got != "A Drama - Staffel 2" {
		t.Errorf("title = %q, want the season named in it", got)
	}
	if got := len(ardRefs(widget.Teasers, "")); got != 2 {
		t.Errorf("got %d episodes, want 2", got)
	}
}

// TestARDPaginationOnlyTotalElementsIsTrustworthy pins the one field the
// paging loop may read. A second page echoes an offset and a count where the
// page number and page size were asked for, so a loop believing those would
// either stop after one page or never stop at all.
func TestARDPaginationOnlyTotalElementsIsTrustworthy(t *testing.T) {
	var page ardWidget
	if err := json.Unmarshal([]byte(ardLaterPage), &page); err != nil {
		t.Fatal(err)
	}
	if page.Pagination.TotalElements != 101 {
		t.Errorf("totalElements = %d, want 101", page.Pagination.TotalElements)
	}
	if len(page.Teasers) != 1 {
		t.Errorf("got %d teasers", len(page.Teasers))
	}
}

// TestARDTarget covers every link shape the site publishes. The awkward ones
// are the series links: the id is followed by the season number, and on the
// audio-described season page by that and two more characters, so it is not
// the last segment and counting from the front is no better — the slugs in
// front of it vary in number.
func TestARDTarget(t *testing.T) {
	const (
		item = "Y3JpZDovL2V4YW1wbGUudGVzdC8xMTExX2dhbnplU2VuZHVuZw"
		show = "Y3JpZDovL2V4YW1wbGUudGVzdC9hLWRyYW1h"
	)
	tests := map[string]ardRef{
		"https://www.ardmediathek.de/video/" + item:                              {item: item},
		"https://www.ardmediathek.de/video/a-drama/folge-1/ndr/" + item:          {item: item},
		"https://www.ardmediathek.de/video/a-drama/folge-1/ndr/" + item + "/":    {item: item},
		"https://www.ardmediathek.de/sendung/a-drama/" + show:                    {show: show},
		"https://www.ardmediathek.de/film/a-film-oder-drama/" + show:             {show: show},
		"https://www.ardmediathek.de/serie/a-drama/staffel-3/" + show + "/3":     {show: show, season: "3"},
		"https://www.ardmediathek.de/serie/a-drama/staffel-12/" + show + "/12":   {show: show, season: "12"},
		"https://www.ardmediathek.de/serie/a-drama/staffel-1/" + show + "/1#top": {show: show, season: "1"},
		// The audio-described season page, whose trailing "AD" would be taken
		// for the id by anything reading the last segment.
		"https://www.ardmediathek.de/serie/a-drama/staffel-2-mit-audiodeskription/" + show + "/2/AD": {show: show, season: "2"},
		// Nothing this resolves.
		"https://www.ardmediathek.de/":                         {},
		"https://www.ardmediathek.de/sammlung/alle-rubriken/x": {},
		"https://www.ardmediathek.de/live":                     {},
		"https://www.ardmediathek.de/serie/a-drama-with-no-id": {},
		"https://www.ardmediathek.de/serie/a-drama/staffel-3":  {},
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := ardTarget(u); got != want {
			t.Errorf("ardTarget(%s) = %+v, want %+v", raw, got, want)
		}
	}
}

// TestARDJoin covers the naming rule, which differs from NRK's on purpose:
// ARD writes the programme's name into the episode title in full, so a
// prefix that only checked for equality would print it twice.
func TestARDJoin(t *testing.T) {
	tests := []struct {
		show, episode, want string
	}{
		{"A Drama", "Folge 12 · Staffel 4 | A Drama (S04/E12)", "Folge 12 · Staffel 4 | A Drama (S04/E12)"},
		{"Inas Nacht", "Inas Nacht mit einem Gast", "Inas Nacht mit einem Gast"},
		{"A Film", "A Film", "A Film"},
		{"Tatort", "Der Fall X", "Tatort - Der Fall X"},
		{"A Drama", "Staffel 2", "A Drama - Staffel 2"},
		{"", "A Documentary", "A Documentary"},
		{"A Documentary", "", "A Documentary"},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := ardJoin(tc.show, tc.episode); got != tc.want {
			t.Errorf("ardJoin(%q, %q) = %q, want %q", tc.show, tc.episode, got, tc.want)
		}
	}
}

func TestARDMatch(t *testing.T) {
	a := NewARD(nil)
	for _, host := range []string{"www.ardmediathek.de", "ardmediathek.de", "ARDMEDIATHEK.DE"} {
		if !a.Match(&url.URL{Scheme: "https", Host: host, Path: "/video/x"}) {
			t.Errorf("%s was not matched", host)
		}
	}
	// The other public-broadcaster sites run their own players, and matching
	// them would promise more than this resolves.
	for _, host := range []string{"www.zdf.de", "daserste.de", "sportschau.de", "ardmediathek.de.example.test"} {
		if a.Match(&url.URL{Scheme: "https", Host: host, Path: "/video/x"}) {
			t.Errorf("%s was matched", host)
		}
	}
}

// ardTeaserWithID builds a listing entry that carries nothing but its id.
func ardTeaserWithID(id string) ardTeaser {
	var t ardTeaser
	t.CoreAssetType = "EPISODE"
	t.Links.Target.ID = id
	return t
}

// ardSays asserts that a refusal written for a person carries the fact the
// person needs, whatever else it says around it.
func ardSays(message, want string) bool {
	return strings.Contains(strings.ToLower(message), strings.ToLower(want))
}
