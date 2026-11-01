// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestBunkrAlbumEntriesMergeCardsAndPlainLinks(t *testing.T) {
	root := mustParseDoc(t, `<html><body>
<a href="https://files.example.test/f/First" title="Wrong.jpg">Plain duplicate before card</a>
<div class="theItem" title="First.jpg">
  <a href="/f/First">Open</a><p class="theSize">2 MB</p>
</div>
<div class="theItem" title="Also wrong.jpg"><a href="/f/First">Duplicate card</a></div>
<a href="/v/Second" title="Second.mp4">Open</a>
<a href="//files.example.test/v/Second" title="Duplicate.mp4">Duplicate link</a>
<a href="/i/Third"><img alt="Third.png"></a>
<a href="/faq">FAQ</a><a href="/a/Another">Another album</a>
<a href="/f/First/extra">Not a file page</a>
</body></html>`)
	base := mustParse(t, "https://files.example.test/a/SyntheticAlbum")
	entries := bunkrAlbumEntries(NewBunkr(nil), root, base)
	want := []struct {
		path, name string
		size       int64
		approx     bool
	}{
		{"/f/First", "First.jpg", 2 << 20, true},
		{"/v/Second", "Second.mp4", -1, false},
		{"/i/Third", "Third.png", -1, false},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d distinct file links", len(entries), len(want))
	}
	for i, expected := range want {
		entry := entries[i]
		file := entry.file
		if entry.href != "https://files.example.test"+expected.path || file.Name != expected.name || file.Size != expected.size || file.SizeApprox != expected.approx {
			t.Errorf("entry %d: href %q, name %q, size %d, approximate %v; want %+v", i, entry.href, file.Name, file.Size, file.SizeApprox, expected)
		}
		if file.Resolve == nil || file.URL != "" {
			t.Errorf("entry %d must defer its signed URL until download", i)
		}
	}
}

func TestBunkrAlbumKeepsPlainLinksBesideRepeatedCards(t *testing.T) {
	const card = `<div class="theItem" title="First.jpg"><a href="/f/First">Open</a><p class="theSize">2 MB</p></div>`
	const second = `<a href="/f/Second" title="Second.jpg">Open</a>`
	const third = `<a href="/f/Third" title="Third.jpg">Open</a>`
	for _, tc := range []struct {
		name  string
		pages []string
		cap   int
		calls int
		want  []string
		note  string
	}{
		{"last page has a fresh plain link", []string{card, card + second}, 0, 2, []string{"First.jpg", "Second.jpg"}, ""},
		{"middle page has a fresh plain link", []string{card, card + second, card + second + third}, 0, 3, []string{"First.jpg", "Second.jpg", "Third.jpg"}, ""},
		{"file cap includes plain links", []string{card, card + second + third}, 2, 2, []string{"First.jpg", "Second.jpg"}, fileLimitNote},
		{"exact terminal file cap", []string{card, card + second}, 2, 2, []string{"First.jpg", "Second.jpg"}, ""},
		{"file cap stops further requests", []string{card, card + second}, 1, 1, []string{"First.jpg"}, fileLimitNote},
		{"repeated mixed page ends quietly", []string{card + second, card + second, card + third}, 0, 2, []string{"First.jpg", "Second.jpg"}, ""},
		{"plain links only", []string{second, second + third}, 0, 2, []string{"Second.jpg", "Third.jpg"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := int(calls.Add(1))
				query := ""
				if n > 1 {
					query = fmt.Sprintf("page=%d", n)
				}
				// Listing must not open file pages, call metadata/signing
				// endpoints, or fetch media bodies.
				if r.Method != http.MethodGet || r.URL.Path != "/a/SyntheticAlbum" || r.URL.RawQuery != query || n > len(tc.pages) {
					t.Errorf("unexpected request %d: %s %s", n, r.Method, r.URL)
					http.NotFound(w, r)
					return
				}
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprintf(w, `<html><head><title>Synthetic album</title></head><body>%s<a href="?page=%d">Last page</a></body></html>`, tc.pages[n-1], len(tc.pages))
			}))
			defer srv.Close()
			res, err := NewBunkr(testClient()).Extract(t.Context(), mustParse(t, srv.URL+"/a/SyntheticAlbum"), Options{limits: Limits{Files: tc.cap, Sources: 1}})
			if got := int(calls.Load()); got != tc.calls {
				t.Errorf("listing requests = %d, want %d", got, tc.calls)
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, file := range res.Files {
				names = append(names, file.Name)
				if file.Resolve == nil || file.URL != "" {
					t.Errorf("%s must retain a deferred resolver", file.Name)
				}
				if file.Name == "First.jpg" && (file.Size != 2<<20 || !file.SizeApprox) {
					t.Errorf("card metadata lost: size %d, approximate %v", file.Size, file.SizeApprox)
				}
			}
			if !slices.Equal(names, tc.want) || res.Note != tc.note {
				t.Errorf("names %v, note %q; want %v, %q", names, res.Note, tc.want, tc.note)
			}
		})
	}
}
