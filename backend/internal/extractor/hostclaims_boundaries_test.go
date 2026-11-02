// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// claimProxyClient keeps host-dependent dispatch tests on loopback while
// preserving the submitted host. The proxy never opens an upstream connection.
func claimProxyClient(t *testing.T, handler http.HandlerFunc) *httpx.Client {
	t.Helper()
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)
	client, err := httpx.New("claim-test", "en-US", 0, time.Second).ThroughProxy(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

// TestHostClaimBoundariesPreserveDirectResults exercises the full registry:
// a shared domain cannot turn a binary endpoint, an archived page or a
// document export into an unrelated API request.
func TestHostClaimBoundariesPreserveDirectResults(t *testing.T) {
	const video = `<title>A page</title><video src="https://media.example.test/clip.mp4"></video>`
	for _, tc := range []struct {
		name, host, path, query, kind, body, want string
	}{
		{"model download", "civitai.com", "/api/download/models/42", "type=Model", "application/octet-stream", "", ""},
		{"other model page", "civitai.com", "/collections/42", "", "text/html", video, "https://media.example.test/clip.mp4"},
		{"archived page", "web.archive.org", "/web/20000101000000/https://example.test/article", "", "text/html", video, "https://media.example.test/clip.mp4"},
		{"archive subdomain item lookalike", "web.archive.org", "/details/synthetic-item", "", "text/html", video, "https://media.example.test/clip.mp4"},
		{"archive home page", "archive.org", "/", "", "text/html", video, "https://media.example.test/clip.mp4"},
		{"document export", "docs.google.com", "/document/d/invented-document-key/export", "format=txt", "text/plain", "Example export", ""},
		{"spreadsheet export", "docs.google.com", "/spreadsheets/d/invented-document-key/export", "format=csv&gid=0", "text/csv", "Column\nValue\n", ""},
		{"document endpoint lookalike", "docs.google.com", "/u/2/uc-extra", "id=invented-document-key", "application/octet-stream", "", ""},
		{"transfer storage", "download.wetransfer.com", "/container/v1/synthetic", "signature=example", "application/octet-stream", "", ""},
		{"other transfer page", "wetransfer.com", "/about", "", "text/html", video, "https://media.example.test/clip.mp4"},
		{"video CDN item lookalike", "cdn-a.streamable.com", "/abc1234", "", "video/mp4", "", ""},
		{"audio subdomain", "ondemand.npr.org", "/synthetic-object", "", "audio/mpeg", "", ""},
		{"image subdomain", "image.pbs.org", "/synthetic-object", "", "image/jpeg", "", ""},
		{"other broadcaster page", "www.pbs.org", "/about/synthetic-report", "", "text/html", video, "https://media.example.test/clip.mp4"},
		{"news article outside player", "www.srf.ch", "/news/synthetic-report", "", "text/html", video, "https://media.example.test/clip.mp4"},
		{"file outside player", "www.srf.ch", "/synthetic-object", "", "application/octet-stream", "", ""},
		{"unrelated extensionless file", "media.example.test", "/synthetic-object", "", "application/octet-stream", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests, unexpected atomic.Int32
			client := claimProxyClient(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method == http.MethodConnect || r.URL.Hostname() != tc.host || r.URL.Path != tc.path || r.URL.RawQuery != tc.query {
					unexpected.Add(1)
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				w.Header().Set(httpx.HeaderContentType, tc.kind)
				fmt.Fprint(w, tc.body)
			})
			reg := NewRegistry(&config.Config{}, client)
			target := (&url.URL{Scheme: "http", Host: tc.host, Path: tc.path, RawQuery: tc.query}).String()
			want := tc.want
			if want == "" {
				want = target
			}
			res, ex, err := reg.Extract(t.Context(), target, Options{})
			if err != nil || ex != reg.fallback {
				t.Fatalf("result %+v from %v: %v; want the direct fallback", res, ex, err)
			}
			if len(res.Files) != 1 || res.Files[0].URL != want || res.Files[0].Resolve != nil {
				t.Errorf("files %+v; want one file at %s without a deferred resolver", res.Files, want)
			}
			if requests.Load() == 0 || unexpected.Load() != 0 {
				t.Errorf("requests = %d, unexpected = %d; want only the submitted endpoint", requests.Load(), unexpected.Load())
			}
		})
	}
}

