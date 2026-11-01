// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

type wikiContinuationStep struct {
	query, body string
	status      int
}

// Each request must echo exactly the latest continuation, in addition to
// the original query. Unlisted requests cannot silently extend the walk.
func wikiContinuationServer(t *testing.T, base url.Values, steps []wikiContinuationStep) (mediaWikiSite, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		if r.Method != http.MethodGet || r.URL.Path != "/api.php" || n >= len(steps) {
			t.Errorf("unexpected request %d: %s %s", n+1, r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		step := steps[n]
		want := maps.Clone(base)
		continuation, err := url.ParseQuery(step.query)
		if err != nil {
			t.Error(err)
		}
		maps.Copy(want, continuation)
		if got := r.URL.Query().Encode(); got != want.Encode() {
			t.Errorf("request %d query = %q, want %q", n+1, got, want.Encode())
		}
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		if step.status != 0 {
			w.WriteHeader(step.status)
		}
		fmt.Fprint(w, step.body)
	}))
	t.Cleanup(srv.Close)
	return mediaWikiSite{api: srv.URL + "/api.php", name: "Example Wiki"}, &calls
}

func TestMediaWikiContinuesEmptyBatches(t *testing.T) {
	for _, subcategories := range []bool{false, true} {
		kind, cursor, field := "files", "gcmcontinue", "pages"
		first, second := mediaWikiFileJSON("File:First.jpg"), mediaWikiFileJSON("File:Second.jpg")
		firstName, secondName := "First.jpg", "Second.jpg"
		query := mediaWikiFileQuery("generator", "categorymembers")
		if subcategories {
			kind, cursor, field = "subcategories", "cmcontinue", "categorymembers"
			first, second = `{"title":"Category:First"}`, `{"title":"Category:Second"}`
			firstName, secondName = "Category:First", "Category:Second"
			query = mediaWikiQuery("list", "categorymembers", "cmtitle", "Category:Example", "cmtype", "subcat", "cmlimit", mediaWikiPageLimit)
		}
		body := func(item, next string) string {
			cont := ""
			if next != "" {
				cont = fmt.Sprintf(`,"continue":{"continue":"-||",%q:%q}`, cursor, next)
			}
			return fmt.Sprintf(`{"batchcomplete":true,"query":{%q:[%s]}%s}`, field, item, cont)
		}
		for _, tc := range []struct {
			name  string
			steps []wikiContinuationStep
			want  []string
		}{
			{"leading", []wikiContinuationStep{
				{body: body("", "one")},
				{query: cursor + "=one&continue=-||", body: body(first, "")},
			}, []string{firstName}},
			{"middle without query payload", []wikiContinuationStep{
				{body: body(first, "one")},
				{query: cursor + "=one&continue=-||", body: fmt.Sprintf(`{"batchcomplete":true,"continue":{"continue":"-||",%q:"two"}}`, cursor)},
				{query: cursor + "=two&continue=-||", body: body(second, "")},
			}, []string{firstName, secondName}},
			{"terminal", []wikiContinuationStep{
				{body: body(first, "one")},
				{query: cursor + "=one&continue=-||", body: `{"batchcomplete":true}`},
			}, []string{firstName}},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				site, calls := wikiContinuationServer(t, query, tc.steps)
				before := query.Encode()
				m := testMediaWiki(t)
				t.Cleanup(m.client.CloseIdleConnections)
				var names []string
				var note string
				var err error
				if subcategories {
					names, note, err = m.subcategories(t.Context(), site, "Category:Example", Options{})
				} else {
					var files []File
					files, note, err = m.list(t.Context(), site, query, 10, nil)
					for _, file := range files {
						names = append(names, file.Name)
					}
				}
				if err != nil || note != "" || !slices.Equal(names, tc.want) || int(calls.Load()) != len(tc.steps) {
					t.Fatalf("names=%v note=%q err=%v calls=%d; want %v after %d calls", names, note, err, calls.Load(), tc.want, len(tc.steps))
				}
				if query.Encode() != before {
					t.Error("the caller's query was changed")
				}
			})
		}
	}
}

