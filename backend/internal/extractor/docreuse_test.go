// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
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
		{
			name: "coomerfans", link: "https://coomerfans.com/p/1/2/onlyfans", doc: coomerFansPostPage,
			ex: NewCoomerFans(nil),
			fromString: func(doc string, u *url.URL) (*Result, error) {
				title, media, err := coomerFansPost(doc, u)
				if err != nil {
					return nil, err
				}
				return coomerFansPostResult(u, title, NewCoomerFans(nil).mediaFiles(u, title, media))
			},
		},
		{
			name: "pornpics", link: "https://pics.example.test/galleries/a-gallery-name-10000001/",
			doc: strings.Replace(pornpicsGalleryPage, "</head>", `<script>var PP_LANG = "en";</script></head>`, 1),
			ex:  NewPornPics(nil), fromString: pornpicsGallery,
		},
		{
			name: "chevereto", link: "https://pics.example.test/i/holiday.AAA111", doc: cheveretoImageViewer,
			ex: NewChevereto(nil),
			fromString: func(doc string, u *url.URL) (*Result, error) {
				root, err := parseHTML(doc)
				if err != nil {
					return nil, err
				}
				return cheveretoSingle(root, u, cheveretoTitle(root, u))
			},
		},
		{
			name: "bandzoogle", link: "https://aband.example.test/music", doc: bandzooglePage,
			ex: NewBandzoogle(nil),
			fromString: func(doc string, u *url.URL) (*Result, error) {
				root, err := parseHTML(doc)
				if err != nil {
					return nil, err
				}
				return bandzoogleRead(context.Background(), nil, root, u, Options{})
			},
		},
	}
}

// withoutHooks is a result with its files' hooks taken out, which no two
// closures compare equal by, and which hooks each file had, for comparing
// two results field by field.
func withoutHooks(res *Result) (*Result, []string) {
	if res == nil {
		return nil, nil
	}
	out := *res
	out.Files = make([]File, len(res.Files))
	hooks := make([]string, len(res.Files))
	for i, f := range res.Files {
		hooks[i] = fmt.Sprintf("resolve=%v refresh=%v check=%v pace=%v", f.Resolve != nil, f.Refresh != nil, f.Check != nil, f.Pace != nil)
		f.Resolve, f.Refresh, f.Check, f.Pace = nil, nil, nil, nil
		out.Files[i] = f
	}
	return &out, hooks
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
			gotRes, gotHooks := withoutHooks(got)
			wantRes, wantHooks := withoutHooks(want)
			if !reflect.DeepEqual(gotRes, wantRes) || !reflect.DeepEqual(gotHooks, wantHooks) {
				t.Errorf("from the tree:\n%+v %v\nfrom the string:\n%+v %v", gotRes, gotHooks, wantRes, wantHooks)
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
