// SPDX-License-Identifier: MIT

package extractor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// kemonoShellPage is the sites' app shell: nothing but its loader, an empty
// application root and the site's name.
const kemonoShellPage = `<!doctype html><html><head><title>Kemono</title>
<script src="/static/js/lazy-styles.js"></script><meta name="og:site_name" content="Kemono">
<script type="module" src="/assets/index-AAAA.js"></script></head><body><div id="root"></div></body></html>`

func TestKemonoRecognise(t *testing.T) {
	k := NewKemono(nil)
	expectRecognised(t, k, htmlPage(t, "https://archive.example.test/sample/user/creator", kemonoShellPage), true)
	expectRecognised(t, k, htmlPage(t, "https://archive.example.test/sample/user/creator/post/42", kemonoShellPage), true)
	// The right form and the name, without the shell's loader.
	expectRecognised(t, k, htmlPage(t, "https://archive.example.test/sample/user/creator",
		`<html><head><title>Kemono</title></head><body><div id="root"></div></body></html>`), false)
	// The shell on a path that names no creator.
	expectRecognised(t, k, htmlPage(t, "https://archive.example.test/artists", kemonoShellPage), false)
	// A page that cannot say, with no client to ask the API through.
	expectRecognised(t, k, fixturePage(t, "https://archive.example.test/sample/user/creator", http.StatusForbidden, "text/html", ""), false)

	expectShape(t, k, "https://archive.example.test/sample/user/creator", true)
	expectShape(t, k, "https://archive.example.test/sample/user/creator/post/42", true)
	expectShape(t, k, "https://archive.example.test/sample/user", false)
	expectShape(t, k, "https://archive.example.test/user/creator", false)
}

func TestKemonoFilesOf(t *testing.T) {
	posts := []kemonoPost{
		{
			ID:    "111",
			Title: "First Post",
			File:  kemonoAttachment{Name: "cover.jpg", Path: "/aa/bb/cover"},
			Attachments: []kemonoAttachment{
				{Name: "one.mp4", Path: "/cc/dd/one"},
				// The primary file listed again among the attachments, which
				// the sites do; it must not queue twice.
				{Name: "cover.jpg", Path: "/aa/bb/cover"},
			},
		},
		{
			ID: "112",
			// No title: the folder falls back to the post id.
			File: kemonoAttachment{Path: "/ee/ff/plain.png"},
		},
		{
			ID: "113", Title: "Text only",
			// No file and no attachments: contributes nothing.
		},
	}

	k := NewKemono(nil)
	files := k.filesOf("https://kemono.cr", posts, true)
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3 (the repeat folded, the text post empty)", len(files))
	}

	first := files[0]
	if first.Name != "cover.jpg" || first.Dir != "First Post" {
		t.Errorf("first = %q in %q, want cover.jpg in the post's folder", first.Name, first.Dir)
	}
	if want := "https://kemono.cr/data/aa/bb/cover"; first.URL != want {
		t.Errorf("url = %q, want %q", first.URL, want)
	}
	// The sites answer an ordinary browser request with 403 and a message
	// naming the header they want instead; every file must carry it.
	if first.Headers[httpx.HeaderAccept] != kemonoScrapeAccept {
		t.Errorf("accept = %q, want %q", first.Headers[httpx.HeaderAccept], kemonoScrapeAccept)
	}

	// An attachment with no name is saved under the path's own base.
	if files[2].Name != "plain.png" || files[2].Dir != "112" {
		t.Errorf("nameless attachment = %q in %q, want plain.png in the id folder",
			files[2].Name, files[2].Dir)
	}

	// A single post's files go unfoldered: the job title already names it.
	flat := k.filesOf("https://kemono.cr", posts[:1], false)
	for _, f := range flat {
		if f.Dir != "" {
			t.Errorf("%s filed under %q, want no folder for a single post", f.Name, f.Dir)
		}
	}
}