// TestHostClaimBoundariesKeepUnprobedRoutes protects canonical, legacy and
// refusal routes while narrowing the host claims. In particular, Civitai
// posts and old short links need not match the automatic-discovery Shape.
func TestHostClaimBoundariesKeepUnprobedRoutes(t *testing.T) {
	var requests atomic.Int32
	client := claimProxyClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected probe", http.StatusBadRequest)
	})
	reg := NewRegistry(&config.Config{}, client)
	for _, tc := range []struct{ host, path, query, want string }{
		{"civitai.com", "/images/42", "", "civitai"},
		{"civitai.com", "/posts/42", "", "civitai"},
		{"civitai.com", "/models/42/example-model", "", "civitai"},
		{"civitai.com", "/user/example-user/videos", "", "civitai"},
		{"archive.org", "/details/synthetic-item", "", "archive.org"},
		{"WWW.ARCHIVE.ORG.:443", "/download/synthetic-item/example.bin", "", "archive.org"},
		{"archive.org", "/metadata/synthetic-item", "", "archive.org"},
		{"drive.google.com", "/file/d/invented-document-key/view", "", "drive"},
		{"drive.google.com", "/drive/folders/invented-folder-key", "", "drive"},
		{"drive.usercontent.google.com", "/download", "id=invented-document-key", "drive"},
		{"docs.google.com", "/uc", "id=invented-document-key", "drive"},
		{"DOCS.GOOGLE.COM.", "/u/2/uc/", "id=invented-document-key", "drive"},
		{"docs.google.com", "/uc", "", "drive"},
		{"wetransfer.com", "/downloads/ExampleID/ExampleHash", "", "wetransfer"},
		{"wetransfer.com", "/downloads/missing-hash", "", "wetransfer"},
		{"we.tl", "/t-ExampleCode", "", "wetransfer"},
		{"we.tl", "/legacy-short-code", "", "wetransfer"},
		{"streamable.com", "/abc1234", "", "streamable"},
		{"WWW.STREAMABLE.COM.:443", "/e/abc1234", "", "streamable"},
	} {
		u := &url.URL{Scheme: "https", Host: tc.host, Path: tc.path, RawQuery: tc.query}
		ex, page := reg.identify(t.Context(), u)
		if ex.Name() != tc.want || page != nil {
			t.Errorf("%s: %s, probed %v; want %s with no probe", u, ex.Name(), page != nil, tc.want)
		}
	}
	if requests.Load() != 0 {
		t.Errorf("made %d requests; want no probes", requests.Load())
	}
}

// Known page hosts keep their native error handling even when the page
// carries no recognisable player. Narrowing CDN/article claims must not
// redirect a removed native programme to the generic page scanner.
func TestHostClaimBoundariesKeepNativeRefusals(t *testing.T) {
	for _, tc := range []struct{ host, path, query, want string }{
		{"npr.org", "/synthetic-story", "", "npr"},
		{"WWW.NPR.ORG.:80", "/synthetic-story", "", "npr"},
		{"www.pbs.org", "/video/synthetic-programme/extra/", "", "pbs"},
		{"player.pbs.org", "/widget/partnerplayer/1234567/", "", "pbs"},
		{"www.pbs.org", "/show/synthetic-programme/episodes/", "", "pbs"},
		{"www.srf.ch", "/play/tv/synthetic-programme", "", "srf"},
		{"www.srf.ch", "/PLAY/tv/synthetic-programme", "", "srf"},
		{"www.srf.ch", "/synthetic-programme", "urn=urn:srf:video:12345678-1234-1234-1234-123456789012", "srf"},
	} {
		t.Run(tc.want+tc.path, func(t *testing.T) {
			client := claimProxyClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				http.Error(w, "This programme is no longer available", http.StatusNotFound)
			})
			reg := NewRegistry(&config.Config{}, client)
			u := &url.URL{Scheme: "http", Host: tc.host, Path: tc.path, RawQuery: tc.query}
			ex, page := reg.identify(t.Context(), u)
			if ex.Name() != tc.want || page == nil || page.Status != http.StatusNotFound {
				t.Fatalf("extractor=%s page=%+v; want %s with its refused page", ex.Name(), page, tc.want)
			}
		})
	}
}

