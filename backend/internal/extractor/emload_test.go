// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func emloadTestPage(server, token string) string {
	return fmt.Sprintf(`<div class="dl-file">
		<h2 class="filename wordwrap">First &amp; Second.mp4</h2><h2 class="filesize">1,234.5 KB</h2>
		<a class="init-free-download">Free Download</a>
		<div class="free-downloader" data-srv="%s" data-dl="%s">
			<div id="dl-captcha" data-key="synthetic-site-key"></div>
		</div><div class="premium-plans">Premium only extras</div></div>`, url.PathEscape(server), token)
}

func TestEmloadRefreshesTokensCookiesAndRedirectedReferer(t *testing.T) {
	var pages, transfers atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file/test-id":
			n := pages.Add(1)
			http.Redirect(w, r, "/v2/file/test-id/old-name.mp4?visit="+strconv.Itoa(int(n)), http.StatusFound)
		case "/v2/file/test-id/old-name.mp4":
			visit := r.URL.Query().Get("visit")
			http.SetCookie(w, &http.Cookie{Name: "visit", Value: visit, Path: "/"})
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, emloadTestPage("http://"+r.Host+"/storage+files/", "token+"+visit+"/="))
		case "/storage+files/download.php":
			transfers.Add(1)
			token := "token+" + strconv.Itoa(int(pages.Load())) + "/="
			cookie, err := r.Cookie("visit")
			wantReferer := "http://" + r.Host + "/v2/file/test-id/old-name.mp4?visit=" + strconv.Itoa(int(pages.Load()))
			if r.URL.Query().Get("accesstoken") != token || r.Header.Get(httpx.HeaderReferer) != wantReferer ||
				err != nil || cookie.Value != strconv.Itoa(int(pages.Load())) || r.Header.Get(httpx.HeaderRange) != "bytes=0-1023" {
				t.Error("transfer lost the fresh token, cookie, redirected referer or range")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set(httpx.HeaderContentType, "application/octet-stream")
			w.Header().Set(httpx.HeaderContentRange, "bytes 0-1023/2048")
			w.WriteHeader(http.StatusPartialContent)
			fmt.Fprint(w, strings.Repeat("x", 1024))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := httpx.New("test-agent", "en-US", 0, time.Second)
	u, _ := url.Parse(srv.URL + "/file/test-id#ignored")
	res, err := NewEmload(client).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "test-id" || len(res.Files) != 1 || pages.Load() != 0 {
		t.Fatalf("discovery fetched a page before leasing a route: %+v pages=%d", res, pages.Load())
	}
	f := res.Files[0]
	if f.URL != "" || f.Resolve == nil || f.External != "" || f.Name != res.Title || f.Size != -1 || f.SizeApprox ||
		f.Pace == nil || f.Pace.Group != "emload" || f.Pace.Files != 1 || f.Pace.Streams != 1 || !f.Pace.PerRoute || transfers.Load() != 0 {
		t.Fatalf("metadata, deferred resolution or pacing: %+v", f)
	}
	for range 2 {
		target, err := f.Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if target.Size != -1 || target.Name != "First & Second.mp4" {
			t.Fatalf("rounded size became exact or filename changed: %+v", target)
		}
		req, _ := client.NewRequest(context.Background(), http.MethodGet, target.URL, nil)
		for k, v := range target.Headers {
			req.Header.Set(k, v)
		}
		req.Header.Set(httpx.HeaderRange, "bytes=0-1023")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || len(body) != 1024 || resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("range response: bytes=%d status=%d err=%v", len(body), resp.StatusCode, err)
		}
	}
	if pages.Load() != 2 || transfers.Load() != 2 {
		t.Fatalf("pages=%d transfers=%d; want two fresh resolutions", pages.Load(), transfers.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Resolve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestEmloadRejectsRestrictedAndMalformedPages(t *testing.T) {
	valid := emloadTestPage("https://storage.example.test/", "synthetic-token")
	for _, tc := range []struct{ name, doc, want string }{
		{"missing", `<div class="nofile">Unavailable</div>`, "no longer available"},
		{"password", `<input class="_filepassw" type="password">`, "password-protected"},
		{"maintenance", `<div class="download sv4"></div>`, "maintenance"},
		{"size limit", `<div class="dl-file"><div class="free-downloader" data-srv="dlsize" data-dl="dlsize"></div></div>`, "size limit"},
		{"server blocked", `<div class="dl-file"><div class="free-downloader" data-srv="serverblocked" data-dl="serverblocked"></div></div>`, "maintenance"},
		// Premium pages also contain a plausible token, even below the size
		// limit. Neither the token nor the filename makes the share free.
		{"premium with token", strings.Replace(valid, `<a class="init-free-download">Free Download</a>`, `This link is for premium only user.`, 1), "Premium account"},
		{"no button with upsell", strings.Replace(valid, `<a class="init-free-download">Free Download</a>`, "", 1), "no free download button"},
		{"disabled", strings.Replace(valid, `class="init-free-download"`, `class="init-free-download" disabled`, 1), "no free download button"},
		{"aria disabled", strings.Replace(valid, `class="init-free-download"`, `class="init-free-download" aria-disabled="true"`, 1), "no free download button"},
		{"no token", emloadTestPage("https://storage.example.test/", ""), "invalid free download"},
		{"bad escape", strings.Replace(valid, url.PathEscape("https://storage.example.test/"), "%zz", 1), "invalid download server"},
		{"relative server", emloadTestPage("/storage/", "token"), "invalid free download"},
		{"wrong scheme", emloadTestPage("javascript:alert(1)", "token"), "invalid free download"},
		{"credentials", emloadTestPage("https://user:pass@storage.example.test/", "token"), "invalid free download"},
		{"server query", emloadTestPage("https://storage.example.test/?next=", "token"), "invalid free download"},
		{"page shell", `<title>EMLOAD</title>`, "no file download"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tc.doc) }))
			defer srv.Close()
			u, _ := url.Parse(srv.URL + "/v2/file/test-id/First.mp4")
			res, err := NewEmload(httpx.New("test-agent", "en-US", 0, time.Second)).Extract(context.Background(), u, Options{})
			if err != nil {
				t.Fatal(err)
			}
			target, err := res.Files[0].Resolve(context.Background())
			if err == nil || target != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("target=%+v error=%v; want %q", target, err, tc.want)
			}
		})
	}
	for _, path := range []string{"/", "/v2/", "/v2/file", "/file", "/v2/folder/test-id", "/v2/file/test-id/name/extra"} {
		u, _ := url.Parse("https://example.test" + path)
		if _, err := NewEmload(nil).Extract(context.Background(), u, Options{}); err == nil {
			t.Errorf("accepted non-file %s", path)
		}
	}
}

