// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"sync"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"golang.org/x/net/html"
)

// countNodes is how many nodes the tree under root holds.
func countNodes(root *html.Node) int {
	n := 0
	walk(root, func(*html.Node) { n++ })
	return n
}

// A Page is shared by every reader of its URL, so what one extraction makes
// of it must leave it as it was: its URLs, its document and its parse. Rajce
// trims the fragment from the address it files under, which is the edit that
// once reached the shared page.
func TestSharedPageIsLeftAsItWas(t *testing.T) {
	const submitted = "https://photos.example.test/legacy-album"
	const final = "https://photos.example.test/photographer/album/field-notes#selected-photo"
	p := htmlPage(t, submitted, rajceAlbumPage)
	p.Final = mustParse(t, final)
	root := p.HTML()
	nodes := countNodes(root)

	r := NewRajce(nil)
	ctx := withPage(context.Background(), p)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			res, err := r.Extract(ctx, mustParse(t, submitted), Options{})
			if err != nil {
				t.Error(err)
				return
			}
			if got := res.Files[0].Headers[httpx.HeaderReferer]; got != "https://photos.example.test/photographer/album/field-notes" {
				t.Errorf("referer = %q", got)
			}
		})
	}
	wg.Wait()

	if p.URL.String() != submitted || p.Final.String() != final {
		t.Errorf("shared page's URLs changed: %s, %s", p.URL, p.Final)
	}
	if p.HTML() != root || countNodes(root) != nodes || p.Body != rajceAlbumPage {
		t.Error("shared page's document changed")
	}
}

// The final URL a reused probe hands back is the caller's to change, as the
// one a fetch returns would be.
func TestReusedProbeHandsBackAURLOfItsOwn(t *testing.T) {
	u := mustParse(t, "https://photos.example.test/album")
	p := htmlPage(t, u.String(), "<html><body><p>album</p></body></html>")
	ctx := withPage(context.Background(), p)

	_, final, ok := probedDocument(ctx, u)
	if !ok || final == p.Final {
		t.Fatalf("probedDocument shares the page's URL (ok %v)", ok)
	}
	final.Fragment = "edited"
	// Before recognition parsed it, the page is parsed for the reader alone
	// rather than indexed; after, the probe's tree is the one read.
	if root, _, err := fetchRoot(ctx, nil, u, nil); err != nil || root == nil || p.indexed.Load() {
		t.Fatalf("fetchRoot of an unparsed probe: root %v, indexed %v, err %v", root != nil, p.indexed.Load(), err)
	}
	p.HTML()
	root, final, err := fetchRoot(ctx, nil, u, nil)
	if err != nil || root != p.HTML() || final == p.Final {
		t.Fatalf("fetchRoot: root reused %v, URL shared %v, err %v", root == p.HTML(), final == p.Final, err)
	}
	if p.Final.Fragment != "" {
		t.Errorf("page's final URL changed to %s", p.Final)
	}
}
