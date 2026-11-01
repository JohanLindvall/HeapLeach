// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Fixed mirrors must survive discovery as well as direct submission. The
// real host labels exercise the inventory; every request goes to this local
// proxy, whose pages, filenames and media links are synthetic.
func TestGalleryMirrorDiscoveryHarvestsGalleries(t *testing.T) {
	const board = "http://board.example.test/collection"
	hosts := []string{"pornpics.com", "pornpics.de", "www.pornpics.de"}
	pages := make(map[string]string)
	media := make(map[string]string)
	var links []string
	for i, host := range hosts {
		gallery := (&url.URL{Scheme: "http", Host: host, Path: "/galleries/synthetic/"}).String()
		image := fmt.Sprintf("https://cdni.example.test/fixture-%d.jpg", i+1)
		pages[gallery] = fmt.Sprintf(`<html><h1>Example Gallery %d</h1><script>var PP_LANG = "en";</script><a class="rel-link" href="%s">Image</a></html>`, i+1, image)
		media[gallery] = image
		links = append(links, gallery)
	}
	for _, tc := range []struct {
		name string
		urls []string
	}{
		{"primary", links[:1]},
		{"mirror", links[1:2]},
		{"mirror subdomain", links[2:]},
		{"mixed", links},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				if r.Method == http.MethodGet {
					if r.URL.String() == board {
						fmt.Fprint(w, "<html><title>Example Collection</title>")
						for _, link := range tc.urls {
							fmt.Fprintf(w, `<a href="%s">Example Gallery</a>`, link)
						}
						fmt.Fprint(w, "</html>")
						return
					}
					if page, ok := pages[r.URL.String()]; ok {
						fmt.Fprint(w, page)
						return
					}
				}
				t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				http.NotFound(w, r)
			}))
			t.Cleanup(proxy.Close)
			client, err := testClient().ThroughProxy(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.CloseIdleConnections)
			for _, source := range append(append([]string{}, tc.urls...), board, "links:"+board) {
				t.Run(source, func(t *testing.T) {
					// A fresh registry prevents direct submission from priming
					// the cache and hiding a discovery failure.
					reg := NewRegistry(&config.Config{}, client)
					res, ex, err := reg.Extract(t.Context(), source, Options{})
					if err != nil || res == nil {
						t.Fatalf("extract = %+v via %v, %v", res, ex, err)
					}
					wanted := tc.urls
					if _, direct := media[source]; direct {
						wanted = []string{source}
						if ex == nil || ex.Name() != "pornpics" {
							t.Fatalf("direct gallery recognised by %v", ex)
						}
					}
					if len(res.Files) != len(wanted) {
						t.Fatalf("files=%+v, want %d galleries", res.Files, len(wanted))
					}
					for i, link := range wanted {
						if file := res.Files[i]; file.URL != media[link] || file.Resolve != nil {
							t.Errorf("file %d=%+v, want the gallery's original image %q", i, file, media[link])
						}
					}
				})
			}
		})
	}
}

func TestGalleryMirrorDiscoveryHostBoundaries(t *testing.T) {
	reg := NewRegistry(&config.Config{}, testClient())
	from := mustParse(t, "http://board.example.test/collection")
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"pornpics.com", true},
		{"www.pornpics.com", true},
		{"pornpics.de", true},
		{"www.pornpics.de", true},
		{"WWW.PORNPICS.DE.", true},
		{"pornpics.de:8080", true},
		{"notpornpics.de", false},
		{"pornpics.de.example.test", false},
		{"pornpics.example.test", false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			u := &url.URL{Scheme: "http", Host: tc.host, Path: "/galleries/synthetic/"}
			ex, ok := reg.Follows(u, from)
			if got := ok && ex.Name() == "pornpics"; got != tc.want {
				t.Errorf("discovery=%v via %v, want gallery match=%v", ok, ex, tc.want)
			}
		})
	}
}

