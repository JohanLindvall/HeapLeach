// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Each instance is a fresh, unregistered origin. Detection must reach the
// existing extractor, retaining original files, headers and transfer pacing.
func TestPlatformDetectionAtUnknownOrigins(t *testing.T) {
	for _, tc := range []struct {
		name, page, api, body, file string
		count                       int
	}{
		{"pixeldrain file", "/u/SampleID", "/api/file/SampleID/info",
			`{"id":"SampleID","name":"sample.bin","size":12,"mime_type":"application/octet-stream"}`, "/api/file/SampleID?download", 1},
		{"pixeldrain list", "/l/SampleList", "/api/list/SampleList",
			`{"success":true,"title":"Samples","files":[{"id":"SampleID","name":"sample.bin","size":12,"mime_type":"application/octet-stream"}]}`, "/api/file/SampleID?download", 1},
		{"pixeldrain filesystem", "/d/Shared", "/api/filesystem/Shared",
			`{"base_index":0,"path":[{"type":"dir","name":"Shared"}],"children":[{"type":"file","name":"sample.bin","file_size":12,"file_type":"application/octet-stream"}]}`, "/api/filesystem/Shared/sample.bin?attach", 1},
		{"danbooru", "/posts/42", "/posts.json",
			`[{"id":42,"file_url":"/original/sample.jpg","file_size":12}]`, "/original/sample.jpg", 1},
		{"e621", "/posts/42", "/posts.json",
			`{"posts":[{"id":42,"file":{"url":"/original/sample.jpg","size":12}}]}`, "/original/sample.jpg", 1},
		{"moebooru", "/post/show/42", "/post.json",
			`[{"id":42,"file_url":"/original/sample.jpg"}]`, "/original/sample.jpg", 1},
		{"gelbooru", "/index.php?page=post&s=view&id=42", "/index.php",
			`{"post":[{"id":42,"directory":7,"image":"sample.jpg"}]}`, "/images/7/sample.jpg", 1},
		{"philomena", "/images/42", "/api/v1/json/search/images",
			`{"images":[{"id":42,"representations":{"full":"/original/sample.jpg"}}]}`, "/original/sample.jpg", 1},
		{"twibooru", "/posts/42", "/api/v3/search/posts",
			`{"posts":[{"id":42,"representations":{"full":"/original/sample.jpg"}}]}`, "/original/sample.jpg", 1},
		{"szurubooru", "/post/42", "/api/posts/",
			`{"results":[{"id":42,"contentUrl":"original/sample.jpg","fileSize":12}]}`, "/original/sample.jpg", 1},
		{"foolfuuka thread", "/b/thread/9998/", foolFuukaThreadAPI,
			foolFuukaThreadJSON, "https://media.example.test/b/image/1600/00/1600000000001.png", 4},
		{"foolfuuka post", "/b/post/42/", foolFuukaPostAPI,
			`{"num":"42","media":{"media":"sample.jpg","media_filename":"sample.jpg","media_link":"https://media.example.test/sample.jpg","media_size":"12","media_status":"normal"}}`, "https://media.example.test/sample.jpg", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.api {
					if r.URL.Path == strings.Split(tc.page, "?")[0] {
						w.Header().Set(httpx.HeaderContentType, "text/html")
						fmt.Fprint(w, `<title>Platform page</title><a href="/wiki/File:Ignored.bin">Advertisement</a>`)
						return
					}
					http.NotFound(w, r)
					return
				}
				if tc.name == "szurubooru" && !strings.Contains(r.Header.Get(httpx.HeaderAccept), "application/json") {
					t.Error("szurubooru API requires JSON Accept")
				}
				w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			client := httpx.New("test-agent", "en-US", 0, time.Second)
			res, _, err := NewRegistry(&config.Config{}, client).Extract(context.Background(), srv.URL+tc.page, Options{})
			if err != nil || res == nil {
				t.Fatalf("extract: %v, %v", res, err)
			}
			if len(res.Files) != tc.count {
				t.Fatalf("files = %d, want %d", len(res.Files), tc.count)
			}
			want := tc.file
			if strings.HasPrefix(want, "/") {
				want = srv.URL + want
			}
			if res.Files[0].URL != want {
				t.Errorf("file = %q, want %q", res.Files[0].URL, want)
			}
			if strings.HasPrefix(tc.name, "pixeldrain") && (res.Files[0].Pace == nil || res.Files[0].Pace.Streams != 1) {
				t.Error("lost Pixeldrain connection limit")
			}
			if res.Files[0].Headers[httpx.HeaderReferer] != srv.URL+"/" {
				t.Errorf("lost instance referer: %v", res.Files[0].Headers)
			}
			if tc.name == "foolfuuka thread" && res.Files[len(res.Files)-1].Resolve == nil {
				t.Error("lost deferred remote attachment resolver")
			}
		})
	}
}

