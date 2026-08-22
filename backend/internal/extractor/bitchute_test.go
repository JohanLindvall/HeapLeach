package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Nothing here came off bitchute. The endpoints are reproduced from their
// documented shapes and the fixtures are written by hand, which is also the
// only way to test the parts that are ours rather than theirs: a channel of
// 117 videos, a page that stops the paging, a video the API will not resolve.
// Proving those against the live host would mean hundreds of requests to
// watch behaviour that is entirely on this side of the wire.

// bitchuteEmbedPage is the shape of the embed player's inline script, decoys
// included: a thumbnail URL is assigned two lines above the media URL and a
// commented-out advertising URL sits below it. A pattern looking for "a URL
// being assigned" rather than for this variable takes one of those instead.
const bitchuteEmbedPage = `<!DOCTYPE html><html><head><meta charset="utf-8"></head><body>
<div id="player"></div>
<script>
  function getQueryParam(paramName) {
    return new URLSearchParams(window.location.search).get(paramName) || 'none';
  }
  var thumbnail_url = 'https://static.example.test/live/cover_images/CHANNEL/VIDEO_640x360.jpg';
  var media_url = 'https://seed999.example.test/CHANNEL/VIDEO.mp4';
  var video_id = 'VIDEO';
  var state_id = 'published';
  //var vastTagURL = 'https://ads.example.test/vast?cp.vidref=VIDEO';
</script>
</body></html>`

// bitchuteMediaPlaylist stands in for the adaptive case the defensive branch
// exists for.
const bitchuteMediaPlaylist = `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXTINF:4.0,
part0.ts
#EXTINF:4.0,
part1.ts
#EXT-X-ENDLIST
`

// bitchuteStub stands in for the API and for the embed page.
type bitchuteStub struct {
	// videos is a channel's listing, in the order the site would give it.
	videos []bitchuteVideo
	// mediaHost is the host the API mints every media URL on.
	mediaHost string
	// refuse names the video ids the media endpoint answers 404 for.
	refuse map[string]bool
	// embeds are the embed pages on offer, by video id.
	embeds map[string]string
	// playlists names ids the API answers with an adaptive stream.
	playlists map[string]bool
	// noTitles and noChannelName make those two lookups fail, which neither
	// a video nor a channel job may depend on.
	noTitles      bool
	noChannelName bool

	mu sync.Mutex
	// calls counts requests per path, so a test can pin how many pages were
	// asked for as well as what came back.
	calls map[string]int
	// offsets, limits and channelIDs record what the listing was asked.
	offsets    []int
	limits     []int
	channelIDs []string
	root       string
}

func (s *bitchuteStub) start(t *testing.T) *BitChute {
	t.Helper()
	s.calls = make(map[string]int)
	if s.mediaHost == "" {
		s.mediaHost = "seed999.example.test"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serve(t, w, r)
	}))
	t.Cleanup(srv.Close)
	s.root = srv.URL

	return &BitChute{
		// No retries: a 404 from this stub is an answer, not a hiccup, and
		// a test should not wait out a backoff to see it.
		client: httpx.New("test-agent", "en-US", 0, 5*time.Second),
		api:    srv.URL + "/api/beta",
		site:   srv.URL,
	}
}

func (s *bitchuteStub) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.calls[r.URL.Path]++
	s.mu.Unlock()

	// The live API answers a GET with 405, so an extractor that drifted onto
	// one should fail here rather than out on the site.
	if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodPost {
		t.Errorf("%s %s: the API is POST only", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	switch {
	case r.URL.Path == "/api/beta/video/media":
		s.mediaEndpoint(t, w, r)
	case r.URL.Path == "/api/beta/video":
		s.titleEndpoint(t, w, r)
	case r.URL.Path == "/api/beta/channel/videos":
		s.listingEndpoint(t, w, r)
	case r.URL.Path == "/api/beta/channel":
		s.channelEndpoint(t, w, r)
	case strings.HasPrefix(r.URL.Path, "/embed/"):
		s.embedPage(w, r)
	case strings.HasSuffix(r.URL.Path, ".m3u8"):
		_, _ = io.WriteString(w, bitchuteMediaPlaylist)
	default:
		http.NotFound(w, r)
	}
}

// bitchuteNotFound is the API's own refusal, which carries a JSON body rather
// than an empty one.
func bitchuteNotFound(w http.ResponseWriter, what string) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintf(w, `{"errors":[{"context":"HTTP","message":"Not Found - Unable to locate %s"}]}`, what)
}

