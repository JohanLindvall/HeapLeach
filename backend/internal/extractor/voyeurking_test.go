// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Synthetic React Router data, including an unrelated file in the related
// videos. The primary video's field is the only download to hand off.
func voyeurKingVideoHTML(slug, title, file string) string {
	table := []any{
		map[string]int{"_1": 2}, "loaderData", map[string]int{"_3": 4}, "routes/video.$slug",
		map[string]int{"_5": 6, "_7": 8}, "video", map[string]int{"_9": 10, "_11": 12, "_13": 14},
		"relatedVideos", []int{15}, "slug", slug, "title", title, "file", file,
		map[string]int{"_13": 16}, "https://files.example.test/file/recommendation",
	}
	data, _ := json.Marshal(table)
	return voyeurKingStreamHTML(string(data) + "\n")
}

func voyeurKingStreamHTML(chunks ...string) string {
	var out strings.Builder
	for _, chunk := range chunks {
		quoted, _ := json.Marshal(chunk)
		fmt.Fprintf(&out, `<script>window.__reactRouterContext.streamController.enqueue(%s);</script>`, quoted)
	}
	return out.String()
}

func voyeurKingIndexHTML(next string, slugs ...string) string {
	var out strings.Builder
	out.WriteString(`<nav><a href="/video/navigation">Unrelated navigation</a></nav><main><h1>Example Collection</h1>`)
	for _, slug := range slugs {
		fmt.Fprintf(&out, `<a href="/video/%s"><img src="/preview.jpg"></a>
<a href="/video/%s?tracking=title#player">Example video</a>`, slug, slug)
	}
	if next != "" {
		fmt.Fprintf(&out, `<nav><a rel="next" href="%s">Next</a></nav>`, next)
	}
	out.WriteString(`</main>`)
	out.WriteString(voyeurKingStreamHTML(`[{"_1":2},"loaderData",{"_3":4},"routes/collection.$slug",{}]` + "\n"))
	return out.String()
}

