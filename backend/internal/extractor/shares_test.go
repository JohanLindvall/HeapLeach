// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
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
