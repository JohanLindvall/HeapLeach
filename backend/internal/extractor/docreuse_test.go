// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// probedReader is an extractor that reads the probed page's own tree, with
// the string entry point its fixtures use beside it.
type probedReader struct {
	name, link, doc string
	ex              Extractor
	fromString      func(doc string, u *url.URL) (*Result, error)
}

func probedReaders() []probedReader {
	const abcLink = abcListenRoot + "/listen/programs/a-programme/an-episode/100000001"
	return []probedReader{
		{
			name: "npr", link: "https://news.example.test/programs/a-programme/", doc: nprListingPage,
			ex: NewNPR(nil), fromString: nprResult,
		},
		{
			name: "abclisten", link: abcLink, doc: abcListenHTML("An Episode - ABC listen", abcListenAACIsland),
			ex: NewABCListen(nil),
			fromString: func(doc string, u *url.URL) (*Result, error) {
				page, err := abcListenParse(doc)
				if err != nil {
					return nil, err
				}
				return NewABCListen(nil).episode(u, page.props.Data.DocumentProps)
			},
		},
	}
}

// probedPage is a 200 HTML page as the registry's probe leaves it.
func probedPage(link, doc string) *Page {
	u, _ := url.Parse(link)
	return &Page{URL: u, Final: u, Status: http.StatusOK,
		Header: http.Header{httpx.HeaderContentType: {"text/html; charset=utf-8"}}, Body: doc}
}

// A reader handed the probed page reads the tree recognition parsed, asks
// nothing (the extractors here have no client to ask with), leaves the tree
// as it was, and comes to exactly what its string entry point does: titles,
// names and directories byte for byte.
func TestProbedReadersReuseTheParsedDocument(t *testing.T) {
	for _, r := range probedReaders() {
		t.Run(r.name, func(t *testing.T) {
			p := probedPage(r.link, r.doc)
			if !r.ex.(Recogniser).Recognise(t.Context(), p) {
				t.Fatal("fixture not recognised")
			}
			root := p.HTML()
			nodes := countNodes(root)
			got, err := r.ex.Extract(withPage(t.Context(), p), p.URL, Options{})
			if err != nil {
				t.Fatal(err)
			}
			want, err := r.fromString(r.doc, p.URL)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("from the tree:\n%+v\nfrom the string:\n%+v", got, want)
			}
			if p.HTML() != root || countNodes(root) != nodes {
				t.Error("the shared tree changed")
			}
		})
	}
}

// The JSON island read off the tree is the one written in the body: spacing
// inside its strings, a non-breaking space and markup-like text included.
func TestNextDataInKeepsTheIslandAsWritten(t *testing.T) {
	island := "{\"props\":{\"pageProps\":{\"title\":\"Two  spaces, a no-break & <b>tag</b>\"}},\n  \"page\":\"/x\"}"
	doc := `<html><head><script id="other">{}</script></head><body>` +
		`<script id="__NEXT_DATA__" type="application/json">` + island + `</script></body></html>`
	root, err := parseHTML(doc)
	if err != nil {
		t.Fatal(err)
	}
	fromTree, ok := nextDataIn(root)
	fromBody, _ := nextData(doc)
	if !ok || fromTree != island || fromTree != fromBody {
		t.Errorf("island from the tree %q, from the body %q, want %q", fromTree, fromBody, island)
	}
	if _, ok := nextDataIn(mustParseDoc(t, "<html><body></body></html>")); ok {
		t.Error("an island found on a page without one")
	}
}

// BenchmarkProbedReaders measures recognising a fresh probed page and
// reading it: "reparse" parses the body again from its string, as the
// readers did, and "reuse" reads the probe's own tree.
func BenchmarkProbedReaders(b *testing.B) {
	for _, r := range probedReaders() {
		recognise := r.ex.(Recogniser).Recognise
		b.Run(r.name+"/reparse", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				p := probedPage(r.link, r.doc)
				if !recognise(context.Background(), p) {
					b.Fatal("not recognised")
				}
				if _, err := r.fromString(r.doc, p.URL); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(r.name+"/reuse", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				p := probedPage(r.link, r.doc)
				if !recognise(context.Background(), p) {
					b.Fatal("not recognised")
				}
				if _, err := r.ex.Extract(withPage(context.Background(), p), p.URL, Options{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
