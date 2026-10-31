// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestFileJumpKeepsStableDownloadEndpointAndRoundedSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/s/test-share" || r.URL.Query().Get("access") != "download" {
			t.Errorf("extraction followed the preview or spent a storage signature: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<title>Fallback - Shared file</title>
		<div class="title">First &amp; Second.mp4</div>
		<div class="meta">Size: 1,234.5 KB • Type: video/mp4</div>
		<a href="https://unrelated.example.test/s/test-share/download">advert</a>
		<a href="/s/other-share/download">related file</a>
		<video src="/s/test-share/content"></video>
		<a class="btn" href="/s/test-share/download">Download</a>`)
	}))
	defer srv.Close()
	f := NewFileJump(testClient())
	for _, suffix := range []string{"", "/", "/download", "/content"} {
		res := extractOK(t, f, srv.URL+"/s/test-share"+suffix+"?access=download#preview", Options{})
		if res.Title != "First & Second.mp4" || len(res.Files) != 1 {
			t.Fatalf("lost share metadata: %+v", res)
		}
		file := res.Files[0]
		if file.URL != srv.URL+"/s/test-share/download" || file.Resolve != nil ||
			file.Size != 1264128 || !file.SizeApprox || file.Headers[httpx.HeaderReferer] != srv.URL+"/s/test-share?access=download" {
			t.Fatalf("download endpoint, rounded size or referer changed: %+v", file)
		}
	}
}

func TestFileJumpRejectsUnavailableDownloads(t *testing.T) {
	for _, doc := range []string{
		`<div class="error">Share expired</div>`,
		`<form><input type="password" name="password"></form>`,
		`<video src="/s/test-share/content"></video>`,
		`<a href="/s/test-share/download" disabled>Download</a>`,
		`<a href="https://unrelated.example.test/s/test-share/download">Download</a>`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, doc) }))
		f := NewFileJump(testClient())
		u, _ := url.Parse(srv.URL + "/s/test-share")
		_, err := f.Extract(context.Background(), u, Options{})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "no public download link") {
			t.Fatalf("accepted a share without its download button: %v", err)
		}
	}
	for _, path := range []string{"/", "/login", "/s", "/s/test-share/other", "/s/test-share/download/other"} {
		u, _ := url.Parse("https://example.test" + path)
		if _, err := NewFileJump(nil).Extract(context.Background(), u, Options{}); err == nil {
			t.Errorf("accepted non-share %s", path)
		}
	}
}

func TestPlainPageFindsRegisteredFileJumpShares(t *testing.T) {
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<title>First Document.pdf - Shared file</title><a href="/s/test-share/download">Download</a>`)
	}))
	defer files.Close()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<a href="%s/s/test-share?access=download">FileJump share</a>`, files.URL)
	}))
	defer page.Close()
	reg := NewRegistry(&config.Config{}, testClient())
	res, _, err := reg.Extract(context.Background(), strings.Replace(page.URL, "127.0.0.1", "localhost", 1)+"/post.html", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "First Document.pdf" || res.Files[0].URL != files.URL+"/s/test-share/download" || res.Files[0].Size != -1 {
		t.Fatalf("plain-page FileJump handoff: %+v", res)
	}
}

func TestFileJumpRecognise(t *testing.T) {
	f := NewFileJump(nil)
	share := `<html><head><title>First Document.pdf - Shared file</title></head><body>
		<div class="title">First Document.pdf</div><a class="btn" href="/s/test-share/download">Download</a></body></html>`
	expectRecognised(t, f, htmlPage(t, "https://share.example.test/s/test-share", share), true)
	expectRecognised(t, f, htmlPage(t, "https://share.example.test/s/test-share?access=download", share), true)
	// The form, without the share's own download button: disabled, another
	// share's, or on another site.
	for _, body := range []string{
		`<html><title>First Document.pdf - Shared file</title><a href="/s/test-share/download" disabled>Download</a></html>`,
		`<html><title>First Document.pdf - Shared file</title><a href="/s/other-share/download">Download</a></html>`,
		`<html><title>First Document.pdf - Shared file</title><a href="https://unrelated.example.test/s/test-share/download">Download</a></html>`,
	} {
		expectRecognised(t, f, htmlPage(t, "https://share.example.test/s/test-share", body), false)
	}
	// The button, on a page of another form.
	expectRecognised(t, f, htmlPage(t, "https://share.example.test/files/test-share", share), false)

	expectShape(t, f, "https://share.example.test/s/test-share", true)
	expectShape(t, f, "https://share.example.test/s/test-share/download", true)
	expectShape(t, f, "https://share.example.test/s/test-share/other", false)
	expectShape(t, f, "https://share.example.test/s", false)
}
