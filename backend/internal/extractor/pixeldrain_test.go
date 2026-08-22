package extractor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestPixeldrainMatch(t *testing.T) {
	p := NewPixeldrain(nil)
	for _, raw := range []string{
		"https://pixeldrain.com/l/abcd1234",
		"https://pixeldrain.com/u/abcd1234",
		"https://nova.storage/d/abcd1234",
		"https://nova.storage/f/abcd1234",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !p.Match(u) {
			t.Errorf("Match(%q) = false", raw)
		}
	}
	other, _ := ParseURL("https://pixeldrain.example.test/l/abcd1234")
	if p.Match(other) {
		t.Error("matched a lookalike host")
	}
}

// Stat answers for a synthetic share, shaped like the real ones: a breadcrumb
// of nodes from the share's root down to the one asked about, base_index
// naming which of them that is, and the directory's children beside it.
//
// Three things in here are the point of the fixture. The reported node is
// refused by the host itself, so queueing it would only produce a failure
// later. The directory that will not stat stands for a share whose subtree
// has gone, which must cost that subtree and nothing else. And the name with
// a space in it has to survive into the URL, since these are names from a
// real filesystem rather than ids.
const (
	pixeldrainShareRoot = `{
  "path": [
    {"type": "dir", "path": "/", "name": "shared drive", "mode_string": "drwxrwxrwx"}
  ],
  "base_index": 0,
  "children": [
    {"type": "file", "path": "/one.txt", "name": "one.txt", "file_size": 12, "file_type": "text/plain"},
    {"type": "dir", "path": "/photos", "name": "photos"},
    {"type": "file", "path": "/reported.bin", "name": "reported.bin", "file_size": 99,
     "abuse_type": "copyright", "abuse_report_time": "2024-02-04T18:34:13.466276Z"},
    {"type": "dir", "path": "/broken", "name": "broken"}
  ],
  "permissions": {"owner": false, "read": true, "write": false, "delete": false}
}`

	// The breadcrumb of a link pointing into the tree carries the share's
	// root as well, and base_index — not the position — is what says which
	// entry was asked about.
	pixeldrainSharePhotos = `{
  "path": [
    {"type": "dir", "path": "/", "name": "shared drive"},
    {"type": "dir", "path": "/photos", "name": "photos"}
  ],
  "base_index": 1,
  "children": [
    {"type": "file", "path": "/photos/cat pic.jpg", "name": "cat pic.jpg", "file_size": 34567,
     "file_type": "image/jpeg"}
  ]
}`

	pixeldrainShareFile = `{
  "path": [
    {"type": "file", "path": "/", "name": "one alone.bin", "file_size": 4096, "file_type": "application/octet-stream"}
  ],
  "base_index": 0
}`
)