func (s *bitchuteStub) videoID(t *testing.T, r *http.Request) string {
	t.Helper()
	var req bitchuteVideoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("%s: undecodable body: %v", r.URL.Path, err)
	}
	return req.VideoID
}

func (s *bitchuteStub) mediaEndpoint(t *testing.T, w http.ResponseWriter, r *http.Request) {
	id := s.videoID(t, r)
	if s.refuse[id] {
		bitchuteNotFound(w, id)
		return
	}
	media := bitchuteMedia{
		Type: "MPEG-4",
		URL:  "https://" + s.mediaHost + "/CHANNEL/" + id + ".mp4",
	}
	if s.playlists[id] {
		media = bitchuteMedia{Type: "HLS", URL: s.root + "/hls/" + id + ".m3u8"}
	}
	_ = json.NewEncoder(w).Encode(media)
}

func (s *bitchuteStub) titleEndpoint(t *testing.T, w http.ResponseWriter, r *http.Request) {
	id := s.videoID(t, r)
	if s.noTitles {
		bitchuteNotFound(w, id)
		return
	}
	_, _ = fmt.Fprintf(w, `{"video_id":%q,"video_name":"A Video Called %s","duration":"1:23"}`, id, id)
}

func (s *bitchuteStub) listingEndpoint(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var req bitchuteChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("%s: undecodable body: %v", r.URL.Path, err)
	}

	s.mu.Lock()
	s.offsets = append(s.offsets, req.Offset)
	s.limits = append(s.limits, req.Limit)
	s.channelIDs = append(s.channelIDs, req.ChannelID)
	s.mu.Unlock()

	// The live endpoint caps the limit at fifty however large a number it is
	// given, and never says so.
	size := min(req.Limit, bitchutePageSize)
	page := []bitchuteVideo{}
	if req.Offset < len(s.videos) {
		page = s.videos[req.Offset:min(req.Offset+size, len(s.videos))]
	}
	_ = json.NewEncoder(w).Encode(struct {
		Videos []bitchuteVideo `json:"videos"`
	}{page})
}

func (s *bitchuteStub) channelEndpoint(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var req bitchuteChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("%s: undecodable body: %v", r.URL.Path, err)
	}
	if s.noChannelName {
		bitchuteNotFound(w, req.ChannelID)
		return
	}
	_, _ = fmt.Fprintf(w, `{"channel_id":%q,"channel_name":"A Channel","url_slug":"a-legacy-slug"}`, req.ChannelID)
}

func (s *bitchuteStub) embedPage(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/embed/"), "/")
	page, ok := s.embeds[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = io.WriteString(w, page)
}

func (s *bitchuteStub) hits(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path]
}

// bitchuteVideos builds a listing of n synthetic records.
func bitchuteVideos(n int) []bitchuteVideo {
	out := make([]bitchuteVideo, n)
	for i := range out {
		out[i] = bitchuteVideo{
			ID:   fmt.Sprintf("ID%09d", i),
			Name: fmt.Sprintf("video-%03d", i),
		}
	}
	return out
}

func bitchuteExtract(t *testing.T, b *BitChute, raw string) *Result {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract(%s): %v", raw, err)
	}
	return res
}

// hostOf is the host of a link, for assertions that care which mirror served
// it and not about the rest.
func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}

// ------------------------------------------------------------------ video

