package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A finished VOD media playlist. The segments are relative, which is how
// hosts write them and what makes the base URL matter.
const hlsCompleteMedia = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:6.000,
seg-1.ts
#EXTINF:6.000,
seg-2.ts
#EXTINF:3.480,
seg-3.ts
#EXT-X-ENDLIST
`

// The same playlist while it is still being produced: a sliding window with
// no last segment. Downloading this saves whatever existed at the moment the
// queue reached it, under a name that claims to be the whole stream.
const hlsLiveMedia = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:2481
#EXTINF:6.000,
seg-2481.ts
#EXTINF:6.000,
seg-2482.ts
`

// An event that is still running. It says so twice — the type and the missing
// end tag — and the message is expected to name the first, because that is
// what tells the user to come back later.
const hlsEventMedia = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-PLAYLIST-TYPE:EVENT
#EXT-X-TARGETDURATION:6
#EXTINF:6.000,
seg-1.ts
`

// A master whose largest rendition is the unusable one: both variants
// advertise a video and an audio codec, but the 1080 one names an audio group
// that has a playlist of its own, so its segments carry no sound. The 720 one
// names nothing and is self-contained. Taking the biggest would be wrong.
const hlsMixedMaster = `#EXTM3U
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="stereo",NAME="English",DEFAULT=YES,LANGUAGE="en",URI="audio/media.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=5600000,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",AUDIO="stereo"
video/1080/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1900000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2"
video/720/index.m3u8
`

// A master offering nothing self-contained: every variant leaves its audio in
// the group. Joining any one of them yields a silent file.
const hlsDemuxedOnlyMaster = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="stereo",NAME="English",DEFAULT=YES,LANGUAGE="en",URI="audio/media.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=5600000,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",AUDIO="stereo"
video/1080/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1900000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2",AUDIO="stereo"
video/720/index.m3u8
`

// hlsTestClient is a client with no retries, so a test that expects a refusal
// gets one at once.
func hlsTestClient() *httpx.Client {
	return httpx.New("test-agent", "en-US", 0, 5*time.Second)
}

// hlsServer serves a fixed set of documents by path, so a master and the
// variant it points at can be laid out the way a host lays them out. The
// count is the number of requests it saw, which is what the sniff guard is
// measured with.
func hlsServer(t *testing.T, contentType string, pages map[string]string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		doc, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(httpx.HeaderContentType, contentType)
		_, _ = io.WriteString(w, doc)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

const hlsManifestContentType = "application/vnd.apple.mpegurl"

// TestHLSDirectMatchesManifestsByPath pins what the extractor claims. The
// query is where these URLs carry their signature, so it must not take part
// in the decision; DASH is claimed only so that Extract can refuse it with a
// reason instead of the fallback saving it as a small XML file.
func TestHLSDirectMatchesManifestsByPath(t *testing.T) {
	tests := map[string]bool{
		"https://cdn.example.test/hls/a-clip/master.m3u8":             true,
		"https://cdn.example.test/hls/a-clip/master.m3u8?t=1&sig=abc": true,
		"https://cdn.example.test/hls/a-clip/MASTER.M3U8":             true,
		"https://cdn.example.test/dash/a-clip/manifest.mpd":           true,
		"https://cdn.example.test/hls/a-clip/video.mp4":               false,
		"https://cdn.example.test/hls/a-clip/":                        false,
		"https://cdn.example.test/watch?src=video.m3u8":               false,
	}
	h := NewHLSDirect(nil)
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := h.Match(u); got != want {
			t.Errorf("Match(%s) = %v, want %v", raw, got, want)
		}
	}
}

// TestHLSDirectRefusesDASH covers the one thing that must never be joined.
// The nil client is part of the test: the refusal is reached before anything
// is fetched, so a DASH URL costs no request at all.
func TestHLSDirectRefusesDASH(t *testing.T) {
	u, err := ParseURL("https://cdn.example.test/dash/a-clip/manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewHLSDirect(nil).Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a DASH manifest was accepted; joining one would give a silent file")
	}
	for _, want := range []string{"DASH", "silent", "yt-dlp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestHLSLiveEdge(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		live bool
		why  string
	}{
		{"a finished VOD is complete", hlsCompleteMedia, false, ""},
		{"a sliding window has no last segment", hlsLiveMedia, true, "#EXT-X-ENDLIST"},
		{"an ongoing event says which it is", hlsEventMedia, true, "EVENT"},
		{
			// An event that has ended carries the end tag like anything
			// else, and refusing it on the type alone would turn away a
			// recording that is complete and downloadable.
			name: "a finished event is complete",
			doc:  hlsEventMedia + "#EXT-X-ENDLIST\n",
			live: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			live, why := hlsLiveEdge(tc.doc)
			if live != tc.live {
				t.Fatalf("live = %v, want %v (%q)", live, tc.live, why)
			}
			if tc.why != "" && !strings.Contains(why, tc.why) {
				t.Errorf("the reason %q does not mention %q", why, tc.why)
			}
		})
	}
}