func TestPlatformFoolFuukaBoardAndSearch(t *testing.T) {
	for _, route := range []string{"/b/", "/b/search/text/sample/"} {
		t.Run(route, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/_/api/chan/") {
					http.NotFound(w, r)
					return
				}
				w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
				if r.URL.Path == foolFuukaSearchAPI && r.URL.Query().Get("page") != "1" {
					fmt.Fprint(w, `{"error":"No results found."}`)
					return
				}
				fmt.Fprint(w, foolFuukaThreadJSON)
			}))
			defer srv.Close()
			res, err := NewDirect(httpx.New("test", "en", 0, time.Second)).Extract(context.Background(), mustParse(t, srv.URL+route), Options{})
			if err != nil || res == nil || len(res.Files) != 4 {
				t.Fatalf("archive = %v, %v", res, err)
			}
		})
	}
}

func TestPlatformHTMLIdentification(t *testing.T) {
	for _, tc := range []struct{ name, page, body string }{
		{"legacy gelbooru", "/index.php?page=post&s=view&id=42", strings.Replace(gelbooru01PostPage, "<head>", `<head><meta name="generator" content="Gelbooru 0.1">`, 1)},
		{"custom wiki", "/An_article", `<meta name="generator" content="MediaWiki 1.43"><link rel="EditURI" href="/custom/api.php?action=rsd">`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/custom/api.php" {
					w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
					switch {
					case r.URL.Query().Get("meta") == "siteinfo":
						fmt.Fprint(w, mediaWikiSiteInfo)
					case r.URL.Query().Get("generator") == "images":
						fmt.Fprint(w, mediaWikiArticleImages)
					default:
						fmt.Fprint(w, `{"query":{"pages":[{"ns":0,"title":"An article"}]}}`)
					}
					return
				}
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			client := httpx.New("test", "en", 0, time.Second)
			res, err := NewDirect(client).pageSniff(context.Background(), mustParse(t, srv.URL+tc.page), Options{})
			if err != nil || res == nil || len(res.Files) == 0 || strings.HasPrefix(res.Files[0].URL, srv.URL) {
				t.Fatalf("HTML platform = %v, %v", res, err)
			}
		})
	}
}

func TestPlatformKVSFailedPlayerDoesNotFollowRelatedVideos(t *testing.T) {
	doc := `<script>var flashvars = {license_code: '123456789'};</script>
<div id="list_videos_related"><a class="item" href="/videos/42/related/">Related</a></div>`
	res, err := platformPage(context.Background(), nil, mustParse(t, "https://example.test/watch/sample"), mustParseDoc(t, doc), doc, Options{})
	if err == nil || res != nil {
		t.Fatalf("broken identified player fell through: %v, %v", res, err)
	}
}

func TestPlatformDetectionDoesNotBroadenKnownLinks(t *testing.T) {
	reg := NewRegistry(&config.Config{}, httpx.New("test", "en", 0, time.Second))
	for _, path := range []string{"/posts/42", "/u/sample", "/b/thread/42/", "/wiki/Article", "/members/42/videos/", "/sample/user/creator"} {
		ex, known := reg.Known(mustParse(t, "https://example.test"+path))
		// /u/name was already claimed by the fediverse path matcher.
		if wantKnown := path == "/u/sample"; known != wantKnown || (known && ex.Name() != "fediverse") {
			t.Errorf("ordinary navigation %s newly claimed by %s before inspecting content", path, ex.Name())
		}
	}
	for _, host := range []string{"pixeldrain.com", "desuarchive.org", "thisvid.com", "kemono.cr", "e621.net"} {
		if _, known := reg.Known(mustParse(t, "https://"+host)); !known {
			t.Errorf("lost registered host %s for embedded-link discovery", host)
		}
	}
}

func TestPlatformKemonoCreatorAndPost(t *testing.T) {
	for _, route := range []string{"/sample/user/creator", "/sample/user/creator/post/42"} {
		t.Run(route, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/api/") {
					http.NotFound(w, r)
					return
				}
				if r.Header.Get(httpx.HeaderAccept) != kemonoScrapeAccept {
					t.Errorf("wrong API Accept: %q", r.Header.Get(httpx.HeaderAccept))
				}
				w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
				post := `{"id":"42","user":"creator","service":"sample","title":"Sample Post","file":{"name":"sample.bin","path":"/aa/sample.bin"},"attachments":[]}`
				switch r.URL.Path {
				case "/api/v1/sample/user/creator/profile":
					fmt.Fprint(w, `{"id":"creator","service":"sample","name":"A Creator"}`)
				case "/api/v1/sample/user/creator/posts":
					fmt.Fprintf(w, "[%s]", post)
				case "/api/v1/sample/user/creator/post/42":
					fmt.Fprintf(w, `{"post":%s}`, post)
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			res, err := NewDirect(httpx.New("test", "en", 0, time.Second)).Extract(context.Background(), mustParse(t, srv.URL+route), Options{})
			if err != nil || res == nil || len(res.Files) != 1 {
				t.Fatalf("extract = %v, %v", res, err)
			}
			if res.Files[0].URL != srv.URL+"/data/aa/sample.bin" {
				t.Fatalf("attachment = %v", res.Files[0])
			}
			if route == "/sample/user/creator" && res.Files[0].Dir != "Sample Post" {
				t.Errorf("lost creator post folder: %q", res.Files[0].Dir)
			}
		})
	}
}

