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
)

func TestGoonBoxReadsOriginalWithoutChallengedViewer(t *testing.T) {
	var pageReads, mediaReads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/images/AbCd12":
			if r.Referer() != "http://"+r.Host+"/img/AbCd12" || r.Header.Get(httpx.HeaderAccept) != httpx.AcceptJSON {
				t.Errorf("API request identity: Referer=%q Accept=%q", r.Referer(), r.Header.Get(httpx.HeaderAccept))
			}
			w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
			fmt.Fprint(w, `{"image":{"encoded_id":"AbCd12","original_filename":"First Picture.jpg","original_url":"https://media.example.test/original.jpg","medium_url":"https://media.example.test/original.md.jpg","thumb_url":"https://media.example.test/original.th.jpg","size_bytes":42042},"album_images":[{"original_url":"https://media.example.test/unrelated.jpg"}],"is_deleted":false}`)
		default:
			if strings.HasPrefix(r.URL.Path, "/img/") {
				pageReads.Add(1)
			} else {
				mediaReads.Add(1)
			}
			w.Header().Set(httpx.HeaderCFMitigated, "challenge")
			http.Error(w, "Checking your browser", http.StatusForbidden)
		}
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	reg := NewRegistry(&config.Config{}, client)
	aliasClaims(reg, "goonbox", "127.0.0.1")
	for _, path := range []string{"/img/AbCd12", "/img/First-Picture.AbCd12", "/img/AbCd12/?source=share"} {
		res, ex, err := reg.Extract(t.Context(), srv.URL+path, Options{})
		if err != nil || ex == nil || ex.Name() != "goonbox" || res == nil || len(res.Files) != 1 {
			t.Fatalf("%s: result=%+v extractor=%v error=%v", path, res, ex, err)
		}
		file := res.Files[0]
		if file.Name != "First Picture.jpg" || file.URL != "https://media.example.test/original.jpg" ||
			file.Size != 42042 || file.SizeApprox || file.Headers[httpx.HeaderReferer] != srv.URL+"/" || file.Resolve != nil {
			t.Errorf("original metadata changed: %+v", file)
		}
	}
	if pageReads.Load() != 0 || mediaReads.Load() != 0 {
		t.Errorf("extraction requested viewer=%d media=%d times", pageReads.Load(), mediaReads.Load())
	}
}

func TestGoonBoxRefusesMissingOrInvalidOriginals(t *testing.T) {
	for _, tc := range []struct {
		name, body, problem string
		status              int
	}{
		{"removed", `{"is_deleted":true}`, "removed", 200},
		{"deleted timestamp", `{"image":{"encoded_id":"AbCd12","deleted_at":"2026-01-01T00:00:00Z"}}`, "removed", 200},
		{"missing", `{"image":null}`, "does not describe", 200},
		{"wrong item", `{"image":{"encoded_id":"Other2","original_url":"https://media.example.test/other.jpg"}}`, "does not describe", 200},
		{"thumbnail only", `{"image":{"encoded_id":"AbCd12","thumb_url":"https://media.example.test/preview.jpg"}}`, "no original", 200},
		{"invalid URL", `{"image":{"encoded_id":"AbCd12","original_url":"file:///tmp/picture.jpg"}}`, "no original", 200},
		{"malformed JSON", `{"image":`, "decode json", 200},
		{"not found", `{"message":"Image not found"}`, "", 404},
		{"refused", `{"message":"Not public"}`, "", 403},
		{"server error", `{"message":"Unavailable"}`, "", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			reg := NewRegistry(&config.Config{}, client)
			aliasClaims(reg, "goonbox", "127.0.0.1")
			res, ex, err := reg.Extract(t.Context(), srv.URL+"/img/AbCd12", Options{})
			if err == nil || ex == nil || ex.Name() != "goonbox" || res != nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("result=%+v error=%v; want %q", res, err, tc.problem)
			}
			if tc.status != http.StatusOK && !httpx.HasStatus(err, tc.status) {
				t.Errorf("lost HTTP status %d: %v", tc.status, err)
			}
		})
	}
}

func TestGoonBoxUnknownSizeAndNameUseOriginalURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"image":{"encoded_id":"AbCd12","original_url":"/files/Original%20Picture.png"}}`)
	}))
	defer srv.Close()
	res := extractOK(t, NewGoonBox(testClient()), srv.URL+"/img/AbCd12", Options{})
	file := res.Files[0]
	if file.Name != "Original Picture.png" || file.Size != -1 || file.URL != srv.URL+"/files/Original%20Picture.png" {
		t.Fatalf("fallback metadata: %+v", file)
	}
}

func TestGoonBoxRecognitionAndHostBoundaries(t *testing.T) {
	g := NewGoonBox(nil)
	doc := `<meta property="og:site_name" content="GoonBox"><script type="module" src="/assets/index-example.js"></script><div id="root"></div>`
	expectRecognised(t, g, htmlPage(t, "https://images.example.test/img/AbCd12", doc), true)
	for _, bad := range []string{
		`<title>GoonBox</title><div id="root"></div>`,
		`<title>Other app</title><script src="/assets/index-example.js"></script><div id="root"></div>`,
	} {
		expectRecognised(t, g, htmlPage(t, "https://images.example.test/img/AbCd12", bad), false)
	}
	for _, path := range []string{"/img/AbCd12", "/img/First.Picture.AbCd12", "/img/AbCd12/"} {
		expectShape(t, g, "https://images.example.test"+path, true)
	}
	reg := NewRegistry(&config.Config{}, nil)
	for _, path := range []string{"/", "/login", "/a/AbCd12", "/img/", "/img/AbCd12/other", "/images/original.jpg", "/api/images/AbCd12"} {
		u := mustParse(t, "https://goonbox.cr"+path)
		expectShape(t, g, u.String(), false)
		if ex := reg.hostClaim(u, claimFirst, claimFirstForm, claimFallback); ex != nil {
			t.Errorf("non-viewer path claimed by %s: %s", ex.Name(), path)
		}
		if _, err := g.Extract(t.Context(), u, Options{}); err == nil {
			t.Errorf("non-viewer path accepted: %s", path)
		}
	}
	for _, host := range []string{"goonbox.cr", "www.goonbox.cr"} {
		u := mustParse(t, "https://"+host+"/img/AbCd12")
		if ex := reg.hostClaim(u, claimFirstForm); ex == nil || ex.Name() != "goonbox" {
			t.Errorf("viewer not claimed on %s", host)
		}
		if ex, ok := reg.Follows(u, mustParse(t, "https://forum.example.test/thread")); !ok || ex.Name() != "goonbox" {
			t.Errorf("viewer not followed on %s", host)
		}
	}
	if ex := reg.hostClaim(mustParse(t, "https://goonbox.cr.example.test/img/AbCd12"), claimFirstForm); ex != nil {
		t.Errorf("lookalike host claimed by %s", ex.Name())
	}
}
