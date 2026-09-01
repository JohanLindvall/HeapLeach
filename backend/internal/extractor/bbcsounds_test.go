package extractor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// bbcDownloadableEpisode is a speech item: the BBC publishes an offline copy,
// which is the good case, because a plain file gets the segmented engine,
// resume and the skip check.
//
// Two traps are reproduced. The top-level id is the VERSION pid and is not
// the episode pid in the urn — the download URL is keyed on the former, the
// page on the latter. And file_size is rounded to two significant figures for
// the label beside it, which is why it may never be treated as exact.
const bbcDownloadableEpisode = `{
  "type": "playable_item",
  "id": "p000vers",
  "urn": "urn:bbc:radio:episode:p000epis",
  "titles": { "primary": "A Documentary Podcast", "secondary": "The One About Cheese", "tertiary": null, "entity_title": "The One About Cheese" },
  "duration": { "value": 1620, "label": "27 mins" },
  "container": { "type": "brand", "id": "p000brnd", "urn": "urn:bbc:radio:brand:p000brnd", "title": "A Documentary Podcast" },
  "download": {
    "type": "non_drm",
    "quality_variants": {
      "low": { "bitrate": 96, "file_url": "https://open.live.example.test/mediaselector/6/redir/version/2.0/mediaset/audio-nondrm-download-low/proto/https/vpid/p000vers.mp3", "file_size": 19000000, "label": "19 MB" },
      "medium": { "bitrate": 128, "file_url": "https://open.live.example.test/mediaselector/6/redir/version/2.0/mediaset/audio-nondrm-download/proto/https/vpid/p000vers.mp3", "file_size": 26000000, "label": "26 MB" },
      "high": { "bitrate": 320, "file_url": "https://open.live.example.test/mediaselector/6/redir/version/2.0/mediaset/audio-nondrm-download-high/proto/https/vpid/p000vers.mp3", "file_size": 65000000, "label": "65 MB" }
    }
  },
  "availability": { "from": "2026-08-22T15:32:30Z", "to": null, "label": "Available for over a year" }
}`

// bbcDRMFlaggedEpisode is the trap this whole extractor is arranged around. A
// music or drama item is flagged "drm" and every quality_variant has null
// where its URL would be — but the flag is about download RIGHTS, not about
// the bytes: the same item streams a clear playlist. Refusing on the flag
// would refuse most of the catalogue for nothing.
const bbcDRMFlaggedEpisode = `{
  "type": "playable_item",
  "id": "m000vers",
  "urn": "urn:bbc:radio:episode:m000epis",
  "titles": { "primary": "A Serial", "secondary": "The Second Book", "tertiary": "10. A Fateful Night", "entity_title": "10. A Fateful Night" },
  "container": { "type": "brand", "id": "m000brnd", "urn": "urn:bbc:radio:brand:m000brnd", "title": "A Serial" },
  "download": {
    "type": "drm",
    "quality_variants": {
      "low": { "bitrate": 96, "file_url": null, "file_size": 47000000, "label": "47 MB" },
      "medium": { "bitrate": 128, "file_url": null, "file_size": 62000000, "label": "62 MB" },
      "high": { "bitrate": 320, "file_url": null, "file_size": 152000000, "label": "152 MB" }
    }
  },
  "availability": { "from": "2026-08-22T15:00:00Z", "to": "2031-08-21T15:00:00Z", "label": "Available for over a year" }
}`

