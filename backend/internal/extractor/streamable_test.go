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
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// streamableReadyDoc is a finished video in the shape the API serves, cut to
// the fields that matter and carrying the document's two traps.
//
// The mobile rendition is listed first, as the site lists it, so nothing may
// be inferred from position. And original_size sits above the renditions,
// describing the file that was uploaded: it is an order of magnitude larger
// than the transcode that is actually served, which is why it must never
// reach File.Size.
const streamableReadyDoc = `{
  "shortcode": "abcd12",
  "status": 2,
  "percent": 100,
  "title": "A Synthetic Clip",
  "original_name": "a synthetic clip (1080p).mov",
  "original_size": 442093890,
  "duration": 12.0,
  "width": 1920,
  "height": 1080,
  "files": {
    "mp4-mobile": {
      "url": "//cdn.example.test/video/mp4-mobile/abcd12.mp4?Expires=1&Signature=aaa&Key-Pair-Id=bbb",
      "width": 640, "height": 360, "size": 3512386, "status": 2
    },
    "mp4": {
      "url": "//cdn.example.test/video/mp4/abcd12.mp4?Expires=1&Signature=ccc&Key-Pair-Id=bbb",
      "width": 1920, "height": 1080, "size": 44039389, "status": 2
    }
  }
}`

// streamableEarlyDoc is a video from the site's early years: the rendition
// carries no size and the video no title, while original_size is present and
// wrong for it.
const streamableEarlyDoc = `{
  "shortcode": "abcd12",
  "status": 2,
  "percent": 100,
  "title": "",
  "original_name": "cow.mov",
  "original_size": 34220382,
  "files": {
    "mp4": {
      "url": "//cdn.example.test/video/abcd12.mp4?Expires=1&Signature=ddd",
      "width": 852, "height": 480
    }
  }
}`

// streamablePartialDoc is a video the site will serve now and improve later:
// the taller rendition is listed with its dimensions but no link, because it
// is still being made.
const streamablePartialDoc = `{
  "shortcode": "abcd12",
  "status": 2,
  "waiting_for_best": true,
  "title": "Half Done",
  "files": {
    "mp4": {"width": 1920, "height": 1080, "status": 1, "percent": 40},
    "mp4-mobile": {
      "url": "//cdn.example.test/video/mp4-mobile/abcd12.mp4?Expires=1",
      "width": 640, "height": 360, "size": 3512386, "status": 2
    }
  }
}`

// streamableDoc decodes a fixture the way the fetch does, so the JSON tags
// are under test too.
func streamableDoc(t *testing.T, doc string) *streamableVideo {
	t.Helper()
	var video streamableVideo
	if err := json.Unmarshal([]byte(doc), &video); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return &video
}

func TestStreamableTargetPrefersTheTallerRendition(t *testing.T) {
	target, err := streamableTarget(streamableDoc(t, streamableReadyDoc), "abcd12")
	if err != nil {
		t.Fatalf("streamableTarget: %v", err)
	}

	// Protocol-relative in the document; unusable until it is given a scheme.
	want := "https://cdn.example.test/video/mp4/abcd12.mp4?Expires=1&Signature=ccc&Key-Pair-Id=bbb"
	if target.URL != want {
		t.Errorf("url = %q, want the full-height rendition with a scheme", target.URL)
	}
	if target.Size != 44039389 {
		t.Errorf("size = %d, want the rendition's own byte count", target.Size)
	}
	if target.Name != "A Synthetic Clip.mp4" {
		t.Errorf("name = %q, want the title with the served extension", target.Name)
	}
}

// The upload's size is the decoy in this document: publishing it would be an
// exact size describing a different file, so a finished download would look
// unfinished and a file already on disk would never be skipped.
func TestStreamableTargetNeverTakesTheUploadsSize(t *testing.T) {
	for _, doc := range []string{streamableReadyDoc, streamableEarlyDoc} {
		target, err := streamableTarget(streamableDoc(t, doc), "abcd12")
		if err != nil {
			t.Fatalf("streamableTarget: %v", err)
		}
		if target.Size == 442093890 || target.Size == 34220382 {
			t.Errorf("size = %d, which is original_size and describes the upload", target.Size)
		}
	}
}

