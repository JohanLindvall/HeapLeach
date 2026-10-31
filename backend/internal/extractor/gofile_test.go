// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// TestGofileOrderedChildren pins the listing order: gofile's own ordering
// where it supplied one, and a name sort for anything it left out — so a
// folder renders the same way the website shows it, and deterministically
// either way.
func TestGofileOrderedChildren(t *testing.T) {
	content := &gofileContent{
		ChildrenIDs: []string{"id-b", "id-a", "id-b"}, // the repeat must not double
		Children: map[string]gofileContent{
			"id-a": {ID: "id-a", Name: "Alpha"},
			"id-b": {ID: "id-b", Name: "Beta"},
			// Not in ChildrenIDs at all: sorted by name after the ordered ones.
			"id-z": {ID: "id-z", Name: "aardvark"},
			"id-y": {ID: "id-y", Name: "Zebra"},
		},
	}
	var names []string
	for _, child := range content.orderedChildren() {
		names = append(names, child.Name)
	}
	want := "Beta Alpha aardvark Zebra"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}

func TestGofileAccessError(t *testing.T) {
	password := &gofileContent{PasswordStatus: "passwordRequired"}
	if err := password.accessError(); !errors.Is(err, ErrPasswordRequired) {
		t.Errorf("a password gate reported %v", err)
	}
	// Some responses carry the password as a field rather than a status.
	flagged := &gofileContent{Password: json.RawMessage(`true`)}
	if err := flagged.accessError(); !errors.Is(err, ErrPasswordRequired) {
		t.Errorf("a password flag reported %v", err)
	}
	expired := &gofileContent{Expire: json.RawMessage(`1700000000`)}
	if err := expired.accessError(); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("an expired link reported %v", err)
	}
	if err := (&gofileContent{}).accessError(); err == nil {
		t.Error("an inaccessible node with no stated reason reported nothing")
	}
}

func TestGofileStatusError(t *testing.T) {
	g := NewGofile(&config.Config{}, nil)
	if err := g.statusError("error-passwordRequired"); !errors.Is(err, ErrPasswordRequired) {
		t.Errorf("passwordRequired mapped to %v", err)
	}
	if err := g.statusError("error-notFound"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("notFound mapped to %v", err)
	}
	if err := g.statusError("error-wrongToken"); !errors.Is(err, errGofileStaleToken) {
		t.Errorf("wrongToken mapped to %v", err)
	}
}

// TestGofileExplain covers the two refusals the API words as a status error:
// a rejected signature — which must start the cooldown, since gofile blocks
// an address that keeps signing badly — and a stale account token, which the
// caller retries with a fresh one.
func TestGofileExplain(t *testing.T) {
	g := NewGofile(&config.Config{}, nil)
	rejected := g.explain(&httpx.StatusError{Code: 401, Body: `{"status":"error-notPremium"}`})
	if rejected == nil || !strings.Contains(rejected.Error(), "HEAPLEACH_GOFILE_SECRET") {
		t.Errorf("a rejected signature explained itself as %v", rejected)
	}
	if g.rejectedAt.IsZero() {
		t.Error("a rejected signature did not start the cooldown")
	}

	if err := NewGofile(&config.Config{}, nil).explain(&httpx.StatusError{Code: 401, Body: `{"status":"error-wrongToken"}`}); !errors.Is(err, errGofileStaleToken) {
		t.Errorf("a stale token explained itself as %v", err)
	}
}