// TestBitChuteVideoIsAnUnsignedFileOnEveryMirror pins the shape of the whole
// result: the link is permanent, so it sits on the File itself; the length is
// unknown, so it must not masquerade as known; and the resolver exists only
// to rotate hosts, which is what makes a transient refusal survivable.
func TestBitChuteVideoIsAnUnsignedFileOnEveryMirror(t *testing.T) {
	stub := &bitchuteStub{}
	b := stub.start(t)

	res := bitchuteExtract(t, b, "https://www.bitchute.com/video/AAAABBBBCCCC/")

	if res.Title != "A Video Called AAAABBBBCCCC" {
		t.Errorf("title = %q, want the video's own name", res.Title)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	file := res.Files[0]

	if want := "A Video Called AAAABBBBCCCC.mp4"; file.Name != want {
		t.Errorf("name = %q, want %q", file.Name, want)
	}
	if file.Size != -1 {
		t.Errorf("size = %d, want -1: the API publishes no length", file.Size)
	}
	if file.SizeApprox {
		t.Error("SizeApprox is set on a file whose size was never reported at all")
	}
	if got := hostOf(t, file.URL); got != stub.mediaHost {
		t.Errorf("the first link is on %q, want the host the API named", got)
	}
	if file.Resolve == nil {
		t.Fatal("no resolver, so a refusal from one host could not be retried on another")
	}

	// One pass through the whole set: every seed host once, the API's own
	// choice leading, and the path untouched throughout.
	seen := make(map[string]int)
	for range len(bitchuteSeedHosts) + 1 {
		target, err := file.Resolve(context.Background())
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		u, err := url.Parse(target.URL)
		if err != nil {
			t.Fatal(err)
		}
		if want := "/CHANNEL/AAAABBBBCCCC.mp4"; u.Path != want {
			t.Errorf("mirror %s changed the path to %q, want %q", u.Host, u.Path, want)
		}
		if target.Size != -1 || target.Name != file.Name {
			t.Errorf("mirror %s lost the file's name or size: %+v", u.Host, target)
		}
		seen[u.Host]++
	}

	for _, host := range append([]string{stub.mediaHost}, bitchuteSeedHosts...) {
		if seen[host] != 1 {
			t.Errorf("%s was offered %d times in one pass, want exactly 1", host, seen[host])
		}
	}
}

// TestBitChuteVideoNameFallsBackToTheID guards the ordering that makes the
// title lookup optional: the media is resolved first, so a title that cannot
// be read costs a good name and nothing else.
func TestBitChuteVideoNameFallsBackToTheID(t *testing.T) {
	stub := &bitchuteStub{noTitles: true}
	b := stub.start(t)

	res := bitchuteExtract(t, b, "https://old.bitchute.com/video/AAAABBBBCCCC/")

	if res.Title != "bitchute-AAAABBBBCCCC" {
		t.Errorf("title = %q, want the id", res.Title)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "bitchute-AAAABBBBCCCC.mp4" {
		t.Errorf("files = %+v, want one named after the id", res.Files)
	}
}

// TestBitChuteEmbedPageIsASecondRouteToTheMedia covers the fallback for the
// API moving out from under us, and with it the decoys on that page.
func TestBitChuteEmbedPageIsASecondRouteToTheMedia(t *testing.T) {
	stub := &bitchuteStub{
		refuse: map[string]bool{"AAAABBBBCCCC": true},
		embeds: map[string]string{"AAAABBBBCCCC": bitchuteEmbedPage},
	}
	b := stub.start(t)

	res := bitchuteExtract(t, b, "https://www.bitchute.com/embed/AAAABBBBCCCC/")

	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	got := res.Files[0].URL
	if !strings.HasSuffix(got, "/CHANNEL/VIDEO.mp4") {
		t.Errorf("url = %q, want the media the page named", got)
	}
	if strings.Contains(got, "cover_images") {
		t.Error("the thumbnail URL assigned above the media URL was taken instead")
	}
	if strings.Contains(got, "vast") {
		t.Error("the commented-out advertising URL was taken instead")
	}
}

// TestBitChuteEmbedMediaIgnoresTheOtherAssignments tests the same trap
// directly, since the pattern is the whole of the fallback.
func TestBitChuteEmbedMediaIgnoresTheOtherAssignments(t *testing.T) {
	m := bitchuteEmbedMedia.FindStringSubmatch(bitchuteEmbedPage)
	if m == nil {
		t.Fatal("no media url found on the page")
	}
	if want := "https://seed999.example.test/CHANNEL/VIDEO.mp4"; m[1] != want {
		t.Errorf("got %q, want %q", m[1], want)
	}

	// A page with the media assignment removed must yield nothing rather
	// than falling back onto whichever URL is left.
	stripped := strings.ReplaceAll(bitchuteEmbedPage, "media_url", "other_url")
	if m := bitchuteEmbedMedia.FindStringSubmatch(stripped); m != nil {
		t.Errorf("a page naming no media url yielded %q", m[1])
	}
}

// TestBitChuteReportsAVideoNeitherRouteCanResolve keeps the two failures from
// hiding each other: with no embed page either, the API's own complaint is
// what the user should see.
func TestBitChuteReportsAVideoNeitherRouteCanResolve(t *testing.T) {
	stub := &bitchuteStub{refuse: map[string]bool{"AAAABBBBCCCC": true}}
	b := stub.start(t)

	u, err := ParseURL("https://www.bitchute.com/video/AAAABBBBCCCC/")
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a video that resolves nowhere was accepted")
	}
	if !strings.Contains(err.Error(), "AAAABBBBCCCC") {
		t.Errorf("error does not name the video: %v", err)
	}
	if stub.hits("/embed/AAAABBBBCCCC/") == 0 {
		t.Error("the embed page was never tried")
	}
}

// TestBitChutePlaylistMediaArrivesAsSegments covers the defensive branch. No
// sampled video took it, which is exactly why it is worth a test: nothing
// else would notice it rotting.
func TestBitChutePlaylistMediaArrivesAsSegments(t *testing.T) {
	stub := &bitchuteStub{playlists: map[string]bool{"AAAABBBBCCCC": true}}
	b := stub.start(t)

	res := bitchuteExtract(t, b, "https://www.bitchute.com/video/AAAABBBBCCCC/")

	file := res.Files[0]
	if file.URL != "" {
		t.Errorf("a playlist-backed file carries a URL (%q); the downloader would fetch the manifest", file.URL)
	}
	if len(file.Segments) != 2 {
		t.Fatalf("got %d segments, want 2: %v", len(file.Segments), file.Segments)
	}
	if !strings.HasSuffix(file.Name, ".ts") {
		t.Errorf("name = %q, want the concatenated-parts extension", file.Name)
	}
}

// ---------------------------------------------------------------- channel

// TestBitChuteChannelPagesUntilAShortPage pins the only stop condition the
// endpoint offers. A listing that never went short would page to the backstop.
func TestBitChuteChannelPagesUntilAShortPage(t *testing.T) {
	stub := &bitchuteStub{videos: bitchuteVideos(117)}
	b := stub.start(t)

	res := bitchuteExtract(t, b, "https://www.bitchute.com/channel/A-Legacy-Slug/")

	if res.Title != "A Channel" {
		t.Errorf("title = %q, want the channel's own name", res.Title)
	}
	if len(res.Files) != 117 {
		t.Fatalf("got %d files, want 117", len(res.Files))
	}
	if got := stub.hits("/api/beta/channel/videos"); got != 3 {
		t.Errorf("asked for %d pages, want 3: 50, 50 and a short one", got)
	}

	// Files keep the channel's order however the media calls interleaved.
	for i, file := range res.Files {
		if want := fmt.Sprintf("video-%03d.mp4", i); file.Name != want {
			t.Fatalf("file %d is %q, want %q", i, file.Name, want)
		}
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	for i, offset := range stub.offsets {
		if want := i * bitchutePageSize; offset != want {
			t.Errorf("page %d asked for offset %d, want %d", i, offset, want)
		}
	}
	for i, limit := range stub.limits {
		// Asking for more is pointless: the endpoint caps it here silently.
		if limit != bitchutePageSize {
			t.Errorf("page %d asked for a limit of %d, want %d", i, limit, bitchutePageSize)
		}
	}
	// The path component goes to the API verbatim, because the endpoint takes
	// a legacy slug as readily as an id — normalising it would break exactly
	// the links that need it.
	for _, id := range stub.channelIDs {
		if id != "A-Legacy-Slug" {
			t.Errorf("channel id %q was rewritten on the way to the API", id)
		}
	}
}

// TestBitChuteChannelPagesExactlyToTheEnd covers the boundary a short page
// cannot signal: a channel whose count is a multiple of the page size only
// ends when an empty page comes back.
func TestBitChuteChannelPagesExactlyToTheEnd(t *testing.T) {
	stub := &bitchuteStub{videos: bitchuteVideos(2 * bitchutePageSize)}
	b := stub.start(t)

	res := bitchuteExtract(t, b, "https://www.bitchute.com/channel/CHANNELID/")

	if len(res.Files) != 2*bitchutePageSize {
		t.Fatalf("got %d files, want %d", len(res.Files), 2*bitchutePageSize)
	}
	if got := stub.hits("/api/beta/channel/videos"); got != 3 {
		t.Errorf("asked for %d pages, want 3: two full ones and the empty one that ends it", got)
	}
}

// TestBitChuteChannelSkipsAVideoThatWillNotResolve is the behaviour a listing
// of hundreds depends on: one dead video is not worth failing the rest, and
// dropping it must not shuffle what is left.
func TestBitChuteChannelSkipsAVideoThatWillNotResolve(t *testing.T) {
	videos := bitchuteVideos(5)
	stub := &bitchuteStub{
		videos: videos,
		refuse: map[string]bool{videos[2].ID: true},
	}
	b := stub.start(t)

	res := bitchuteExtract(t, b, "https://www.bitchute.com/channel/CHANNELID/")

	want := []string{"video-000.mp4", "video-001.mp4", "video-003.mp4", "video-004.mp4"}
	if len(res.Files) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(res.Files), len(want), res.Files)
	}
	for i, file := range res.Files {
		if file.Name != want[i] {
			t.Errorf("file %d is %q, want %q", i, file.Name, want[i])
		}
	}
}

