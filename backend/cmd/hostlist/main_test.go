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

// The rendered inventory names every site and every extractor, sorted, with
// the headline count agreeing with the table.
func TestRenderListsEverySite(t *testing.T) {
	catalogue := testCatalogue()
	out := render(catalogue)

	if !strings.Contains(out, "Do not edit by hand") {
		t.Error("the generated section does not say it is generated")
	}
	for _, host := range catalogue {
		if !strings.Contains(out, "| `"+host.Name+"` |") {
			t.Errorf("%s is missing from the rendered inventory", host.Name)
		}
		for _, site := range host.Sites {
			if !strings.Contains(out, "`"+site+"`") {
				t.Errorf("%s: site %s is missing from the rendered inventory", host.Name, site)
			}
		}
	}

	count := countSites(catalogue)
	if count == 0 {
		t.Fatal("the catalogue counts no sites at all")
	}
	if !strings.Contains(out, fmt.Sprintf("\n%d sites across ", count)) {
		t.Errorf("the headline does not state %d sites:\n%s", count, out)
	}
}

// A shape-matching extractor with no description would render as the vague
// fallback, which reads as a defect in the README rather than as the point.
func TestEveryShapeExtractorIsDescribed(t *testing.T) {
	for _, host := range testCatalogue() {
		if !host.ByShape {
			continue
		}
		if shapeOf(host.Name) == shapeOf("\x00no such extractor") {
			t.Errorf("%s matches by shape but shapeOf has nothing to say about it", host.Name)
		}
	}
	if shapeOf("direct") == shapeOf("\x00no such extractor") {
		t.Error("the fallback, which every unrecognised link reaches, has no description")
	}
}

// countSites counts domains, not extractors: a family covering thirty boards
// is thirty sites, and two extractors naming one domain are one.
func TestCountSitesCountsDistinctDomains(t *testing.T) {
	catalogue := []extractor.HostInfo{
		{Name: "a", Sites: []string{"one.test", "two.test"}},
		{Name: "b", Sites: []string{"two.test", "three.test"}},
		{Name: "c", ByShape: true},
	}
	if got := countSites(catalogue); got != 3 {
		t.Errorf("countSites = %d, want 3", got)
	}
}
