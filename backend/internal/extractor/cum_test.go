// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func cumTestAttachment(hash, kind, name string) cumAttachment {
	return cumAttachment{SHA256: strings.Repeat(hash, 64), Kind: kind, OriginalFilename: name}
}

func cumTestOriginal(att cumAttachment, variant string, size int64) cumAttachment {
	// Include a smaller rendition first to catch a thumbnail or preview
	// being mistaken for the requested original.
	data, _ := json.Marshal(map[string]any{
		"sha256": att.SHA256, "kind": att.Kind, "originalFilename": att.OriginalFilename,
		"bytes":    999999,
		"variants": []map[string]any{{"name": "720p.mp4", "bytes": 12}, {"name": variant, "bytes": size}},
	})
	_ = json.Unmarshal(data, &att)
	return att
}

func cumTestPage(w http.ResponseWriter, r *http.Request) {
	edges, _ := json.Marshal("http://" + r.Host + "/edge-one,http://" + r.Host + "/edge-two")
	fmt.Fprintf(w, `<img src="/avatar.jpg"><img src="/thumbnail.webp"><script>var env={EDGE_BASE_URLS:%s};</script>`, edges)
}

func cumTestProfile(w http.ResponseWriter) {
	fmt.Fprint(w, `{"id":"42","service":"patreon","name":"example-creator","displayName":"Example & Friends"}`)
}

func cumTestList(w http.ResponseWriter, total int, posts ...cumPost) {
	if posts == nil {
		posts = []cumPost{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"total": total, "posts": posts})
}