// bbcContainerListing is a brand expanded into its episodes, in the shape the
// listing endpoint serves one. Every row is a complete playable item, which is
// what makes a container of downloadable episodes cost a single request.
//
// The rows carry three traps between them: a daily edition named by a date
// written with slashes, which SafeName would read as a path; a row filling in
// all three title levels beside one filling in two; and a row with no download
// block at all, which must not be mistaken for a download of nothing.
const bbcContainerListing = `{
  "total": 3,
  "limit": 100,
  "offset": 0,
  "data": [
    {
      "id": "p000ver1",
      "urn": "urn:bbc:radio:episode:p000epi1",
      "titles": { "primary": "Any Answers?", "secondary": "22/08/2026", "tertiary": null, "entity_title": "22/08/2026" },
      "container": { "type": "brand", "id": "p000brnd", "title": "Any Answers?" },
      "download": { "type": "non_drm", "quality_variants": {
        "low": { "bitrate": 128, "file_url": "https://open.live.example.test/redir/vpid/p000ver1.mp3", "file_size": 42000000, "label": "42 MB" },
        "medium": { "bitrate": 128, "file_url": "https://open.live.example.test/redir/vpid/p000ver1.mp3", "file_size": 42000000, "label": "42 MB" },
        "high": { "bitrate": 128, "file_url": "https://open.live.example.test/redir/vpid/p000ver1.mp3", "file_size": 42000000, "label": "42 MB" }
      } }
    },
    {
      "id": "p000ver2",
      "urn": "urn:bbc:radio:episode:p000epi2",
      "titles": { "primary": "Any Answers?", "secondary": "Series 3", "tertiary": "15/08/2026", "entity_title": "15/08/2026" },
      "container": { "type": "brand", "id": "p000brnd", "title": "Any Answers?" },
      "download": { "type": "drm", "quality_variants": { "high": { "bitrate": 320, "file_url": null, "file_size": 152000000, "label": "152 MB" } } }
    },
    {
      "id": "p000ver3",
      "urn": "urn:bbc:radio:episode:p000epi3",
      "titles": { "primary": "Any Answers?", "secondary": null, "tertiary": null, "entity_title": "Any Answers?" },
      "container": { "type": "brand", "id": "p000brnd", "title": "Any Answers?" },
      "download": null
    }
  ]
}`

// bbcSelectionDocument is the media selector's answer for a version.
//
// Three things in it are deliberate. A thumbnail track is listed beside the
// audio, and downloading that would finish cleanly and report success. The
// plaintext HTTP copies come before the HTTPS ones. And DASH comes before HLS,
// though nothing here can read a DASH manifest.
const bbcSelectionDocument = `{
  "disclaimer": "This code and data form part of the BBC iPlayer content protection system.",
  "media": [
    {
      "kind": "image",
      "type": "image/jpeg",
      "connection": [
        { "priority": "10", "protocol": "https", "supplier": "akamai", "transferFormat": "hls", "href": "https://cdn.example.test/thumbnails/index.m3u8" }
      ]
    },
    {
      "bitrate": "320",
      "kind": "audio",
      "type": "audio/mp4",
      "encoding": "aac",
      "connection": [
        { "priority": "20", "protocol": "http", "supplier": "akamai", "transferFormat": "dash", "href": "http://cdn.example.test/a/master.mpd" },
        { "priority": "20", "protocol": "https", "supplier": "akamai", "transferFormat": "dash", "href": "https://cdn.example.test/a/master.mpd" },
        { "priority": "20", "protocol": "http", "supplier": "akamai", "transferFormat": "hls", "href": "http://cdn.example.test/a/master.m3u8" },
        { "priority": "20", "protocol": "https", "supplier": "akamai", "transferFormat": "hls", "href": "https://cdn.example.test/a/master.m3u8" },
        { "priority": "40", "protocol": "https", "supplier": "cloudfront", "transferFormat": "hls", "href": "https://cdn2.example.test/a/master.m3u8" }
      ]
    }
  ]
}`

// bbcMasterPlaylist is the fallback route's manifest, and it is why the
// fallback is usable at all. Radio is audio only: there is no EXT-X-MEDIA
// entry to declare an audio group elsewhere, so nothing can be left behind by
// concatenating one variant's segments, and there is no EXT-X-KEY either —
// the "drm" flag on the metadata withholds the download, not the stream.
const bbcMasterPlaylist = `#EXTM3U
#EXT-X-VERSION:2
## Created with Unified Streaming Platform(version=1.7.32)

# variants
#EXT-X-STREAM-INF:BANDWIDTH=51000,CODECS="mp4a.40.5"
vf_m000vers_00000000-0000-0000-0000-000000000000.ism.hlsv2-audio_eng_1=48000.m3u8
`

