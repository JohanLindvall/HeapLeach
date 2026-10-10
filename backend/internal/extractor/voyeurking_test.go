// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	return out.String()
}

func voyeurKingTestIndex(t *testing.T, pages map[string]string, k *Keep2Share) (*Registry, *VoyeurKing, string, func() []string) {
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
	reg := &Registry{fallback: NewDirect(nil)}
	v := NewVoyeurKing(client, reg)
	base, _ := url.Parse(srv.URL)
	v.hostSet = hostSet{base.Hostname()}
	k.hostSet = hostSet{"files.example.test"}
	reg.extractors = []Extractor{v, k}
	return reg, v, srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

func TestVoyeurKingCategoryHandsOffDeferredK2SFiles(t *testing.T) {
	var resolved atomic.Int32
	k := keep2ShareTestSite(t, func(w http.ResponseWriter, _ map[string]string) {
		resolved.Add(1)
		fmt.Fprint(w, `{"status":"success","url":"https://storage.example.test/first-clip"}`)
	})
	pages := map[string]string{
		"/categories/example?sort=mv":        voyeurKingIndexHTML("/categories/example/page/2?sort=mv", "first", "second"),
		"/categories/example/page/2?sort=mv": voyeurKingIndexHTML("", "second", "third"),
	}
	for _, slug := range []string{"first", "second", "third"} {
		pages["/video/"+slug] = voyeurKingVideoHTML(slug, "Example "+slug, "https://files.example.test/file/"+slug)
	}
	// The shared K2S protocol fixture redeems this synthetic file id.
	pages["/video/first"] = voyeurKingVideoHTML("first", "Example first", "https://files.example.test/file/test-file")
	reg, _, base, asked := voyeurKingTestIndex(t, pages, k)
	res, ex, err := reg.Extract(context.Background(), base+"/categories/example/page/7?sort=mv#top", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Name() != "voyeurking" || res.Title != "Example Collection" || res.Note != "" || len(res.Files) != 3 {
		t.Fatalf("unexpected category result: %+v", res)
	}
	if resolved.Load() != 0 {
		t.Fatal("listing spent a K2S download ticket before dispatch")
	}
	for i, slug := range []string{"first", "second", "third"} {
		file := res.Files[i]
		if file.Dir != "Example "+slug || file.Name != "First Clip.mp4" || file.URL != "" || file.Resolve == nil || file.Pace == nil || *file.Pace != keep2SharePace {
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
	if len(requests) != 5 {
		t.Fatalf("followed something outside the category: %v", requests)
	}
	// The propagated resolver still obeys the route selected at dispatch.
	k.wait(httpx.WithRoute(context.Background(), "cooling", k.client, nil), time.Hour)
	if _, err := res.Files[0].Resolve(httpx.WithRoute(context.Background(), "cooling", k.client, nil)); !isWait(err) {
		t.Fatalf("lost route cooldown: %v", err)
	}
	if _, err := res.Files[0].Resolve(httpx.WithRoute(context.Background(), "available", k.client, nil)); err != nil {
		t.Fatal(err)
	}
	if resolved.Load() != 1 {
		t.Fatalf("download resolutions = %d", resolved.Load())
	}
}

func TestVoyeurKingLimitsAndPartialListings(t *testing.T) {
	for _, tc := range []struct {
		name, next, note string
		limits           Limits
		secondPage       bool
		files, requests  int
	}{
		{name: "source limit", next: "/categories/example/page/2", limits: Limits{Sources: 1}, note: "source limit", files: 1, requests: 2},
		{name: "file limit", limits: Limits{Files: 1}, note: "file limit", files: 1, requests: 3},
		{name: "next page failed", next: "/categories/example/page/2", note: "could not fetch next page", files: 2, requests: 4},
		{name: "repeated page", next: "/categories/example/page/2", secondPage: true, note: "repeated", files: 2, requests: 4},
		{name: "other category", next: "/categories/unrelated/page/2", note: "invalid next page", files: 2, requests: 3},
		{name: "other host", next: "https://unrelated.example.test/categories/example/page/2", note: "invalid next page", files: 2, requests: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := keep2ShareTestSite(t, func(http.ResponseWriter, map[string]string) { t.Error("resolved early") })
			pages := map[string]string{
				"/categories/example": voyeurKingIndexHTML(tc.next, "first", "second"),
				"/video/first":        voyeurKingVideoHTML("first", "First", "https://files.example.test/file/first"),
				"/video/second":       voyeurKingVideoHTML("second", "Second", "https://files.example.test/file/second"),
			}
			if tc.secondPage {
				pages["/categories/example/page/2"] = voyeurKingIndexHTML("/categories/example/page/2", "first", "second")
			}
			reg, _, base, asked := voyeurKingTestIndex(t, pages, k)
			res, _, err := reg.Extract(context.Background(), base+"/categories/example", Options{limits: tc.limits})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != tc.files || !strings.Contains(res.Note, tc.note) || len(asked()) != tc.requests {
				t.Fatalf("files=%d note=%q requests=%v", len(res.Files), res.Note, asked())
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
	for _, path := range []string{"/video/wrong-page", "/video/other-host", "/video/no-file", "/video/no-data", "/", "/categories", "/categories/example/page/0"} {
		if _, _, err := reg.Extract(context.Background(), base+path, Options{}); err == nil {
			t.Errorf("accepted %s", path)
		}
	}
	if len(asked()) != 4 {
		t.Fatalf("fetched unsupported listing paths: %v", asked())
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
