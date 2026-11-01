// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func cheveretoBudgetAlbum(name string) string {
	return fmt.Sprintf(`<div data-type="album"><a class="list-item-desc-title-link" href="/album/%s">%s</a></div>`, name, name)
}

func cheveretoBudgetFile(name string) string {
	object := fmt.Sprintf(`{"filename":"%s.jpg","url":"https://cdn.example.test/%s.jpg","size":1}`, name, name)
	return cheveretoTile(object, "", "https://cdn.example.test/thumbnail.jpg")
}

// Discovering an album spends a source slot, not a file slot. An empty or
// failed first album leaves room for the next one's files, even when its
// link is on a later listing page.
func TestCheveretoAlbumDiscoveryDoesNotSpendTheFileBudget(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		failed, later, larger bool
	}{
		{name: "empty first album"},
		{name: "failed first album", failed: true},
		{name: "later listing page", later: true},
		{name: "empty before larger album", larger: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			albums, next := cheveretoBudgetAlbum("empty")+cheveretoBudgetAlbum("kept"), ""
			if tc.later {
				albums, next = cheveretoBudgetAlbum("empty"), "?page=2"
			}
			files := cheveretoBudgetFile("first")
			if tc.larger {
				files += cheveretoBudgetFile("second")
			}
			pages := map[string]string{
				"/profile":        cheveretoPage("Profile", albums, next, ""),
				"/profile?page=2": cheveretoPage("Profile", cheveretoBudgetAlbum("kept"), "", ""),
				"/album/kept":     cheveretoPage("Kept", files, "", ""),
			}
			if !tc.failed {
				pages["/album/empty"] = cheveretoPage("Empty", "", "", "")
			}
			srv, hits := fakeSiteBy(t, (*url.URL).RequestURI, sitePages("text/html", pages))
			res, err := NewChevereto(testClient()).Extract(t.Context(), mustParse(t, srv.URL+"/profile"), Options{limits: Limits{Files: 1, Sources: 10}})
			if err != nil {
				t.Fatalf("extract: %v; requests=%v", err, hits.requested())
			}
			if len(res.Files) != 1 || res.Files[0].URL != "https://cdn.example.test/first.jpg" || res.Files[0].Dir != "kept" {
				t.Errorf("files=%+v, want the first file of the later album", res.Files)
			}
			if !slices.Contains(hits.requested(), "/album/kept") || (tc.later && !slices.Contains(hits.requested(), "/profile?page=2")) {
				t.Errorf("later album was not reached: %v", hits.requested())
			}
		})
	}
}

// The distinct album cap bounds discovery as well as expansion, including
// a page that reaches it exactly and advertises another listing page.
func TestCheveretoAlbumDiscoveryStopsAtTheSourceCap(t *testing.T) {
	for _, next := range []string{"", "?page=2"} {
		t.Run(cmpOr(next, "last page"), func(t *testing.T) {
			albums := cheveretoBudgetAlbum("first") + cheveretoBudgetAlbum("first")
			if next == "" {
				albums += cheveretoBudgetAlbum("second")
			}
			pages := map[string]string{
				"/profile":        cheveretoPage("Profile", albums, next, ""),
				"/profile?page=2": cheveretoPage("Profile", cheveretoBudgetAlbum("second"), "", ""),
				"/album/first":    cheveretoPage("First", cheveretoBudgetFile("first"), "", ""),
				"/album/second":   cheveretoPage("Second", cheveretoBudgetFile("second"), "", ""),
			}
			srv, hits := fakeSiteBy(t, (*url.URL).RequestURI, sitePages("text/html", pages))
			res := extractOK(t, NewChevereto(testClient()), srv.URL+"/profile", Options{limits: Limits{Files: 10, Sources: 1}})
			if len(res.Files) != 1 || res.Files[0].URL != "https://cdn.example.test/first.jpg" || res.Note != sourceLimitNote {
				t.Errorf("files=%+v note=%q, want the first album and only a source-limit note", res.Files, res.Note)
			}
			if got := hits.requested(); !slices.Equal(got, []string{"/profile", "/album/first"}) {
				t.Errorf("requests=%v, want no discovery or expansion past the source cap", got)
			}
		})
	}
}