// bbcMediaPlaylist is what that variant resolves to: plain transport-stream
// parts, listed in order, with nothing to decrypt.
const bbcMediaPlaylist = `#EXTM3U
#EXT-X-VERSION:2
#EXT-X-MEDIA-SEQUENCE:1
#EXT-X-TARGETDURATION:6
#USP-X-TIMESTAMP-MAP:MPEGTS=900000,LOCAL=1970-01-01T00:00:00Z
#EXTINF:6, no desc
vf_m000vers_00000000-0000-0000-0000-000000000000.ism.hlsv2-audio_eng_1=48000-1.ts
#EXTINF:6, no desc
vf_m000vers_00000000-0000-0000-0000-000000000000.ism.hlsv2-audio_eng_1=48000-2.ts
#EXT-X-ENDLIST
`

// TestBBCDRMFlagIsNotEncryption is the claim the extractor is built on, and
// the one most likely to be "corrected" by a later reader. An item flagged
// "drm" carries no download URL and must fall through to the stream; nothing
// may refuse it, and nothing may invent a URL for it either.
func TestBBCDRMFlagIsNotEncryption(t *testing.T) {
	var item bbcItem
	if err := json.Unmarshal([]byte(bbcDRMFlaggedEpisode), &item); err != nil {
		t.Fatal(err)
	}
	if item.Download.Type != "drm" {
		t.Fatalf("fixture no longer carries the flag: %q", item.Download.Type)
	}
	if variant, ok := item.Download.best(); ok {
		t.Errorf("a URL was found for an item that publishes none: %+v", variant)
	}
	// The version pid is what the media selector is asked for, and it is not
	// the pid in the urn.
	if item.ID != "m000vers" || item.pid() != "m000epis" {
		t.Errorf("version %q, episode %q: the two must not be confused", item.ID, item.pid())
	}
}

// TestBBCBestDownloadPrefersTheLargestPublished covers the other half: where
// URLs exist, the highest bitrate wins.
func TestBBCBestDownloadPrefersTheLargestPublished(t *testing.T) {
	var item bbcItem
	if err := json.Unmarshal([]byte(bbcDownloadableEpisode), &item); err != nil {
		t.Fatal(err)
	}
	variant, ok := item.Download.best()
	if !ok {
		t.Fatal("no download was found for an item that publishes three")
	}
	if variant.Bitrate != 320 {
		t.Errorf("chose the %d kbps copy, want the largest", variant.Bitrate)
	}
	if variant.size() != 65000000 {
		t.Errorf("size = %d", variant.size())
	}
}

// TestBBCBestDownloadIsStableAcrossRuns guards the tie-break. The tiers are
// keyed by name rather than listed, so an unranked map iteration would pick a
// different one each run; the ordinary speech case is three identical copies.
func TestBBCBestDownloadIsStableAcrossRuns(t *testing.T) {
	var download bbcDownload
	body := `{"type":"non_drm","quality_variants":{
		"low":{"bitrate":128,"file_url":"https://open.live.example.test/low.mp3","file_size":42000000},
		"medium":{"bitrate":128,"file_url":"https://open.live.example.test/medium.mp3","file_size":42000000},
		"high":{"bitrate":128,"file_url":"https://open.live.example.test/high.mp3","file_size":42000000}}}`
	if err := json.Unmarshal([]byte(body), &download); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		variant, ok := download.best()
		if !ok || variant.FileURL != "https://open.live.example.test/high.mp3" {
			t.Fatalf("run %d chose %q", i, variant.FileURL)
		}
	}
}

