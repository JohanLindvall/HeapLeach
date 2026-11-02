// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// A post row counts against the post cap once, however many pages repeat
// it: a subreddit serving its first page forever ends quietly below the cap,
// and a feed whose pages overlap still collects as many distinct posts as
// the cap allows.
func TestImgurPostCapCountsDistinctPosts(t *testing.T) {
	repeat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"hash":"a","is_album":true},{"hash":"b","is_album":true},{"hash":"c","is_album":true}]}`))
	}))
	defer repeat.Close()
	i := NewImgur(testClient())
	entries, note, err := i.listingEntries(t.Context(), nil, repeat.URL, "/r/things", Options{limits: Limits{Sources: 5}}, nil)
	if err != nil || len(entries) != 3 || note != "" {
		t.Errorf("repeated first page: rows=%d note=%q err=%v, want 3 rows and no note", len(entries), note, err)
	}

	// Each page repeats the previous page's last two rows.
	overlap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/t/cats/page/"), "/hit.json"))
		var rows []string
		for k := range 4 {
			rows = append(rows, fmt.Sprintf(`{"hash":"p%d","is_album":true}`, n*2+k))
		}
		_, _ = w.Write([]byte(`{"data":[` + strings.Join(rows, ",") + `]}`))
	}))
	defer overlap.Close()
	entries, note, err = i.listingEntries(t.Context(), nil, overlap.URL, "/t/cats", Options{limits: Limits{Sources: 10}}, nil)
	if err != nil || len(entries) != 10 || note != "partial — source limit" {
		t.Errorf("overlapping pages: rows=%d note=%q err=%v, want 10 distinct posts", len(entries), note, err)
	}
}

// At the default source limit, post rows are held to config.MaxListingPosts,
// the ceiling these listings always had; a source limit the user sets below
// its default replaces it, and one raised above it leaves it be.
func TestImgurPostCapDefault(t *testing.T) {
	for _, tc := range []struct {
		sources int
		want    int
		note    string
	}{
		{0, config.MaxListingPosts, "partial — listing limit"},
		{7, 7, "partial — source limit"},
		{config.MaxExpandedSources, config.MaxListingPosts, "partial — listing limit"},
		{config.MaxExpandedSources * 2, config.MaxListingPosts, "partial — listing limit"},
	} {
		got, note := imgurPostCap(Options{limits: Limits{Sources: tc.sources}})
		if got != tc.want || note != tc.note {
			t.Errorf("sources %d: post cap = %d %q, want %d %q", tc.sources, got, note, tc.want, tc.note)
		}
	}
}

// Rows that are the media themselves are held to config.MaxListingPosts as
// well, the ceiling the listing always had, when the file limit is higher;
// a lower file limit cuts them first.
func TestImgurMediaRowsCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/r/things/page/"), "/hit.json"))
		var rows []string
		for k := range 100 {
			rows = append(rows, fmt.Sprintf(`{"hash":"m%d","ext":".jpg"}`, n*100+k))
		}
		_, _ = w.Write([]byte(`{"data":[` + strings.Join(rows, ",") + `]}`))
	}))
	defer srv.Close()
	i := NewImgur(testClient())
	i.root = srv.URL
	res, err := i.listing(t.Context(), nil, imgurTarget{Listing: "/r/things"}, Options{limits: Limits{Files: 100000}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != config.MaxListingPosts || res.Note != "partial — listing limit" {
		t.Errorf("high file limit: files=%d note=%q, want %d and the listing limit", len(res.Files), res.Note, config.MaxListingPosts)
	}
	res, err = i.listing(t.Context(), nil, imgurTarget{Listing: "/r/things"}, Options{limits: Limits{Files: 250}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 250 || res.Note != "partial — file limit" {
		t.Errorf("low file limit: files=%d note=%q, want 250 and the file limit", len(res.Files), res.Note)
	}
}

// Empty and failed posts spend the inspection budget, not the file budget.
// A healthy row later on the same page or the next page still fills the job,
// and no later child or listing page is fetched once it does.
func TestImgurListingFileBudgetSkipsUnavailablePosts(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		for _, paged := range []bool{false, true} {
			t.Run(fmt.Sprintf("status_%d/paged_%t", status, paged), func(t *testing.T) {
				var pages, posts, unexpected atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					healthy := `{"hash":"healthy01","ext":".jpg","size":12},{"hash":"unread01","is_album":true}`
					switch r.URL.Path {
					case "/user/invented/submitted/page/0/hit.json":
						pages.Add(1)
						rows := `{"hash":"empty01","is_album":true}`
						if !paged {
							rows += "," + healthy
						}
						_, _ = fmt.Fprintf(w, `{"data":[%s]}`, rows)
					case "/user/invented/submitted/page/1/hit.json":
						pages.Add(1)
						_, _ = fmt.Fprintf(w, `{"data":[%s]}`, healthy)
					case "/gallery/empty01":
						posts.Add(1)
						w.Header().Set("Content-Type", "text/html")
						w.WriteHeader(status)
						_, _ = fmt.Fprintf(w, `<script>window.postDataJSON=%q</script>`, `{"id":"empty01","title":"Empty Album","media":[]}`)
					default:
						unexpected.Add(1)
						http.NotFound(w, r)
					}
				}))
				defer srv.Close()
				i := NewImgur(testClient())
				i.root = srv.URL
				reg := claimedRegistry(i)
				reg.limits = Limits{Files: 1}
				res, ex, err := reg.Extract(t.Context(), srv.URL+"/user/invented", Options{})
				if err != nil {
					t.Fatal(err)
				}
				if ex != i || len(res.Files) != 1 || res.Files[0].Name != "healthy01.jpg" {
					t.Fatalf("extractor=%v result=%+v, want the later image", ex, res)
				}
				if res.Note != "partial — file limit; 1 left out: could not be read" {
					t.Errorf("note=%q, want the file limit and unreadable post", res.Note)
				}
				wantPages := int32(1)
				if paged {
					wantPages++
				}
				if pages.Load() != wantPages || posts.Load() != 1 || unexpected.Load() != 0 {
					t.Errorf("requests: pages=%d posts=%d unexpected=%d, want %d, 1, 0", pages.Load(), posts.Load(), unexpected.Load(), wantPages)
				}
			})
		}
	}
}

// One album can fill the file budget by itself. Its surplus files are
// trimmed and neither another post nor another listing page is requested.
func TestImgurListingStopsWhenAlbumFillsFileBudget(t *testing.T) {
	var pages, posts, unexpected atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/invented/submitted/page/0/hit.json":
			pages.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"hash":"album01","is_album":true},{"hash":"unread01","is_album":true}]}`))
		case "/gallery/album01":
			posts.Add(1)
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<script>window.postDataJSON=%q</script>`, `{"id":"album01","title":"Invented Album","media":[{"id":"first01","url":"https://media.example.test/first.jpg","type":"image"},{"id":"second01","url":"https://media.example.test/second.jpg","type":"image"}]}`)
		default:
			unexpected.Add(1)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	i := NewImgur(testClient())
	i.root = srv.URL
	reg := claimedRegistry(i)
	reg.limits = Limits{Files: 1}
	res, _, err := reg.Extract(t.Context(), srv.URL+"/user/invented", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "first.jpg" || res.Note != "partial — file limit" {
		t.Errorf("result=%+v, want only the album's first file and a file-limit note", res)
	}
	if pages.Load() != 1 || posts.Load() != 1 || unexpected.Load() != 0 {
		t.Errorf("requests: pages=%d posts=%d unexpected=%d, want 1, 1, 0", pages.Load(), posts.Load(), unexpected.Load())
	}
}