// A rotating alias must reach content recognition when linked from another
// page. The /f/ form avoids another platform's album shape masking a lost
// discovery match; its deferred resolver must remain untouched by harvesting.
func TestRotatingAliasDiscoveryHarvestsTheFile(t *testing.T) {
	const listing = "http://forum.example.test/list"
	const filePage = "http://bunkrr.example/f/synthetic"
	var mu sync.Mutex
	requests := make(map[string]int)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.Host + r.URL.RequestURI()
		mu.Lock()
		requests[key]++
		mu.Unlock()
		w.Header().Set(httpx.HeaderContentType, "text/html")
		switch key {
		case "GET forum.example.test/list":
			_, _ = io.WriteString(w, `<html><a href="`+filePage+`">A file</a></html>`)
		case "GET bunkrr.example/f/synthetic":
			_, _ = io.WriteString(w, `<html><h1>First Clip.bin</h1><div data-file-id="123"></div></html>`)
		default:
			t.Errorf("unexpected request during discovery: %s", key)
			http.Error(w, "no synthetic response", http.StatusBadGateway)
		}
	}))
	t.Cleanup(proxy.Close)
	client, err := testClient().ThroughProxy(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	reg := NewRegistry(&config.Config{}, client)
	res, ex, err := reg.Extract(t.Context(), "links:"+listing, Options{})
	if err != nil || ex == nil || ex.Name() != linksScheme || res == nil || len(res.Files) != 1 {
		t.Errorf("harvest: result=%+v extractor=%v err=%v; want one file", res, ex, err)
	} else if file := res.Files[0]; file.Name != "First Clip.bin" || file.Resolve == nil {
		t.Errorf("harvested file=%+v, want the named file with its deferred resolver", file)
	}
	if followed, ok := reg.Follows(mustParse(t, filePage), mustParse(t, listing)); !ok || followed.Name() != "bunkr" {
		t.Errorf("discovery chose %v, %v; want the intended extractor", followed, ok)
	}
	// Check the recognised extractor too: discovering a generic shape or
	// queueing the page itself would not restore the original file contract.
	direct, recognized, err := reg.Extract(t.Context(), filePage, Options{})
	if err != nil || recognized == nil || recognized.Name() != "bunkr" || direct == nil || len(direct.Files) != 1 || direct.Files[0].Resolve == nil {
		t.Errorf("file page: result=%+v extractor=%v err=%v; want the deferred file extractor", direct, recognized, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || requests["GET forum.example.test/list"] != 1 || requests["GET bunkrr.example/f/synthetic"] != 3 {
		t.Errorf("requests=%v; want one listing read, one file-page probe and two Referer-bearing reads, without metadata, signing or media requests", requests)
	}
}

func TestRotatingAliasDiscoveryHostBoundaries(t *testing.T) {
	reg := NewRegistry(&config.Config{}, testClient())
	from := mustParse(t, "http://forum.example.test/list")
	for _, host := range []string{
		"bunkr.example",
		"bunkrr.example",
		"bunkr-alt.example",
		"media.bunkrr.example",
		"bunkrr.example.test",
		"bunkrr.assets.bunkrr.example",
		"bunkrr.assets.bunkr.example",
		"WWW.BUNKRR.EXAMPLE.",
		"bunkrr.example:8080",
	} {
		t.Run(host, func(t *testing.T) {
			u := mustParse(t, "http://"+host+"/f/synthetic")
			if !onSiteHosts(u, siteHosts["bunkr"]) {
				t.Error("rotating alias did not match its service's host rules")
			}
			if ex, ok := reg.Follows(u, from); !ok || ex.Name() != "bunkr" {
				t.Errorf("external discovery=%v, %v; want the intended extractor", ex, ok)
			}
			home := mustParse(t, "http://"+host+"/profile")
			if ex, ok := reg.Follows(u, home); !ok || ex.Name() != "bunkr" {
				t.Errorf("same-site file discovery=%v, %v; want the shaped file", ex, ok)
			}
			if ex, ok := reg.Follows(mustParse(t, "http://"+host+"/about"), home); ok && ex.Name() == "bunkr" {
				t.Error("an unshaped same-site navigation link became a source")
			}
		})
	}
	for _, host := range []string{
		"unrelated.example",
		"notbunkrr.example",
		"otherbunkr.example",
		"bunkrr",
		"bunkrr.example.test.invalid",
		"bunkr.example.test.invalid",
	} {
		t.Run("reject "+host, func(t *testing.T) {
			u := mustParse(t, "http://"+host+"/f/synthetic")
			if onSiteHosts(u, siteHosts["bunkr"]) {
				t.Error("unrelated host or excessive suffix depth matched the rotating-alias rule")
			}
			if ex, ok := reg.Follows(u, from); ok && ex.Name() == "bunkr" {
				t.Error("unrelated host was followed as a rotating alias")
			}
		})
	}
	// Other any-TLD entries keep exact labels; prefix matching is opt-in.
	for _, service := range []string{"xhamster", "yandex", "filester"} {
		t.Run("exact label "+service, func(t *testing.T) {
			for _, tc := range []struct {
				host string
				want bool
			}{
				{service + ".example", true},
				{"www." + service + ".example.test", true},
				{service + "extra.example", false},
				{service + ".example.test.invalid", false},
			} {
				u := mustParse(t, "http://"+tc.host+"/f/synthetic")
				if got := onSiteHosts(u, siteHosts[service]); got != tc.want {
					t.Errorf("host %s match=%v, want %v", tc.host, got, tc.want)
				}
			}
		})
	}
}
