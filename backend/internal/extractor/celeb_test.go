// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

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
		{"cap within page", 1, []string{"101.jpg"}, []string{"first"}, "1 of 4 images"},
		{"cap on boundary", 2, []string{"101.jpg", "102.png"}, []string{"first"}, "2 of 4 images"},
		{"cap on later page", 3, []string{"101.jpg", "102.png", "103.webp"}, []string{"first", "2"}, "3 of 4 images"},
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
			c := NewCeleb(httpx.New("test", "en-US", 0, 5*time.Second))
			u, _ := url.Parse(srv.URL + "/creator/a-creator-42/images?page=8&sort=popular#gallery")
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
	u, _ := url.Parse(srv.URL + "/creator/a-creator-42/images/102")
	res, err := NewCeleb(httpx.New("test", "en-US", 0, 5*time.Second)).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "A Creator" || len(res.Files) != 1 || res.Files[0].Name != "102.png" {
		t.Fatalf("single image = %+v", res)
	}
	// A removed image must not quietly become the gallery or a neighbour.
	u.Path = "/creator/a-creator-42/images/103"
	if _, err := NewCeleb(httpx.New("test", "en-US", 0, 5*time.Second)).Extract(context.Background(), u, Options{}); err == nil {
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
	u, _ := url.Parse(srv.URL + "/creator/a-creator-42")
	res, err := NewCeleb(httpx.New("test", "en-US", 0, 5*time.Second)).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(res.Files) != 2 || res.Note != "2 of 4 images" {
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
		{"malformed API", celebFixture("Creator/Media", celebGalleryProps), `{"error":"unavailable"}`},
		{"HTTP failure", celebFixture("Creator/Media", celebGalleryProps), ""},
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
			u, _ := url.Parse(srv.URL + "/creator/a-creator-42/images")
			if _, err := NewCeleb(httpx.New("test", "en-US", 0, 5*time.Second)).Extract(context.Background(), u, Options{}); err == nil {
				t.Fatal("accepted a missing, malformed or incomplete gallery")
			}
		})
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
	u, _ := url.Parse(srv.URL + "/creator/a-creator-42/images")
	_, err := NewCeleb(httpx.New("test", "en-US", 0, 5*time.Second)).Extract(ctx, u, Options{})
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
	u, _ := url.Parse(srv.URL + "/creator/a-creator-42/images")
	res, err := NewCeleb(httpx.New("test", "en-US", 0, 5*time.Second)).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if requests != config.MaxAlbumPages || len(res.Files) != config.MaxAlbumPages ||
		res.Note != fmt.Sprintf("%d of %d images", config.MaxAlbumPages, config.MaxAlbumPages+1) {
		t.Errorf("requests = %d, files = %d, note = %q", requests, len(res.Files), res.Note)
	}
}

func TestCelebRejectsUnsupportedPaths(t *testing.T) {
	for _, path := range []string{"/", "/search", "/creator", "/creator/a-creator/videos", "/creator/a-creator/images/no-id", "/creator/a-creator/images/0", "/creator/a-creator/images/1/extra"} {
		u, _ := url.Parse("https://example.test" + path)
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
