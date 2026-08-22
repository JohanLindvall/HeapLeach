package extractor

import (
	"net/url"
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

// TestRegistryFindNeverReturnsNil is the contract Manager relies on: an
// unrecognised host is the direct-link extractor's job, not an error.
func TestRegistryFindNeverReturnsNil(t *testing.T) {
	reg := testRegistry(t, nil)
	for _, raw := range []string{
		"https://mega.nz/file/AAAAAAAA#key",
		"https://www.dropbox.com/s/abc/file.zip",
		"https://thisvid.com/videos/1/x/",
		"https://nobody.example.test/some/file.bin",
		"http://192.0.2.1/",
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if ex := reg.Find(u); ex == nil {
			t.Errorf("Find(%s) returned nil", raw)
		}
	}
}

// TestRegistryRoutesToTheExpectedHost pins the routing for the hosts added
// most recently, where a Match that is too broad or too narrow is easiest to
// introduce and hardest to notice.
func TestRegistryRoutesToTheExpectedHost(t *testing.T) {
	reg := testRegistry(t, nil)
	tests := map[string]string{
		"https://mega.nz/file/AAAAAAAA#key":               "mega",
		"https://mega.co.nz/folder/BBBBBBBB#key":          "mega",
		"https://www.dropbox.com/scl/fi/a/b.zip":          "dropbox",
		"https://www.mediafire.com/file/k/name/file":      "mediafire",
		"https://drive.google.com/file/d/ID/view":         "drive",
		"https://streamtape.com/e/ID":                     "streamtape",
		"https://dood.to/e/ID":                            "doodstream",
		"https://mixdrop.co/f/ID":                         "mixdrop",
		"https://thisvid.com/videos/1/x/":                 "thisvid",
		"https://coomerfans.com/u/onlyfans/1/a-creator":   "coomerfans",
		"https://coomerfans.com/p/1/2/onlyfans":           "coomerfans",
		"https://ok.ru/video/1234567890":                  "ok.ru",
		"https://odnoklassniki.ru/video/1234567890":       "ok.ru",
		"https://vimeo.com/123456789":                     "vimeo",
		"https://player.vimeo.com/video/123456789":        "vimeo",
		"https://pornone.com/funny/a-clip/1/":             "pornone",
		"https://pixhost.to/gallery/abc123":               "pixhost",
		"https://pixhost.to/show/9999/1_a.jpg":            "pixhost",
		"https://www.suvobox.com/a/abcd1234":              "suvobox",
		"https://suvobox.com/f/abcd1234":                  "suvobox",
		"https://www.imagepond.net/i/abcd1234":            "imagepond",
		"https://imagepond.net/i/a-title-slug":            "imagepond",
		"https://yandex.kz/video/preview/1234567890":      "yandex",
		"https://nobody.example.test/some/file.bin":       "direct",
		"https://nobody.example.test/videos/1/some-clip/": "direct",
	}
	for raw, want := range tests {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := reg.Find(u).Name(); got != want {
			t.Errorf("%s went to %q, want %q", raw, got, want)
		}
	}
}

// TestRegistryAcceptsExtraKVSHosts covers the escape hatch for a platform
// whose site list outruns any release.
func TestRegistryAcceptsExtraKVSHosts(t *testing.T) {
	reg := testRegistry(t, &config.Config{
		ExtraHosts: map[string][]string{config.FamilyKVS: {"tube.example.test"}},
	})

	u, err := ParseURL("https://tube.example.test/videos/1/x/")
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Find(u).Name(); got != "tube" {
		t.Errorf("a host from HEAPLEACH_KVS_HOSTS went to %q, want the kvs extractor", got)
	}
}

// TestRegistryFindIsHostOnly documents why the KVS sniff exists at all: Find
// never fetches anything, so a host nobody named can only be recognised
// later, by the fallback, from the page itself.
func TestRegistryFindIsHostOnly(t *testing.T) {
	reg := testRegistry(t, nil)
	u, err := url.Parse("https://unknown-tube.example.test/videos/1/a-clip/")
	if err != nil {
		t.Fatal(err)
	}
	if _, isDirect := reg.Find(u).(*Direct); !isDirect {
		t.Error("an unregistered host was matched by something other than the fallback")
	}
}