func TestMediaWikiContinuationBounds(t *testing.T) {
	first, second := mediaWikiFileJSON("File:First.jpg"), mediaWikiFileJSON("File:Second.jpg")
	body := func(item, next string) string {
		return fmt.Sprintf(`{"query":{"pages":[%s]},"continue":{"gcmcontinue":%q}}`, item, next)
	}
	emptyRun := []wikiContinuationStep{{body: body(first, "0")}}
	for i := range config.MaxSkippedListingPages + 1 {
		emptyRun = append(emptyRun, wikiContinuationStep{query: fmt.Sprintf("gcmcontinue=%d", i), body: body("", fmt.Sprint(i+1))})
	}
	for _, tc := range []struct {
		name         string
		steps        []wikiContinuationStep
		limit, pages int
		want         []string
		note         string
	}{
		{"repeated cursor", []wikiContinuationStep{{body: body(first, "one")}, {query: "gcmcontinue=one", body: body(second, "one")}},
			10, 0, []string{"First.jpg", "Second.jpg"}, "partial — repeated continuation"},
		{"cursor cycle across an empty batch", []wikiContinuationStep{
			{body: body(first, "one")}, {query: "gcmcontinue=one", body: body("", "two")}, {query: "gcmcontinue=two", body: body(second, "one")},
		}, 10, 0, []string{"First.jpg", "Second.jpg"}, "partial — repeated continuation"},
		{"consecutive empty bound", emptyRun, 10, 0, []string{"First.jpg"}, "partial — repeated or empty page"},
		{"page cap", []wikiContinuationStep{{body: body("", "one")}, {query: "gcmcontinue=one", body: body("", "two")}},
			10, 2, nil, "partial — page limit"},
		{"file cap before empty batch", []wikiContinuationStep{{body: body(first, "one")}}, 1, 0, []string{"First.jpg"}, fileLimitNote},
		{"file cap after empty batch", []wikiContinuationStep{
			{body: body(first, "one")}, {query: "gcmcontinue=one", body: body("", "two")}, {query: "gcmcontinue=two", body: body(second, "three")},
		}, 2, 0, []string{"First.jpg", "Second.jpg"}, fileLimitNote},
		{"duplicate entries still stop", []wikiContinuationStep{{body: body(first, "one")}, {query: "gcmcontinue=one", body: body(first, "two")}},
			10, 0, []string{"First.jpg"}, "partial — repeated or empty page"},
		{"failure after empty batch", []wikiContinuationStep{
			{body: body(first, "one")}, {query: "gcmcontinue=one", body: body("", "two")}, {query: "gcmcontinue=two", status: http.StatusNotFound},
		}, 10, 0, []string{"First.jpg"}, couldNotFetchNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := mediaWikiFileQuery("generator", "categorymembers")
			site, calls := wikiContinuationServer(t, query, tc.steps)
			m := testMediaWiki(t)
			t.Cleanup(m.client.CloseIdleConnections)
			var files []File
			var note string
			var err error
			if tc.pages == 0 {
				files, note, err = m.list(t.Context(), site, query, tc.limit, nil)
			} else {
				_, note, err = mediaWikiWalk(t.Context(), m, site, query, pageWalk[mediaWikiPage]{
					key: func(p mediaWikiPage) string { return p.Title }, limit: tc.limit, pages: tc.pages,
				}, func(resp *mediaWikiResponse) []mediaWikiPage { return resp.Query.Pages })
			}
			var names []string
			for _, f := range files {
				names = append(names, f.Name)
			}
			if err != nil || !slices.Equal(names, tc.want) || note != tc.note || int(calls.Load()) != len(tc.steps) {
				t.Fatalf("names=%v note=%q err=%v calls=%d; want %v, %q, %d calls", names, note, err, calls.Load(), tc.want, tc.note, len(tc.steps))
			}
		})
	}
}

// A continuation is the whole set of module tokens: the generator may stay
// fixed while imageinfo advances, then drop its completed property token.
func TestMediaWikiCompoundContinuation(t *testing.T) {
	query := mediaWikiFileQuery("generator", "categorymembers")
	site, calls := wikiContinuationServer(t, query, []wikiContinuationStep{
		{body: `{"continue":{"gcmcontinue":"one","iicontinue":"a","continue":"||"}}`},
		{query: "gcmcontinue=one&iicontinue=a&continue=||", body: `{"continue":{"gcmcontinue":"one","iicontinue":"b","continue":"||"}}`},
		{query: "gcmcontinue=one&iicontinue=b&continue=||", body: `{"batchcomplete":true,"continue":{"gcmcontinue":"two","continue":"-||"}}`},
		{query: "gcmcontinue=two&continue=-||", body: `{"batchcomplete":true,"query":{"pages":[` + mediaWikiFileJSON("File:Later.jpg") + `]}}`},
	})
	m := testMediaWiki(t)
	t.Cleanup(m.client.CloseIdleConnections)
	files, note, err := m.list(t.Context(), site, query, 10, nil)
	if err != nil || len(files) != 1 || files[0].Name != "Later.jpg" || note != "" || calls.Load() != 4 {
		t.Fatalf("files=%+v note=%q err=%v calls=%d", files, note, err, calls.Load())
	}
	if strings.Contains(query.Encode(), "continue") {
		t.Error("continuation leaked into the caller's query")
	}
}
