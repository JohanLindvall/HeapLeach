// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// fixturePage is a probed page built in memory: what a recogniser would be
// handed for rawURL had it answered with this status, type and body.
func fixturePage(t *testing.T, rawURL string, status int, kind, body string) *Page {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{}
	if kind != "" {
		header.Set(httpx.HeaderContentType, kind)
	}
	return &Page{URL: u, Final: u, Status: status, Header: header, Body: body}
}

// htmlPage is a 200 text/html fixture.
func htmlPage(t *testing.T, rawURL, body string) *Page {
	t.Helper()
	return fixturePage(t, rawURL, http.StatusOK, "text/html; charset=utf-8", body)
}

// rehosted is the same page served from somewhere else entirely.
func rehosted(t *testing.T, p *Page) *Page {
	t.Helper()
	move := func(u *url.URL) *url.URL {
		moved := *u
		moved.Host = "moved.example.test"
		return &moved
	}
	return &Page{URL: move(p.URL), Final: move(p.Final), Status: p.Status, Header: p.Header,
		Body: p.Body, Challenged: p.Challenged, Err: p.Err}
}

// expectRecognised asserts a recogniser's answer for a page, and that the
// answer is the same with the page served from an unrelated host: nothing
// about recognition may depend on where a page lives.
func expectRecognised(t *testing.T, r Recogniser, p *Page, want bool) {
	t.Helper()
	if got := r.Recognise(context.Background(), p); got != want {
		t.Errorf("Recognise(%s) = %v, want %v", p.URL, got, want)
	}
	if got := r.Recognise(context.Background(), rehosted(t, p)); got != want {
		t.Errorf("Recognise(%s, rehosted) = %v, want %v", p.URL, got, want)
	}
}

// expectShape asserts a URL-form answer, and its independence of the host.
func expectShape(t *testing.T, r Recogniser, rawURL string, want bool) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Shape(u); got != want {
		t.Errorf("Shape(%s) = %v, want %v", rawURL, got, want)
	}
	moved := *u
	moved.Host = "moved.example.test"
	if got := r.Shape(&moved); got != want {
		t.Errorf("Shape(%s, rehosted) = %v, want %v", rawURL, got, want)
	}
}

// TestRecognitionNeverReadsTheHost holds the package to its dispatch rule
// statically: no Shape or Recognise method may look at a URL's host, nor
// compare it with a domain. The fixture tests catch what a helper hides;
// this catches the direct case before any fixture is written.
func TestRecognitionNeverReadsTheHost(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || (fn.Name.Name != "Shape" && fn.Name.Name != "Recognise") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				var name string
				switch n := n.(type) {
				case *ast.SelectorExpr:
					name = n.Sel.Name
				case *ast.Ident:
					name = n.Name
				}
				switch name {
				case "Host", "Hostname", "HostMatches", "matchesHosts", "hostSet":
					t.Errorf("%s: %s reads %s", fset.Position(n.Pos()), fn.Name.Name, name)
				}
				return true
			})
		}
	}
}

// TestEveryRecogniserDescribesItself keeps the generated inventory complete:
// a recognising extractor has no domain list to show, only its account of
// what it takes.
func TestEveryRecogniserDescribesItself(t *testing.T) {
	for _, e := range testCatalogueRegistry(t).extractors {
		if _, ok := e.(Recogniser); !ok {
			continue
		}
		if d, ok := e.(Describer); !ok || d.Recognises() == "" {
			t.Errorf("%s recognises pages but does not describe them", e.Name())
		}
	}
}