func TestGofileRecognise(t *testing.T) {
	g := NewGofile(&config.Config{}, nil)
	shell := `<html><head><title>Gofile — Cloud Storage</title><meta property="og:site_name" content="Gofile">` +
		`<script src="/js/wt.obf.js"></script><script src="/js/app.js"></script></head><body></body></html>`
	// Named, but not the app: the share link's form and the name alone are
	// not enough.
	expectRecognised(t, g, htmlPage(t, "https://share.example.test/d/Abc123",
		`<html><head><title>Gofile</title><meta property="og:site_name" content="Gofile"></head></html>`), false)
	expectRecognised(t, g, htmlPage(t, "https://share.example.test/d/Abc123", shell), true)
	expectRecognised(t, g, htmlPage(t, "https://share.example.test/", shell), false)
	expectRecognised(t, g, htmlPage(t, "https://share.example.test/d/Abc123", `<html><title>A blog</title></html>`), false)
	expectShape(t, g, "https://share.example.test/d/Abc123", true)
	expectShape(t, g, "https://share.example.test/d/Abc123/extra", false)
	expectShape(t, g, "https://share.example.test/about", false)
}

// gofileContentServer serves a folder's pages: page n (from 1) holds the
// files pages[n-1] names, with another page said to follow while hasNext
// does; a page past the list fails.
func gofileContentServer(t *testing.T, pages [][]string, hasNext func(n int) bool) (*Gofile, *atomic.Int32) {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		n, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if r.URL.Path != "/contents/root" || n < 1 || n > len(pages) {
			http.Error(w, "no such page", http.StatusNotFound)
			return
		}
		children := map[string]any{}
		for _, id := range pages[n-1] {
			children[id] = map[string]any{"id": id, "type": "file", "name": id + ".bin", "link": "https://cdn.example.test/" + id}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"data":     map[string]any{"id": "root", "type": "folder", "name": "Root", "childrenIds": pages[n-1], "children": children},
			"metadata": map[string]any{"hasNextPage": hasNext(n)},
		})
	}))
	t.Cleanup(srv.Close)
	return &Gofile{client: testClient(), fallback: "a-configured-secret", lang: "en-US", api: srv.URL}, &asked
}

// TestGofileFetchContentPages covers a folder's pages: merged in order, cut
// short with a note where they stop early, and no further than the files
// that can still be taken.
func TestGofileFetchContentPages(t *testing.T) {
	always := func(int) bool { return true }
	for _, tc := range []struct {
		name    string
		pages   [][]string
		hasNext func(int) bool
		room    int
		ids     string
		note    string
		asked   int32
	}{
		{"whole", [][]string{{"a", "b"}, {"c"}}, func(n int) bool { return n < 2 }, 10, "a,b,c", "", 2},
		{"empty page ends it", [][]string{{"a"}, {}}, always, 10, "a", "", 2},
		{"later page fails", [][]string{{"a"}}, always, 10, "a", "partial — could not fetch next page", 2},
		{"repeated page", [][]string{{"a"}, {"a"}}, always, 10, "a", "partial — repeated or empty page", 2},
		{"no room for more", [][]string{{"a", "b"}, {"c"}}, always, 1, "a,b", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, asked := gofileContentServer(t, tc.pages, tc.hasNext)
			c, note, err := g.fetchContent(context.Background(), "root", "token", "", tc.room)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(c.ChildrenIDs, ","); got != tc.ids || note != tc.note || asked.Load() != tc.asked {
				t.Errorf("children = %s, note = %q, asked %d; want %s, %q, %d", got, note, asked.Load(), tc.ids, tc.note, tc.asked)
			}
			if c.Name != "Root" || len(c.Children) != len(c.ChildrenIDs) {
				t.Errorf("merged folder = %q with %d children for %d ids", c.Name, len(c.Children), len(c.ChildrenIDs))
			}
		})
	}

	// A page cap reached while the API still says there is more is noted.
	pages := make([][]string, config.MaxAlbumPages+1)
	for i := range pages {
		pages[i] = []string{strconv.Itoa(i)}
	}
	g, _ := gofileContentServer(t, pages, always)
	c, note, err := g.fetchContent(context.Background(), "root", "token", "", 1<<20)
	if err != nil || len(c.ChildrenIDs) != config.MaxAlbumPages || note != "partial — page limit" {
		t.Fatalf("children = %d, note = %q, err = %v", len(c.ChildrenIDs), note, err)
	}
}
