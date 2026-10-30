// SPDX-License-Identifier: MIT

package extractor

import (
	"regexp"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func testCatalogueRegistry(t *testing.T) *Registry {
	t.Helper()
	cfg := &config.Config{UserAgent: config.DefaultUserAgent, Language: config.DefaultLanguage}
	return NewRegistry(cfg, httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), 0, 0))
}

// domainLike is a token that reads as a host name.
var domainLike = regexp.MustCompile(`(?i)\b[a-z0-9-]+\.(com|net|org|io|tv|to|cc|me|st|cr|nz|ru|se|no|fi|dk|de|fr|be|nl|it|es|pt|ca|uk|jp|au|is|at|ch|xxx|xyz|su|party|li|so|ws|ly|gg)\b`)

// The inventory README.md is generated from lists every extractor once, each
// with an account of what it recognises, and none of them names a domain:
// nothing is chosen by one.
func TestCatalogueDescribesEveryExtractorWithoutDomains(t *testing.T) {
	reg := testCatalogueRegistry(t)
	catalogue := reg.Catalogue()
	if len(catalogue) != len(reg.extractors)+1 {
		t.Fatalf("catalogue lists %d entries, registry holds %d extractors and the fallback", len(catalogue), len(reg.extractors))
	}
	seen := make(map[string]bool, len(catalogue))
	for _, info := range catalogue {
		switch {
		case info.Name == "":
			t.Error("an extractor has no name")
		case seen[info.Name]:
			t.Errorf("%q is registered twice; a failure could not be attributed", info.Name)
		case info.Recognises == "":
			t.Errorf("%s does not say what it recognises", info.Name)
		case domainLike.MatchString(info.Recognises):
			t.Errorf("%s describes itself by a domain: %q", info.Name, info.Recognises)
		}
		seen[info.Name] = true
	}
}
