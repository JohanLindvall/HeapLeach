// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func fileLinksTestRegistry(t *testing.T, doc string) (*Registry, string) {
	t.Helper()
	mux := http.NewServeMux()
	asked := keep2ShareStatusAPI(t, mux)
	t.Cleanup(func() {
		for key, n := range asked() {
			if n > 1 {
				t.Errorf("duplicate file metadata request: %s", key)
			}
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html; charset=utf-8")
		fmt.Fprint(w, doc)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	doc = keep2ShareShells(t).Replace(doc)
	client := testClient()
	reg := NewRegistry(&config.Config{}, client)
	keep2ShareAPIs(reg, srv.URL)
	return reg, srv.URL
}

func TestPlainPageHarvestsFileLinksBeforePreviewAndKeepsPacing(t *testing.T) {
	for _, page := range []string{"/", "/thread", "/thread.html", "/thread.PHP"} {
		t.Run(page, func(t *testing.T) {
			reg, base := fileLinksTestRegistry(t, `<html><title>Shared Files | Example Board</title>
			<video src="https://media.example.test/preview.mp4"></video>
			<a href="https://keep2share.example.test/file/first/First.bin?ref=board">first</a>
			<a href="http://keep2share-alias.example.test/file/first#download">same file</a>
			<a href="//fileboom.example.test/file/first">same ID, the other service's file</a>
			<p>https://fileboom.example.test/file/second.</p>
			<a href="https://fileboom.example.test/file/second/Second.bin">duplicate</a>
			<a href="https://keep2share.example.test/premium">upgrade</a>
			<a href="https://fileboom.example.test/folder/example">folder</a>
			<a href="https://keep2share.example.test/file/">missing ID</a>
			<a href="/download/third">unsupported</a>
			<a href="/thread/next">navigation</a></html>`)
			res, ex, err := reg.Extract(context.Background(), base+page, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if ex.Name() != "direct" || res.Title != "Shared Files" || len(res.Files) != 3 || res.Note != "" {
				t.Fatalf("plain page did not expand unique file links: ex=%s result=%+v", ex.Name(), res)
			}
			// The services keep their file stores apart: the same id on
			// each is two files.
			for i, want := range []struct{ name, group string }{
				{"first.bin", "keep2share"}, {"first.bin", "fileboom"}, {"second.bin", "fileboom"},
			} {
				f := res.Files[i]
				if f.Name != want.name || f.URL != "" || f.Resolve == nil || f.Dir == "" || f.Size != 1234 ||
					f.Pace == nil || *f.Pace != (Pace{Streams: 1, Files: 1, Group: want.group, PerRoute: true}) {
					t.Fatalf("file %d lost host download behavior: %+v", i, f)
				}
			}
		})
	}
}

func TestPlainPageFileLinksRespectCapsAndReportUnavailableSources(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits Limits
		count  int
		err    bool
	}{
		{"partial", Limits{}, 2, false},
		{"source cap", Limits{Sources: 2}, 1, false},
		{"file cap", Limits{Files: 1}, 1, false},
		{"all unavailable", Limits{Sources: 1}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, base := fileLinksTestRegistry(t, `<html><title>Shared files</title>
			<a href="https://fileboom.example.test/file/removed">removed</a>
			<a href="https://keep2share.example.test/file/first">first</a>
			<a href="https://fileboom.example.test/file/second">second</a></html>`)
			res, _, err := reg.Extract(context.Background(), base+"/thread.html", Options{limits: tc.limits})
			if tc.err {
				if err == nil || !strings.Contains(err.Error(), "could be read: fileboom: file is unavailable") {
					t.Fatalf("unavailable links fell back to downloading the HTML: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != tc.count || res.Note == "" {
				t.Fatalf("missing cap or partial result: %+v", res)
			}
		})
	}
}

func TestPlainPageScansOtherRegisteredHostsWithoutCrawlingHTML(t *testing.T) {
	var navCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if r.URL.Path != "/thread.html" {
			navCalls++
			fmt.Fprint(w, `<a href="https://albums.example.test/never-followed">another source</a>`)
			return
		}
		fmt.Fprint(w, `<title>Shared albums</title>
		<a href="/another-page.html">ordinary webpage</a>
		<a href="https://albums.example.test/first">supported album</a>
		<a href="https://albums.example.test/first">duplicate album</a>
		<p>https://encrypted.example.test/file/second#decryption-key</p>`)
	}))
	defer srv.Close()
	reg := NewRegistry(&config.Config{}, testClient())
	albums := &linksStub{hostClaim: "albums.example.test", files: 2, title: "Album"}
	encrypted := &linksStub{hostClaim: "encrypted.example.test", files: 1, title: "Encrypted"}
	reg.extractors = append(reg.extractors, albums, encrypted)
	res, _, err := reg.Extract(context.Background(), srv.URL+"/thread.html", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 3 || len(albums.passwords) != 1 || len(encrypted.passwords) != 1 || navCalls != 0 {
		t.Fatalf("registered hosts were skipped or HTML was crawled: files=%d albums=%d encrypted=%d navigation=%d",
			len(res.Files), len(albums.passwords), len(encrypted.passwords), navCalls)
	}
	for _, f := range res.Files {
		if f.Resolve == nil || f.Dir == "" {
			t.Fatalf("source lost its resolver or directory: %+v", f)
		}
	}
	const keyed = "https://encrypted.example.test/file/second#decryption-key"
	if got := fileLinkSources(reg, []string{keyed, keyed, srv.URL + "/another-page.html"}, nil); len(got) != 1 || got[0] != keyed {
		t.Fatalf("source filtering lost a fragment or followed plain HTML: %v", got)
	}
}

func TestPlainPageWithoutFileLinksKeepsMediaAndDirectFallbacks(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"media", `<video src="https://media.example.test/clip.mp4"></video>`, "https://media.example.test/clip.mp4"},
		// A page with nothing on it is refused as the page it is, never
		// queued as the file.
		{"ordinary page", `<a href="https://fileboom.example.test/premium">upgrade</a>`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, base := fileLinksTestRegistry(t, tc.doc)
			u := base + "/thread"
			res, _, err := reg.Extract(context.Background(), u, Options{})
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "is a web page, not a file") {
					t.Fatalf("page was queued: %+v, %v", res, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != 1 || res.Files[0].URL != tc.want {
				t.Fatalf("fallback changed: %+v", res)
			}
		})
	}
}

