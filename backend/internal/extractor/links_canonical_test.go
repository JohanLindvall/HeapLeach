// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A canonical item need not have an extractor-wide Shape: its host and
// bounded route can establish it for an explicit harvest. Those same links
// still stay out of automatic scans, and navigation that happens to fit a
// site's short-code or two-slug pattern is not an item.
func TestExplicitHarvestCanonicalItems(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		items      []string
		navigation []string
	}{
		{"youtube", "youtu.be", []string{"/AbCdEf12345"}, []string{"/", "/account"}},
		{"dailymotion", "dai.ly", []string{"/x8abcde"}, []string{"/login", "/register"}},
		{"bilibili", "b23.tv", []string{"/AbCd12"}, []string{"/login", "/account"}},
		{"soundcloud", "on.soundcloud.com", []string{"/AbCdEf"}, []string{"/login", "/account"}},
		{"soundcloud", "soundcloud.com", []string{"/synthetic-artist/first-track", "/synthetic-artist/first-track/s-AbCd",
			"/synthetic-artist/you", "/synthetic-artist/latest", "/synthetic-artist/about", "/you/first-track"},
			[]string{"/settings/privacy", "/discover/sets", "/synthetic-artist/tracks", "/synthetic-artist/likes", "/synthetic-artist/cover.jpg"}},
		{"mixcloud", "www.mixcloud.com", []string{"/synthetic-user/first-show/", "/synthetic-user/you/", "/synthetic-user/latest/", "/you/first-show/"},
			[]string{"/genres/pop/", "/charts/ambient/", "/synthetic-user/favorites/", "/synthetic-user/uploads/"}},
		{"streamable", "streamable.com", []string{"/abcd12"}, []string{"/account", "/login", "/dashboard"}},
		{"sendvid", "sendvid.com", []string{"/synthetic1"}, []string{"/account", "/register", "/ACCOUNT", "/settings"}},
		{"lulustream", "luluvdo.com", []string{"/abcd1234wxyz"}, []string{"/account", "/upload", "/register"}},
		{"vidmoly", "vidmoly.to", []string{"/abcdefghijkl"}, []string{"/register", "/account", "/support"}},
		{"imgur", "imgur.com", []string{"/AbCdEfG"}, []string{"/account", "/upload", "/gallery"}},
		{"chevereto", "ibb.co", []string{"/AbCdEf1"}, []string{"/login", "/account", "/explore"}},
		{"kvs", "www.sexvid.xxx", []string{"/a-synthetic-clip.html"}, []string{"/login.html", "/account.html", "/categories/synthetic/"}},
	} {
		t.Run(tc.name+"/"+tc.host, func(t *testing.T) {
			reg := NewRegistry(&config.Config{}, nil)
			base := "https://" + tc.host
			from := mustParse(t, base+"/selection")
			var candidates, want []string
			for _, path := range append(tc.navigation, tc.items...) {
				candidates = append(candidates, base+path)
			}
			for _, path := range tc.items {
				want = append(want, base+path)
				if ex, ok := reg.harvests(mustParse(t, base+path), from); !ok || ex.Name() != tc.name {
					t.Errorf("harvest %s = %v, %v; want %s", path, ex, ok, tc.name)
				}
			}
			if got := supportedSources(reg, candidates, NewLinks(nil, reg), from); !slices.Equal(got, want) {
				t.Errorf("explicit sources = %q, want %q", got, want)
			}
			if got := fileLinkSources(reg, candidates, from); len(got) != 0 {
				t.Errorf("automatic scan followed same-site canonical links: %q", got)
			}
			for i := range candidates {
				candidates[i] = strings.Replace(candidates[i], tc.host, "unrelated.example.test", 1)
			}
			if got := supportedSources(reg, candidates, NewLinks(nil, reg), mustParse(t, "https://unrelated.example.test/selection")); len(got) != 0 {
				t.Errorf("unrelated site admitted canonical links: %q", got)
			}
		})
	}
}