// TestHLSDirectEmitsOneSegmentedFile is the whole point of the extractor: a
// manifest becomes one file with an ordered part list instead of a few
// kilobytes of text. It also pins the choice of rendition end to end — the
// master's largest variant is the one whose audio lives elsewhere.
func TestHLSDirectEmitsOneSegmentedFile(t *testing.T) {
	srv, _ := hlsServer(t, hlsManifestContentType, map[string]string{
		"/hls/a-clip/master.m3u8":          hlsMixedMaster,
		"/hls/a-clip/video/720/index.m3u8": hlsCompleteMedia,
	})

	res, err := hlsExtractFrom(t, srv.URL+"/hls/a-clip/master.m3u8")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want one joined from the segments", len(res.Files))
	}
	file := res.Files[0]

	if res.Title != "a-clip" || file.Name != "a-clip.ts" {
		t.Errorf("named %q / %q, want the path segment that says what this is", res.Title, file.Name)
	}
	if file.URL != "" {
		t.Errorf("URL = %q, want nothing: a playlist has no single resource to fetch", file.URL)
	}
	if file.Size != -1 {
		t.Errorf("Size = %d, want -1: the length is not known until every part is in", file.Size)
	}
	if file.Headers[httpx.HeaderReferer] != srv.URL+"/" {
		t.Errorf("Referer = %q, want the manifest's own origin", file.Headers[httpx.HeaderReferer])
	}

	want := []string{
		srv.URL + "/hls/a-clip/video/720/seg-1.ts",
		srv.URL + "/hls/a-clip/video/720/seg-2.ts",
		srv.URL + "/hls/a-clip/video/720/seg-3.ts",
	}
	if len(file.Segments) != len(want) {
		t.Fatalf("got %d segments, want %d: %v", len(file.Segments), len(want), file.Segments)
	}
	for i, seg := range want {
		if file.Segments[i] != seg {
			t.Errorf("segment %d = %q, want %q", i, file.Segments[i], seg)
		}
	}
}

// TestHLSDirectAcceptsABareMediaPlaylist is the guard on the master-versus-
// media test. A media playlist declares no codecs, so it can never look
// self-contained; a demuxed check applied to it would refuse every playlist
// pasted straight in, which is the commonest thing a user does.
func TestHLSDirectAcceptsABareMediaPlaylist(t *testing.T) {
	srv, _ := hlsServer(t, hlsManifestContentType, map[string]string{
		"/stream/a-clip/index.m3u8": hlsCompleteMedia,
	})

	res, err := hlsExtractFrom(t, srv.URL+"/stream/a-clip/index.m3u8")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 1 || len(res.Files[0].Segments) != 3 {
		t.Fatalf("got %+v, want one file of three segments", res.Files)
	}
	if res.Files[0].Name != "a-clip.ts" {
		t.Errorf("Name = %q, want a-clip.ts", res.Files[0].Name)
	}
}