// TestBBCBestDownloadIgnoresTiersWithNoURL covers the mixed case, which is
// what a partially withheld item looks like.
func TestBBCBestDownloadIgnoresTiersWithNoURL(t *testing.T) {
	tests := map[string]string{
		"nothing at all":      `{"type":"drm","quality_variants":{}}`,
		"no download block":   `{}`,
		"every tier withheld": `{"type":"drm","quality_variants":{"high":{"bitrate":320,"file_url":null}}}`,
		"a blank url":         `{"type":"non_drm","quality_variants":{"high":{"bitrate":320,"file_url":"   "}}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			var download bbcDownload
			if err := json.Unmarshal([]byte(body), &download); err != nil {
				t.Fatal(err)
			}
			if variant, ok := download.best(); ok {
				t.Errorf("accepted %+v", variant)
			}
		})
	}

	// The highest tier is withheld while a lower one is not: take what there
	// is rather than concluding the item cannot be downloaded.
	var download bbcDownload
	body := `{"type":"non_drm","quality_variants":{
		"high":{"bitrate":320,"file_url":null,"file_size":65000000},
		"medium":{"bitrate":128,"file_url":"https://open.live.example.test/medium.mp3","file_size":26000000}}}`
	if err := json.Unmarshal([]byte(body), &download); err != nil {
		t.Fatal(err)
	}
	variant, ok := download.best()
	if !ok || variant.Bitrate != 128 {
		t.Errorf("got %+v, want the one tier that carries a URL", variant)
	}
}

// TestBBCDeclaredSizeIsNeverExact pins why SizeApprox is set. The declared
// length is rounded to two significant figures for the label beside it, so
// letting the skip check believe it would leave a truncated file in place and
// call the job done.
func TestBBCDeclaredSizeIsNeverExact(t *testing.T) {
	var item bbcItem
	if err := json.Unmarshal([]byte(bbcDownloadableEpisode), &item); err != nil {
		t.Fatal(err)
	}
	variant, _ := item.Download.best()
	if variant.size()%1_000_000 != 0 {
		t.Errorf("size %d is not the rounded figure the API publishes", variant.size())
	}

	// A tier with no length at all reports -1, which is the contract's way of
	// saying "unknown" and is not the same as zero.
	var empty bbcVariant
	if empty.size() != -1 {
		t.Errorf("an undeclared size reported %d, want -1", empty.size())
	}
}

// TestBBCSelectionSkipsTheThumbnailTrack is the regression guard for the media
// walk. The selector lists a programme's images beside its audio, and a
// picture downloads to the end and reports success.
func TestBBCSelectionSkipsTheThumbnailTrack(t *testing.T) {
	var doc bbcSelection
	if err := json.Unmarshal([]byte(bbcSelectionDocument), &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.stream(); got != "https://cdn.example.test/a/master.m3u8" {
		t.Errorf("chose %q, want the first HTTPS HLS copy of the audio", got)
	}
}

func TestBBCSelectionRejectsWhatCannotBeFetched(t *testing.T) {
	tests := map[string]string{
		"nothing at all": `{"media":[]}`,
		"only dash": `{"media":[{"kind":"audio","type":"audio/mp4","connection":[
			{"protocol":"https","transferFormat":"dash","href":"https://cdn.example.test/a/master.mpd"}]}]}`,
		"only plaintext": `{"media":[{"kind":"audio","type":"audio/mp4","connection":[
			{"protocol":"http","transferFormat":"hls","href":"http://cdn.example.test/a/master.m3u8"}]}]}`,
		"only images": `{"media":[{"kind":"image","type":"image/jpeg","connection":[
			{"protocol":"https","transferFormat":"hls","href":"https://cdn.example.test/thumbs/index.m3u8"}]}]}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			var doc bbcSelection
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatal(err)
			}
			if got := doc.stream(); got != "" {
				t.Errorf("accepted %q", got)
			}
		})
	}
}