func TestExplicitHarvestCanonicalHostBoundaries(t *testing.T) {
	reg := NewRegistry(&config.Config{}, nil)
	for _, tc := range []struct{ host, path string }{
		{"i.imgur.com", "/AbCdEfG"}, {"api.streamable.com", "/abcd12"},
		{"cdn.sendvid.com", "/synthetic1"}, {"imgur.com.unrelated.test", "/AbCdEfG"},
		{"sexvid.xxx.unrelated.test", "/a-synthetic-clip.html"},
		// These follow forms are broader listings, not canonical items.
		{"feeds.example.test", "/a-show"}, {"npostart.nl", "/a-programme/a-season"},
		{"vrtmax.be", "/a-z/a-programme/"},
	} {
		base := "https://" + tc.host
		if ex, ok := reg.harvests(mustParse(t, base+tc.path), mustParse(t, base+"/selection")); ok {
			t.Errorf("harvest %s%s = %s; want excluded", tc.host, tc.path, ex.Name())
		}
	}
}

// Route requests for actual host labels through a local proxy, so the real
// registry's admission and player extraction are tested without contacting
// a service. No navigation or media request should leave the harvest.
func TestLinksHarvestsCanonicalItems(t *testing.T) {
	for _, tc := range []struct {
		name, host, first, second string
		cap                       int
	}{
		{"bare player", "sendvid.com", "/synthetic1", "/synthetic2", 0},
		{"source cap", "sendvid.com", "/synthetic1", "/synthetic2", 1},
		{"custom KVS slug", "sexvid.xxx", "/first-synthetic-clip.html", "/second-synthetic-clip.html", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			reads := make(map[string]int)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				reads[r.URL.RequestURI()]++
				mu.Unlock()
				w.Header().Set(httpx.HeaderContentType, "text/html")
				switch r.URL.Path {
				case "/selection":
					fmt.Fprintf(w, `<html><title>Synthetic selection</title><a href="/account">Account</a>
<a href="/register">Register</a><a href="/selection#items">Self</a>
<a href="%[1]s?token=synthetic%%2Fvalue+raw">First</a><a href="%[1]s?token=synthetic%%2Fvalue+raw">Duplicate</a>
<a href="%[2]s">Second</a></html>`, tc.first, tc.second)
				case tc.first, tc.second:
					name := "First Clip"
					if r.URL.Path == tc.second {
						name = "Second Clip"
					}
					if tc.host == "sexvid.xxx" {
						fmt.Fprintf(w, `<html><title>%[1]s</title><script>var flashvars = {video_title: '%[1]s', video_url: 'https://cdn.example.test%[2]s.mp4'};</script></html>`, name, r.URL.Path)
					} else {
						fmt.Fprintf(w, `<html><meta property="og:site_name" content="Sendvid"><meta property="og:title" content="%s"><video><source id="video_source" src="https://cdn.example.test%s.mp4" type="video/mp4"></video></html>`, name, r.URL.Path)
					}
				default:
					t.Errorf("unexpected navigation or media request: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(proxy.Close)
			client, err := testClient().ThroughProxy(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.CloseIdleConnections)
			reg := NewRegistry(&config.Config{MaxSources: tc.cap}, client)
			res, ex, err := reg.Extract(t.Context(), "links:http://"+tc.host+"/selection", Options{})
			want := 2
			if tc.cap > 0 {
				want = tc.cap
			}
			if err != nil || res == nil || len(res.Files) != want || ex.Name() != "links" {
				t.Fatalf("extract = %+v via %v, %v; want %d files", res, ex, err, want)
			}
			for i, f := range res.Files {
				path := []string{tc.first, tc.second}[i]
				if f.URL != "https://cdn.example.test"+path+".mp4" || f.Dir != []string{"First Clip", "Second Clip"}[i] {
					t.Errorf("file %d = %+v; want the source's media and title", i, f)
				}
			}
			if tc.cap > 0 && res.Note != "1 of 2 links" {
				t.Errorf("note = %q; want the source cap counted", res.Note)
			}
			mu.Lock()
			defer mu.Unlock()
			if reads["/selection"] != 1 || reads[tc.first+"?token=synthetic%2Fvalue+raw"] == 0 || (reads[tc.second] > 0) != (want == 2) {
				t.Errorf("requests = %v; want only the selected sources with their original query", reads)
			}
		})
	}
}
