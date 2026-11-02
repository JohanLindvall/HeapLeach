// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"golang.org/x/net/html/atom"
)

// A named reader still needs its requested Referer when recognition saw a
// 200 soft gate. Reusing that gate must not suppress the actual page read.
func TestRegistryNamedReaderHonorsReferer(t *testing.T) {
	var invited, navigation atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/videos/synthetic/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if r.Referer() == srv.URL+"/" {
			invited.Add(1)
			fmt.Fprint(w, `<html><title>Example Clip</title><script>var flashvars = {video_title: 'Example Clip', video_url: 'https://cdn.example.test/synthetic.mp4'};</script></html>`)
			return
		}
		navigation.Add(1)
		fmt.Fprint(w, `<html><meta name="generator" content="KVS"><title>Open from this site</title></html>`)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	// An unfamiliar install still needs recognition before its reader's
	// request policy is known. Known video hosts send that policy first.
	reg := NewRegistry(&config.Config{}, client)
	res, _, err := reg.Extract(t.Context(), srv.URL+"/videos/synthetic/", Options{})
	if err != nil {
		t.Fatalf("requests carrying origin Referer=%d: %v", invited.Load(), err)
	}
	if len(res.Files) != 1 || res.Files[0].URL != "https://cdn.example.test/synthetic.mp4" || invited.Load() != 1 || navigation.Load() != 1 {
		t.Fatalf("files=%+v, origin Referer requests=%d, navigation requests=%d; want one navigation and one invited read", res.Files, invited.Load(), navigation.Load())
	}
}

// Explicit Referers name distinct representations. A reader with no Referer
// policy accepts the probe's navigation, including its refusal retry. A
// reader of the final URL compares against the request that reached it,
// including an automatically generated redirect Referer.
func TestPageAcquisitionHonorsReferer(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusFound && r.URL.Path == "/start" {
					http.Redirect(w, r, "/page", http.StatusFound)
					return
				}
				hits.Add(1)
				w.Header().Set(httpx.HeaderContentType, "text/html")
				if hits.Load() == 1 && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
					w.WriteHeader(status)
					fmt.Fprint(w, "<html><title>Try the origin Referer</title></html>")
					return
				}
				fmt.Fprintf(w, "<html><title>Referer: %s</title></html>", r.Referer())
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			u := mustParse(t, srv.URL+"/page")
			if status == http.StatusFound {
				u = mustParse(t, srv.URL+"/start")
			}
			p := probePage(t.Context(), client, u)
			if _, ok := p.Document(); !ok {
				t.Fatalf("probe failed: %v", p.Err)
			}
			ctx := withPage(t.Context(), p)
			initial, final := "", ""
			switch status {
			case http.StatusUnauthorized, http.StatusForbidden:
				initial, final = srv.URL+"/", srv.URL+"/"
			case http.StatusFound:
				final = u.String()
			}
			var emptyFetches int32
			if final != "" {
				emptyFetches = 3
			}
			for _, tc := range []struct {
				name, link string
				headers    httpx.Header
				want       string
				fetches    int32
			}{
				{"no policy", u.String(), nil, final, 0},
				{"matching submitted request", u.String(), httpx.Referer(initial), final, 0},
				{"matching final request", p.Final.String(), httpx.Header{"rEfErEr": final}, final, 0},
				{"different submitted request", u.String(), httpx.Referer(srv.URL + "/invitation"), srv.URL + "/invitation", 3},
				{"different final request", p.Final.String(), httpx.Referer(srv.URL + "/other"), srv.URL + "/other", 3},
				{"explicitly no Referer at final URL", p.Final.String(), httpx.Referer(""), "", emptyFetches},
			} {
				t.Run(tc.name, func(t *testing.T) {
					before := hits.Load()
					ask := mustParse(t, tc.link)
					want := "<html><title>Referer: " + tc.want + "</title></html>"
					body, _, err := fetchDocument(ctx, client, ask, tc.headers)
					if err != nil || body != want {
						t.Fatalf("fetchDocument = %q, %v; want %q", body, err, want)
					}
					root, _, err := fetchRoot(ctx, client, ask, tc.headers)
					if err != nil || firstText(root, atom.Title) != strings.TrimSpace("Referer: "+tc.want) {
						t.Fatalf("fetchRoot = %v, %v", root, err)
					}
					d, err := readDocument(ctx, client, ask, tc.headers)
					if err != nil || d.body != want {
						t.Fatalf("readDocument = %+v, %v; want %q", d, err, want)
					}
					if got := hits.Load() - before; got != tc.fetches {
						t.Errorf("page requests = %d, want %d", got, tc.fetches)
					}
				})
			}
		})
	}
}

