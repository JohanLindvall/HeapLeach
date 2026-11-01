// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	entries, note, err := i.listingEntries(t.Context(), nil, repeat.URL, "/r/things", Options{limits: Limits{Sources: 5}})
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
	entries, note, err = i.listingEntries(t.Context(), nil, overlap.URL, "/t/cats", Options{limits: Limits{Sources: 10}})
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
	entries, note, err := i.listingEntries(t.Context(), nil, srv.URL, "/r/things", Options{limits: Limits{Files: 100000}})
	if err != nil || len(entries) != config.MaxListingPosts || note != "partial — listing limit" {
		t.Errorf("high file limit: rows=%d note=%q err=%v, want %d and the listing limit", len(entries), note, err, config.MaxListingPosts)
	}
	entries, note, err = i.listingEntries(t.Context(), nil, srv.URL, "/r/things", Options{limits: Limits{Files: 250}})
	if err != nil || len(entries) != 250 || note != "partial — file limit" {
		t.Errorf("low file limit: rows=%d note=%q err=%v, want 250 and the file limit", len(entries), note, err)
	}
}