func TestEmloadRechecksAvailabilityBeforeTransfer(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, emloadTestPage("https://storage.example.test/", "old-token"))
		} else {
			fmt.Fprint(w, `<div class="nofile">Removed</div>`)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/v2/file/test-id")
	res, err := NewEmload(httpx.New("test-agent", "en-US", 0, time.Second)).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Files[0].Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if target, err := res.Files[0].Resolve(context.Background()); target != nil || err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("reused a removed file's token: target=%+v err=%v", target, err)
	}
}

func TestEmloadDailyLimitWaitsWithoutHoldingAWorker(t *testing.T) {
	var limited atomic.Bool
	limited.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if limited.Load() {
			fmt.Fprint(w, `<div class="dl-file"><h2 class="filename">First.mp4</h2>
				<div class="free-downloader" data-srv="dlreached" data-dl="dlreached"></div></div>`)
		} else {
			fmt.Fprint(w, emloadTestPage("https://storage.example.test/", "fresh-token"))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/v2/file/test-id/First.mp4")
	res, err := NewEmload(httpx.New("test-agent", "en-US", 0, time.Second)).Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "First.mp4" || res.Files[0].Size != -1 || res.Files[0].SizeApprox {
		t.Fatalf("limited file was not queued with its metadata: %+v", res)
	}
	started := time.Now()
	target, err := res.Files[0].Resolve(context.Background())
	var wait *WaitError
	if target != nil || !errors.As(err, &wait) || wait.Until.Before(started.Add(config.EmloadDailyWait)) ||
		wait.Until.After(time.Now().Add(config.EmloadDailyWait)) || !strings.Contains(wait.Reason, "daily") {
		t.Fatalf("daily limit did not defer the file: target=%+v error=%v", target, err)
	}
	limited.Store(false)
	if target, err := res.Files[0].Resolve(context.Background()); err != nil || target == nil || !strings.Contains(target.URL, "fresh-token") {
		t.Fatalf("allowance was not rechecked: target=%+v error=%v", target, err)
	}
}

func TestPlainPageFindsRegisteredEmloadShare(t *testing.T) {
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprint(w, emloadTestPage("https://storage.example.test/", "synthetic-token"))
	}))
	defer files.Close()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(httpx.HeaderContentType, "text/html")
		fmt.Fprintf(w, `<a href="%s/v2/file/test-id/First.mp4">Shared file</a>`, files.URL)
	}))
	defer page.Close()
	reg := NewRegistry(&config.Config{}, httpx.New("test-agent", "en-US", 0, time.Second))
	res, _, err := reg.Extract(context.Background(), strings.Replace(page.URL, "127.0.0.1", "localhost", 1)+"/post.html", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "First.mp4" || res.Files[0].Resolve == nil || res.Files[0].Pace == nil || !res.Files[0].Pace.PerRoute {
		t.Fatalf("plain-page handoff lost the file's native resolver or pacing: %+v", res)
	}
}

func TestEmloadRecognise(t *testing.T) {
	e := NewEmload(nil)
	share := "https://share.example.test/v2/file/dGVzdC1pZA==/First.mp4"
	expectRecognised(t, e, htmlPage(t, share, emloadTestPage("https://storage.example.test/", "synthetic-token")), true)
	expectRecognised(t, e, htmlPage(t, "https://share.example.test/file/dGVzdC1pZA==", `<div class="nofile">File not found</div>`), true)
	// The form and the site's name, without the download panel.
	expectRecognised(t, e, htmlPage(t, share, `<html><head><title>EMLOAD</title></head><body></body></html>`), false)
	// The panel, on a page of another form.
	expectRecognised(t, e, htmlPage(t, "https://share.example.test/v2/premium", emloadTestPage("https://storage.example.test/", "t")), false)

	expectShape(t, e, share, true)
	expectShape(t, e, "https://share.example.test/v2/file/dGVzdC1pZA==", true)
	expectShape(t, e, "https://share.example.test/v2/", false)
	expectShape(t, e, "https://share.example.test/v2/file/a/b/c", false)
}