// A rendition with no length must say so, rather than borrowing one.
func TestStreamableTargetReportsAnUnknownSize(t *testing.T) {
	target, err := streamableTarget(streamableDoc(t, streamableEarlyDoc), "abcd12")
	if err != nil {
		t.Fatalf("streamableTarget: %v", err)
	}
	if target.Size != -1 {
		t.Errorf("size = %d, want -1 for a rendition that states none", target.Size)
	}
	// No title, so the upload names the file — but not its container, which
	// is the one the site transcoded away from.
	if target.Name != "cow.mp4" {
		t.Errorf("name = %q, want the upload's stem with the served extension", target.Name)
	}
}

// A rendition that is still being made is listed in full except for its link,
// so height alone would pick something that cannot be fetched.
func TestStreamableTargetSkipsARenditionWithNoLink(t *testing.T) {
	target, err := streamableTarget(streamableDoc(t, streamablePartialDoc), "abcd12")
	if err != nil {
		t.Fatalf("streamableTarget: %v", err)
	}
	if !strings.Contains(target.URL, "mp4-mobile") {
		t.Errorf("url = %q, want the one rendition that has a link", target.URL)
	}
}

// One document must always name the same rendition: Resolve re-reads it
// before every attempt, and a transfer that resumed into the other encode
// would splice two files together. Map iteration is randomised, so a tie
// decided by iteration order would be a rare, unreproducible corruption.
func TestStreamableBestIsDeterministicOnATie(t *testing.T) {
	files := map[string]streamableFile{
		"mp4":        {URL: "https://cdn.example.test/a.mp4", Height: 720, Size: 1000},
		"mp4-mobile": {URL: "https://cdn.example.test/b.mp4", Height: 720, Size: 1000},
		"mp4-alt":    {URL: "https://cdn.example.test/c.mp4", Height: 720, Size: 1000},
	}
	first, ok := streamableBest(files)
	if !ok {
		t.Fatal("nothing was chosen")
	}
	for range 50 {
		got, ok := streamableBest(files)
		if !ok || got.URL != first.URL {
			t.Fatalf("chose %q then %q for the same document", first.URL, got.URL)
		}
	}
}

func TestStreamableBestHandlesAwkwardLists(t *testing.T) {
	tests := []struct {
		name   string
		files  map[string]streamableFile
		want   string
		wantOK bool
	}{
		{name: "nothing at all"},
		{
			name:  "listed but not yet made",
			files: map[string]streamableFile{"mp4": {Height: 1080}},
		},
		{
			name: "the larger size breaks a tie in height",
			files: map[string]streamableFile{
				"mp4":        {URL: "https://cdn.example.test/a.mp4", Height: 480, Size: 900},
				"mp4-mobile": {URL: "https://cdn.example.test/b.mp4", Height: 480, Size: 2000},
			},
			want:   "https://cdn.example.test/b.mp4",
			wantOK: true,
		},
		{
			// A name this does not know about is still worth downloading.
			name: "an unfamiliar rendition name",
			files: map[string]streamableFile{
				"webm-1440": {URL: "https://cdn.example.test/c.mp4", Height: 1440, Size: 10},
				"mp4":       {URL: "https://cdn.example.test/a.mp4", Height: 1080, Size: 20},
			},
			want:   "https://cdn.example.test/c.mp4",
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := streamableBest(tc.files)
			if ok != tc.wantOK || got.URL != tc.want {
				t.Errorf("streamableBest = %q, %v; want %q, %v", got.URL, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// A video that is not transcoded yet is an answer, not a failure, and the
// message has to say which of the two it is: one is worth waiting for and the
// other never will be.
func TestStreamableTargetReportsWhyAVideoIsNotReady(t *testing.T) {
	tests := []struct {
		doc  string
		want string
	}{
		{`{"status": 0}`, "uploading"},
		{`{"status": 1, "percent": 42}`, "42%"},
		{`{"status": 3}`, "failed to transcode"},
		{`{"status": 7}`, "status 7"},
	}
	for _, tc := range tests {
		_, err := streamableTarget(streamableDoc(t, tc.doc), "abcd12")
		if err == nil {
			t.Fatalf("%s resolved to something", tc.doc)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s reported %q, which does not say %q", tc.doc, err, tc.want)
		}
	}
}

// A finished video with nothing behind it is its own case: the status says
// everything is fine, so the error must not blame the transcode.
func TestStreamableTargetRejectsAReadyVideoWithNoRenditions(t *testing.T) {
	if _, err := streamableTarget(streamableDoc(t, `{"status": 2, "files": {}}`), "abcd12"); err == nil {
		t.Fatal("a video offering no rendition resolved to something")
	}
}

// streamableServer stands in for the API, minting a link with a fresh
// signature on every call the way CloudFront does, and counting the calls.
func streamableServer(t *testing.T, status int, body string) (*Streamable, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "Video does not exist")
			return
		}
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "Signature=ccc", fmt.Sprintf("Signature=sig%d", n)))
	}))
	t.Cleanup(srv.Close)
	return &Streamable{
		client: httpx.New("test-agent", "en-US", 0, 5*time.Second),
		api:    srv.URL + "/videos/",
	}, &hits
}

