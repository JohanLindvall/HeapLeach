// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func celebFixture(component, props string) string {
	return `<div id="app" data-page="` + html.EscapeString(`{"component":"`+component+`","props":`+props+`}`) + `">
		<img src="https://cdn.example.test/avatar.jpg">
		<img src="https://cdn.example.test/thumbnail.jpg">
	</div>`
}

const celebGalleryProps = `{"creator":{"name":"A & B - Studio","slug":"a-creator-42","imageCount":4,
	"avatar":"https://cdn.example.test/avatar.jpg"},"mediaType":"media","pages":3,"currentPage":1,
	"media":[
		{"id":101,"src":"ARgbBhZXVlwGCAhHCRcXCB0VFksYAxoYQBcVBFYFVEMPBA0IE0oLEAEWGEgDHAhJEQISFgtRCQcJSQ5YXw==",
		 "thumbnail":"https://cdn.example.test/api/v1/thumbnail/first.jpg"},
		{"id":102,"src":"https://cdn.example.test/api/v1/image/second.png"}
	]}`

func TestCelebGalleryPagesThroughAPI(t *testing.T) {
	for _, tc := range []struct {
		name     string
		limit    int
		wantIDs  []string
		requests []string
		note     string
	}{
		{"whole", 0, []string{"101.jpg", "102.png", "103.webp", "104.jpg"}, []string{"first", "2", "3"}, ""},
		{"cap within page", 1, []string{"101.jpg"}, []string{"first"}, "partial — file limit; 1 of 4 images"},
		{"cap on boundary", 2, []string{"101.jpg", "102.png"}, []string{"first"}, "partial — file limit; 2 of 4 images"},
		{"cap on later page", 3, []string{"101.jpg", "102.png", "103.webp"}, []string{"first", "2"}, "partial — file limit; 3 of 4 images"},
		{"exact cap", 4, []string{"101.jpg", "102.png", "103.webp", "104.jpg"}, []string{"first", "2", "3"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("sort") != "popular" {
					t.Errorf("request lost the sort: %s", r.URL)
				}
				switch r.URL.Path {
				case "/creator/a-creator-42/images":
					if r.URL.Query().Has("page") {
						t.Errorf("gallery did not start at page one: %s", r.URL)
					}
					requests = append(requests, "first")
					fmt.Fprint(w, celebFixture("Creator/Media", celebGalleryProps))
				case "/api/v1/creator/a-creator-42/media":
					if r.URL.Query().Get("mediaType") != "media" || !strings.Contains(r.Referer(), "/images?sort=popular") {
						t.Errorf("API request lost its image tab or referer: %s, %s", r.URL, r.Referer())
					}
					page := r.URL.Query().Get("page")
					requests = append(requests, page)
					switch page {
					case "2":
						// A page can overlap its predecessor as new images arrive.
						fmt.Fprint(w, `[{"id":102,"src":"https://cdn.example.test/api/v1/image/second.png"},
							{"id":103,"src":"https://cdn.example.test/api/v1/image/third.webp"}]`)
					case "3":
						fmt.Fprint(w, `[{"id":104,"src":"https://cdn.example.test/api/v1/image/fourth.jpg"}]`)
					default:
						t.Errorf("unexpected page: %s", r.URL)
						http.NotFound(w, r)
					}
				default:
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c := NewCeleb(testClient())
			u := mustParse(t, srv.URL+"/creator/a-creator-42/images?page=8&sort=popular#gallery")
			res, err := c.Extract(context.Background(), u, Options{limits: Limits{Files: tc.limit}})
			if err != nil {
				t.Fatal(err)
			}
			if res.Title != "A & B - Studio" || res.Note != tc.note {
				t.Errorf("title = %q, note = %q", res.Title, res.Note)
			}
			var names []string
			for _, file := range res.Files {
				names = append(names, file.Name)
				if !strings.Contains(file.URL, "/api/v1/image/") || file.Size != -1 || file.Resolve != nil {
					t.Errorf("not a direct full-size image: %+v", file)
				}
				if file.Headers[httpx.HeaderReferer] != srv.URL+"/creator/a-creator-42/images?sort=popular" {
					t.Errorf("file referer = %q", file.Headers)
				}
			}
			if !slices.Equal(names, tc.wantIDs) || !slices.Equal(requests, tc.requests) {
				t.Errorf("files = %v, requests = %v; want %v and %v", names, requests, tc.wantIDs, tc.requests)
			}
			if !strings.HasSuffix(res.Files[0].URL, "?token=one&x=2") {
				t.Errorf("URL query was changed: %s", res.Files[0].URL)
			}
			if u.Query().Get("page") != "8" || u.Fragment != "gallery" {
				t.Error("mutated the caller's URL")
			}
		})
	}
}

func TestCelebSingleImageIgnoresRecommendations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/creator/a-creator-42/images/102" && r.URL.Path != "/creator/a-creator-42/images/103" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		fmt.Fprint(w, celebFixture("Creator/MediaObject", `{"creator":{"name":"A Creator"},
			"mediaId":102,"mediaUrl":"ARgbBhZXVlwGCAhHCRcXCB0VFksYAxoYQBcVBFYFVEMPBA0IE0oeHBAKAgJHHAER",
			"moreMedia":[{"id":103,"src":"https://cdn.example.test/api/v1/image/third.jpg"}],"nextId":103}`))
	}))
	defer srv.Close()
	u := mustParse(t, srv.URL+"/creator/a-creator-42/images/102")
	res, err := NewCeleb(testClient()).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "A Creator" || len(res.Files) != 1 || res.Files[0].Name != "102.png" {
		t.Fatalf("single image = %+v", res)
	}
	// A removed image must not quietly become the gallery or a neighbour.
	u.Path = "/creator/a-creator-42/images/103"
	if _, err := NewCeleb(testClient()).Extract(context.Background(), u, Options{}); err == nil {
		t.Error("accepted a different image from the requested one")
	}
}

func TestCelebRepeatedPageStopsAndAdmitsPartialGallery(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if r.URL.Path != "/creator/a-creator-42/images" {
				t.Errorf("bare creator was not sent to its image gallery: %s", r.URL)
			}
			fmt.Fprint(w, celebFixture("Creator/Media", celebGalleryProps))
		} else {
			fmt.Fprint(w, `[{"id":101,"src":"https://cdn.example.test/first.jpg"},
				{"id":102,"src":"https://cdn.example.test/second.png"}]`)
		}
	}))
	defer srv.Close()
	res := extractOK(t, NewCeleb(testClient()), srv.URL+"/creator/a-creator-42", Options{})
	if requests != 2 || len(res.Files) != 2 || res.Note != "partial — repeated or empty page; 2 of 4 images" {
		t.Errorf("requests = %d, files = %d, note = %q", requests, len(res.Files), res.Note)
	}
}

func TestCelebBadPagesFailInsteadOfReturningACompleteGallery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first string
		next  string
	}{
		{"missing props", `<html>Gone</html>`, ""},
		{"invalid props", `<div data-page="{broken}"></div>`, ""},
		{"wrong tab", celebFixture("Creator/Media", strings.Replace(celebGalleryProps, `"mediaType":"media"`, `"mediaType":"videos"`, 1)), ""},
		{"empty gallery", celebFixture("Creator/Media", `{"mediaType":"media","currentPage":1,"pages":1,"media":[]}`), ""},
		{"invalid image", celebFixture("Creator/Media", strings.Replace(celebGalleryProps, "https://cdn.example.test/api/v1/image/second.png", "broken", 1)), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/creator/") {
					fmt.Fprint(w, tc.first)
				} else if tc.next != "" {
					fmt.Fprint(w, tc.next)
				} else {
					http.Error(w, "unavailable", http.StatusForbidden)
				}
			}))
			defer srv.Close()
			u := mustParse(t, srv.URL+"/creator/a-creator-42/images")
			if _, err := NewCeleb(testClient()).Extract(context.Background(), u, Options{}); err == nil {
				t.Fatal("accepted a missing, malformed or incomplete gallery")
			}
		})
	}
}

// A later page that fails ends the gallery with what was read, admitted.
func TestCelebLaterPageFailureAdmitsPartialGallery(t *testing.T) {
	for _, next := range []string{`{"error":"unavailable"}`, ""} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/creator/") {
				fmt.Fprint(w, celebFixture("Creator/Media", celebGalleryProps))
			} else if next != "" {
				fmt.Fprint(w, next)
			} else {
				http.Error(w, "unavailable", http.StatusForbidden)
			}
		}))
		res := extractOK(t, NewCeleb(testClient()), srv.URL+"/creator/a-creator-42/images", Options{})
		srv.Close()
		if len(res.Files) != 2 || res.Note != "partial — could not fetch next page; 2 of 4 images" {
			t.Errorf("files = %d, note = %q", len(res.Files), res.Note)
		}
	}
}

func TestCelebCancellationPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/creator/") {
			fmt.Fprint(w, celebFixture("Creator/Media", celebGalleryProps))
		} else {
			cancel()
			<-r.Context().Done()
		}
	}))
	defer srv.Close()
	u := mustParse(t, srv.URL+"/creator/a-creator-42/images")
	_, err := NewCeleb(testClient()).Extract(ctx, u, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestCelebPageLimit(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			fmt.Fprint(w, celebFixture("Creator/Media", fmt.Sprintf(`{"creator":{"name":"A Creator","imageCount":%d},
				"mediaType":"media","currentPage":1,"pages":%d,
				"media":[{"id":1,"src":"https://cdn.example.test/first.jpg"}]}`,
				config.MaxAlbumPages+1, config.MaxAlbumPages+1)))
		} else {
			if r.URL.Query().Get("page") != fmt.Sprint(requests) {
				t.Errorf("unexpected page: %s", r.URL)
			}
			fmt.Fprintf(w, `[{"id":%d,"src":"https://cdn.example.test/image-%d.jpg"}]`, requests, requests)
		}
	}))
	defer srv.Close()
	res := extractOK(t, NewCeleb(testClient()), srv.URL+"/creator/a-creator-42/images", Options{})
	if requests != config.MaxAlbumPages || len(res.Files) != config.MaxAlbumPages ||
		res.Note != fmt.Sprintf("partial — page limit; %d of %d images", config.MaxAlbumPages, config.MaxAlbumPages+1) {
		t.Errorf("requests = %d, files = %d, note = %q", requests, len(res.Files), res.Note)
	}
}

func TestCelebRejectsUnsupportedPaths(t *testing.T) {
	for _, path := range []string{"/", "/search", "/creator", "/creator/a-creator/videos", "/creator/a-creator/images/no-id", "/creator/a-creator/images/0", "/creator/a-creator/images/1/extra"} {
		u := mustParse(t, "https://example.test"+path)
		if _, err := NewCeleb(nil).Extract(context.Background(), u, Options{}); err == nil {
			t.Errorf("accepted %s", path)
		}
	}
}

func TestCelebImageURL(t *testing.T) {
	const full = "https://cdn.example.test/api/v1/image/first.jpg?token=one&x=2"
	const encoded = "ARgbBhZXVlwGCAhHCRcXCB0VFksYAxoYQBcVBFYFVEMPBA0IE0oLEAEWGEgDHAhJEQISFgtRCQcJSQ5YXw=="
	for _, raw := range []string{full, encoded, strings.TrimRight(encoded, "=")} {
		link, err := celebImageURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if link.String() != full {
			t.Errorf("decoded URL = %q, want %q", link, full)
		}
	}
	for _, raw := range []string{"", "%%%", "bm90IGEgdXJs", "https:///no-host.jpg", "https://user:password@example.test/image.jpg"} {
		if _, err := celebImageURL(raw); err == nil {
			t.Errorf("accepted invalid URL %q", raw)
		}
	}
}

func TestCelebRecognise(t *testing.T) {
	c := NewCeleb(nil)
	inertia := func(component string) string {
		data := html.EscapeString(`{"component":"` + component + `","props":{"creator":{"name":"A Creator","slug":"a-creator-1"}}}`)
		return `<html><head><title>A Creator - Celeb.st</title></head><body><div id="app" data-page="` + data + `"></div></body></html>`
	}
	gallery := "https://gallery.example.test/creator/a-creator-1/images"
	expectRecognised(t, c, htmlPage(t, gallery, inertia("Creator/Media")), true)
	expectRecognised(t, c, htmlPage(t, gallery+"/42", inertia("Creator/MediaObject")), true)
	// A creator's own page answers the app's not-found component; the app's
	// data-page under the site's name is what is checked there.
	expectRecognised(t, c, htmlPage(t, "https://gallery.example.test/creator/a-creator-1", inertia("404")), true)
	// The right form and the name, but not the gallery's props.
	expectRecognised(t, c, htmlPage(t, gallery, inertia("Feed/Hot")), false)
	expectRecognised(t, c, htmlPage(t, gallery, `<html><head><title>A Creator - Celeb.st</title></head></html>`), false)
	// The gallery's props on a page of another form.
	expectRecognised(t, c, htmlPage(t, "https://gallery.example.test/feed/hot", inertia("Creator/Media")), false)

	expectShape(t, c, gallery, true)
	expectShape(t, c, gallery+"/42", true)
	expectShape(t, c, "https://gallery.example.test/creator/a-creator-1", true)
	expectShape(t, c, "https://gallery.example.test/creator/a-creator-1/videos", false)
	expectShape(t, c, "https://gallery.example.test/feed/hot", false)
}

// An unchanged gallery is read once: the extractor reuses the probe the
// registry recognised it by, props and all. A creator page canonicalises to
// its gallery, which is another page and is read on its own.
func TestCelebReusesTheProbedGallery(t *testing.T) {
	var hits sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := hits.LoadOrStore(r.URL.Path, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		w.Header().Set(httpx.HeaderContentType, "text/html")
		switch r.URL.Path {
		case "/creator/a-creator-42/images":
			fmt.Fprint(w, celebFixture("Creator/Media", `{"creator":{"name":"A Creator","slug":"a-creator-42","imageCount":1},
				"mediaType":"media","pages":1,"currentPage":1,
				"media":[{"id":102,"src":"https://cdn.example.test/api/v1/image/second.png"}]}`))
		case "/creator/a-creator-42":
			fmt.Fprint(w, `<html><head><title>A Creator - Celeb.st</title></head><body>`+celebFixture("404", `{}`)+`</body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	count := func(path string) int32 {
		n, ok := hits.Load(path)
		if !ok {
			return 0
		}
		return n.(*atomic.Int32).Load()
	}
	client := testClient()
	reg := &Registry{client: client, extractors: []Extractor{NewCeleb(client)}, fallback: NewDirect(client)}
	res, ex, err := reg.Extract(context.Background(), srv.URL+"/creator/a-creator-42/images", Options{})
	if err != nil || ex.Name() != "celeb" || len(res.Files) != 1 {
		t.Fatalf("gallery: %v via %v, %v", res, ex, err)
	}
	if got := count("/creator/a-creator-42/images"); got != 1 {
		t.Errorf("unchanged gallery fetched %d times, want 1", got)
	}
	res, _, err = reg.Extract(context.Background(), srv.URL+"/creator/a-creator-42", Options{})
	if err != nil || len(res.Files) != 1 {
		t.Fatalf("creator: %v, %v", res, err)
	}
	if count("/creator/a-creator-42") != 1 || count("/creator/a-creator-42/images") != 2 {
		t.Errorf("creator page read %d times, its gallery %d in all", count("/creator/a-creator-42"), count("/creator/a-creator-42/images"))
	}
}
