package extractor

import (
	"encoding/json"
	"errors"
	"github.com/JohanLindvall/HeapLeach/internal/config"
	"net/url"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// peerTubeHLSOnlyVideo is the record an instance that transcodes to HLS only
// serves, with every trap such a record actually carries:
//
//   - files[] is not empty, but the only thing in it is the audio-only web
//     version. Reading that list first and falling back only when it is empty
//     — which is the obvious way to write this — downloads the soundtrack.
//   - The 720p rendition is the larger of the two in bytes, so size cannot
//     order them.
//   - fileUrl names a different host from the instance being asked, which is
//     ordinary for a federated video.
//   - fileDownloadUrl is present throughout, since it is the field that
//     sounds like the one to use and must never be chosen.
const peerTubeHLSOnlyVideo = `{
  "uuid": "00000000-0000-4000-8000-000000000001",
  "shortUUID": "aaaaaaaaaaaaaaaaaaaaaa",
  "name": "A Synthetic Clip",
  "isLive": false,
  "state": { "id": 1, "label": "Published" },
  "files": [
    {
      "resolution": { "id": 0, "label": "Audio only" },
      "size": 2707257,
      "hasAudio": true,
      "hasVideo": false,
      "fileUrl": "https://media.example.com/web-videos/clip-0.mp4",
      "fileDownloadUrl": "https://example.test/download/web-videos/clip-0.mp4"
    }
  ],
  "streamingPlaylists": [
    {
      "id": 7,
      "type": 1,
      "playlistUrl": "https://media.example.com/streaming-playlists/hls/clip/master.m3u8",
      "files": [
        {
          "resolution": { "id": 720, "label": "720p" },
          "size": 44875231,
          "hasAudio": true,
          "hasVideo": true,
          "fileUrl": "https://media.example.com/streaming-playlists/hls/clip/clip-720-fragmented.mp4",
          "fileDownloadUrl": "https://example.test/download/streaming-playlists/hls/videos/clip-720-fragmented.mp4"
        },
        {
          "resolution": { "id": 1080, "label": "1080p" },
          "size": 41890062,
          "hasAudio": true,
          "hasVideo": true,
          "fileUrl": "https://media.example.com/streaming-playlists/hls/clip/clip-1080-fragmented.mp4",
          "fileDownloadUrl": "https://example.test/download/streaming-playlists/hls/videos/clip-1080-fragmented.mp4"
        }
      ]
    }
  ]
}`

// peerTubeBothListsVideo is the ordinary record, where the same resolution
// exists as a plain web video and inside the streaming playlist.
const peerTubeBothListsVideo = `{
  "uuid": "00000000-0000-4000-8000-000000000002",
  "name": "Both Lists",
  "state": { "id": 1, "label": "Published" },
  "files": [
    {
      "resolution": { "id": 1080, "label": "1080p" },
      "size": 121009970,
      "hasAudio": true,
      "hasVideo": true,
      "fileUrl": "https://media.example.com/videos/both-1080.mp4"
    },
    {
      "resolution": { "id": 480, "label": "480p" },
      "size": 9913494,
      "hasAudio": true,
      "hasVideo": true,
      "fileUrl": "https://media.example.com/videos/both-480.mp4"
    }
  ],
  "streamingPlaylists": [
    {
      "playlistUrl": "https://media.example.com/streaming-playlists/hls/both/master.m3u8",
      "files": [
        {
          "resolution": { "id": 1080, "label": "1080p" },
          "size": 120988924,
          "hasAudio": true,
          "hasVideo": true,
          "fileUrl": "https://media.example.com/streaming-playlists/hls/both/both-1080-fragmented.mp4"
        }
      ]
    }
  ]
}`

// peerTubeDecodeVideo parses a fixture, so the field tags are under test
// alongside the logic that reads them.
func peerTubeDecodeVideo(t *testing.T, doc string) *peerTubeVideo {
	t.Helper()
	var video peerTubeVideo
	if err := json.Unmarshal([]byte(doc), &video); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return &video
}

func peerTubeURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestPeerTubeBestIgnoresAnAudioOnlyWebVersion is the one that matters most.
// An instance transcoding to HLS only still publishes an audio-only entry in
// files[], so that list is non-empty and useless, and picking it would
// produce a plausible-looking MP4 with no picture in it.
func TestPeerTubeBestIgnoresAnAudioOnlyWebVersion(t *testing.T) {
	video := peerTubeDecodeVideo(t, peerTubeHLSOnlyVideo)

	best, ok := peerTubeBest(peerTubeCandidates(video))
	if !ok {
		t.Fatal("no rendition chosen from a record that offers three")
	}
	if !best.HasVideo || best.Resolution != 1080 {
		t.Errorf("chose the %q rendition, want the 1080p one", best.Label)
	}
	if best.URL != "https://media.example.com/streaming-playlists/hls/clip/clip-1080-fragmented.mp4" {
		t.Errorf("chose %s, want the 1080p fragmented file", best.URL)
	}
	if best.Size != 41890062 {
		t.Errorf("size = %d, want the API's own exact length", best.Size)
	}
}

// TestPeerTubeBestRanksByResolutionNotSize pins the reason resolution leads:
// renditions are encoded independently, so the 720p file in the fixture is
// the larger of the two and sorting on bytes picks the wrong one.
func TestPeerTubeBestRanksByResolutionNotSize(t *testing.T) {
	video := peerTubeDecodeVideo(t, peerTubeHLSOnlyVideo)

	candidates := peerTubeCandidates(video)
	var largest peerTubeCandidate
	for _, c := range candidates {
		if c.Size > largest.Size {
			largest = c
		}
	}
	if largest.Resolution != 720 {
		t.Fatalf("the fixture no longer has the larger file at the lower resolution (largest is %dp)",
			largest.Resolution)
	}

	best, _ := peerTubeBest(candidates)
	if best.Resolution != 1080 {
		t.Errorf("chose %dp, the largest file, rather than the highest resolution", best.Resolution)
	}
}

// TestPeerTubeBestPrefersThePlainWebVideo covers the tie: the same resolution
// offered both ways should come from files[], which is an ordinary
// progressive MP4 rather than a fragmented one.
func TestPeerTubeBestPrefersThePlainWebVideo(t *testing.T) {
	video := peerTubeDecodeVideo(t, peerTubeBothListsVideo)

	best, ok := peerTubeBest(peerTubeCandidates(video))
	if !ok {
		t.Fatal("no rendition chosen")
	}
	if !best.Progressive || best.URL != "https://media.example.com/videos/both-1080.mp4" {
		t.Errorf("chose %s, want the plain 1080p web video", best.URL)
	}
}

// TestPeerTubeFileForKeepsTheFederatedHostAndExactSize guards the two
// properties the whole extractor is built around: the link is followed to
// whatever host the API named, and the length is exact rather than a
// listing's rounding.
func TestPeerTubeFileForKeepsTheFederatedHostAndExactSize(t *testing.T) {
	video := peerTubeDecodeVideo(t, peerTubeHLSOnlyVideo)
	best, _ := peerTubeBest(peerTubeCandidates(video))

	u := peerTubeURL(t, "https://example.test/w/aaaaaaaaaaaaaaaaaaaaaa")
	file := peerTubeFileFor(u, video.Name, best, "")

	media, err := url.Parse(file.URL)
	if err != nil {
		t.Fatal(err)
	}
	if media.Host != "media.example.com" {
		t.Errorf("file host = %q, want the one the API named rather than the instance", media.Host)
	}
	if strings.Contains(file.URL, "/download/") {
		t.Errorf("file URL %s is the instance's download route, which honours no Range", file.URL)
	}
	if file.Size != 41890062 {
		t.Errorf("size = %d, want 41890062", file.Size)
	}
	if file.SizeApprox {
		t.Error("size was marked approximate; the API reports the stored file's own length")
	}
	if file.Name != "A Synthetic Clip-1080p.mp4" {
		t.Errorf("name = %q, want the video and its rendition", file.Name)
	}
	if file.Headers[httpx.HeaderReferer] != "https://example.test/" {
		t.Errorf("referer = %q, want the instance root", file.Headers[httpx.HeaderReferer])
	}
}

// TestPeerTubeFileHeadersKeepThePasswordOnTheInstance covers the one place a
// federated media host is a hazard rather than a convenience: the password
// unlocks the video on the instance that was asked, and a link pointing
// somewhere else must not be handed the user's secret.
func TestPeerTubeFileHeadersKeepThePasswordOnTheInstance(t *testing.T) {
	u := peerTubeURL(t, "https://example.test/w/aaaaaaaaaaaaaaaaaaaaaa")

	tests := []struct {
		name     string
		media    string
		password string
		want     string
	}{
		{
			name:     "sent to the instance that was asked",
			media:    "https://example.test/static/web-videos/private/clip-1080.mp4",
			password: "hunter2",
			want:     "hunter2",
		},
		{
			name:     "withheld from a federated media host",
			media:    "https://media.example.com/videos/clip-1080.mp4",
			password: "hunter2",
			want:     "",
		},
		{
			name:  "absent when no password was given",
			media: "https://example.test/static/web-videos/clip-1080.mp4",
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := peerTubeFileHeaders(u, tc.media, tc.password)
			if got := headers[peerTubeHeaderPassword]; got != tc.want {
				t.Errorf("password header = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPeerTubeParseRoute pins the routing, where the two hazards are that
// /w/p/<id> is a playlist rather than a video called "p", and that a link to
// a comment carries a matrix parameter the API answers 404 to.
func TestPeerTubeParseRoute(t *testing.T) {
	tests := []struct {
		raw  string
		kind peerTubeKind
		id   string
	}{
		{raw: "https://example.test/w/aBcDeFgHiJkLmNoPqRsTuV", kind: peerTubeVideoPage, id: "aBcDeFgHiJkLmNoPqRsTuV"},
		{raw: "https://example.test/videos/watch/00000000-0000-4000-8000-000000000001",
			kind: peerTubeVideoPage, id: "00000000-0000-4000-8000-000000000001"},
		{raw: "https://example.test/videos/embed/aBcDeFgHiJkLmNoPqRsTuV", kind: peerTubeVideoPage, id: "aBcDeFgHiJkLmNoPqRsTuV"},
		// A comment link. The instance strips this itself; so must we.
		{raw: "https://example.test/w/aBcDeFgHiJkLmNoPqRsTuV;threadId=42",
			kind: peerTubeVideoPage, id: "aBcDeFgHiJkLmNoPqRsTuV"},
		// The playlist prefix has to win over the video route it starts with.
		{raw: "https://example.test/w/p/pLpLpLpLpLpLpLpLpLpLpL", kind: peerTubePlaylistPage, id: "pLpLpLpLpLpLpLpLpLpLpL"},
		{raw: "https://example.test/videos/watch/playlist/pLpLpLpLpLpLpLpLpLpLpL",
			kind: peerTubePlaylistPage, id: "pLpLpLpLpLpLpLpLpLpLpL"},
		{raw: "https://example.test/video-playlists/embed/pLpLpLpLpLpLpLpLpLpLpL",
			kind: peerTubePlaylistPage, id: "pLpLpLpLpLpLpLpLpLpLpL"},
		{raw: "https://example.test/c/a_channel", kind: peerTubeChannelPage, id: "a_channel"},
		// A channel's video tab means the channel.
		{raw: "https://example.test/c/a_channel/videos", kind: peerTubeChannelPage, id: "a_channel"},
		{raw: "https://example.test/video-channels/a_channel", kind: peerTubeChannelPage, id: "a_channel"},
		{raw: "https://example.test/a/someone", kind: peerTubeAccountPage, id: "someone"},
		// A remote handle carries the host it belongs to.
		{raw: "https://example.test/a/someone@elsewhere.example", kind: peerTubeAccountPage, id: "someone@elsewhere.example"},
		{raw: "https://example.test/accounts/someone/videos", kind: peerTubeAccountPage, id: "someone"},

		{raw: "https://example.test/", kind: peerTubeNothing},
		{raw: "https://example.test/w/", kind: peerTubeNothing},
		{raw: "https://example.test/about/instance", kind: peerTubeNothing},
		// The KVS page shape, which shares the /videos/ prefix and nothing
		// else. Matching it here would cost a wasted probe on every one.
		{raw: "https://example.test/videos/12345/a-clip/", kind: peerTubeNothing},
	}

	for _, tc := range tests {
		route := peerTubeParseRoute(peerTubeURL(t, tc.raw))
		if route.Kind != tc.kind || route.ID != tc.id {
			t.Errorf("%s parsed as (%d, %q), want (%d, %q)", tc.raw, route.Kind, route.ID, tc.kind, tc.id)
		}
	}
}

// TestPeerTubeLabel pins how an instance is named. Operators put "video" or
// "tube" in the hostname often enough that taking the first label would give
// several instances the same name, which the registry cannot allow.
func TestPeerTubeLabel(t *testing.T) {
	tests := map[string]string{
		"framatube.org":            "framatube",
		"tilvids.com":              "tilvids",
		"video.blender.org":        "blender",
		"makertube.net":            "makertube",
		"tube.tchncs.de":           "tchncs",
		"kolektiva.media":          "kolektiva",
		"spectra.video":            "spectra",
		"videos.pair2jeux.example": "pair2jeux",
		"www.example.test":         "example",
		"example.test:8443":        "example",
		// The site's name is not the leftmost label, so "tube" is skipped.
		"tube.example.co": "example",
		// Nothing but generic words: the first label beats no label at all.
		"peertube.tv":    "peertube",
		"peertube.co.uk": "peertube",
		"video.tube":     "video",
		"":               "peertube",
	}
	for host, want := range tests {
		if got := peerTubeLabel(host); got != want {
			t.Errorf("peerTubeLabel(%q) = %q, want %q", host, got, want)
		}
	}
}

// TestPeerTubeSitesNameAndMatchTheirHosts guards the compiled-in table: two
// instances resolving to one label would be indistinguishable in the UI and
// in every error message.
func TestPeerTubeSitesNameAndMatchTheirHosts(t *testing.T) {
	sites := NewPeerTubeSites(nil, nil)
	if len(sites) != len(peerTubeKnownHosts) {
		t.Fatalf("got %d extractors for %d hosts", len(sites), len(peerTubeKnownHosts))
	}

	seen := make(map[string]string, len(sites))
	for i, host := range peerTubeKnownHosts {
		name := sites[i].Name()
		if name == "" {
			t.Errorf("%s produced an empty label", host)
		}
		if other, dup := seen[name]; dup {
			t.Errorf("%s and %s are both labelled %q", other, host, name)
		}
		seen[name] = host

		if !sites[i].Match(&url.URL{Scheme: "https", Host: host, Path: "/w/x"}) {
			t.Errorf("%s did not match its own host", host)
		}
		if sites[i].Match(&url.URL{Scheme: "https", Host: "unrelated.example.test"}) {
			t.Errorf("%s matched an unrelated host", host)
		}
	}
}

// TestPeerTubeListingEntriesUnwrapPlaylistPositions covers the shape a
// playlist's listing has and the two others do not: its rows are positions
// wrapping a video, and a position whose video has since been deleted or made
// private arrives with that field null.
func TestPeerTubeListingEntriesUnwrapPlaylistPositions(t *testing.T) {
	const listing = `{
	  "total": 3,
	  "data": [
	    { "id": 1, "position": 1, "type": 0,
	      "video": { "uuid": "00000000-0000-4000-8000-00000000000a", "shortUUID": "aa", "name": "First" } },
	    { "id": 2, "position": 2, "type": 1, "video": null },
	    { "id": 3, "position": 3, "type": 0,
	      "video": { "uuid": "00000000-0000-4000-8000-00000000000c", "shortUUID": "cc", "name": "Third" } }
	  ]
	}`

	var page peerTubeListing
	if err := json.Unmarshal([]byte(listing), &page); err != nil {
		t.Fatalf("decode listing: %v", err)
	}

	var ids []string
	for _, entry := range page.Data {
		if id := entry.videoID(); id != "" {
			ids = append(ids, id)
		}
	}
	want := []string{"00000000-0000-4000-8000-00000000000a", "00000000-0000-4000-8000-00000000000c"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want the two surviving videos in order", ids)
	}
}

// TestPeerTubeListingEntriesReadPlainRows covers the other two listings,
// whose rows are the videos themselves.
func TestPeerTubeListingEntriesReadPlainRows(t *testing.T) {
	const listing = `{
	  "total": 1,
	  "data": [ { "uuid": "00000000-0000-4000-8000-00000000000d", "shortUUID": "dd", "name": "Only" } ]
	}`

	var page peerTubeListing
	if err := json.Unmarshal([]byte(listing), &page); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if got := page.Data[0].videoID(); got != "00000000-0000-4000-8000-00000000000d" {
		t.Errorf("videoID = %q, want the row's own uuid", got)
	}
}

// TestPeerTubeExplain pins the wording a refusal turns into. The status alone
// cannot say which of these it is — 401 and 403 are how a private, a blocked
// and a password-protected video all answer — so the instance's own error
// code in the body is what decides.
func TestPeerTubeExplain(t *testing.T) {
	tests := []struct {
		name       string
		status     *httpx.StatusError
		wantPrompt bool
		wantText   string
	}{
		{
			name:       "no password supplied",
			status:     &httpx.StatusError{Code: 401, Body: `{"type":"video_requires_password","detail":"..."}`},
			wantPrompt: true,
		},
		{
			name:     "wrong password supplied",
			status:   &httpx.StatusError{Code: 403, Body: `{"type":"incorrect_video_password","detail":"..."}`},
			wantText: "not accepted",
		},
		{
			name:     "gone",
			status:   &httpx.StatusError{Code: 404, Body: `{"error":"Not found"}`},
			wantText: "does not exist on this instance",
		},
		{
			name:     "private, with nothing said about why",
			status:   &httpx.StatusError{Code: 403, Body: `{}`},
			wantText: "not available to anonymous callers",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := peerTubeExplain(tc.status, "example", "an-id")
			if err == nil {
				t.Fatal("a refusal was turned into no error at all")
			}
			if got := errors.Is(err, ErrPasswordRequired); got != tc.wantPrompt {
				t.Errorf("errors.Is(err, ErrPasswordRequired) = %v, want %v (%v)", got, tc.wantPrompt, err)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not mention %q", err, tc.wantText)
			}
			if !strings.HasPrefix(err.Error(), "example:") {
				t.Errorf("error %q does not open with the instance's label", err)
			}
		})
	}
}

// TestPeerTubeNoMedia covers the records that carry nothing to fetch, where
// saying so plainly is the whole value: a live stream and a video still being
// transcoded are both temporary states that look identical to a removed
// video otherwise.
func TestPeerTubeNoMedia(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "live stream",
			doc:  `{"name":"Live Now","isLive":true,"state":{"id":4,"label":"Waiting for livestream"}}`,
			want: "live stream",
		},
		{
			name: "still transcoding",
			doc:  `{"name":"Fresh Upload","isLive":false,"state":{"id":2,"label":"To transcode"}}`,
			want: "To transcode",
		},
		{
			name: "published but empty",
			doc:  `{"name":"Empty","isLive":false,"state":{"id":1,"label":"Published"}}`,
			want: "lists no media file",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := peerTubeNoMedia(peerTubeDecodeVideo(t, tc.doc), "example")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestPeerTubePlaylistURL covers the last resort, used only when no rendition
// anywhere carries a file of its own.
func TestPeerTubePlaylistURL(t *testing.T) {
	video := peerTubeDecodeVideo(t, peerTubeHLSOnlyVideo)
	if got := peerTubePlaylistURL(video); got != "https://media.example.com/streaming-playlists/hls/clip/master.m3u8" {
		t.Errorf("playlist = %q, want the master playlist", got)
	}

	// A record with a playlist but no renditions listed under it is exactly
	// the case that fallback exists for.
	bare := peerTubeDecodeVideo(t, `{"name":"Bare","files":[],
	  "streamingPlaylists":[{"playlistUrl":"https://media.example.com/hls/bare/master.m3u8","files":[]}]}`)
	if _, ok := peerTubeBest(peerTubeCandidates(bare)); ok {
		t.Error("a record listing no file URLs still produced a rendition")
	}
	if got := peerTubePlaylistURL(bare); got == "" {
		t.Error("no playlist found on a record that has one")
	}
}

// TestPeerTubeSegmentNaming pins how a joined playlist is named: MPEG-TS
// parts keep their own container, and fragmented MP4 parts — including the
// initialisation segment that leads them and names nothing — become a .mp4.
func TestPeerTubeSegmentNaming(t *testing.T) {
	variant := hlsVariant{URL: "https://media.example.com/hls/master.m3u8"}
	tests := []struct {
		name     string
		segments []string
		want     string
	}{
		{
			name:     "fragmented mp4, led by an init segment",
			segments: []string{"https://media.example.com/hls/init.mp4", "https://media.example.com/hls/0.mp4"},
			want:     ".mp4",
		},
		{
			name:     "mpeg-ts",
			segments: []string{"https://media.example.com/hls/0.ts", "https://media.example.com/hls/1.ts"},
			want:     ".ts",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := segmentsExtension(tc.segments, variant); got != tc.want {
				t.Errorf("segmentsExtension = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPeerTubeFileName covers naming, which takes the rendition's own label
// so two downloads of one video at different qualities do not collide, and
// the extension from the link rather than assuming every instance serves MP4.
func TestPeerTubeFileName(t *testing.T) {
	tests := []struct {
		name      string
		candidate peerTubeCandidate
		want      string
	}{
		{
			name:      "labelled rendition",
			candidate: peerTubeCandidate{URL: "https://media.example.com/videos/clip-1080.mp4", Label: "1080p"},
			want:      "A Clip-1080p.mp4",
		},
		{
			name:      "another container",
			candidate: peerTubeCandidate{URL: "https://media.example.com/videos/clip-720.webm", Label: "720p"},
			want:      "A Clip-720p.webm",
		},
		{
			name:      "no label and no extension",
			candidate: peerTubeCandidate{URL: "https://media.example.com/videos/clip"},
			want:      "A Clip.mp4",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := peerTubeFileName("A Clip", tc.candidate); got != tc.want {
				t.Errorf("peerTubeFileName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPeerTubeSniffCostsNothingOffTheInstanceRoutes pins the cheap half of
// the sniff's bargain. A URL shaped like nothing an instance serves must be
// rejected before any request, which a nil client proves: reaching the
// network at all would panic here.
func TestPeerTubeSniffCostsNothingOffTheInstanceRoutes(t *testing.T) {
	for _, raw := range []string{
		"https://nobody.example.test/files/archive.zip",
		"https://nobody.example.test/videos/12345/a-clip/",
		"https://nobody.example.test/",
	} {
		res, handled, err := peerTubeSniff(t.Context(), nil, peerTubeURL(t, raw), Options{})
		if handled || res != nil || err != nil {
			t.Errorf("%s was taken up by the sniff", raw)
		}
	}
}

// TestPeerTubeFileForSpellsAnAbsentLengthMinusOne guards the one value that
// must not be passed through as it stands: a zero would read as a file that
// is already completely downloaded.
func TestPeerTubeFileForSpellsAnAbsentLengthMinusOne(t *testing.T) {
	u := peerTubeURL(t, "https://example.test/w/aaaaaaaaaaaaaaaaaaaaaa")
	file := peerTubeFileFor(u, "No Size", peerTubeCandidate{
		URL: "https://media.example.com/videos/clip-1080.mp4", Label: "1080p",
	}, "")
	if file.Size != -1 {
		t.Errorf("size = %d, want -1 for a record that reported none", file.Size)
	}
}

// TestPeerTubeExtraHosts covers the runtime escape, which exists because
// seven compiled-in instances stand in for some seventeen hundred.
func TestPeerTubeExtraHosts(t *testing.T) {
	cfg := &config.Config{ExtraHosts: map[string][]string{
		config.FamilyPeerTube: {"tube.example.test", "video.example.test", "third.example.test"},
	}}

	sites := NewPeerTubeSites(cfg, nil)
	if len(sites) != len(peerTubeKnownHosts)+3 {
		t.Fatalf("got %d extractors, want the %d built in plus 3", len(sites), len(peerTubeKnownHosts))
	}
	u := &url.URL{Scheme: "https", Host: "tube.example.test", Path: "/w/x"}

	matched := false
	for _, site := range sites {
		if site.Match(u) {
			matched = true
		}
	}
	if !matched {
		t.Error("a host added through HEAPLEACH_EXTRA_HOSTS was not matched")
	}
}
