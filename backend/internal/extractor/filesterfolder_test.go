// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

const filesterTestGate = `<form id="password-form" method="post"><input name="nonce" value="test-nonce"><input name="password"></form>`

func filesterTestCard(slug, name, size string) string {
	return fmt.Sprintf(`<div class="file-item" data-name="%s" data-size="%s" onclick="window.location.href='/d/%s'"><img src="/thumbnail.jpg"><div class="file-meta">1 MB</div></div>`, html.EscapeString(name), size, slug)
}

func filesterTestListing(cards, pager string) string {
	return `<h1>Example &amp; Friends</h1><aside>` + filesterTestCard("outside", "Not In Folder.mp4", "100") + `</aside><div id="filesGrid">` + cards + `</div><div class="pagination">` + pager + `</div>`
}

func TestFilesterFolderPaginationAndDeferredSigning(t *testing.T) {
	var pages, signs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/f/example":
			pages.Add(1)
			switch r.URL.Query().Get("page") {
			case "":
				fmt.Fprint(w, filesterTestListing(filesterTestCard("first", "First Clip.mp4", "123456")+filesterTestCard("second", "A & B's Clip.mp4", "987654"), `<a class="page-link" href="?page=2">→</a>`))
			case "2":
				fmt.Fprint(w, filesterTestListing(filesterTestCard("first", "Duplicate.mp4", "123456")+filesterTestCard("third", "Third Clip.mp4", "invalid"), `<a class="page-link" href="?page=1">←</a>`))
			default:
				t.Errorf("unexpected page: %s", r.URL)
			}
		case filesterTokenPath:
			signs.Add(1)
			var in map[string]string
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			if in["file_slug"] != "third" || !strings.HasSuffix(r.Referer(), "/d/third") {
				t.Errorf("wrong file signed: %v, referer %q", in, r.Referer())
			}
			fmt.Fprintf(w, `{"success":true,"server":"https://cdn.example.test","file":"stored.mp4","name":"Third Clip.mp4","token":"fresh-%d"}`, signs.Load())
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := NewFilester(httpx.New("test", "en", 0, time.Second))
	res, err := f.Extract(context.Background(), mustParse(t, srv.URL+"/f/example?page=9"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var sizes []int64
	for _, file := range res.Files {
		names = append(names, file.Name)
		sizes = append(sizes, file.Size)
		if file.URL != "" || file.Resolve == nil || file.SizeApprox {
			t.Fatalf("file has eager media or approximate size: %+v", file)
		}
	}
	if res.Title != "Example & Friends" || res.Note != "" || pages.Load() != 2 || signs.Load() != 0 ||
		!reflect.DeepEqual(names, []string{"First Clip.mp4", "A & B's Clip.mp4", "Third Clip.mp4"}) ||
		!reflect.DeepEqual(sizes, []int64{123456, 987654, -1}) {
		t.Fatalf("result=%+v names=%v sizes=%v pages=%d signs=%d", res, names, sizes, pages.Load(), signs.Load())
	}
	for i := 1; i <= 2; i++ {
		target, err := res.Files[2].Resolve(context.Background())
		if err != nil || !strings.HasSuffix(target.URL, fmt.Sprintf("?token=fresh-%d", i)) {
			t.Fatalf("Resolve = %+v, %v", target, err)
		}
	}
}

func TestFilesterProtectedFolderSessionsAndExpiry(t *testing.T) {
	const password = "päss+word"
	var epoch, unlocks, signs atomic.Int32
	epoch.Store(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == filesterTokenPath {
			signs.Add(1)
			var in map[string]string
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			cookie, err := r.Cookie("folder_access_token")
			if err != nil || cookie.Value != fmt.Sprintf("%s-%d", in["file_slug"], epoch.Load()) {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"success":false,"error":"E2007","message":"Access denied"}`)
				return
			}
			fmt.Fprint(w, `{"success":true,"server":"https://cdn.example.test","file":"stored.mp4","name":"Example.mp4","token":"fresh"}`)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/f/")
		if id != "first" && id != "second" {
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		token := fmt.Sprintf("%s-%d", id, epoch.Load())
		if r.Method == http.MethodPost {
			unlocks.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			decoded, err := base64.StdEncoding.DecodeString(r.Form.Get("password"))
			parts := strings.Split(string(decoded), "|")
			if err != nil || len(parts) != 3 {
				t.Errorf("invalid password envelope: %v", err)
				fmt.Fprint(w, filesterTestGate)
				return
			}
			stamp, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil || time.Since(time.UnixMilli(stamp)).Abs() > time.Minute || parts[2] != "test-nonce" || r.Form.Get("nonce") != "test-nonce" {
				t.Error("missing nonce or millisecond timestamp")
			}
			if r.Header.Get(httpx.HeaderContentType) != httpx.ContentTypeForm || r.Referer() != "http://"+r.Host+r.URL.Path || r.Header.Get(httpx.HeaderOrigin) != "http://"+r.Host {
				t.Error("missing form headers")
			}
			if parts[0] != password {
				fmt.Fprint(w, filesterTestGate)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "folder_access_token", Value: token, Path: "/"})
			http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie("folder_access_token")
		if err != nil || cookie.Value != token {
			fmt.Fprint(w, filesterTestGate)
			return
		}
		fmt.Fprint(w, filesterTestListing(filesterTestCard(id, "Example.mp4", "100"), ""))
	}))
	defer srv.Close()
	f := NewFilester(httpx.New("test", "en", 0, time.Second))
	for _, pass := range []string{"", "wrong"} {
		if _, err := f.Extract(context.Background(), mustParse(t, srv.URL+"/f/first"), Options{Password: pass}); !errors.Is(err, ErrPasswordRequired) {
			t.Fatalf("missing/wrong password: %v", err)
		}
	}
	var files []File
	for _, id := range []string{"first", "second"} {
		res, err := f.Extract(context.Background(), mustParse(t, srv.URL+"/f/"+id), Options{Password: password})
		if err != nil || len(res.Files) != 1 {
			t.Fatalf("Extract = %+v, %v", res, err)
		}
		files = append(files, res.Files[0])
	}
	if unlocks.Load() != 3 || signs.Load() != 0 {
		t.Fatalf("unlocks=%d signs=%d", unlocks.Load(), signs.Load())
	}
	resolve := func() {
		var wg sync.WaitGroup
		for _, file := range files {
			wg.Go(func() {
				if _, err := file.Resolve(context.Background()); err != nil {
					t.Errorf("Resolve: %v", err)
				}
			})
		}
		wg.Wait()
	}
	resolve()
	if unlocks.Load() != 3 || signs.Load() != 2 {
		t.Fatal("one folder overwrote another folder's session")
	}
	epoch.Add(1)
	resolve()
	if unlocks.Load() != 5 || signs.Load() != 6 {
		t.Fatalf("expired sessions were not renewed once: unlocks=%d signs=%d", unlocks.Load(), signs.Load())
	}
}

func TestFilesterFolderLimitsAndPartialPages(t *testing.T) {
	for _, tc := range []struct {
		name, next, second, note string
		limit, files, pages      int
	}{
		{name: "cap within page", limit: 1, files: 1, pages: 1, note: "file limit"},
		{name: "cap before next page", limit: 2, next: "?page=2", files: 2, pages: 1, note: "file limit"},
		{name: "exact cap", limit: 2, files: 2, pages: 1},
		{name: "repeated page", next: "?page=2", second: "repeat", files: 2, pages: 2, note: "repeated or empty page"},
		{name: "failed page", next: "?page=2", second: "fail", files: 2, pages: 2, note: "could not read a folder page"},
		{name: "missing listing", next: "?page=2", second: "missing", files: 2, pages: 2, note: "folder listing missing"},
		{name: "foreign pager", next: "https://other.example.test/f/example?page=2", files: 2, pages: 1, note: "invalid next page"},
		{name: "other folder pager", next: "/f/other?page=2", files: 2, pages: 1, note: "invalid next page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pages atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pages.Add(1)
				if r.URL.Path != "/f/example" || pages.Load() > 2 {
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
					return
				}
				if r.URL.Query().Get("page") == "2" && tc.second == "fail" {
					http.Error(w, "unavailable", http.StatusInternalServerError)
					return
				}
				if r.URL.Query().Get("page") == "2" && tc.second == "missing" {
					fmt.Fprint(w, `<h1>Unavailable</h1>`)
					return
				}
				pager := ""
				if tc.next != "" {
					pager = `<a class="page-link" href="` + html.EscapeString(tc.next) + `">Next</a>`
				}
				fmt.Fprint(w, filesterTestListing(filesterTestCard("first", "First.mp4", "100")+filesterTestCard("second", "Second.mp4", "200"), pager))
			}))
			defer srv.Close()
			f := NewFilester(httpx.New("test", "en", 0, time.Second))
			res, err := f.Extract(context.Background(), mustParse(t, srv.URL+"/f/example"), Options{limits: Limits{Files: tc.limit}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != tc.files || pages.Load() != int32(tc.pages) || !strings.Contains(res.Note, tc.note) || (tc.note == "" && res.Note != "") || res.Title != "Example & Friends" {
				t.Fatalf("result=%+v pages=%d", res, pages.Load())
			}
		})
	}
}

func TestFilesterSubfoldersKeepDirectoriesAndStopCycles(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		if pages.Load() > 2 {
			t.Error("folder cycle was followed")
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/f/root":
			fmt.Fprint(w, filesterTestListing(filesterTestCard("first", "First.mp4", "100"), "")+`<div id="subfoldersGrid"><a class="subfolder-item" href="/f/child"><span class="subfolder-name">Second Folder (1/2)</span></a></div>`)
		case "/f/child":
			fmt.Fprint(w, filesterTestListing(filesterTestCard("second", "Second.mp4", "200"), "")+`<div id="subfoldersGrid"><a class="subfolder-item" href="/f/root"><span class="subfolder-name">Parent</span></a></div>`)
		default:
			t.Errorf("unexpected page: %s", r.URL)
		}
	}))
	defer srv.Close()
	f := NewFilester(httpx.New("test", "en", 0, time.Second))
	res, err := f.Extract(context.Background(), mustParse(t, srv.URL+"/f/root"), Options{limits: Limits{Files: 2}})
	if err != nil || len(res.Files) != 2 || res.Files[0].Dir != "" || res.Files[1].Dir != "Second Folder (1_2)" || pages.Load() != 2 || res.Note != "" {
		t.Fatalf("result=%+v pages=%d err=%v", res, pages.Load(), err)
	}
}

func TestFilesterFolderRejectsMissingAndMalformedPages(t *testing.T) {
	for _, doc := range []string{
		`<h1>Not found</h1>`,
		`<form id="password-form"><input name="password"></form>`,
		filesterTestListing("", ""),
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, doc) }))
		f := NewFilester(httpx.New("test", "en", 0, time.Second))
		_, err := f.Extract(context.Background(), mustParse(t, srv.URL+"/f/example"), Options{Password: "test"})
		srv.Close()
		if err == nil {
			t.Errorf("accepted a missing or malformed folder: %s", doc)
		}
	}
}