// A path may look supported even when the page is an unrelated frontend.
// Its own navigation must not defeat the generic media fallback or use up
// the source cap, including after a redirect and with fragment anchors.
func TestPlainPageIgnoresSelfLinks(t *testing.T) {
	for _, route := range []string{"/sample/user/creator", "/posts/42", "/b/thread/42/", "/wiki/Category:Examples"} {
		for _, linked := range []bool{false, true} {
			for _, redirect := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/linked=%t/redirect=%t", route, linked, redirect), func(t *testing.T) {
					mux := http.NewServeMux()
					asked := keep2ShareStatusAPI(t, mux)
					mux.HandleFunc("/api/v1/", http.NotFound)
					mux.HandleFunc("/api/v3/", http.NotFound)
					download := ""
					if linked {
						download = keep2ShareShells(t).Replace(`<a href="https://keep2share.example.test/file/first">Download</a>`)
					}
					mux.HandleFunc("/entry", func(w http.ResponseWriter, r *http.Request) {
						http.Redirect(w, r, route, http.StatusFound)
					})
					mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set(httpx.HeaderContentType, "text/html")
						fmt.Fprintf(w, `<title>Example page</title><a href="%s">Self</a><a href="%s#files">Files</a>
<script type="application/ld+json">{"url":"http://%s%s"}</script><video src="https://media.example.test/clip.mp4"></video>`, route, route, r.Host, route)
						if redirect {
							fmt.Fprint(w, `<a href="/entry">Original address</a>`)
						}
						fmt.Fprint(w, download)
					})
					srv := httptest.NewServer(mux)
					defer srv.Close()
					reg := platformRegistry()
					keep2ShareAPIs(reg, srv.URL)
					start := route
					if redirect {
						start = "/entry"
					}
					res, _, err := reg.Extract(t.Context(), srv.URL+start, Options{limits: Limits{Sources: 1}})
					if err != nil {
						t.Fatal(err)
					}
					if len(res.Files) != 1 || res.Note != "" {
						t.Fatalf("self-links changed the result: %+v", res)
					}
					if linked {
						if res.Files[0].Resolve == nil || len(asked()) != 1 {
							t.Fatalf("self-link used the source cap: %+v", res)
						}
					} else if res.Files[0].URL != "https://media.example.test/clip.mp4" {
						t.Fatalf("lost embedded media: %+v", res.Files[0])
					}
				})
			}
		}
	}
}

func TestPlainPageSniffDoesNotHarvestBinaryResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "application/octet-stream")
		fmt.Fprint(w, `<a href="https://fileboom.example.test/file/first">not a page</a>`)
	}))
	defer srv.Close()
	reg := NewRegistry(&config.Config{}, testClient())
	u, _ := ParseURL(srv.URL + "/signed")
	res, read, err := reg.fallback.(*Direct).pageSniff(context.Background(), u, Options{})
	if res != nil || read || err != nil {
		t.Fatalf("binary response was harvested: %+v %v", res, err)
	}
}