// A player asking for a playlist with its own Referer must get that
// representation, even if the probe got a different, valid playlist.
func TestPlaylistReaderHonorsReferer(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.Header().Set(httpx.HeaderContentType, "application/vnd.apple.mpegurl")
		segment := "public.ts"
		if r.Referer() != "" {
			segment = "invited.ts"
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXTINF:4,\n%s\n#EXT-X-ENDLIST\n", segment)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	// An extensionless URL is initially navigated to without a Referer;
	// named manifests already send the origin on their first request.
	u := mustParse(t, srv.URL+"/playlist")
	p := probePage(t.Context(), client, u)
	ctx := withPage(t.Context(), p)
	doc, _, err := fetchPlaylist(ctx, client, u.String(), nil)
	if err != nil || !strings.Contains(doc, "public.ts") || reads.Load() != 1 {
		t.Fatalf("matching playlist = %q, %v after %d reads", doc, err, reads.Load())
	}
	doc, _, err = fetchPlaylist(ctx, client, u.String(), httpx.Referer(srv.URL+"/player"))
	if err != nil || !strings.Contains(doc, "invited.ts") || reads.Load() != 2 {
		t.Fatalf("invited playlist = %q, %v after %d reads", doc, err, reads.Load())
	}
}

// A complete pasted master is consumed once; its variants and queued
// transfers still carry the origin Referer they need.
func TestPastedMasterKeepsVariantReferer(t *testing.T) {
	var masters, variants atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "application/vnd.apple.mpegurl")
		switch r.URL.Path {
		case "/master.m3u8":
			if masters.Add(1) != 1 {
				w.WriteHeader(http.StatusGone)
				return
			}
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\n/variant.m3u8\n")
		case "/variant.m3u8":
			variants.Add(1)
			if r.Referer() != srv.URL+"/" {
				t.Errorf("variant Referer = %q, want the origin", r.Referer())
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprint(w, invitedMediaPlaylist)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	reg := NewRegistry(&config.Config{}, client)
	res, ex, err := reg.Extract(t.Context(), srv.URL+"/master.m3u8", Options{})
	if err != nil || ex == nil || ex.Name() != "hls" || len(res.Files) != 1 {
		t.Fatalf("result=%+v via %v, %v", res, ex, err)
	}
	if f := res.Files[0]; len(f.Segments) != 2 || f.Headers[httpx.HeaderReferer] != srv.URL+"/" || masters.Load() != 1 || variants.Load() != 1 {
		t.Fatalf("file=%+v, masters=%d, variants=%d", f, masters.Load(), variants.Load())
	}
}

// A named manifest uses the reader's origin Referer on its first and only
// fetch. An extensionless manifest first discovered by navigation must ask
// for the right representation when Vary says the Referer matters, including
// when a redirect selects the representation rather than the final response.
func TestPastedManifestHonorsOriginReferer(t *testing.T) {
	for _, tc := range []struct {
		name, path, vary string
		reads            int32
	}{
		{"named", "/first.m3u8", "Referer", 1},
		{"extensionless", "/playlist", "Accept-Encoding, rEfErEr", 2},
		{"wildcard", "/playlist", "*", 2},
		{"redirect", "/start", "Referer", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads, invited atomic.Int32
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.URL.Path == tc.path && r.Referer() == srv.URL+"/" {
					invited.Add(1)
				}
				if tc.name != "redirect" || r.URL.Path == "/start" {
					w.Header().Set(httpx.HeaderVary, tc.vary)
				}
				segment := "preview.ts"
				if r.Referer() == srv.URL+"/" || r.URL.Path == "/full" {
					segment = "full.ts"
				}
				if r.URL.Path == "/start" {
					destination := "/preview"
					if segment == "full.ts" {
						destination = "/full"
					}
					http.Redirect(w, r, destination, http.StatusFound)
					return
				}
				w.Header().Set(httpx.HeaderContentType, "application/vnd.apple.mpegurl")
				fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\n%s\n#EXT-X-ENDLIST\n", segment)
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			res, _, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), srv.URL+tc.path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != 1 || len(res.Files[0].Segments) != 1 || res.Files[0].Segments[0] != srv.URL+"/full.ts" {
				t.Fatalf("wrong manifest representation: %+v", res.Files)
			}
			// Other recognizers can inspect an extensionless redirect before
			// HLS claims it. Its reader must make one origin-Referer request.
			if invited.Load() != 1 {
				t.Errorf("origin-Referer acquisitions=%d, want one", invited.Load())
			}
			if tc.reads != 0 && reads.Load() != tc.reads {
				t.Errorf("manifest requests=%d, want %d", reads.Load(), tc.reads)
			}
		})
	}
}

// A manifest discovered at an extensionless URL still consumes a single-use
// navigation response when the origin declares no Referer-dependent variant.
func TestExtensionlessManifestIsFetchedOnce(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reads.Add(1) != 1 {
			http.Error(w, "single-use link spent", http.StatusGone)
			return
		}
		w.Header().Set(httpx.HeaderContentType, "application/vnd.apple.mpegurl")
		fmt.Fprint(w, invitedMediaPlaylist)
	}))
	t.Cleanup(srv.Close)
	client := testClient()
	t.Cleanup(client.CloseIdleConnections)
	res, _, err := NewRegistry(&config.Config{}, client).Extract(t.Context(), srv.URL+"/playlist", Options{})
	if err != nil || len(res.Files) != 1 || len(res.Files[0].Segments) != 2 || reads.Load() != 1 {
		t.Fatalf("result=%+v, err=%v, reads=%d; want one successful acquisition", res, err, reads.Load())
	}
}