func TestHLSDirectRefusesAMasterWithNoSelfContainedVariant(t *testing.T) {
	srv, _ := hlsServer(t, hlsManifestContentType, map[string]string{
		"/hls/a-clip/master.m3u8":           hlsDemuxedOnlyMaster,
		"/hls/a-clip/video/1080/index.m3u8": hlsCompleteMedia,
	})

	_, err := hlsExtractFrom(t, srv.URL+"/hls/a-clip/master.m3u8")
	if err == nil {
		t.Fatal("a master offering only video-only variants was accepted; the result would be silent")
	}
	for _, want := range []string{"audio", "yt-dlp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestHLSDirectRefusesAPlaylistStillBeingWritten(t *testing.T) {
	srv, _ := hlsServer(t, hlsManifestContentType, map[string]string{
		"/live/a-stream/index.m3u8": hlsLiveMedia,
	})

	_, err := hlsExtractFrom(t, srv.URL+"/live/a-stream/index.m3u8")
	if err == nil {
		t.Fatal("a live playlist was accepted; what lands on disk is a fragment named as the whole")
	}
	if !strings.Contains(err.Error(), "#EXT-X-ENDLIST") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// TestHLSSniffRecognisesAManifestServedAsText covers the case the extension
// cannot: a host that serves a playlist from a path naming no file, and
// labels it the way object storage labels everything.
func TestHLSSniffRecognisesAManifestServedAsText(t *testing.T) {
	srv, seen := hlsServer(t, "text/plain; charset=utf-8", map[string]string{
		"/stream/a-clip": hlsCompleteMedia,
	})

	res, err := hlsSniffFrom(t, srv.URL+"/stream/a-clip")
	if err != nil {
		t.Fatalf("hlsSniff: %v", err)
	}
	if res == nil {
		t.Fatal("a playlist opening with #EXTM3U was not recognised")
	}
	if len(res.Files) != 1 || len(res.Files[0].Segments) != 3 {
		t.Fatalf("got %+v, want one file of three segments", res.Files)
	}
	// One request settles what it is, and the resolution reads it properly
	// afterwards — the sniff only ever sees the opening bytes.
	if got := seen.Load(); got != 3 {
		t.Errorf("the host saw %d requests, want the sniff plus two reads of the playlist", got)
	}
}

// TestHLSSniffFallsThroughOnAnythingElse is the kvsSniff bargain: a wrong
// guess costs one small request and the caller carries on as though it had
// never happened.
func TestHLSSniffFallsThroughOnAnythingElse(t *testing.T) {
	srv, _ := hlsServer(t, "text/html; charset=utf-8", map[string]string{
		"/watch/a-clip": "<html><head><title>A clip</title></head><body></body></html>",
	})

	res, err := hlsSniffFrom(t, srv.URL+"/watch/a-clip")
	if err != nil {
		t.Fatalf("a page that is not a manifest reported an error: %v", err)
	}
	if res != nil {
		t.Errorf("an HTML page was taken for a playlist: %+v", res)
	}
}

// TestHLSSniffReportsAManifestItCannotDownload is the one place this departs
// from kvsSniff. Once the response has said it is a playlist, falling through
// would hand it back to the fallback and save the text as a file — the very
// failure the extractor exists to stop — so the reason surfaces instead.
func TestHLSSniffReportsAManifestItCannotDownload(t *testing.T) {
	srv, _ := hlsServer(t, hlsManifestContentType, map[string]string{
		"/live/a-stream": hlsLiveMedia,
	})

	res, err := hlsSniffFrom(t, srv.URL+"/live/a-stream")
	if err == nil {
		t.Fatal("a live playlist fell through to the fallback, which would save the manifest itself")
	}
	if res != nil {
		t.Errorf("a result was returned alongside the error: %+v", res)
	}
}

// TestHLSSniffLeavesNamedFilesAlone pins the guard rather than its outcome.
// Some hosts sign a link for a single use, so a sniff of every direct
// download would spend the signature before the transfer got to it.
func TestHLSSniffLeavesNamedFilesAlone(t *testing.T) {
	srv, seen := hlsServer(t, hlsManifestContentType, map[string]string{
		"/media/a-clip.bin": hlsCompleteMedia,
	})

	res, err := hlsSniffFrom(t, srv.URL+"/media/a-clip.bin")
	if res != nil || err != nil {
		t.Fatalf("a URL naming a file was sniffed: %+v, %v", res, err)
	}
	if got := seen.Load(); got != 0 {
		t.Errorf("the host saw %d requests, want none", got)
	}
}

func TestHLSSniffable(t *testing.T) {
	tests := map[string]bool{
		"https://cdn.example.test/stream/a-clip":     true,
		"https://cdn.example.test/stream/a-clip?t=1": true,
		"https://cdn.example.test/a-clip.bin":        false,
		"https://cdn.example.test/":                  false,
		"https://cdn.example.test":                   false,
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := hlsSniffable(u); got != want {
			t.Errorf("hlsSniffable(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestHLSManifestType(t *testing.T) {
	tests := map[string]bool{
		"application/vnd.apple.mpegurl":                true,
		"application/x-mpegURL":                        true,
		"Application/Vnd.Apple.Mpegurl; charset=UTF-8": true,
		"audio/x-mpegurl":                              true,
		"text/plain; charset=utf-8":                    false,
		"video/mp4":                                    false,
		"":                                             false,
	}
	for value, want := range tests {
		if got := hlsManifestType(value); got != want {
			t.Errorf("hlsManifestType(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestHLSManifestBody(t *testing.T) {
	tests := map[string]bool{
		hlsCompleteMedia:                  true,
		"\ufeff#EXTM3U\n#EXT-X-VERSION:3": true, // an origin that re-encoded the file
		"\n  #EXTM3U\n":                   true,
		"<html><body>Not found":           false,
		"#EXTINF:6.000,\nseg-1.ts\n":      false, // a fragment, not a document
		"":                                false,
	}
	for body, want := range tests {
		if got := hlsManifestBody(body); got != want {
			t.Errorf("hlsManifestBody(%.20q) = %v, want %v", body, got, want)
		}
	}
}

// TestHLSName covers the naming trap: a manifest is almost always called
// after its role, so the basename alone would file most of these downloads
// as index.ts.
func TestHLSName(t *testing.T) {
	tests := map[string]string{
		"https://cdn.example.test/hls/a-clip/index.m3u8":      "a-clip",
		"https://cdn.example.test/media/47110815/master.m3u8": "47110815",
		"https://cdn.example.test/a-clip.m3u8":                "a-clip",
		"https://cdn.example.test/a%20clip/playlist.m3u8":     "a clip",
		"https://cdn.example.test/hls/index.m3u8":             "cdn.example.test",
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := hlsName(u); got != want {
			t.Errorf("hlsName(%s) = %q, want %q", raw, got, want)
		}
	}
}

// hlsExtractFrom runs the extractor against a served manifest.
func hlsExtractFrom(t *testing.T, raw string) (*Result, error) {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return NewHLSDirect(hlsTestClient()).Extract(context.Background(), u, Options{})
}

// hlsSniffFrom runs the sniff the fallback calls.
func hlsSniffFrom(t *testing.T, raw string) (*Result, error) {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return hlsSniff(context.Background(), hlsTestClient(), u)
}