func TestCumCreatorPaginationAndSharedPostResolution(t *testing.T) {
	first := cumTestAttachment("a", "image", "First Frame.jpeg")
	second := cumTestAttachment("b", "video", "Second Clip.mov")
	third := cumTestAttachment("c", "image", "original.png")
	var offsets []string
	var fullPosts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/creators/patreon/42":
			if r.URL.Query().Has("pppage") {
				t.Error("did not start at the first creator page")
			}
			cumTestPage(w, r)
		case "/api/v1/patreon/user/42/profile":
			cumTestProfile(w)
		case "/api/v1/patreon/user/42/posts":
			query := r.URL.Query()
			for key, want := range map[string]string{"q": "a & b", "sort": "oldest", "exact": "1", "min_duration": "10", "max_duration": "20", "tag": "demo", "compact": "1"} {
				if query.Get(key) != want {
					t.Errorf("lost %s=%q: %s", key, want, r.URL)
				}
			}
			offsets = append(offsets, query.Get("o"))
			switch query.Get("o") {
			case "0":
				cumTestList(w, 3,
					cumPost{ID: "101", Service: "patreon", Title: "Repeated / Title", Attachments: []cumAttachment{first, second, first}},
					cumPost{ID: "102", Service: "patreon", Title: "Text only"})
			case "2":
				cumTestList(w, 3, cumPost{ID: "103", Service: "patreon", Title: "Repeated / Title", Attachments: []cumAttachment{third}})
			default:
				t.Errorf("advanced by the requested page size instead of the returned count: %s", r.URL)
				http.NotFound(w, r)
			}
		case "/api/v1/patreon/user/42/post/101":
			fullPosts.Add(1)
			if !strings.HasSuffix(r.Referer(), "/creators/patreon/42/post/101") {
				t.Errorf("wrong post referer: %q", r.Referer())
			}
			_ = json.NewEncoder(w).Encode(cumPost{ID: "101", Service: "patreon", Attachments: []cumAttachment{
				cumTestOriginal(first, "original.jpg", 12345), cumTestOriginal(second, "original.mp4", 67890),
			}})
		case "/api/v1/patreon/user/42/post/103":
			fullPosts.Add(1)
			_ = json.NewEncoder(w).Encode(cumPost{ID: "103", Service: "patreon", Attachments: []cumAttachment{cumTestOriginal(third, "original.png", 100)}})
		default:
			t.Errorf("unexpected media, thumbnail or page request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	u := mustParse(t, srv.URL+"/creators/patreon/42?pppage=8&ppq=a+%26+b&ppsort=oldest&ppexact=1&ppmin_duration=10&ppmax_duration=20&pptag=demo#grid")
	res, err := NewCum(httpx.New("test", "en", 0, time.Second)).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Example & Friends" || res.Note != "" || len(res.Files) != 3 || fullPosts.Load() != 0 || !reflect.DeepEqual(offsets, []string{"0", "2"}) {
		t.Fatalf("result=%+v offsets=%v full posts=%d", res, offsets, fullPosts.Load())
	}
	if u.Query().Get("pppage") != "8" || u.Fragment != "grid" {
		t.Fatal("changed the caller's URL")
	}
	for i, file := range res.Files {
		if file.URL != "" || file.Size != -1 || file.Resolve == nil || file.External != "" || file.SizeApprox {
			t.Fatalf("file %d did not defer original metadata: %+v", i, file)
		}
	}
	if res.Files[0].Dir != "Repeated _ Title [101]" || res.Files[2].Dir != "Repeated _ Title [103]" {
		t.Fatal("repeated post titles collided")
	}
	targets := make([]*Target, 2)
	var wg sync.WaitGroup
	for i := range targets {
		wg.Go(func() {
			var err error
			targets[i], err = res.Files[i].Resolve(context.Background())
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for i, target := range targets {
		if target == nil || !strings.HasPrefix(target.URL, srv.URL+"/edge-one/media/") || strings.Contains(target.URL, "720p") || target.Headers[httpx.HeaderReferer] != srv.URL+"/creators/patreon/42/post/101" {
			t.Fatalf("target %d = %+v", i, target)
		}
	}
	if targets[0].Name != "First Frame.jpg" || targets[0].Size != 12345 || targets[1].Name != "Second Clip.mp4" || targets[1].Size != 67890 || fullPosts.Load() != 1 {
		t.Fatalf("wrong original metadata: %+v, %+v, requests=%d", targets[0], targets[1], fullPosts.Load())
	}
	target, err := res.Files[0].Resolve(context.Background())
	if err != nil || !strings.HasPrefix(target.URL, srv.URL+"/edge-two/") || fullPosts.Load() != 1 {
		t.Fatalf("mirror retry = %+v, %v, requests=%d", target, err, fullPosts.Load())
	}
	target, err = res.Files[2].Resolve(context.Background())
	if err != nil || target.Name != third.SHA256+".png" || target.Size != 100 || fullPosts.Load() != 2 {
		t.Fatalf("later post = %+v, %v", target, err)
	}
}

func TestCumCreatorCapsFiltersAndPartialPages(t *testing.T) {
	for _, tc := range []struct {
		name, filter, later, note string
		limits                    Limits
		files, pages              int
	}{
		{name: "file cap", limits: Limits{Files: 1}, files: 1, pages: 1, note: "file limit"},
		{name: "file boundary", limits: Limits{Files: 3}, files: 3, pages: 1, note: "file limit"},
		{name: "source cap", limits: Limits{Sources: 1}, files: 2, pages: 1, note: "source limit"},
		{name: "source boundary", limits: Limits{Sources: 2}, files: 3, pages: 1, note: "source limit"},
		{name: "videos", filter: "videos", files: 2, pages: 2},
		{name: "photos", filter: "photos", files: 2, pages: 2},
		{name: "all", files: 4, pages: 2},
		{name: "exact file cap", limits: Limits{Files: 4}, files: 4, pages: 2},
		{name: "repeat", later: "repeat", files: 3, pages: 2, note: "partial"},
		{name: "empty", later: "empty", files: 3, pages: 2, note: "repeated or empty"},
		{name: "failed", later: "fail", files: 3, pages: 2, note: "could not read"},
		{name: "malformed", later: "malformed", files: 3, pages: 2, note: "could not read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := cumPost{ID: "101", Service: "patreon", Title: "First", Attachments: []cumAttachment{cumTestAttachment("a", "image", "First.jpg"), cumTestAttachment("b", "video", "Second.mp4")}}
			second := cumPost{ID: "102", Service: "patreon", Title: "Second", Attachments: []cumAttachment{cumTestAttachment("c", "gif", "Third.gif")}}
			var pages atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/creators/patreon/42":
					cumTestPage(w, r)
				case "/api/v1/patreon/user/42/profile":
					cumTestProfile(w)
				case "/api/v1/patreon/user/42/posts":
					pages.Add(1)
					if r.URL.Query().Get("type") != tc.filter {
						t.Error("lost the media filter")
					}
					if pages.Load() > 2 {
						t.Error("pagination did not stop")
						http.NotFound(w, r)
						return
					}
					if r.URL.Query().Get("o") == "0" {
						cumTestList(w, 3, first, second)
						return
					}
					switch tc.later {
					case "repeat":
						cumTestList(w, 3, first, second)
					case "empty":
						cumTestList(w, 3)
					case "fail":
						http.Error(w, "unavailable", http.StatusInternalServerError)
					case "malformed":
						fmt.Fprint(w, `{"error":"unavailable"}`)
					default:
						cumTestList(w, 3, cumPost{ID: "103", Service: "patreon", Title: "Third", Attachments: []cumAttachment{cumTestAttachment("d", "video", "Fourth.mp4")}})
					}
				default:
					t.Errorf("unexpected request during discovery: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			res, err := NewCum(httpx.New("test", "en", 0, time.Second)).Extract(context.Background(), mustParse(t, srv.URL+"/creators/patreon/42?pptype="+tc.filter), Options{limits: tc.limits})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != tc.files || pages.Load() != int32(tc.pages) || !strings.Contains(res.Note, tc.note) || (tc.note == "" && res.Note != "") || res.Title != "Example & Friends" {
				t.Fatalf("result=%+v pages=%d", res, pages.Load())
			}
		})
	}
}

func TestCumSinglePostUsesOriginalsAndRetriesFailedMetadata(t *testing.T) {
	attachment := cumTestOriginal(cumTestAttachment("a", "audio", "First Song.mp3"), "original.mp3", 4321)
	post := cumPost{ID: "101", Service: "patreon", Title: "First Post", Attachments: []cumAttachment{attachment, attachment}}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/creators/patreon/42/post/101":
			cumTestPage(w, r)
		case "/api/v1/patreon/user/42/post/101":
			if requests.Add(1) == 1 {
				http.Error(w, "try again", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(post)
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
	}))
	defer srv.Close()
	c := NewCum(httpx.New("test", "en", 0, time.Second))
	loader := &cumPostLoader{c: c, base: srv.URL + "/api/v1/patreon/user/42", service: "patreon", id: "101", referer: srv.URL + "/creators/patreon/42/post/101", edges: []string{srv.URL + "/edge"}}
	if _, err := loader.resolve(context.Background(), attachment.SHA256); err == nil {
		t.Fatal("failed metadata fetch was accepted")
	}
	target, err := loader.resolve(context.Background(), attachment.SHA256)
	if err != nil || target.Size != 4321 || requests.Load() != 2 {
		t.Fatalf("metadata retry = %+v, %v; requests=%d", target, err, requests.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loader.resolve(ctx, attachment.SHA256); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached resolution ignored cancellation: %v", err)
	}
	res, err := c.Extract(context.Background(), mustParse(t, loader.referer), Options{})
	if err != nil || res.Title != "First Post" || len(res.Files) != 1 || res.Files[0].Dir != "" || res.Files[0].Size != 4321 || res.Files[0].Name != "First Song.mp3" {
		t.Fatalf("single post = %+v, %v", res, err)
	}
	if _, err := loader.resolve(context.Background(), strings.Repeat("b", 64)); err == nil {
		t.Fatal("missing attachment silently became another file")
	}
}

func TestCumRejectsInvalidOriginalsAndServerConfiguration(t *testing.T) {
	for _, att := range []cumAttachment{
		cumTestAttachment("a", "video", "Clip.mp4"),
		cumTestOriginal(cumAttachment{SHA256: "../bad"}, "original.jpg", 100),
		cumTestOriginal(cumTestAttachment("a", "image", "Frame.jpg"), "original../file.jpg", 100),
		cumTestOriginal(cumTestAttachment("a", "image", "Frame.jpg"), "original.", 100),
	} {
		if _, err := att.file([]string{"https://cdn.example.test"}, "https://example.test/"); err == nil {
			t.Error("accepted an invalid original or a preview-only attachment")
		}
	}
	for _, doc := range []string{
		`<h1>Removed</h1>`,
		`<script>var env={EDGE_BASE_URLS:"javascript:bad,https://user:pass@example.test"}</script>`,
		`<script>var env={EDGE_BASE_URLS:"https://example.test/?signature=temporary"}</script>`,
		`<p>EDGE_BASE_URLS:"https://example.test"</p>`,
	} {
		if _, err := cumEdges(doc); err == nil {
			t.Errorf("accepted invalid media configuration: %s", doc)
		}
	}
	edges, err := cumEdges(`<script>var env={EDGE_BASE_URLS:"https:\/\/first.example.test/, https://second.example.test,https://first.example.test"}</script>`)
	if err != nil || !reflect.DeepEqual(edges, []string{"https://first.example.test", "https://second.example.test"}) {
		t.Fatalf("media configuration = %v, %v", edges, err)
	}
}

func TestCumRejectsUnsupportedPages(t *testing.T) {
	for _, route := range []string{"/", "/creators", "/creators/random", "/creators/patreon/42/comments", "/creators/patreon/42/post", "/creators/patreon/42?tab=dms", "/creators/patreon/42?tab=similar", "/creators/patreon/42?tab=tags"} {
		if _, err := NewCum(nil).Extract(context.Background(), mustParse(t, "https://example.test"+route), Options{}); err == nil {
			t.Errorf("accepted %s", route)
		}
	}
}

func TestCumRecognise(t *testing.T) {
	c := NewCum(nil)
	page := `<html><head><title>Creator "Someone" on a site | OnlyHaven</title></head><body>` +
		`<script>var env={EDGE_BASE_URLS:"https://edge.example.test"};</script></body></html>`
	expectRecognised(t, c, htmlPage(t, "https://fans.example.test/creators/patreon/42", page), true)
	expectRecognised(t, c, htmlPage(t, "https://fans.example.test/creators/patreon/42/post/7", page), true)
	// The right form and the name, but no media servers advertised.
	expectRecognised(t, c, htmlPage(t, "https://fans.example.test/creators/patreon/42",
		`<html><head><title>Creator | OnlyHaven</title></head><body></body></html>`), false)
	// Advertised media servers on a page of another form.
	expectRecognised(t, c, htmlPage(t, "https://fans.example.test/creators/patreon/42/comments", page), false)
	for raw, want := range map[string]bool{
		"https://fans.example.test/creators/patreon/42":        true,
		"https://fans.example.test/creators/patreon/42/post/7": true,
		"https://fans.example.test/creators/patreon":           false,
		"https://fans.example.test/creators":                   false,
		"https://fans.example.test/patreon/user/42":            false,
	} {
		expectShape(t, c, raw, want)
	}
}
