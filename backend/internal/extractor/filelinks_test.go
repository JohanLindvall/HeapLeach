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
			<a href="//fileboom.example.test/file/first">same ID, the other service's domain</a>
			<p>https://fileboom.example.test/file/second.</p>
			<a href="https://fileboom.example.test/file/second/Second.bin">duplicate</a>
			<a href="https://keep2share.example.test/premium">upgrade</a>
			<a href="https://fileboom.example.test/folder/example">folder</a>
			<a href="https://keep2share.example.test/file/">missing ID</a>
			<a href="https://elsewhere.example.test/download/third">unsupported</a>
			<a href="/thread/next">navigation</a></html>`)
			res, ex, err := reg.Extract(context.Background(), base+page, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if ex.Name() != "direct" || res.Title != "Shared Files" || len(res.Files) != 2 || res.Note != "" {
				t.Fatalf("plain page did not expand unique file links: ex=%s result=%+v", ex.Name(), res)
			}
			for i, want := range []struct{ name, group string }{
				{"first.bin", "keep2share"}, {"second.bin", "keep2share"},
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
				if err == nil || !strings.Contains(err.Error(), "resolved to a file") {
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
	if got := fileLinkSources(reg, []string{keyed, keyed, srv.URL + "/another-page.html"}); len(got) != 1 || got[0] != keyed {
		t.Fatalf("source filtering lost a fragment or followed plain HTML: %v", got)
	}
}

func TestPlainPageWithoutFileLinksKeepsMediaAndDirectFallbacks(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"media", `<video src="https://media.example.test/clip.mp4"></video>`, "https://media.example.test/clip.mp4"},
		{"ordinary page", `<a href="https://fileboom.example.test/premium">upgrade</a>`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, base := fileLinksTestRegistry(t, tc.doc)
			u := base + "/thread"
			res, _, err := reg.Extract(context.Background(), u, Options{})
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want == "" {
				want = u
			}
			if len(res.Files) != 1 || res.Files[0].URL != want {
				t.Fatalf("fallback changed: %+v", res)
			}
		})
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
	res, err := reg.fallback.(*Direct).pageSniff(context.Background(), u, Options{})
	if res != nil || err != nil {
		t.Fatalf("binary response was harvested: %+v %v", res, err)
	}
}
