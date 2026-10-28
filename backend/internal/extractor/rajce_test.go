// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Invented album data with the site's separate photo, video and poster
// fields. Recommendations are deliberately outside legacy_media.
const rajceAlbumPage = `<html><head><title>Unrelated page title</title></head><body>
<p>var settings = this text is not a script</p>
<script>var unrelated = {};</script><script>
var settings = {
  "album_name": "Field  notes (1/2) \u2014 \u017e",
  "media_count": 3,
  "miniplayer_data": {"video_url":"https://media.example.test/unrelated.mp4"},
  "legacy_media": [
    {"id":101,"type":"photo","is_video":false,"filename":"Morning  light.jpg",
     "image_url":"https:\/\/media.example.test\/images\/Morning%20%20light.jpg?download=1",
     "thumb_url":"https://media.example.test/thumb/Morning.jpg","version":2},
    {"id":102,"type":"video","is_video":true,"filename":"River.clip.jpg",
     "image_url":"https://media.example.test/images/poster.jpg",
     "video_url":"https://media.example.test/video/102"},
    {"id":"103","type":"video","is_video":true,"filename":"103",
     "image_url":"https://media.example.test/images/103",
     "video_url":"/storage/video/103"},
    {"id":"102","type":"video","is_video":true,"filename":"duplicate.jpg",
     "video_url":"https://media.example.test/video/102?duplicate=1"},
    {"id":104,"type":"video","is_video":true,"filename":"duplicate-url.jpg",
     "video_url":"/storage/video/103"}
  ]
}; var domain = "example.test";
</script><img src="https://media.example.test/logo.jpg"></body></html>`

func rajceTestServer(t *testing.T, handler http.HandlerFunc) (*Rajce, *url.URL) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL + "/photographer/album/field-notes")
	if err != nil {
		t.Fatal(err)
	}
	return NewRajce(httpx.New("test-agent", "en-US", 0, 5*time.Second)), u
}

func TestRajceAlbumPhotosAndVideos(t *testing.T) {
	var requests []string
	r, u := rajceTestServer(t, func(w http.ResponseWriter, req *http.Request) {
		requests = append(requests, req.URL.Path)
		switch req.URL.Path {
		case "/legacy-album":
			http.Redirect(w, req, "/photographer/album/field-notes", http.StatusFound)
		case "/photographer/album/field-notes":
			_, _ = w.Write([]byte(rajceAlbumPage))
		default:
			t.Errorf("discovery fetched media or another page: %s", req.URL.Path)
			http.NotFound(w, req)
		}
	})
	final := u.String()
	u.Path, u.Fragment = "/legacy-album", "selected-photo"
	res, err := r.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Field  notes (1/2) — ž" || res.Note != "" {
		t.Errorf("title=%q note=%q, want the unchanged album title and no note", res.Title, res.Note)
	}
	want := []struct{ name, link string }{
		{"Morning  light.jpg", "https://media.example.test/images/Morning%20%20light.jpg?download=1&ver=2"},
		{"River.clip.mp4", "https://media.example.test/video/102"},
		{"103.mp4", u.Scheme + "://" + u.Host + "/storage/video/103"},
	}
	if len(res.Files) != len(want) {
		t.Fatalf("got %d files, want %d unique album items", len(res.Files), len(want))
	}
	for i, file := range res.Files {
		if file.Name != want[i].name || file.URL != want[i].link {
			t.Errorf("file %d = %q %q, want %q %q", i, file.Name, file.URL, want[i].name, want[i].link)
		}
		if file.Size != -1 || file.SizeApprox {
			t.Errorf("file %d invents a size: %d (approx=%v)", i, file.Size, file.SizeApprox)
		}
		if file.Headers[httpx.HeaderReferer] != final {
			t.Errorf("file %d referer=%q, want final album URL %q", i, file.Headers[httpx.HeaderReferer], final)
		}
	}
	if got := strings.Join(requests, " "); got != "/legacy-album /photographer/album/field-notes" {
		t.Errorf("requests=%q, want only the redirect and album", got)
	}
}

