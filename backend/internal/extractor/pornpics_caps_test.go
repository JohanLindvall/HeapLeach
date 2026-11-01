// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"strings"
	"testing"
)

// A listing's galleries are bounded by the file cap, not the source cap:
// one block names hundreds of galleries, and a category is taken whole up
// to the files the user allows.
func TestPornPicsListingIsNotHeldToTheSourceCap(t *testing.T) {
	srv, _ := pornpicsServer(t)

	res := extractOK(t, testPornPics(), srv.URL+"/some-category/", Options{limits: Limits{Sources: 2}})
	if len(res.Files) != 6 {
		t.Fatalf("resolved %d files, want all three galleries of two", len(res.Files))
	}
	if strings.Contains(res.Note, "source limit") {
		t.Errorf("note = %q, want no source cap on a gallery listing", res.Note)
	}
}

// A search the site redirects to a category is paged as that category,
// through its own path, not as the search it was asked as.
func TestPornPicsRedirectedSearchIsPagedAsWhereItLanded(t *testing.T) {
	srv, site := pornpicsServer(t)

	res := extractOK(t, testPornPics(), srv.URL+"/?q=some+category", Options{})
	if len(res.Files) != 6 || res.Note != "" {
		t.Fatalf("resolved %d files, note %q; want three galleries of two and no note", len(res.Files), res.Note)
	}
	if !site.asked("/some-category/?limit=500&offset=20") {
		t.Errorf("the category's own path was not paged; requests were %v", site.requests)
	}
}

// A search at the site's root carries its QUERY global and is a listing
// like any other search; the front page without one still is not.
func TestPornPicsRecognisesARootSearch(t *testing.T) {
	p := NewPornPics(nil)
	search := fmt.Sprintf(pornpicsSearchListing, "https://pics.example.test")
	expectRecognised(t, p, htmlPage(t, "https://pics.example.test/?q=a+name", search), true)
	front := strings.Replace(search, "var QUERY='a+name';", "", 1)
	expectRecognised(t, p, htmlPage(t, "https://pics.example.test/", front), false)
}