// Albums beside direct files must neither consume their file budget nor
// stop their pagination. A single page still respects the final file cap.
func TestCheveretoMixedListingsKeepTheDirectFileBudget(t *testing.T) {
	for _, capped := range []bool{false, true} {
		name := "files on later page"
		if capped {
			name = "files exceed cap on one page"
		}
		t.Run(name, func(t *testing.T) {
			files := cheveretoBudgetFile("first") + cheveretoBudgetFile("second")
			albums := cheveretoBudgetAlbum("unused-one") + cheveretoBudgetAlbum("unused-two") + cheveretoBudgetAlbum("unused-three")
			next, limit := "?page=2", 3
			if capped {
				files += cheveretoBudgetFile("third")
				next, limit = "", 1
			}
			srv, hits := fakeSiteBy(t, (*url.URL).RequestURI, sitePages("text/html", map[string]string{
				"/profile":        cheveretoPage("Profile", albums+files, next, ""),
				"/profile?page=2": cheveretoPage("Profile", cheveretoBudgetFile("third"), "", ""),
			}))
			res := extractOK(t, NewChevereto(testClient()), srv.URL+"/profile", Options{limits: Limits{Files: limit, Sources: 1}})
			if len(res.Files) != limit {
				t.Fatalf("files=%+v, want %d direct files", res.Files, limit)
			}
			for i, file := range res.Files {
				want := "https://cdn.example.test/" + []string{"first", "second", "third"}[i] + ".jpg"
				if file.URL != want || file.Dir != "" {
					t.Errorf("file=%+v, want direct file %s", file, want)
				}
			}
			wantNote, requests := "", []string{"/profile", "/profile?page=2"}
			if capped {
				wantNote, requests = fileLimitNote, []string{"/profile"}
			}
			if res.Note != wantNote || !slices.Equal(hits.requested(), requests) {
				t.Errorf("note=%q requests=%v, want %q and %v", res.Note, hits.requested(), wantNote, requests)
			}
		})
	}
}

func TestCheveretoFileWalkSkipsAlbumOnlyPages(t *testing.T) {
	srv, hits := fakeSiteBy(t, (*url.URL).RequestURI, sitePages("text/html", map[string]string{
		"/profile":        cheveretoPage("Profile", cheveretoBudgetFile("first")+cheveretoBudgetAlbum("unused-first"), "?page=2", ""),
		"/profile?page=2": cheveretoPage("Profile", cheveretoBudgetAlbum("unused-second"), "?page=3", ""),
		"/profile?page=3": cheveretoPage("Profile", cheveretoBudgetFile("second"), "", ""),
	}))
	res := extractOK(t, NewChevereto(testClient()), srv.URL+"/profile", Options{limits: Limits{Files: 2, Sources: 1}})
	if got := fileNames(res.Files); !slices.Equal(got, []string{"first.jpg", "second.jpg"}) || res.Note != "" {
		t.Errorf("files=%v note=%q, want both direct files and no note", got, res.Note)
	}
	if got := hits.requested(); !slices.Equal(got, []string{"/profile", "/profile?page=2", "/profile?page=3"}) {
		t.Errorf("requests=%v, want all listing pages and no album reads", got)
	}
}

func TestCheveretoFileWalkStillEndsAtRepeatedPages(t *testing.T) {
	for _, albumsOnly := range []bool{false, true} {
		name := "file page"
		if albumsOnly {
			name = "album-only page"
		}
		t.Run(name, func(t *testing.T) {
			first := cheveretoPage("Profile", cheveretoBudgetFile("first")+cheveretoBudgetAlbum("unused-first"), "?page=2", "")
			pages := map[string]string{"/profile": first, "/profile?page=2": first}
			wantRequests := []string{"/profile", "/profile?page=2"}
			if albumsOnly {
				bridge := cheveretoPage("Profile", cheveretoBudgetAlbum("unused-second"), "?page=3", "")
				pages["/profile?page=2"], pages["/profile?page=3"] = bridge, bridge
				wantRequests = append(wantRequests, "/profile?page=3")
			}
			srv, hits := fakeSiteBy(t, (*url.URL).RequestURI, sitePages("text/html", pages))
			res := extractOK(t, NewChevereto(testClient()), srv.URL+"/profile", Options{limits: Limits{Files: 2, Sources: 1}})
			if len(res.Files) != 1 || res.Note != "" || !slices.Equal(hits.requested(), wantRequests) {
				t.Errorf("files=%+v note=%q requests=%v; want one file, no note and %v", res.Files, res.Note, hits.requested(), wantRequests)
			}
		})
	}
}

func TestCheveretoAlbumDiscoveryKeepsItsPaginationOutcome(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		name := "failed later page"
		if repeated {
			name = "repeated later page"
		}
		t.Run(name, func(t *testing.T) {
			listing := cheveretoPage("Profile", cheveretoBudgetAlbum("first"), "?page=2", "")
			pages := map[string]string{
				"/profile":     listing,
				"/album/first": cheveretoPage("First", cheveretoBudgetFile("first"), "", ""),
			}
			if repeated {
				pages["/profile?page=2"] = listing
			}
			srv, hits := fakeSiteBy(t, (*url.URL).RequestURI, sitePages("text/html", pages))
			res := extractOK(t, NewChevereto(testClient()), srv.URL+"/profile", Options{limits: Limits{Files: 1, Sources: 10}})
			if len(res.Files) != 1 || !slices.Contains(hits.requested(), "/profile?page=2") {
				t.Errorf("files=%+v requests=%v, want one file after reading the next listing page", res.Files, hits.requested())
			}
			if repeated {
				if res.Note != "" {
					t.Errorf("repeated page note=%q, want none", res.Note)
				}
			} else if !strings.Contains(res.Note, "could not fetch next page") || !res.Incomplete() {
				t.Errorf("failed later page note=%q, incomplete=%v", res.Note, res.Incomplete())
			}
		})
	}
}