func voyeurKingTestIndex(t *testing.T, pages map[string]string, k *keep2ShareService) (*Registry, *VoyeurKing, string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI())
		mu.Unlock()
		if doc, ok := pages[r.URL.RequestURI()]; ok {
			fmt.Fprint(w, doc)
		} else {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := httpx.New("test-agent", "en-US", 0, time.Second)
	reg := &Registry{client: client, fallback: NewDirect(client)}
	v := NewVoyeurKing(client, reg)
	shell := keep2ShareShell(t, k.label)
	for path, doc := range pages {
		pages[path] = strings.ReplaceAll(doc, "https://files.example.test", shell)
	}
	reg.extractors = []Extractor{v, k.ex}
	return reg, v, srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

func TestVoyeurKingListingsHandOffDeferredFiles(t *testing.T) {
	for _, tc := range []struct{ section, host string }{
		{"categories", "Keep2Share"}, {"collection", "Keep2Share"},
		{"categories", "FileBoom"}, {"collection", "FileBoom"},
	} {
		t.Run(tc.section+"/"+tc.host, func(t *testing.T) {
			prefix := "/" + tc.section + "/example"
			var resolved atomic.Int32
			k := keep2ShareProtocolTestSite(t, tc.host, func(w http.ResponseWriter, _ map[string]string) {
				resolved.Add(1)
				fmt.Fprint(w, `{"status":"success","url":"https://storage.example.test/first-clip"}`)
			})
			pages := map[string]string{
				prefix + "?sort=mv":        voyeurKingIndexHTML(prefix+"/page/2?sort=mv", "first", "second"),
				prefix + "/page/2?sort=mv": voyeurKingIndexHTML("", "second", "third"),
			}
			for _, slug := range []string{"first", "second", "third"} {
				pages["/video/"+slug] = voyeurKingVideoHTML(slug, "Example "+slug, "https://files.example.test/file/"+slug)
			}
			// The shared K2S protocol fixture redeems this synthetic file id.
			pages["/video/first"] = voyeurKingVideoHTML("first", "Example first", "https://files.example.test/file/test-file")
			// The page pasted is probed as it stands before the listing is
			// walked from its first page.
			pages[prefix+"/page/7?sort=mv"] = pages[prefix+"?sort=mv"]
			reg, _, base, asked := voyeurKingTestIndex(t, pages, k)
			res, ex, err := reg.Extract(context.Background(), base+prefix+"/page/7?sort=mv#top", Options{})
			if err != nil {
				t.Fatal(err)
			}
			if ex.Name() != "voyeurking" || res.Title != "Example Collection" || res.Note != "" || len(res.Files) != 3 {
				t.Fatalf("unexpected listing result: %+v", res)
			}
			if resolved.Load() != 0 {
				t.Fatal("listing spent a K2S download ticket before dispatch")
			}
			for i, slug := range []string{"first", "second", "third"} {
				file := res.Files[i]
				want := Pace{Streams: 1, Files: 1, Group: config.GroupKeep2Share, PerRoute: true}
				if file.Dir != "Example "+slug || file.Name != "First Clip.mp4" || file.URL != "" || file.Resolve == nil || file.Pace == nil || *file.Pace != want {
					t.Fatalf("K2S handoff lost ordering, metadata or proxy pacing: %+v", file)
				}
			}
			requests := asked()
			for _, slug := range []string{"first", "second", "third"} {
				count := 0
				for _, path := range requests {
					if path == "/video/"+slug {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("duplicate thumbnail/title/page links fetched %s %d times", slug, count)
				}
			}
			// Six: the pasted page is probed once before the walk starts
			// from the first.
			if len(requests) != 6 {
				t.Fatalf("followed something outside the listing: %v", requests)
			}
			// The propagated resolver still obeys the route selected at dispatch.
			k.wait(httpx.WithRoute(context.Background(), "cooling", k.ex.client, nil), time.Hour)
			if _, err := res.Files[0].Resolve(httpx.WithRoute(context.Background(), "cooling", k.ex.client, nil)); !isWait(err) {
				t.Fatalf("lost route cooldown: %v", err)
			}
			if _, err := res.Files[0].Resolve(httpx.WithRoute(context.Background(), "available", k.ex.client, nil)); err != nil {
				t.Fatal(err)
			}
			if resolved.Load() != 1 {
				t.Fatalf("download resolutions = %d", resolved.Load())
			}
		})
	}
}

// The walk itself is the shared walker's; what is the site's own is the
// caps it is held to and which next pages it will follow.
func TestVoyeurKingLimitsAndInvalidNextPages(t *testing.T) {
	for _, section := range []string{"categories", "collection"} {
		t.Run(section, func(t *testing.T) {
			prefix := "/" + section + "/example"
			other := "/categories/example/page/2"
			if section == "categories" {
				other = "/collection/example/page/2"
			}
			for _, tc := range []struct {
				name, next, note string
				limits           Limits
				files, requests  int
			}{
				{name: "source limit", next: prefix + "/page/2", limits: Limits{Sources: 1}, note: "source limit", files: 1, requests: 2},
				{name: "file limit", limits: Limits{Files: 1}, note: "file limit", files: 1, requests: 3},
				{name: "other listing", next: "/" + section + "/unrelated/page/2", note: "invalid next page", files: 2, requests: 3},
				{name: "other listing type", next: other, note: "invalid next page", files: 2, requests: 3},
				{name: "other host", next: "https://unrelated.example.test" + prefix + "/page/2", note: "invalid next page", files: 2, requests: 3},
			} {
				t.Run(tc.name, func(t *testing.T) {
					k := keep2ShareTestSite(t, func(http.ResponseWriter, map[string]string) { t.Error("resolved early") })
					pages := map[string]string{
						prefix:          voyeurKingIndexHTML(tc.next, "first", "second"),
						"/video/first":  voyeurKingVideoHTML("first", "First", "https://files.example.test/file/first"),
						"/video/second": voyeurKingVideoHTML("second", "Second", "https://files.example.test/file/second"),
					}
					reg, _, base, asked := voyeurKingTestIndex(t, pages, k)
					res, _, err := reg.Extract(context.Background(), base+prefix, Options{limits: tc.limits})
					if err != nil {
						t.Fatal(err)
					}
					if len(res.Files) != tc.files || !strings.Contains(res.Note, tc.note) || len(asked()) != tc.requests {
						t.Fatalf("files=%d note=%q requests=%v", len(res.Files), res.Note, asked())
					}
				})
			}
		})
	}
}

func TestVoyeurKingSkipsUnavailableDetailsAndCancels(t *testing.T) {
	k := keep2ShareTestSite(t, func(http.ResponseWriter, map[string]string) { t.Error("resolved early") })
	reg, _, base, _ := voyeurKingTestIndex(t, map[string]string{
		"/categories/example": voyeurKingIndexHTML("", "missing", "available"),
		"/video/available":    voyeurKingVideoHTML("available", "Available", "https://files.example.test/file/available"),
	}, k)
	res, _, err := reg.Extract(context.Background(), base+"/categories/example", Options{})
	if err != nil || len(res.Files) != 1 || res.Note != "1 of 2 videos resolved" {
		t.Fatalf("result=%+v error=%v", res, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := reg.Extract(ctx, base+"/categories/example", Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestVoyeurKingVideoRejectsWrongOrMissingSource(t *testing.T) {
	k := keep2ShareTestSite(t, func(http.ResponseWriter, map[string]string) { t.Error("resolved early") })
	reg, _, base, asked := voyeurKingTestIndex(t, map[string]string{
		"/video/wrong-page": voyeurKingVideoHTML("another-page", "Another page", "https://files.example.test/file/first"),
		"/video/other-host": voyeurKingVideoHTML("other-host", "Other host", "https://other.example.test/file/first"),
		"/video/no-file":    voyeurKingVideoHTML("no-file", "No file", ""),
		"/video/no-data":    `<main><h1>Empty</h1></main>`,
	}, k)
	// Followed for the form of the link, as a listing's sources are, so a
	// page nobody recognises is reported rather than downloaded.
	strict := Options{strict: true}
	for _, path := range []string{"/video/wrong-page", "/video/other-host", "/video/no-file"} {
		if _, ex, err := reg.Extract(context.Background(), base+path, strict); err == nil || ex == nil || ex.Name() != "voyeurking" {
			t.Errorf("%s: extractor %v, error %v; want the video page refused", path, ex, err)
		}
	}
	// Not the site's video or listing pages: nothing here takes them.
	unsupported := []string{"/video/no-data", "/", "/categories", "/categories/example/page/0", "/collection",
		"/collection/example/page/0", "/collection/example/page/invalid", "/collections/example"}
	for _, path := range unsupported {
		if _, ex, err := reg.Extract(context.Background(), base+path, strict); !errors.Is(err, errUnrecognised) || ex.Name() == "voyeurking" {
			t.Errorf("%s: extractor %v, error %v; want it unrecognised", path, ex.Name(), err)
		}
	}
	// One probe a page, which the video pages' own read reuses.
	if got := asked(); len(got) != 3+len(unsupported) {
		t.Fatalf("fetched %v", got)
	}
}

func TestVoyeurKingStreamDecoding(t *testing.T) {
	data := `[{"_1":2},"file","https://files.example.test/file/example?first=1&second=2",{"_4":5},"title","A  title with \"quotes\" and <tags>"]`
	doc := voyeurKingStreamHTML(data[:19], data[19:]+"\n")
	decoded, err := voyeurKingPageData(doc)
	if err != nil || decoded.text(decoded.field(0, "file")) != "https://files.example.test/file/example?first=1&second=2" || decoded.text(decoded.field(3, "title")) != `A  title with "quotes" and <tags>` {
		t.Fatalf("decoded=%s error=%v", decoded, err)
	}
	for _, raw := range []string{`[]`, `[{"_1":999},"file"]`, `[{"_999":0}]`, `[{"_1":-5},"file"]`, `[null]`, `[1]`} {
		d, err := voyeurKingPageData(voyeurKingStreamHTML(raw))
		if err != nil || d.text(d.field(0, "file")) != "" {
			t.Fatalf("invalid reference %s: %s, %v", raw, d, err)
		}
	}
	for _, doc := range []string{`<script>window.__reactRouterContext.streamController.enqueue(broken);</script>`, voyeurKingStreamHTML("[invalid")} {
		if _, err := voyeurKingPageData(doc); err == nil {
			t.Fatal("accepted malformed page data")
		}
	}
}

func TestVoyeurKingRecognise(t *testing.T) {
	v := NewVoyeurKing(nil, nil)
	video := voyeurKingVideoHTML("first", "Example first", "https://files.example.test/file/first")
	listing := voyeurKingIndexHTML("", "first", "second")
	named := `<html><head><title>Example - VoyeurKing</title></head><body><main></main></body></html>`
	expectRecognised(t, v, htmlPage(t, "https://index.example.test/video/first", video), true)
	expectRecognised(t, v, htmlPage(t, "https://index.example.test/categories/example", listing), true)
	expectRecognised(t, v, htmlPage(t, "https://index.example.test/collection/example/page/2", listing), true)
	// The right form and the name, but no router data.
	expectRecognised(t, v, htmlPage(t, "https://index.example.test/video/first", named), false)
	expectRecognised(t, v, htmlPage(t, "https://index.example.test/categories/example", named), false)
	// Router data for the other route, or on a page of another form.
	expectRecognised(t, v, htmlPage(t, "https://index.example.test/video/first", listing), false)
	expectRecognised(t, v, htmlPage(t, "https://index.example.test/dmca", video), false)

	for raw, want := range map[string]bool{
		"https://index.example.test/video/first":               true,
		"https://index.example.test/categories/example":        true,
		"https://index.example.test/collection/example/page/3": true,
		"https://index.example.test/categories":                false,
		"https://index.example.test/collection/example/page/x": false,
		"https://index.example.test/":                          false,
	} {
		expectShape(t, v, raw, want)
	}
}
