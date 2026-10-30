// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"golang.org/x/net/html"
)

// Helpers shared by the package's tests: the boilerplate around decoding a
// fixture, running one extraction and reading what came back, written once
// so a test states only what it checks.

// decode reads a JSON fixture into a T, failing the test when it does not
// parse.
func decode[T any](t *testing.T, doc string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("decode %T: %v", v, err)
	}
	return v
}

// mustParse parses raw as a URL the way the registry does.
func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// mustParseDoc parses an HTML fixture.
func mustParseDoc(t *testing.T, doc string) *html.Node {
	t.Helper()
	root, err := parseHTML(doc)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// extract runs one extraction of raw, which must parse as a URL.
func extract(t *testing.T, ex Extractor, raw string, opts Options) (*Result, error) {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ex.Extract(context.Background(), u, opts)
}

// extractOK is extract for an extraction that must succeed.
func extractOK(t *testing.T, ex Extractor, raw string, opts Options) *Result {
	t.Helper()
	res, err := extract(t, ex, raw, opts)
	if err != nil {
		t.Fatalf("extract %s: %v", raw, err)
	}
	return res
}

// fileNames lists a result's file names, in order.
func fileNames(files []File) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return names
}

// filePaths lists a result's files as dir/name, in order.
func filePaths(files []File) []string {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Name
		if f.Dir != "" {
			paths[i] = f.Dir + "/" + f.Name
		}
	}
	return paths
}

// sitePage is one document a fakeSite serves, and the Content-Type it is
// served as; with none, the server sniffs one as a real one would.
type sitePage struct {
	kind string
	body string
}

// siteHits counts what a fakeSite was asked: every request, and the ones it
// had a page for. Which one a test pins is the test's own question — every
// request, when what must not be asked is the point; the served ones, when
// other recognisers may ask the origin about a page at no cost to the
// extractor under test.
type siteHits struct {
	asked, served atomic.Int32
}

// fakeSite serves pages by path and 404s anything else.
func fakeSite(t *testing.T, pages map[string]sitePage) (*httptest.Server, *siteHits) {
	t.Helper()
	hits := &siteHits{}
	srv := httptest.NewServer(fakeSiteHandler(pages, hits))
	t.Cleanup(srv.Close)
	return srv, hits
}

// fakeSiteHandler is fakeSite's handler, for a test that guards it.
func fakeSiteHandler(pages map[string]sitePage, hits *siteHits) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.asked.Add(1)
		page, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		hits.served.Add(1)
		if page.kind != "" {
			w.Header().Set(httpx.HeaderContentType, page.kind)
		}
		_, _ = io.WriteString(w, page.body)
	})
}

// sitePages serves every document of pages as the one Content-Type kind.
func sitePages(kind string, pages map[string]string) map[string]sitePage {
	out := make(map[string]sitePage, len(pages))
	for path, body := range pages {
		out[path] = sitePage{kind: kind, body: body}
	}
	return out
}