// A creator's posts are paged by offset to the count its profile states, or,
// where it states none, to the 400 the API answers an offset past the end
// with, which is no failure.
func TestKemonoCreatorPagesToTheStatedEnd(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		asked string
	}{
		{name: "stated count", count: 3, asked: "0,2"},
		{name: "past the end", asked: "0,2,3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/sample/user/creator/profile":
					fmt.Fprintf(w, `{"id":"creator","service":"sample","name":"A Creator","post_count":%d}`, tc.count)
				case "/api/v1/sample/user/creator/posts":
					o := r.URL.Query().Get("o")
					asked = append(asked, o)
					var posts []kemonoPost
					switch o {
					case "0":
						posts = []kemonoPost{{ID: "1", File: kemonoAttachment{Path: "/a/one.jpg"}}, {ID: "2", File: kemonoAttachment{Path: "/a/two.jpg"}}}
					case "2":
						posts = []kemonoPost{{ID: "3", File: kemonoAttachment{Path: "/a/three.jpg"}}}
					default:
						http.Error(w, `{"error":"Offset is bigger than total count."}`, http.StatusBadRequest)
						return
					}
					_ = json.NewEncoder(w).Encode(posts)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			res := extractOK(t, NewKemono(httpx.New("test", "en", 0, time.Second)), srv.URL+"/sample/user/creator", Options{})
			if len(res.Files) != 3 || res.Title != "A Creator" || res.Note != "" || strings.Join(asked, ",") != tc.asked {
				t.Fatalf("files=%d title=%q note=%q asked=%v", len(res.Files), res.Title, res.Note, asked)
			}
			if got := strings.Join(filePaths(res.Files), ","); got != "1/one.jpg,2/two.jpg,3/three.jpg" {
				t.Fatalf("files %s", got)
			}
		})
	}
}

// Preview metadata must neither consume the file cap nor hide a later
// original with the same path. APIs without the availability flag keep
// their existing behavior.
func TestKemonoOriginalAvailability(t *testing.T) {
	for _, flag := range []string{``, `,"has_full":true`, `,"has_full":false`} {
		t.Run(flag, func(t *testing.T) {
			var offsets []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
				switch {
				case strings.HasSuffix(r.URL.Path, "/profile"):
					fmt.Fprint(w, `{"name":"A Creator","post_count":2}`)
				case strings.HasSuffix(r.URL.Path, "/posts"):
					o := r.URL.Query().Get("o")
					offsets = append(offsets, o)
					if o == "0" {
						fmt.Fprint(w, `[{"id":"1","has_full":false,"file":{"path":"/a/sample.jpg"}}]`)
					} else {
						fmt.Fprintf(w, `[{"id":"2","title":"Original","file":{"path":"/a/sample.jpg"}%s}]`, flag)
					}
				case strings.HasSuffix(r.URL.Path, "/post/1"):
					fmt.Fprint(w, `{"id":"1","has_full":false,"file":{"path":"/a/sample.jpg"}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			k := NewKemono(testClient())
			page := mustParse(t, srv.URL+"/sample/user/creator")
			res, err := k.Extract(t.Context(), page, Options{limits: Limits{Files: 1}})
			if strings.Join(offsets, ",") != "0,1" {
				t.Fatalf("preview metadata consumed the file cap: %v", offsets)
			}
			if strings.Contains(flag, "false") {
				if err == nil || res != nil || !strings.Contains(err.Error(), kemonoPreviewsOnly) {
					t.Fatalf("unavailable originals: result=%+v error=%v", res, err)
				}
			} else if err != nil || len(res.Files) != 1 || res.Files[0].Dir != "Original" ||
				res.Files[0].URL != srv.URL+"/data/a/sample.jpg" || !strings.Contains(res.Note, "1 left out: "+kemonoPreviewsOnly) {
				t.Fatalf("mixed availability: result=%+v error=%v", res, err)
			}
			res, err = k.Extract(t.Context(), mustParse(t, page.String()+"/post/1"), Options{})
			if err == nil || res != nil || !strings.Contains(err.Error(), kemonoPreviewsOnly) {
				t.Fatalf("preview-only post: result=%+v error=%v", res, err)
			}
		})
	}
}

func TestFolderNameTrimsRamblingTitles(t *testing.T) {
	if got := folderName("  a tidy title  "); got != "a tidy title" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := folderName(long); len([]rune(got)) != 80 {
		t.Errorf("a %d-rune title survived as %d runes, want 80", len(long), len([]rune(got)))
	}
}