// TestBBCRefusalIsReadOutOfTheErrorBody is why the client's typed status error
// is unwrapped rather than reported. The selector answers 403 or 404 with a
// paragraph of boilerplate and one field that means something, and the bare
// status throws away the only fact the caller can act on.
func TestBBCRefusalIsReadOutOfTheErrorBody(t *testing.T) {
	const boilerplate = `"disclaimer":"This code and data form part of the BBC iPlayer content protection system."`

	tests := []struct {
		name, body string
		code       int
		want       string
	}{
		{
			name: "outside the United Kingdom",
			code: 403,
			body: `{` + boilerplate + `,"result":"geolocation"}`,
			want: "bbcsounds: p000vers: BBC Sounds offers this inside the United Kingdom only",
		},
		{
			name: "past its availability window",
			code: 404,
			body: `{` + boilerplate + `,"result":"selectionunavailable"}`,
			want: "bbcsounds: p000vers: BBC Sounds holds no stream for this any more — its availability window has closed",
		},
		{
			name: "a word nobody here has seen",
			code: 403,
			body: `{` + boilerplate + `,"result":"somethingelse"}`,
			want: "bbcsounds: p000vers: the media selector refused it: somethingelse",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := bbcRefusal("p000vers", &httpx.StatusError{Code: tc.code, Body: tc.body})
			if err == nil {
				t.Fatal("the refusal was not recognised")
			}
			if got := err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBBCRefusalLeavesOtherFailuresAlone covers the other direction: a
// timeout or a proxy's error page is not a refusal, and dressing it up as one
// would report a made-up reason.
func TestBBCRefusalLeavesOtherFailuresAlone(t *testing.T) {
	tests := map[string]error{
		"no status at all":  errors.New("connection reset by peer"),
		"no body":           &httpx.StatusError{Code: 500},
		"not json":          &httpx.StatusError{Code: 502, Body: "<html><body>Bad gateway</body></html>"},
		"json with no word": &httpx.StatusError{Code: 403, Body: `{"disclaimer":"..."}`},
	}
	for name, err := range tests {
		t.Run(name, func(t *testing.T) {
			if refusal := bbcRefusal("p000vers", err); refusal != nil {
				t.Errorf("invented a refusal: %v", refusal)
			}
		})
	}
}

// TestBBCDownloadExtensionComesFromTheURL covers the one place the container
// is named: the document carries no media type beside the URL.
func TestBBCDownloadExtensionComesFromTheURL(t *testing.T) {
	tests := map[string]string{
		"https://open.live.example.test/redir/vpid/p000vers.mp3":       ".mp3",
		"https://open.live.example.test/redir/vpid/p000vers.MP3?x=1":   ".mp3",
		"https://open.live.example.test/redir/vpid/p000vers.m4a":       ".m4a",
		"https://open.live.example.test/mediaset/download/vpid/p000ve": ".mp3",
	}
	for raw, want := range tests {
		if got := bbcDownloadExtension(raw); got != want {
			t.Errorf("bbcDownloadExtension(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestBBCMasterPlaylistIsSelfContained pins the fallback route. Radio is audio
// only, so there is no audio group anywhere in the manifest for a variant to
// leave behind, and concatenating the parts yields the whole programme.
func TestBBCMasterPlaylistIsSelfContained(t *testing.T) {
	base, err := ParseURL("https://cdn.example.test/a/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	variants := parseMasterPlaylist(bbcMasterPlaylist, base)
	if len(variants) != 1 {
		t.Fatalf("parsed %d variants, want the single rendition the BBC renders", len(variants))
	}
	// muxed() asks whether audio lives in a playlist of its own, which is the
	// question that matters for video. There is no video here at all, and no
	// EXT-X-MEDIA entry either, so nothing is left behind whatever it answers.
	if variants[0].audioElsewhere {
		t.Error("the variant names an audio group with a URI of its own; " +
			"concatenating it would give a file with nothing in it")
	}
	best, ok := bestVariant(variants)
	if !ok {
		t.Fatal("no variant was chosen")
	}
	if got := playlistExtension(best); got != ".ts" {
		t.Errorf("extension %q, want .ts", got)
	}

	variantBase, err := ParseURL(best.URL)
	if err != nil {
		t.Fatal(err)
	}
	segments := parseMediaPlaylist(bbcMediaPlaylist, variantBase)
	if len(segments) != 2 {
		t.Fatalf("got %d segments, want 2", len(segments))
	}
	if segments[0] != "https://cdn.example.test/a/vf_m000vers_00000000-0000-0000-0000-000000000000.ism.hlsv2-audio_eng_1=48000-1.ts" {
		t.Errorf("first segment = %q", segments[0])
	}
}

// TestBBCTitlesNameTheEpisodeAtTheRightLevel is the naming rule, and the
// three-level case is the one that is easy to get wrong: there the second
// level is a series and only the third is the episode.
func TestBBCTitlesNameTheEpisodeAtTheRightLevel(t *testing.T) {
	tests := []struct {
		name         string
		titles       bbcTitles
		full, within string
	}{
		{
			name:   "two levels: the second is the episode",
			titles: bbcTitles{Primary: "Any Answers?", Secondary: "22/08/2026"},
			full:   "Any Answers? - 22/08/2026",
			within: "22/08/2026",
		},
		{
			name:   "three levels: the second is a series and the third the episode",
			titles: bbcTitles{Primary: "A Serial", Secondary: "The Second Book", Tertiary: "10. A Fateful Night"},
			full:   "A Serial - The Second Book - 10. A Fateful Night",
			within: "The Second Book - 10. A Fateful Night",
		},
		{
			name:   "one level: there is nothing below the container to use",
			titles: bbcTitles{Primary: "A One-Off"},
			full:   "A One-Off",
			within: "A One-Off",
		},
		{
			name:   "a level repeating the one above it is not said twice",
			titles: bbcTitles{Primary: "A Documentary", Secondary: "a documentary"},
			full:   "A Documentary",
			within: "a documentary",
		},
		{
			name:   "a gap in the middle closes up",
			titles: bbcTitles{Primary: "A Strand", Tertiary: "Part Two"},
			full:   "A Strand - Part Two",
			within: "Part Two",
		},
		{
			name:   "nothing at all",
			titles: bbcTitles{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.titles.full(); got != tc.full {
				t.Errorf("full() = %q, want %q", got, tc.full)
			}
			if got := tc.titles.within(); got != tc.within {
				t.Errorf("within() = %q, want %q", got, tc.within)
			}
		})
	}
}

// TestBBCNameFoldsDateSeparators is the trap that costs a whole series of
// filenames. Radio 4 names a daily edition by its date, SafeName has to read
// a slash as a path separator and keeps only what follows the last one, and
// every edition of the year would land on disk as "2026".
func TestBBCNameFoldsDateSeparators(t *testing.T) {
	tests := map[string]string{
		"Any Answers? - 22/08/2026":  "Any Answers? - 22-08-2026",
		`A Programme \ An Edition`:   "A Programme - An Edition",
		"  A Documentary Podcast  ":  "A Documentary Podcast",
		"The One About Cheese":       "The One About Cheese",
		"Vinyl / Digital / Streamed": "Vinyl - Digital - Streamed",
	}
	for title, want := range tests {
		if got := bbcName(title); got != want {
			t.Errorf("bbcName(%q) = %q, want %q", title, got, want)
		}
	}
}

// TestBBCContainerListingRowsAreCompleteItems covers what makes a container
// cheap: a listing row is a playable item, so a brand of downloadable episodes
// resolves without one further request.
func TestBBCContainerListingRowsAreCompleteItems(t *testing.T) {
	var page bbcListing
	if err := json.Unmarshal([]byte(bbcContainerListing), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || len(page.Data) != 3 {
		t.Fatalf("total %d, %d rows", page.Total, len(page.Data))
	}
	if got := page.Data[0].Container.Title; got != "Any Answers?" {
		t.Errorf("container title = %q; the job's folder is named without a request of its own", got)
	}

	// The first row is downloadable outright.
	variant, ok := page.Data[0].Download.best()
	if !ok || variant.FileURL != "https://open.live.example.test/redir/vpid/p000ver1.mp3" {
		t.Errorf("first row = %+v, want its published URL", variant)
	}
	// The second withholds its URL and must fall through to the stream.
	if _, ok := page.Data[1].Download.best(); ok {
		t.Error("a URL was found for a row that publishes none")
	}
	// The third has no download block at all, which decodes to the zero value
	// and must not read as a download of nothing.
	if _, ok := page.Data[2].Download.best(); ok {
		t.Error("a null download block yielded a variant")
	}

	names := []string{}
	for _, item := range page.Data {
		names = append(names, bbcName(item.Titles.within()))
	}
	want := []string{"22-08-2026", "Series 3 - 15-08-2026", "Any Answers?"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("row %d named %q, want %q", i, names[i], want[i])
		}
	}
}

// TestBBCItemPIDReadsTheURN covers the identifier the error messages use. The
// urn is a colon-separated path and only its last element is the pid.
func TestBBCItemPIDReadsTheURN(t *testing.T) {
	tests := map[string]string{
		`{"urn":"urn:bbc:radio:episode:m000epis","id":"m000vers"}`: "m000epis",
		`{"urn":"urn:bbc:radio:clip:p000clip","id":"p000vers"}`:    "p000clip",
		// No urn at all: the version pid is worse than the episode pid but
		// far better than an empty string in a failure message.
		`{"id":"m000vers"}`: "m000vers",
		`{}`:                "",
	}
	for body, want := range tests {
		var item bbcItem
		if err := json.Unmarshal([]byte(body), &item); err != nil {
			t.Fatal(err)
		}
		if got := item.pid(); got != want {
			t.Errorf("pid of %s = %q, want %q", body, got, want)
		}
	}
}

// TestBBCIsLive guards the one thing on Sounds that must not be queued. A
// station's live stream has no end, and the site addresses it in the same
// position as an episode pid — in two spellings, since the link carries a
// colon and the site redirects it to a path segment.
func TestBBCIsLive(t *testing.T) {
	for _, id := range []string{"live:bbc_radio_four", "live", "LIVE:bbc_6music"} {
		if !bbcIsLive(id) {
			t.Errorf("%q was not recognised as a live stream", id)
		}
	}
	for _, id := range []string{"m000epis", "p000epis", "liveaid", "w3ct9994"} {
		if bbcIsLive(id) {
			t.Errorf("%q was mistaken for a live stream", id)
		}
	}
}

// TestBBCSoundsMatch pins the routing. The Sounds section is claimed and the
// rest of bbc.co.uk is not: news, iPlayer and the World Service site all live
// on the same domain and none of them resolves here.
func TestBBCSoundsMatch(t *testing.T) {
	b := NewBBCSounds(nil)
	matched := []string{
		"https://www.bbc.co.uk/sounds/play/m000epis",
		"https://bbc.co.uk/sounds/brand/b000brnd",
		"https://www.bbc.co.uk/sounds/series/p000sers",
		"https://WWW.BBC.CO.UK/Sounds/Play/m000epis",
		"https://www.bbc.co.uk/sounds",
	}
	for _, raw := range matched {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !b.Match(u) {
			t.Errorf("%s was not matched", raw)
		}
	}

	unmatched := []string{
		"https://www.bbc.co.uk/news/articles/abcdefghijko",
		"https://www.bbc.co.uk/iplayer/episode/m000epis",
		"https://www.bbc.co.uk/programmes/m000epis",
		"https://www.bbc.com/sounds/play/m000epis",
		"https://notbbc.co.uk.example.test/sounds/play/m000epis",
	}
	for _, raw := range unmatched {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if b.Match(u) {
			t.Errorf("%s was matched", raw)
		}
	}
}

// TestBBCSoundsExtractRefusesWhatItCannotResolve covers the shapes that must
// fail before a request is made: nothing here fetches anything, so a nil
// client is enough.
func TestBBCSoundsExtractRefusesWhatItCannotResolve(t *testing.T) {
	b := NewBBCSounds(nil)
	for _, raw := range []string{
		"https://www.bbc.co.uk/sounds",
		"https://www.bbc.co.uk/sounds/podcasts",
		"https://www.bbc.co.uk/sounds/play/live:bbc_radio_four",
		"https://www.bbc.co.uk/sounds/play/live/bbc_radio_four",
		"https://www.bbc.co.uk/sounds/category/music",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Extract(t.Context(), u, Options{}); err == nil {
			t.Errorf("%s resolved to something", raw)
		}
	}
}

// TestBBCSoundsSitesNamesThePath is why this extractor's catalogue entry is
// not simply its domain. It claims one section of bbc.co.uk, and a row
// reading "bbc.co.uk" would promise the news site and iPlayer along with it.
func TestBBCSoundsSitesNamesThePath(t *testing.T) {
	got := NewBBCSounds(nil).Sites()
	if len(got) != 1 || !strings.HasPrefix(got[0], "bbc.co.uk/") {
		t.Errorf("Sites() = %v, want the domain qualified by the section claimed", got)
	}
}