func TestRajceAlbumLimitsAndUnavailableItems(t *testing.T) {
	for _, tc := range []struct {
		name, doc, note string
		limit, files    int
	}{
		{"file limit", rajceAlbumPage, "partial — file limit; 2 of 3 files", 2, 2},
		{"exact limit ignores duplicates", rajceAlbumPage, "", 3, 3},
		{"larger limit", rajceAlbumPage, "", 4, 3},
		{"incomplete metadata", strings.Replace(rajceAlbumPage, `"media_count": 3`, `"media_count": 5`, 1), "3 of 5 files", 0, 3},
		{"unavailable video keeps no poster", strings.Replace(rajceAlbumPage,
			`"video_url":"/storage/video/103"`, `"video_url":null`, -1), "2 of 3 files; 2 album items unavailable", 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, u := rajceTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.doc))
			})
			res, err := r.Extract(context.Background(), u, Options{limits: Limits{Files: tc.limit}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != tc.files || res.Note != tc.note {
				t.Errorf("files=%d note=%q, want %d and %q", len(res.Files), res.Note, tc.files, tc.note)
			}
			if res.Title != "Field  notes (1/2) — ž" {
				t.Errorf("partial result changed folder title to %q", res.Title)
			}
		})
	}
}

func TestRajceInvalidAlbums(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"missing settings", `<html><img src="/logo.jpg"></html>`, "no album media"},
		{"not JSON", `<script>var settings = {broken};</script>`, "read album data"},
		{"truncated JSON", `<script>var settings = {"legacy_media":[</script>`, "read album data"},
		{"null settings", `<script>var settings = null;</script>`, "no album media"},
		{"profile or restricted album", `<script>var settings = {"album_name":"Locked album"};</script>`, "no album media"},
		{"empty album", `<script>var settings = {"legacy_media":[]};</script>`, "no downloadable"},
		{"only poster", `<script>var settings = {"legacy_media":[
          {"type":"video","image_url":"https://media.example.test/poster.jpg"}]};</script>`, "no downloadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, u := rajceTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.doc))
			})
			res, err := r.Extract(context.Background(), u, Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) || res != nil {
				t.Errorf("result=%+v error=%v, want %q", res, err, tc.want)
			}
		})
	}
}

func TestRajceFetchErrors(t *testing.T) {
	r, u := rajceTestServer(t, http.NotFound)
	if _, err := r.Extract(context.Background(), u, Options{}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing album: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Extract(ctx, u, Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled extraction: %v", err)
	}
}

func TestRajceMediaURLs(t *testing.T) {
	page, _ := url.Parse("https://album.example.test/photographer/album/field-notes")
	for _, tc := range []struct {
		ref, want string
	}{
		{"//media.example.test/images/Photo.jpg", "https://media.example.test/images/Photo.jpg"},
		{"/images/Photo.jpg#selected", "https://album.example.test/images/Photo.jpg"},
		{"", ""},
		{"   ", ""},
		{"javascript:alert(1)", ""},
		{"data:image/jpeg;base64,abc", ""},
		{"file:///tmp/Photo.jpg", ""},
		{"https://user:password@media.example.test/Photo.jpg", ""},
		{"https://media.example.test/%zz", ""},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			file, ok := (rajceMedium{Type: "photo", ImageURL: tc.ref}).file(page)
			if ok != (tc.want != "") || file.URL != tc.want {
				t.Errorf("file URL=%q ok=%v, want %q", file.URL, ok, tc.want)
			}
		})
	}
}

func TestRajceRegistryAndStorage(t *testing.T) {
	reg := testRegistry(t, nil)
	for _, host := range []string{"rajce.idnes.cz", "www.rajce.idnes.cz", "rajce.net", "photographer.rajce.net"} {
		u := &url.URL{Scheme: "https", Host: host, Path: "/"}
		if got := reg.Find(u).Name(); got != "rajce" {
			t.Errorf("%s routes to %q", host, got)
		}
	}
	for _, host := range []string{"idnes.cz", "www.idnes.cz", "rajce.idnes.cz.example.test", "notrajce.net"} {
		if NewRajce(nil).Match(&url.URL{Scheme: "https", Host: host}) {
			t.Errorf("claimed unrelated host %s", host)
		}
	}
	for _, tc := range []struct{ path, name string }{
		{"/storage/images/First.jpg", "First.jpg"},
		{"/storage/video/123", "123.mp4"},
	} {
		// Build synthetic media URLs at runtime; no live filename or content
		// URL belongs in the tracked fixtures.
		u := &url.URL{Scheme: "https", Host: "img1.rajce.idnes.cz", Path: tc.path}
		res, err := NewRajce(nil).Extract(context.Background(), u, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Files) != 1 || res.Files[0].Name != tc.name || res.Files[0].URL != u.String() {
			t.Errorf("storage file = %+v, want %q at the original URL", res.Files, tc.name)
		}
	}
}
