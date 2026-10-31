// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// TestShareFormsNeverReadTheHost is TestRecognitionNeverReadsTheHost for the
// rows: a share's forms are its Shape and Recognise, written as data, so the
// same rule is checked inside every shareSite's forms.
func TestShareFormsNeverReadTheHost(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	rows := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if typ, ok := lit.Type.(*ast.Ident); !ok || typ.Name != "shareSite" {
				return true
			}
			rows++
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if key, isIdent := kv.Key.(*ast.Ident); !ok || !isIdent || key.Name != "forms" {
					continue
				}
				ast.Inspect(kv.Value, func(n ast.Node) bool {
					var name string
					switch n := n.(type) {
					case *ast.SelectorExpr:
						name = n.Sel.Name
					case *ast.Ident:
						name = n.Name
					}
					switch name {
					case "Host", "Hostname", "HostMatches", "matchesHosts", "hostSet":
						t.Errorf("%s: a share form reads %s", fset.Position(n.Pos()), name)
					}
					return true
				})
			}
			return true
		})
	}
	if rows == 0 {
		t.Fatal("found no shareSite rows to check")
	}
}

// A share takes a page by the first form its final URL has, and a form that
// only redirects is never recognised itself.
func TestShareRecognisesByTheFirstFormOfTheFinalURL(t *testing.T) {
	site := &shareSite{
		name: "test",
		forms: []shareForm{
			{pathShape(`^/a/`), func(p *Page) bool { return p.Has("album") }},
			{pathShape(`^/a/x$`), func(*Page) bool { return true }},
			{shape: pathShape(`^/t-`)},
		},
	}
	s := newShare(nil, site)
	ctx := context.Background()
	if !s.Recognise(ctx, htmlPage(t, "https://share.example.test/a/x", "<p>album</p>")) {
		t.Error("an album page was not taken")
	}
	if s.Recognise(ctx, htmlPage(t, "https://share.example.test/a/x", "<p>nothing</p>")) {
		t.Error("a later form decided where the first one had refused")
	}
	if s.Recognise(ctx, htmlPage(t, "https://share.example.test/t-abc", "<p>album</p>")) {
		t.Error("a redirecting form was recognised itself")
	}
	for raw, want := range map[string]bool{
		"https://share.example.test/a/y":   true,
		"https://share.example.test/t-abc": true,
		"https://share.example.test/b/y":   false,
	} {
		if got := s.Shape(mustParse(t, raw)); got != want {
			t.Errorf("Shape(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestLinkPathID(t *testing.T) {
	for link, want := range map[string]string{
		"https://share.example.test/f/abc":         "abc",
		"https://share.example.test/e/abc?album=1": "abc",
		"https://share.example.test/f/abc/more":    "",
		"https://share.example.test/a/abc":         "",
		"https://share.example.test/f":             "",
		"://not a url":                             "",
	} {
		if got := linkPathID(link, "f", "e"); got != want {
			t.Errorf("linkPathID(%q) = %q, want %q", link, got, want)
		}
	}
}

// Every registered Share row gives the engine what it calls: a name and an
// inventory line, an extract, and forms that each have a shape, at least
// one of which recognises a page. A form without recognise only redirects
// to another, so a row of nothing but those could never be chosen. Rows are
// registered once each, in the same order every time the registry is built.
func TestShareRowsAreCompleteAndRegisteredOnce(t *testing.T) {
	names := func() []string {
		var out []string
		for _, e := range NewRegistry(&config.Config{}, nil).extractors {
			if s, ok := e.(*Share); ok {
				out = append(out, s.Name())
			}
		}
		return out
	}
	first := names()
	if len(first) == 0 {
		t.Fatal("no Share rows registered")
	}
	if again := names(); !slices.Equal(first, again) {
		t.Errorf("registration order changed between builds: %v then %v", first, again)
	}
	seen := map[string]bool{}
	for _, e := range NewRegistry(&config.Config{}, nil).extractors {
		s, ok := e.(*Share)
		if !ok {
			continue
		}
		site := s.site
		if seen[site.name] {
			t.Errorf("row %q is registered twice", site.name)
		}
		seen[site.name] = true
		if site.name == "" || site.recognises == "" || site.extract == nil || len(site.forms) == 0 || s.shape == nil {
			t.Errorf("row %q leaves out something the engine calls", site.name)
		}
		recognising := 0
		for i, f := range site.forms {
			if f.shape == nil {
				t.Errorf("row %q: form %d has no shape", site.name, i)
			}
			if f.recognise != nil {
				recognising++
			}
		}
		if recognising == 0 {
			t.Errorf("row %q has only redirecting forms", site.name)
		}
	}
}