// The episode API works independently of the web application's page.
// Both accepted section spellings must reach that reader without probing
// HTML, whether the shell is recognisable, unmarked or unavailable.
func TestCaseVariantProgrammeLinksSkipTheirPage(t *testing.T) {
	for _, section := range []string{"sounds", "Sounds"} {
		for _, page := range []struct {
			name, body string
			status     int
		}{
			{"unmarked", `<html><title>Synthetic shell</title></html>`, http.StatusOK},
			{"unavailable", `<html><title>Unavailable</title></html>`, http.StatusForbidden},
			{"recognisable", `<html><title>BBC Sounds</title><script src="/sounds/web/sounds-web-spa/app.js"></script></html>`, http.StatusOK},
		} {
			t.Run(section+"/"+page.name, func(t *testing.T) {
				var requests atomic.Int32
				client := claimProxyClient(t, func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.Header().Set(httpx.HeaderContentType, "text/html")
					w.WriteHeader(page.status)
					fmt.Fprint(w, page.body)
				})
				u := &url.URL{Scheme: "http", Host: "www.bbc.co.uk", Path: "/" + section + "/play/p000epis"}
				ex, probe := NewRegistry(&config.Config{}, client).identify(t.Context(), u)
				if ex.Name() != "bbcsounds" || probe != nil || requests.Load() != 0 {
					t.Fatalf("extractor=%s probe=%+v requests=%d; want the API reader with no page probe", ex.Name(), probe, requests.Load())
				}
			})
		}
	}
}

// TestRajceStorageClaimsPreserveMetadata keeps the storage fast path reachable
// through the registry for named files and extensionless videos alike. Host
// and path lookalikes still use the ordinary direct-file metadata.
func TestRajceStorageClaimsPreserveMetadata(t *testing.T) {
	var requests atomic.Int32
	client := claimProxyClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected storage probe", http.StatusBadRequest)
	})
	reg := NewRegistry(&config.Config{}, client)
	for _, tc := range []struct {
		host, path, name string
		storage          bool
	}{
		{"img1.rajce.idnes.cz", "/storage/images/photo.jpg", "photo.jpg", true},
		{"img1.rajce.idnes.cz", "/storage/video/scene.clip.jpg", "scene.clip.mp4", true},
		{"IMG02.RAJCE.NET.:443", "/storage/video/scene.mp4", "scene.mp4", true},
		{"img3.rajce.net", "/storage/video/123", "123.mp4", true},
		{"www.rajce.idnes.cz", "/storage/images/photo.jpg", "photo.jpg", false},
		{"img.rajce.net", "/storage/images/photo.jpg", "photo.jpg", false},
		{"imgone.rajce.net", "/storage/video/scene.jpg", "scene.jpg", false},
		{"img1.rajce.idnes.cz.example.test", "/storage/video/scene.jpg", "scene.jpg", false},
		{"img1.rajce.idnes.cz", "/assets/video/scene.jpg", "scene.jpg", false},
	} {
		u := &url.URL{Scheme: "https", Host: tc.host, Path: tc.path, RawQuery: "ver=2"}
		res, ex, err := reg.Extract(t.Context(), u.String(), Options{})
		if err != nil || len(res.Files) != 1 {
			t.Fatalf("%s: result %+v from %v: %v; want one file", u, res, ex, err)
		}
		wantExtractor, referer := "direct", util.Origin(u)+"/"
		if tc.storage {
			wantExtractor, referer = "rajce", rajceRoot+"/"
		}
		file := res.Files[0]
		if ex.Name() != wantExtractor || res.Title != tc.name || file.Name != tc.name ||
			file.URL != u.String() || file.Headers[httpx.HeaderReferer] != referer {
			t.Errorf("%s: title %q, file %+v from %s; want %s named %q with Referer %q",
				u, res.Title, file, ex.Name(), wantExtractor, tc.name, referer)
		}
	}
	if requests.Load() != 0 {
		t.Errorf("made %d requests; storage and named files need none", requests.Load())
	}
}