func TestPlatformMediaWikiArticle(t *testing.T) {
	srv, _ := mediaWikiServer(t)
	res, err := NewDirect(httpx.New("test", "en", 0, time.Second)).Extract(context.Background(), mustParse(t, srv.URL+"/wiki/Synthetic_article"), Options{})
	if err != nil || res == nil || len(res.Files) == 0 || strings.HasPrefix(res.Files[0].URL, srv.URL) {
		t.Fatalf("article was not resolved through the wiki API: %v, %v", res, err)
	}
}

func TestPlatformKVSListings(t *testing.T) {
	for _, route := range []string{"/categories/sample/", "/members/42/videos/", "/search/sample/"} {
		t.Run(route, func(t *testing.T) {
			var listingCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				switch r.URL.Path {
				case route:
					listingCalls.Add(1)
					// The same first page is used for the walk. An unrelated
					// download link must not replace the platform's listing.
					fmt.Fprint(w, `<h1>Sample Videos</h1><h2>Sample's Videos</h2>
<a href="/wiki/File:Ignored.bin">Advertisement</a>
<div id="list_videos_common"><a data-action="ajax" data-block-id="list_videos_common" data-parameters="sort_by:post_date">Sort</a>
<a class="item" href="/video/first/">First</a><a class="item" href="/video/second/">Second</a></div>`)
				case "/video/first/", "/video/second/":
					fmt.Fprint(w, kvsPage())
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := httpx.New("test", "en", 0, time.Second)
			d := NewDirect(client)
			d.registry = NewRegistry(&config.Config{}, client)
			res, err := d.pageSniff(context.Background(), mustParse(t, srv.URL+route), Options{limits: Limits{Files: 1}})
			if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != kvsHighDirect {
				t.Fatalf("listing = %v, %v", res, err)
			}
			if listingCalls.Load() != 1 {
				t.Errorf("listing fetched %d times", listingCalls.Load())
			}
		})
	}
}

func TestPlatformUnrelatedPagesFallThrough(t *testing.T) {
	for _, route := range []string{"/u/SampleID", "/l/SampleID", "/d/SampleID", "/posts/42", "/b/thread/42/", "/sample/user/creator", "/wiki/Article", "/index.php?title=Article"} {
		t.Run(route, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "api") || strings.HasSuffix(r.URL.Path, ".json") {
					w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
					// Plausible generic metadata is not a platform signature.
					fmt.Fprint(w, `{"id":"SampleID","name":"sample.bin","posts":[{"id":42,"url":"/sample.jpg"}]}`)
					return
				}
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprint(w, `<title>Ordinary page</title><div id="list_videos"><a href="/unrelated/">Browse</a></div>`)
			}))
			defer srv.Close()
			u := mustParse(t, srv.URL+route)
			res, err := NewDirect(httpx.New("test", "en", 0, time.Second)).Extract(context.Background(), u, Options{})
			if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != u.String() {
				t.Fatalf("false positive: %v, %v", res, err)
			}
		})
	}
}

func TestPlatformIdentifiedFailureDoesNotSavePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		if strings.HasSuffix(r.URL.Path, "/profile") {
			fmt.Fprint(w, `{"id":"creator","service":"sample","name":"A Creator"}`)
		} else {
			fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()
	res, err := NewDirect(httpx.New("test", "en", 0, time.Second)).Extract(context.Background(), mustParse(t, srv.URL+"/sample/user/creator"), Options{})
	if err == nil || res != nil {
		t.Fatalf("empty identified creator should fail, got %v, %v", res, err)
	}
}

func TestPlatformProbeIsBoundedAndDoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name              string
		status            int
		contentType, body string
	}{
		{"rate limit", http.StatusTooManyRequests, httpx.ContentTypeJSON, `{}`},
		{"HTML shell", http.StatusOK, "text/html", `{"id":42}`},
		{"too large", http.StatusOK, httpx.ContentTypeJSON, `{"data":"` + strings.Repeat("x", config.MaxResponseBytes) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set(httpx.HeaderContentType, tc.contentType)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			var out any
			if platformJSON(context.Background(), httpx.New("test", "en", 3, time.Second), srv.URL, nil, &out) {
				t.Fatal("probe accepted an invalid response")
			}
			if calls.Load() != 1 {
				t.Errorf("probe retried %d times", calls.Load())
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := NewDirect(httpx.New("test", "en", 0, time.Second)).Extract(ctx, mustParse(t, "https://example.test/posts/42"), Options{})
	if res != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled detection = %v, %v", res, err)
	}
}