// The point of the whole extractor: no URL is published, because the one the
// document carries is signed and would lapse while the item waited its turn —
// but the size is, and as an exact figure, because it does not expire with
// the link and is what gives the queue a real total and lets an identical
// file already on disk be skipped before a connection is opened.
func TestStreamableExtractDefersTheLinkButNotTheSize(t *testing.T) {
	s, hits := streamableServer(t, http.StatusOK, streamableReadyDoc)
	u, err := ParseURL("https://streamable.com/abcd12")
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	f := res.Files[0]

	if f.URL != "" {
		t.Errorf("url = %q, want none: a signed link must not be published at extraction time", f.URL)
	}
	if f.Resolve == nil {
		t.Fatal("no resolver, so the item would start with no link at all")
	}
	if f.Size != 44039389 || f.SizeApprox {
		t.Errorf("size = %d approx = %v, want the rendition's exact byte count", f.Size, f.SizeApprox)
	}
	if f.Name != "A Synthetic Clip.mp4" || res.Title != "A Synthetic Clip" {
		t.Errorf("name = %q title = %q", f.Name, res.Title)
	}

	// Every attempt re-reads the document, so two attempts get two
	// signatures. Sharing one would be the bug this shape exists to avoid.
	first, err := f.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	second, err := f.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if first.URL == second.URL {
		t.Errorf("both attempts got %q, so the document was not re-read", first.URL)
	}
	if second.Size != 44039389 || second.Name != f.Name {
		t.Errorf("resolved target = %d bytes named %q, want the same file", second.Size, second.Name)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("the API was asked %d times, want one extraction and two attempts", got)
	}
}

// A missing video answers 404 with a line of plain text, so the unhandled
// error would name a JSON decode failure at an endpoint the user never typed.
func TestStreamableExtractReportsAMissingVideoPlainly(t *testing.T) {
	s, _ := streamableServer(t, http.StatusNotFound, "")
	u, err := ParseURL("https://streamable.com/abcd12")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Extract(context.Background(), u, Options{})
	if err == nil {
		t.Fatal("a missing video extracted successfully")
	}
	if !strings.Contains(err.Error(), "no video abcd12") || strings.Contains(err.Error(), "json") {
		t.Errorf("error = %q, want a plain statement that the video is gone", err)
	}
}

func TestStreamableID(t *testing.T) {
	tests := map[string]string{
		"https://streamable.com/abcd12":        "abcd12",
		"https://www.streamable.com/abcd12/":   "abcd12",
		"https://streamable.com/abcd12?t=3":    "abcd12",
		"https://streamable.com/e/abcd12":      "abcd12",
		"https://streamable.com/s/abcd12/slug": "abcd12",
		"https://streamable.com/o/abcd12":      "abcd12",
		"https://streamable.com/":              "",
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := streamableID(u); got != want {
			t.Errorf("streamableID(%s) = %q, want %q", raw, got, want)
		}
	}
}

// The match is deliberately an allowlist. A direct CDN link is already
// downloadable and belongs to the fallback extractor; routed here it would
// become an API lookup for a video id that does not exist.
func TestStreamableMatch(t *testing.T) {
	s := NewStreamable(nil)
	for _, host := range []string{"streamable.com", "www.streamable.com", "Streamable.com", "streamable.com:443"} {
		if !s.Match(&url.URL{Scheme: "https", Host: host, Path: "/abcd12"}) {
			t.Errorf("%s was not matched", host)
		}
	}
	for _, host := range []string{"cdn-cf-west.streamable.com", "ajax.streamable.com", "notstreamable.com"} {
		if s.Match(&url.URL{Scheme: "https", Host: host, Path: "/video/mp4/abcd12.mp4"}) {
			t.Errorf("%s was matched, and it serves no video pages", host)
		}
	}
}
