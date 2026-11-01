// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func testCatalogue() []extractor.HostInfo {
	cfg := &config.Config{UserAgent: config.DefaultUserAgent, Language: config.DefaultLanguage}
	return extractor.NewRegistry(cfg, httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), 0, 0)).Catalogue()
}

// rewrite replaces only what sits between the markers, leaves the file alone
// when nothing changed, and refuses a file it cannot find its place in.
func TestRewriteTouchesOnlyTheMarkedSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "README.md")
	original := "# Title\n\nprose before\n\n" + beginMarker + "\nstale\n" + endMarker + "\n\nprose after\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := rewrite(path, "  fresh table  \n"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "# Title\n\nprose before\n\n" + beginMarker + "\nfresh table\n" + endMarker + "\n\nprose after\n"
	if string(got) != want {
		t.Errorf("rewrite produced:\n%s\nwant:\n%s", got, want)
	}

	// A second pass with the same body is a no-op: the file keeps its mtime,
	// which is what lets `make hosts-check` compare against git honestly.
	before, _ := os.Stat(path)
	if err := rewrite(path, "fresh table"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an unchanged section still rewrote the file")
	}

	unmarked := filepath.Join(t.TempDir(), "plain.md")
	if err := os.WriteFile(unmarked, []byte("# nothing to see\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rewrite(unmarked, "table"); err == nil {
		t.Error("a file without markers was rewritten rather than refused")
	}
}

// The rendered inventory names every extractor with its description, and
// the headline count agrees with the table.
func TestRenderListsEveryExtractor(t *testing.T) {
	catalogue := testCatalogue()
	out := render(catalogue)

	if !strings.Contains(out, "Do not edit by hand") {
		t.Error("the generated section does not say it is generated")
	}
	for _, host := range catalogue {
		if host.Recognises == "" {
			t.Errorf("%s does not say what it recognises", host.Name)
		}
		if !strings.Contains(out, "| `"+host.Name+"` | "+host.Recognises+" |") {
			t.Errorf("%s is missing from the rendered inventory", host.Name)
		}
	}
	if !strings.Contains(out, fmt.Sprintf("\n%d extractors, ", len(catalogue))) {
		t.Errorf("the headline does not state %d extractors:\n%s", len(catalogue), out)
	}
}
