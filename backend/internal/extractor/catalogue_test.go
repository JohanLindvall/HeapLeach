package extractor

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func testCatalogueRegistry(t *testing.T) *Registry {
	t.Helper()
	cfg := &config.Config{UserAgent: config.DefaultUserAgent, Language: config.DefaultLanguage}
	return NewRegistry(cfg, httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), 0, 0))
}

// The inventory README.md is generated from is only as good as the contract
// behind it: every extractor has to say which sites it covers, or be named
// for the one site it does, and what it says has to be a domain rather than
// a label.
func TestCatalogueCoversEveryExtractorHonestly(t *testing.T) {
	reg := testCatalogueRegistry(t)
	catalogue := reg.Catalogue()
	if len(catalogue) != len(reg.extractors) {
		t.Fatalf("catalogue lists %d extractors, registry holds %d", len(catalogue), len(reg.extractors))
	}

	seen := make(map[string]bool, len(catalogue))
	for _, info := range catalogue {
		if info.Name == "" {
			t.Error("an extractor has no name")
		}
		if seen[info.Name] {
			t.Errorf("%q is registered twice; a failure could not be attributed", info.Name)
		}
		seen[info.Name] = true

		if info.ByShape != (len(info.Sites) == 0) {
			t.Errorf("%s: ByShape=%v with %d sites; the two must agree", info.Name, info.ByShape, len(info.Sites))
		}
		for _, site := range info.Sites {
			switch {
			case site == "":
				t.Errorf("%s lists an empty site", info.Name)
			case strings.Contains(site, "://"), strings.ContainsAny(site, " \t"):
				t.Errorf("%s lists %q, which is not a domain", info.Name, site)
			case strings.ToLower(site) != site:
				t.Errorf("%s lists %q; domains are lower-case", info.Name, site)
			}
		}
	}

	// An extractor that skips SiteLister is inventoried under its name, so
	// that name had better be the host — "ok.ru", not "okru".
	for _, e := range reg.extractors {
		if _, ok := e.(SiteLister); ok {
			continue
		}
		if !strings.Contains(e.Name(), ".") {
			t.Errorf("%q implements no Sites and is not named for its domain, so the "+
				"inventory would advertise the label as the host", e.Name())
		}
	}
}

// A hostSet is one declaration answering both Match and Sites, and the point
// of that is that the two cannot drift. What can still go wrong is ordering:
// a site an extractor declares that an earlier extractor also claims would be
// listed under one name and routed to another. So every declared host is
// checked against the registry as a whole, not just its own Match.
func TestEveryDeclaredHostRoutesToItsExtractor(t *testing.T) {
	reg := testCatalogueRegistry(t)
	checked := 0
	for _, e := range reg.extractors {
		hosts, ok := declaredHosts(e)
		if !ok {
			continue
		}
		if len(hosts) == 0 {
			t.Errorf("%s embeds an empty hostSet, so it can never match anything", e.Name())
			continue
		}
		for _, host := range hosts {
			for _, raw := range []string{"https://" + host + "/x", "https://www." + host + "/x"} {
				u, err := url.Parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				if !e.Match(u) {
					t.Errorf("%s declares %s but does not match %s", e.Name(), host, raw)
				}
				if got := reg.Find(u); got != e {
					t.Errorf("%s is routed to %s, though %s declares it", raw, got.Name(), e.Name())
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no extractor declares a hostSet; the test is checking nothing")
	}
}

// declaredHosts reads the embedded hostSet off an extractor, reporting false
// for the ones that match by hand.
func declaredHosts(e Extractor) (hostSet, bool) {
	v := reflect.ValueOf(e)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return nil, false
	}
	f := v.Elem().FieldByName("hostSet")
	if !f.IsValid() {
		return nil, false
	}
	// Read element by element: Interface() refuses an unexported field, and
	// the embedding is unexported by design.
	hosts := make(hostSet, 0, f.Len())
	for i := range f.Len() {
		hosts = append(hosts, f.Index(i).String())
	}
	return hosts, true
}

// A hostSet claims the domain and everything under it, and nothing beside it.
func TestHostSetMatchesSubdomainsOnly(t *testing.T) {
	h := hostSet{"example.test", "other.test"}
	tests := map[string]bool{
		"https://example.test/a":       true,
		"https://www.example.test/a":   true,
		"https://cdn.other.test:8443/": true,
		"https://notexample.test/":     false,
		"https://example.test.evil/":   false,
		"https://example.example/":     false,
	}
	for raw, want := range tests {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := h.Match(u); got != want {
			t.Errorf("Match(%s) = %v, want %v", raw, got, want)
		}
	}

	sites := h.Sites()
	sites[0] = "changed"
	if h[0] != "example.test" {
		t.Error("Sites handed out the declaration itself rather than a copy")
	}
}