// TestBitChuteChannelTitleFallsBackToTheURLSegment keeps the name lookup
// optional, the way the video title lookup is.
func TestBitChuteChannelTitleFallsBackToTheURLSegment(t *testing.T) {
	stub := &bitchuteStub{videos: bitchuteVideos(3), noChannelName: true}
	b := stub.start(t)

	res := bitchuteExtract(t, b, "https://www.bitchute.com/channel/a-legacy-slug/")

	if res.Title != "a-legacy-slug" {
		t.Errorf("title = %q, want the segment from the URL", res.Title)
	}
	if len(res.Files) != 3 {
		t.Errorf("got %d files, want 3: a missing channel name must cost nothing else", len(res.Files))
	}
}

// TestBitChuteChannelReportsAnEmptyListing separates "no such channel" from
// "nothing downloadable", which the registry would otherwise report as the
// same thing.
func TestBitChuteChannelReportsAnEmptyListing(t *testing.T) {
	stub := &bitchuteStub{}
	b := stub.start(t)

	u, err := ParseURL("https://www.bitchute.com/channel/CHANNELID/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Extract(context.Background(), u, Options{}); err == nil {
		t.Fatal("a channel with nothing on it was accepted")
	}
}

// ------------------------------------------------------------------ units

func TestBitChuteRejectsPathsThatNameNeitherAVideoNorAChannel(t *testing.T) {
	b := (&bitchuteStub{}).start(t)

	for _, raw := range []string{
		"https://www.bitchute.com/",
		"https://www.bitchute.com/video/",
		"https://www.bitchute.com/channel/",
		"https://www.bitchute.com/profile/AAAABBBBCCCC/",
		"https://www.bitchute.com/search?query=x",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Extract(context.Background(), u, Options{}); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
}

func TestBitChuteExt(t *testing.T) {
	tests := map[string]string{
		"https://seed999.example.test/CHANNEL/VIDEO.mp4":  ".mp4",
		"https://seed999.example.test/CHANNEL/VIDEO.webm": ".webm",
		// A query string is not part of the name.
		"https://seed999.example.test/CHANNEL/VIDEO.mp4?x=1.zzzzzz": ".mp4",
		// Neither is a dotted path segment with no extension on it: a name
		// ending ".7" is a path with dots in it, not a file type.
		"https://seed999.example.test/CHANNEL/VIDEO":          ".mp4",
		"https://seed999.example.test/1.2.3.4.5.6.7":          ".mp4",
		"https://seed999.example.test/CHANNEL/VIDEO.verylong": ".mp4",
		"": ".mp4",
	}
	for link, want := range tests {
		if got := bitchuteExt(link); got != want {
			t.Errorf("bitchuteExt(%q) = %q, want %q", link, got, want)
		}
	}
}

func TestBitChuteIsPlaylist(t *testing.T) {
	tests := []struct {
		media bitchuteMedia
		want  bool
	}{
		{bitchuteMedia{Type: "MPEG-4", URL: "https://seed999.example.test/C/V.mp4"}, false},
		{bitchuteMedia{Type: "HLS", URL: "https://seed999.example.test/C/V.mp4"}, true},
		{bitchuteMedia{Type: "application/vnd.apple.mpegurl", URL: "https://seed999.example.test/C/V"}, true},
		// Either half is enough: a host is likelier to change one than both.
		{bitchuteMedia{Type: "MPEG-4", URL: "https://seed999.example.test/C/V.m3u8"}, true},
		{bitchuteMedia{}, false},
	}
	for _, tc := range tests {
		if got := bitchuteIsPlaylist(tc.media); got != tc.want {
			t.Errorf("bitchuteIsPlaylist(%+v) = %v, want %v", tc.media, got, tc.want)
		}
	}
}

func TestBitChuteVideoName(t *testing.T) {
	tests := []struct {
		video bitchuteVideo
		want  string
	}{
		{bitchuteVideo{ID: "AAAABBBBCCCC", Name: "A Title"}, "A Title"},
		{bitchuteVideo{ID: "AAAABBBBCCCC", Name: "  A Title  "}, "A Title"},
		{bitchuteVideo{ID: "AAAABBBBCCCC", Name: "   "}, "bitchute-AAAABBBBCCCC"},
		{bitchuteVideo{ID: "AAAABBBBCCCC"}, "bitchute-AAAABBBBCCCC"},
	}
	for _, tc := range tests {
		if got := bitchuteVideoName(tc.video); got != tc.want {
			t.Errorf("bitchuteVideoName(%+v) = %q, want %q", tc.video, got, tc.want)
		}
	}
}

// TestBitChuteSeedHostsAreDistinctAndOnTheSite guards the one list here that
// is a claim about the world: a duplicate would waste a retry on a host
// already tried, and a typo would spend one on a host that does not exist.
func TestBitChuteSeedHostsAreDistinctAndOnTheSite(t *testing.T) {
	seen := make(map[string]bool, len(bitchuteSeedHosts))
	for _, host := range bitchuteSeedHosts {
		if seen[host] {
			t.Errorf("%s is listed twice", host)
		}
		seen[host] = true
		if !strings.HasSuffix(host, ".bitchute.com") {
			t.Errorf("%s is not a bitchute host", host)
		}
	}
	if len(bitchuteSeedHosts) < 2 {
		t.Error("a mirror set of fewer than two hosts is not a mirror set")
	}
}
