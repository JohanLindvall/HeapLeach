// SPDX-License-Identifier: MIT

package extractor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	// A post's revision is read as the post, and a creator's tabs as the creator.
	expectShape(t, k, "https://archive.example.test/sample/user/creator/post/42/revision/7", true)
	expectShape(t, k, "https://archive.example.test/sample/user/creator/community", true)
	expectShape(t, k, "https://archive.example.test/sample/user/creator/post/42/other/7", false)
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

// A creator's posts are paged by offset, a page size at a time, to a short
// page, an empty one, or the 400 the API answers an offset past the end
// with, which is no failure. The profile's post count is not the end: it
// may lag what the API serves, and a stated count above what was listed is
// noted.
func TestKemonoCreatorPagesToTheEnd(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		// served is how many posts the API holds.
		served int
		asked  string
		note   string
	}{
		{name: "short last page", served: kemonoPageSize + 1, asked: "0,50"},
		{name: "count lags the posts", count: kemonoPageSize, served: kemonoPageSize + 1, asked: "0,50"},
		{name: "full last page", served: 2 * kemonoPageSize, asked: "0,50,100"},
		{name: "count above the posts", count: 3 * kemonoPageSize, served: kemonoPageSize + 1, asked: "0,50",
			note: "partial — the profile states 150 posts; the API listed 51"},
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
					from, _ := strconv.Atoi(o)
					if from >= tc.served {
						http.Error(w, `{"error":"Offset is bigger than total count."}`, http.StatusBadRequest)
						return
					}
					var posts []kemonoPost
					for i := from; i < min(from+kemonoPageSize, tc.served); i++ {
						posts = append(posts, kemonoPost{ID: strconv.Itoa(i), File: kemonoAttachment{Path: fmt.Sprintf("/a/%d.jpg", i)}})
					}
					_ = json.NewEncoder(w).Encode(posts)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			res := extractOK(t, NewKemono(httpx.New("test", "en", 0, time.Second)), srv.URL+"/sample/user/creator", Options{})
			if len(res.Files) != tc.served || res.Title != "A Creator" || res.Note != tc.note || strings.Join(asked, ",") != tc.asked {
				t.Fatalf("files=%d title=%q note=%q asked=%v", len(res.Files), res.Title, res.Note, asked)
			}
			if got := filePaths(res.Files)[0]; got != "0/0.jpg" {
				t.Fatalf("first file %s", got)
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
						// A full page of posts whose originals are not archived.
						previews := make([]string, kemonoPageSize)
						for i := range previews {
							previews[i] = fmt.Sprintf(`{"id":"p%d","has_full":false,"file":{"path":"/a/sample.jpg"}}`, i)
						}
						fmt.Fprint(w, "["+strings.Join(previews, ",")+"]")
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
			if strings.Join(offsets, ",") != "0,50" {
				t.Fatalf("preview metadata consumed the file cap: %v", offsets)
			}
			if strings.Contains(flag, "false") {
				if err == nil || res != nil || !strings.Contains(err.Error(), kemonoPreviewsOnly) {
					t.Fatalf("unavailable originals: result=%+v error=%v", res, err)
				}
			} else if err != nil || len(res.Files) != 1 || res.Files[0].Dir != "Original" ||
				res.Files[0].URL != srv.URL+"/data/a/sample.jpg" || !strings.Contains(res.Note, "50 left out: "+kemonoPreviewsOnly) {
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
