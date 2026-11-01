// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

type pornpicsBudgetFixture struct {
	first     []string
	pages     [][]string
	galleries map[string]string
	failEnd   bool
	onGallery func(string)
}

func pornpicsBudgetServer(t *testing.T, fixture pornpicsBudgetFixture) (*httptest.Server, *pornpicsSite) {
	t.Helper()
	site := &pornpicsSite{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site.record(r)
		if r.URL.Path == "/budget/" {
			if offset := r.URL.Query().Get("offset"); offset != "" {
				n, err := strconv.Atoi(offset)
				if err != nil || n < pornpicsFirstOffset {
					http.Error(w, "invalid offset", http.StatusBadRequest)
					return
				}
				page := (n - pornpicsFirstOffset) / config.PornPicsPageSize
				if page >= len(fixture.pages) && fixture.failEnd {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if page < len(fixture.pages) {
					_, _ = io.WriteString(w, pornpicsBlock(srv.URL, fixture.pages[page]...))
				} else {
					_, _ = io.WriteString(w, "[]")
				}
				return
			}
			_, _ = io.WriteString(w, `<html><head><script>var PP_LANG = "en";</script></head><body><h1>Budget collection</h1>`)
			for _, id := range fixture.first {
				_, _ = fmt.Fprintf(w, `<a class="rel-link" href="/galleries/gallery-%s/">Gallery</a>`, id)
			}
			_, _ = io.WriteString(w, "</body></html>")
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/galleries/gallery-"), "/")
		if fixture.onGallery != nil {
			fixture.onGallery(id)
		}
		body, ok := fixture.galleries[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, site
}

func pornpicsBudgetGallery(id, title string, count int) string {
	var body strings.Builder
	_, _ = fmt.Fprintf(&body, "<html><body><h1>%s</h1>", title)
	for i := range count {
		_, _ = fmt.Fprintf(&body, `<a class="rel-link" href="https://cdni.example.test/%s/picture-%d.jpg">Picture</a>`, id, i+1)
	}
	body.WriteString("</body></html>")
	return body.String()
}

func TestPornPicsFileBudgetSkipsUnreadableGalleries(t *testing.T) {
	for _, laterPage := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("later_page=%v/empty=%v", laterPage, empty), func(t *testing.T) {
				fixture := pornpicsBudgetFixture{
					first: []string{"unreadable"},
					galleries: map[string]string{
						"usable": pornpicsBudgetGallery("usable", "Usable gallery", 1),
					},
				}
				if empty {
					fixture.galleries["unreadable"] = pornpicsBudgetGallery("unreadable", "Empty gallery", 0)
				}
				if laterPage {
					fixture.pages = [][]string{{"usable"}}
				} else {
					fixture.first = append(fixture.first, "usable")
				}
				srv, site := pornpicsBudgetServer(t, fixture)
				res := extractOK(t, testPornPics(), srv.URL+"/budget/", Options{limits: Limits{Files: 1, Sources: 1}})
				if len(res.Files) != 1 || res.Files[0].Dir != "Usable gallery" {
					t.Fatalf("files = %v, want the usable gallery despite its unreadable predecessor", filePaths(res.Files))
				}
				if !strings.Contains(res.Note, "could not be read") {
					t.Errorf("note = %q, want the unreadable gallery reported", res.Note)
				}
				if strings.Contains(res.Note, "source limit") || site.askedOffset("520") {
					t.Errorf("note = %q, later block requested = %v; want only the file cap to end expansion", res.Note, site.askedOffset("520"))
				}
			})
		}
	}
}

func TestPornPicsFileBudgetStopsBeforeAnotherBlock(t *testing.T) {
	srv, site := pornpicsBudgetServer(t, pornpicsBudgetFixture{
		first: []string{"first"}, pages: [][]string{{"large"}, {"later"}},
		galleries: map[string]string{
			"first": pornpicsBudgetGallery("first", "First gallery", 1),
			"large": pornpicsBudgetGallery("large", "Large gallery", 3),
			"later": pornpicsBudgetGallery("later", "Later gallery", 1),
		},
	})
	res := extractOK(t, testPornPics(), srv.URL+"/budget/", Options{limits: Limits{Files: 3}})
	if len(res.Files) != 1 || res.Files[0].Dir != "First gallery" {
		t.Fatalf("files = %v, want only the whole first gallery", filePaths(res.Files))
	}
	if !strings.Contains(res.Note, "file limit") {
		t.Errorf("note = %q, want the omitted whole gallery reported", res.Note)
	}
	if site.askedOffset("520") || site.asked("/galleries/gallery-later/") {
		t.Error("continued discovery after the next whole gallery exceeded the remaining file budget")
	}
}

func TestPornPicsFileBudgetTrimsFirstNonemptyGallery(t *testing.T) {
	srv, site := pornpicsBudgetServer(t, pornpicsBudgetFixture{
		first: []string{"empty"}, pages: [][]string{{"large"}, {"later"}},
		galleries: map[string]string{
			"empty": pornpicsBudgetGallery("empty", "Empty gallery", 0),
			"large": pornpicsBudgetGallery("large", "Large gallery", 3),
			"later": pornpicsBudgetGallery("later", "Later gallery", 1),
		},
	})
	res := extractOK(t, testPornPics(), srv.URL+"/budget/", Options{limits: Limits{Files: 2}})
	if len(res.Files) != 2 || res.Files[0].Dir != "Large gallery" || res.Files[1].Dir != "Large gallery" {
		t.Fatalf("files = %v, want the first nonempty gallery trimmed to two files", filePaths(res.Files))
	}
	if !strings.Contains(res.Note, "file limit") || !strings.Contains(res.Note, "could not be read") {
		t.Errorf("note = %q, want both the file cap and empty gallery reported", res.Note)
	}
	if site.askedOffset("520") {
		t.Error("fetched another block after filling the file budget")
	}
}

func TestPornPicsFileBudgetNamesGalleriesAcrossPages(t *testing.T) {
	srv, _ := pornpicsBudgetServer(t, pornpicsBudgetFixture{
		first: []string{"first"}, pages: [][]string{{"first", "second"}},
		galleries: map[string]string{
			"first":  pornpicsBudgetGallery("first", "Shared title", 1),
			"second": pornpicsBudgetGallery("second", "Shared title", 1),
		},
	})
	res := extractOK(t, testPornPics(), srv.URL+"/budget/", Options{limits: Limits{Files: 4}})
	if len(res.Files) != 2 {
		t.Fatalf("files = %v, want two galleries, with the repeated link deduplicated", filePaths(res.Files))
	}
	for i, want := range []string{"Shared title [gallery-first]", "Shared title [gallery-second]"} {
		if res.Files[i].Dir != want {
			t.Errorf("file %d directory = %q, want %q", i, res.Files[i].Dir, want)
		}
	}
	if res.Note != "" {
		t.Errorf("note = %q, want a complete listing", res.Note)
	}
}

func TestPornPicsFileBudgetKeepsLaterPageFailure(t *testing.T) {
	srv, _ := pornpicsBudgetServer(t, pornpicsBudgetFixture{
		first: []string{"first"}, failEnd: true,
		galleries: map[string]string{"first": pornpicsBudgetGallery("first", "First gallery", 1)},
	})
	res := extractOK(t, testPornPics(), srv.URL+"/budget/", Options{limits: Limits{Files: 3}})
	if len(res.Files) != 1 || !res.Incomplete() || !strings.Contains(res.Note, "could not fetch") {
		t.Errorf("files = %v, note = %q, incomplete = %v; want the first gallery and later-page failure", filePaths(res.Files), res.Note, res.Incomplete())
	}
}

func TestPornPicsFileBudgetAcceptsRepeatedBlock(t *testing.T) {
	srv, site := pornpicsBudgetServer(t, pornpicsBudgetFixture{
		first: []string{"first"}, pages: [][]string{{"first"}},
		galleries: map[string]string{"first": pornpicsBudgetGallery("first", "First gallery", 1)},
	})
	res := extractOK(t, testPornPics(), srv.URL+"/budget/", Options{limits: Limits{Files: 3}})
	if len(res.Files) != 1 || res.Note != "" || site.askedOffset("520") {
		t.Errorf("files = %v, note = %q, next block requested = %v; want a repeated block to end the listing", filePaths(res.Files), res.Note, site.askedOffset("520"))
	}
}

func TestPornPicsFileBudgetCancellationIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, _ := pornpicsBudgetServer(t, pornpicsBudgetFixture{
		first: []string{"first"}, pages: [][]string{{"second"}},
		galleries: map[string]string{
			"first":  pornpicsBudgetGallery("first", "First gallery", 1),
			"second": pornpicsBudgetGallery("second", "Second gallery", 1),
		},
		onGallery: func(id string) {
			if id == "second" {
				cancel()
			}
		},
	})
	_, err := testPornPics().Extract(ctx, mustParse(t, srv.URL+"/budget/"), Options{limits: Limits{Files: 4}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation instead of a partial successful listing", err)
	}
}
