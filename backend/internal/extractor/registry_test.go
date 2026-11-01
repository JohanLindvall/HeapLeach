// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

func testRegistry(t *testing.T, cfg *config.Config) *Registry {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	// A nil client is enough to build every extractor; nothing here calls
	// Extract, which is the only thing that would use it.
	return NewRegistry(cfg, nil)
}

// TestRegistryNamesAreUsable guards the host labels, which are what the UI
// lists and what every error message opens with. Two extractors sharing one
// name would make a failure impossible to attribute.
func TestRegistryNamesAreUsable(t *testing.T) {
	hosts := testRegistry(t, nil).Hosts()
	if len(hosts) < 20 {
		t.Errorf("only %d hosts registered, which is fewer than expected", len(hosts))
	}

	seen := make(map[string]bool, len(hosts))
	for _, name := range hosts {
		if name == "" {
			t.Error("an extractor registered with no name")
		}
		if seen[name] {
			t.Errorf("two extractors are both named %q", name)
		}
		seen[name] = true
	}
	t.Logf("%d hosts: %v", len(hosts), hosts)
}

// TestRegistryGuessNeverComesBackEmpty is the contract Manager relies on
// for a job's label before it is read: a link of no extractor's form is the
// fallback's, never nothing.
func TestRegistryGuessNeverComesBackEmpty(t *testing.T) {
	reg := testRegistry(t, nil)
	for _, raw := range []string{
		"https://share.example.test/file/AAAAAAAA#key",
		"https://share.example.test/s/abc/file.zip",
		"https://tube.example.test/videos/1/x/",
		"https://nobody.example.test/some/file.bin",
		"http://192.0.2.1/",
	} {
		u := mustParse(t, raw)
		if got := reg.Guess(u); got == "" {
			t.Errorf("Guess(%s) is empty", raw)
		}
	}
}

// TestRegistryKnowsLinksByTheirForm pins which extractor a link's form alone
// points to — what the link scanner follows and a queued job is labelled —
// whatever host the link is on. Recognition of the page decides for real.
func TestRegistryKnowsLinksByTheirForm(t *testing.T) {
	reg := testRegistry(t, nil)
	tests := map[string]string{
		"/file/AAAAAAAA#key":            "mega",
		"/folder/BBBBBBBB#key":          "mega",
		"/scl/fi/a/b.zip":               "dropbox",
		"/file/abcdefgh12345/name/file": "mediafire",
		"/file/d/FILEID123456/view":     "drive",
		"/u/onlyfans/1/a-creator":       "coomerfans",
		"/p/1/2/onlyfans":               "coomerfans",
		"/videoembed/1234567890":        "ok.ru",
		"/123456789":                    "vimeo",
		"/funny/a-clip/12345678/":       "pornone",
		"/gallery/abc123":               "pixhost",
		"/show/9999/1_a.jpg":            "pixhost",
		"/i/abcd1234":                   "imagepond",
		"/i/a-title-slug":               "imagepond",
		"/video/preview/1234567890":     "yandex",
		"/some/file.bin":                "direct",
		"/videos/1/some-clip/":          "kvs",
	}
	for _, host := range []string{"one.example.test", "another.example.test"} {
		for path, want := range tests {
			u := mustParse(t, "https://"+host+path)
			if got := reg.Guess(u); got != want {
				t.Errorf("%s went to %q, want %q", u, got, want)
			}
		}
	}
}

// A title with a slash in it is a title, not a path: a series' episode
// counter, a band's name. It reaches the downloader whole, the slash as its
// lookalike, where taken as a path it was cut to what followed it.
func TestRegistryKeepsATitleWithASlashWhole(t *testing.T) {
	reg := &Registry{fallback: NewDirect(nil)}
	reg.extractors = []Extractor{stubExtractor{hostClaim: "tube.example.test", title: "A Series (1/7)"}}
	res, _, err := reg.Extract(context.Background(), "https://tube.example.test/v/1", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "A Series (1\u29f87)" || res.Files[0].Name != "A Series (1\u29f87).mp4" {
		t.Errorf("title %q, name %q", res.Title, res.Files[0].Name)
	}
}
