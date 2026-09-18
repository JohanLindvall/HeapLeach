package extractor

import (
	"net/url"
	"testing"
)

func TestKVSSearchPath(t *testing.T) {
	cases := []struct {
		raw     string
		query   string
		listing string
	}{
		{
			raw:     "https://tube.example.test/search/some-query/",
			query:   "some-query",
			listing: "https://tube.example.test/search/some-query/",
		},
		// A later page pasted from a theme that pages by path means the
		// whole search, as a member's section means the member.
		{
			raw:     "https://tube.example.test/search/some-query/3/",
			query:   "some-query",
			listing: "https://tube.example.test/search/some-query/",
		},
		// The escaping the site wrote is the escaping the site gets back;
		// only the title is decoded.
		{
			raw:     "https://tube.example.test/search/one%2Btwo%5D/",
			query:   "one+two]",
			listing: "https://tube.example.test/search/one%2Btwo%5D/",
		},
		// The form-style search, before the theme redirects it.
		{
			raw:     "https://tube.example.test/search/?q=plain",
			query:   "plain",
			listing: "https://tube.example.test/search/plain/",
		},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		query, listing, ok := kvsSearchPath(u)
		if !ok || query != tc.query || listing != tc.listing {
			t.Errorf("kvsSearchPath(%q) = %q, %q, %v; want %q, %q, true",
				tc.raw, query, listing, ok, tc.query, tc.listing)
		}
	}
}

func TestKVSSearchPathRejectsEverythingElse(t *testing.T) {
	for _, raw := range []string{
		"https://tube.example.test/search/",
		"https://tube.example.test/search/?q=%20",
		"https://tube.example.test/videos/11/a-clip/",
		"https://tube.example.test/members/4242/",
		"https://tube.example.test/",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, ok := kvsSearchPath(u); ok {
			t.Errorf("kvsSearchPath(%q) claimed a search", raw)
		}
	}
}
