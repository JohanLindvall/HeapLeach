// SPDX-License-Identifier: MIT

package extractor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

type redgifsListingStep struct {
	items  []redgifsItem
	pages  int
	status int
}

// Only the token and listing endpoints are served. Filtering unavailable
// entries must not require any per-item or media requests.
func redgifsListingServer(t *testing.T, page func(int) redgifsListingStep) (*RedGifs, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		if r.Method == http.MethodGet && r.URL.Path == "/auth/temporary" {
			fmt.Fprint(w, `{"token":"synthetic-token"}`)
			return
		}
		n := int(calls.Add(1))
		wantQuery := fmt.Sprintf("count=%d&order=new&page=%d", config.RedGifsPageSize, n)
		if r.Method != http.MethodGet || r.URL.Path != "/users/synthetic-user/search" || r.URL.Query().Encode() != wantQuery {
			t.Errorf("unexpected request %d: %s %s", n, r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		step := page(n)
		if step.status != 0 {
			w.WriteHeader(step.status)
		}
		_ = json.NewEncoder(w).Encode(struct {
			Gifs  []redgifsItem `json:"gifs"`
			Pages int           `json:"pages"`
		}{step.items, step.pages})
	}))
	t.Cleanup(srv.Close)
	r := NewRedGifs(testClient())
	r.api = srv.URL
	return r, &calls
}

func TestRedGifsUserCountsPlayableFiles(t *testing.T) {
	unavailable := redgifsItem{ID: "Unavailable"}
	first := redgifsItem{ID: "First"}
	first.URLs.HD = "https://media.example.test/First.mp4"
	second := redgifsItem{ID: "Second"}
	second.URLs.SD = "https://media.example.test/Second-mobile.mp4"
	for _, tc := range []struct {
		name  string
		steps []redgifsListingStep
		cap   int
		want  []string
		note  string
	}{
		{"unavailable before playable", []redgifsListingStep{
			{items: []redgifsItem{unavailable, first}, pages: 1},
		}, 1, []string{"First.mp4"}, ""},
		{"unavailable between playable", []redgifsListingStep{
			{items: []redgifsItem{first, unavailable, second}, pages: 1},
		}, 2, []string{"First.mp4", "Second.mp4"}, ""},
		{"same page respects file cap", []redgifsListingStep{
			{items: []redgifsItem{unavailable, first, second}, pages: 1},
		}, 1, []string{"First.mp4"}, fileLimitNote},
		{"leading unusable page", []redgifsListingStep{
			{items: []redgifsItem{unavailable}, pages: 2},
			{items: []redgifsItem{second}, pages: 2},
		}, 1, []string{"Second.mp4"}, ""},
		{"middle unusable page", []redgifsListingStep{
			{items: []redgifsItem{first}, pages: 3},
			{items: []redgifsItem{unavailable}, pages: 3},
			{items: []redgifsItem{second}, pages: 3},
		}, 2, []string{"First.mp4", "Second.mp4"}, ""},
		{"empty page with later pages", []redgifsListingStep{
			{items: []redgifsItem{first}, pages: 3},
			{pages: 3},
			{items: []redgifsItem{second}, pages: 3},
		}, 2, []string{"First.mp4", "Second.mp4"}, ""},
		{"unavailable copy on same page", []redgifsListingStep{
			{items: []redgifsItem{{ID: first.ID}, first}, pages: 1},
		}, 1, []string{"First.mp4"}, ""},
		{"unavailable copy on earlier page", []redgifsListingStep{
			{items: []redgifsItem{{ID: second.ID}}, pages: 2},
			{items: []redgifsItem{second}, pages: 2},
		}, 1, []string{"Second.mp4"}, ""},
		{"terminal page has unusable tail", []redgifsListingStep{
			{items: []redgifsItem{first, unavailable}, pages: 1},
		}, 1, []string{"First.mp4"}, ""},
		{"file cap stops further requests", []redgifsListingStep{
			{items: []redgifsItem{first}, pages: 2},
		}, 1, []string{"First.mp4"}, fileLimitNote},
		{"repeated playable page stops", []redgifsListingStep{
			{items: []redgifsItem{first}, pages: 3},
			{items: []redgifsItem{first, unavailable}, pages: 3},
		}, 2, []string{"First.mp4"}, "partial — repeated or empty page"},
		{"empty page without later pages ends", []redgifsListingStep{
			{items: []redgifsItem{first}},
			{},
		}, 2, []string{"First.mp4"}, ""},
		{"later failure retains files", []redgifsListingStep{
			{items: []redgifsItem{unavailable, first}, pages: 2},
			{status: http.StatusNotFound},
		}, 2, []string{"First.mp4"}, couldNotFetchNote},
		{"all unusable", []redgifsListingStep{
			{items: []redgifsItem{unavailable}, pages: 1},
		}, 1, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, calls := redgifsListingServer(t, func(n int) redgifsListingStep {
				if n > len(tc.steps) {
					t.Errorf("unexpected listing page %d", n)
					return redgifsListingStep{status: http.StatusNotFound}
				}
				return tc.steps[n-1]
			})
			// A per-file feed does not spend the followed-source budget.
			res, err := r.Extract(t.Context(), mustParse(t, "https://clips.example.test/users/synthetic-user"), Options{limits: Limits{Files: tc.cap, Sources: 1}})
			if got := int(calls.Load()); got != len(tc.steps) {
				t.Errorf("listing calls = %d, want %d", got, len(tc.steps))
			}
			if len(tc.want) == 0 {
				if err == nil || !strings.Contains(err.Error(), "no downloadable media") {
					t.Errorf("error = %v, want no downloadable media", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, file := range res.Files {
				names = append(names, file.Name)
				if file.Name == "Second.mp4" && file.URL != second.URLs.SD {
					t.Errorf("SD URL = %q, want %q", file.URL, second.URLs.SD)
				}
			}
			if !slices.Equal(names, tc.want) || res.Note != tc.note {
				t.Errorf("files %v, note %q; want %v, %q", names, res.Note, tc.want, tc.note)
			}
		})
	}
}

func TestRedGifsUserBoundsUnusablePages(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			r, calls := redgifsListingServer(t, func(n int) redgifsListingStep {
				item := redgifsItem{ID: fmt.Sprintf("Entry%d", n)}
				if n == 1 {
					item.URLs.HD = "https://media.example.test/First.mp4"
				}
				items := []redgifsItem{item}
				if n > 1 && empty {
					items = nil
				}
				return redgifsListingStep{items: items, pages: 1000}
			})
			res, err := r.user(t.Context(), "synthetic-user", Options{})
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := 1 + config.MaxSkippedListingPages + 1
			if got := int(calls.Load()); got != wantCalls || len(res.Files) != 1 || res.Note != "partial — repeated or empty page" {
				t.Errorf("calls %d, files %d, note %q; want %d calls, one file and the empty-page stop", got, len(res.Files), res.Note, wantCalls)
			}
		})
	}
}