// pixeldrainServer stands in for a host running this software. It answers
// only stat requests, so a walk that forgot to ask for one fails loudly
// rather than parsing a file's contents as JSON.
func pixeldrainServer(t *testing.T, answers map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := answers[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if _, stat := r.URL.Query()["stat"]; !stat {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testPixeldrain(t *testing.T) *Pixeldrain {
	t.Helper()
	// No retries: a subtree that will not answer should be given up on at
	// once, and the test should not wait out a backoff to see it.
	return &Pixeldrain{client: httpx.New("test-agent", "en-US", 0, 5*time.Second)}
}

func TestPixeldrainSharedDirectory(t *testing.T) {
	srv := pixeldrainServer(t, map[string]string{
		"/api/filesystem/abcd1234":        pixeldrainShareRoot,
		"/api/filesystem/abcd1234/photos": pixeldrainSharePhotos,
	})

	u, err := ParseURL(srv.URL + "/d/abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	res, err := testPixeldrain(t).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if res.Title != "shared drive" {
		t.Errorf("title = %q, want the shared directory's own name", res.Title)
	}
	if len(res.Files) != 2 {
		t.Fatalf("resolved %d files, want 2 (the reported node and the unreadable subtree are not files)",
			len(res.Files))
	}

	first := res.Files[0]
	if first.Name != "one.txt" || first.Dir != "" {
		t.Errorf("first file = %q in %q, want one.txt at the top", first.Name, first.Dir)
	}
	if want := srv.URL + "/api/filesystem/abcd1234/one.txt?attach"; first.URL != want {
		t.Errorf("url = %q, want %q", first.URL, want)
	}
	// The API reports the real length, so it is exact: the downloader may
	// skip a file on disk that already matches it.
	if first.Size != 12 || first.SizeApprox {
		t.Errorf("size = %d approx = %v, want the exact 12 bytes", first.Size, first.SizeApprox)
	}

	nested := res.Files[1]
	if nested.Dir != "photos" {
		t.Errorf("nested file landed in %q, want the tree preserved under photos", nested.Dir)
	}
	if want := srv.URL + "/api/filesystem/abcd1234/photos/cat%20pic.jpg?attach"; nested.URL != want {
		t.Errorf("url = %q, want %q", nested.URL, want)
	}
	if nested.Name != "cat pic.jpg" {
		t.Errorf("name = %q, want the name as the filesystem spells it", nested.Name)
	}
}

// TestPixeldrainSharedSubdirectory covers a link that points into the tree.
// The breadcrumb then holds more than one node, and base_index is the only
// thing saying which of them the job is named after.
func TestPixeldrainSharedSubdirectory(t *testing.T) {
	srv := pixeldrainServer(t, map[string]string{
		"/api/filesystem/abcd1234/photos": pixeldrainSharePhotos,
	})

	u, _ := ParseURL(srv.URL + "/d/abcd1234/photos")
	res, err := testPixeldrain(t).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Title != "photos" {
		t.Errorf("title = %q, want the directory that was asked for", res.Title)
	}
	if len(res.Files) != 1 || res.Files[0].Dir != "" {
		t.Fatalf("got %+v, want the one file at the root of this job", res.Files)
	}
}

// TestPixeldrainSharedFile covers a share that names a file rather than a
// directory, which is the same endpoint answering about a different thing.
func TestPixeldrainSharedFile(t *testing.T) {
	srv := pixeldrainServer(t, map[string]string{
		"/api/filesystem/abcd1234": pixeldrainShareFile,
	})

	u, _ := ParseURL(srv.URL + "/d/abcd1234")
	res, err := testPixeldrain(t).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("resolved %d files, want 1", len(res.Files))
	}
	if res.Files[0].Name != "one alone.bin" || res.Files[0].Size != 4096 {
		t.Errorf("file = %q of %d bytes, want the shared file itself",
			res.Files[0].Name, res.Files[0].Size)
	}
}

// TestPixeldrainOriginIsTheLinksOwn is what lets one extractor serve every
// host running this software: nothing may address a compiled-in host.
func TestPixeldrainOriginIsTheLinksOwn(t *testing.T) {
	const list = `{"success": true, "title": "an album", "files": [
	  {"id": "aaaa1111", "name": "first.jpg", "size": 1234, "mime_type": "image/jpeg"}
	]}`

	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		_, _ = io.WriteString(w, list)
	}))
	t.Cleanup(srv.Close)

	u, _ := ParseURL(srv.URL + "/l/abcd1234")
	res, err := testPixeldrain(t).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if want := "/api/list/abcd1234"; asked != want {
		t.Errorf("asked for %q, want %q", asked, want)
	}
	if want := srv.URL + "/api/file/aaaa1111?download"; res.Files[0].URL != want {
		t.Errorf("url = %q, want it built from the host the link named", res.Files[0].URL)
	}
	if referer := res.Files[0].Headers[httpx.HeaderReferer]; referer != srv.URL+"/" {
		t.Errorf("referer = %q, want the host the link named", referer)
	}
}

func TestPixeldrainFSURL(t *testing.T) {
	origin := "https://example.test"
	cases := []struct {
		segs []string
		want string
	}{
		{segs: []string{"abcd1234"}, want: origin + "/api/filesystem/abcd1234"},
		// A slash inside a name is part of the name, not a separator.
		{segs: []string{"abcd1234", "a/b.txt"}, want: origin + "/api/filesystem/abcd1234/a%2Fb.txt"},
		{segs: []string{"abcd1234", "sub dir", "x&y.bin"}, want: origin + "/api/filesystem/abcd1234/sub%20dir/x&y.bin"},
	}
	for _, tc := range cases {
		if got := pixeldrainFSURL(origin, tc.segs); got != tc.want {
			t.Errorf("pixeldrainFSURL(%v) = %q, want %q", tc.segs, got, tc.want)
		}
	}
}
