// SPDX-License-Identifier: MIT

package extractor

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

const txxxEmbedFixture = `<script>window.constants = {"project_name":"txxx.me","query":{"video_id":"1234567"}}</script>
<script src="https://static.example.test/version/static/js/embed.js"></script>`

func encodeTxxxFixture(s string) string {
	return strings.NewReplacer("A", "А", "B", "В", "C", "С", "E", "Е", "M", "М", "+", ".", "/", ",", "=", "~").Replace(base64.StdEncoding.EncodeToString([]byte(s)))
}

func TestTxxxRecognition(t *testing.T) {
	p := NewPlayer(nil, &txxxPlayer)
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"embed", txxxEmbedFixture, true},
		{"name only", `<title>TXXX</title>`, false},
		{"no bundle", strings.ReplaceAll(txxxEmbedFixture, "/static/js/embed.js", "/static/js/other.js"), false},
		{"wrong id", strings.ReplaceAll(txxxEmbedFixture, "1234567", "1234568"), false},
		{"wrong project", strings.ReplaceAll(txxxEmbedFixture, "txxx.me", "other"), false},
		{"comment", strings.ReplaceAll(txxxEmbedFixture, "window.constants", "// window.constants"), false},
		{"prose", strings.ReplaceAll(txxxEmbedFixture, "<script>window", "<p>window"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectRecognised(t, p, htmlPage(t, "https://example.test/embed/1234567/", tc.body), tc.want)
		})
	}
	expectShape(t, p, "https://example.test/embed/1234567/", true)
	expectShape(t, p, "https://example.test/embed/sample/", false)
	expectRecognised(t, p, htmlPage(t, "https://example.test/categories/", txxxEmbedFixture), false)
}

func TestTxxxSourceEncoding(t *testing.T) {
	// Pinned independently of the encoder, including Cyrillic lookalikes.
	got, err := txxxDecode("L2dldF9maWxlL3NhbXВsZS5tcDQvP3g9YSUyQmI~")
	if err != nil || got != "/get_file/sample.mp4/?x=a+b" {
		t.Fatalf("decode = %q: %v", got, err)
	}
	source := txxxSource{URL: encodeTxxxFixture("/get_file/sample.mp4/?sig=a+b"), Format: "_hq.mp4"}
	list, _ := json.Marshal([]txxxSource{source})
	keyed, _ := json.Marshal(map[string]txxxSource{"0": source})
	encoded, _ := json.Marshal(encodeTxxxFixture(string(list)))
	for _, raw := range [][]byte{list, keyed, encoded} {
		entries, err := txxxSources(raw)
		if err != nil || len(entries) != 1 || entries[0] != source {
			t.Fatalf("entries=%+v err=%v", entries, err)
		}
	}
	for _, raw := range []string{`"bad!"`, `{"error":"removed"}`, `{"1":{"video_url":"abc"}}`, `<html>blocked</html>`} {
		if _, err := txxxSources([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestTxxxRegistryRedirectAndRefresh(t *testing.T) {
	var calls atomic.Int32
	var origin string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/embed/1234567/":
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, txxxEmbedFixture)
		case "/api/json/video/86400/1000000/1234000/1234567.json":
			if r.Referer() != origin+"/embed/1234567/" {
				t.Errorf("metadata referer=%q", r.Referer())
			}
			fmt.Fprint(w, `{"video":{"video_id":"1234567","title":"An invented clip","is_private":"0"}}`)
		case "/api/videofile.php":
			if r.URL.Query().Get("video_id") != "1234567" || r.URL.Query().Get("lifetime") != "8640000" {
				t.Errorf("query=%s", r.URL.RawQuery)
			}
			n := calls.Add(1)
			json.NewEncoder(w).Encode([]txxxSource{
				{URL: encodeTxxxFixture("javascript:alert(1)"), Format: "1080p"},
				{URL: encodeTxxxFixture("/preview.jpg"), Format: "1080p"},
				{URL: encodeTxxxFixture(fmt.Sprintf("/get_file/clip.mp4/?signature=%d+sample&filename=Original.mp4", n)), Format: "_hq.mp4"},
			})
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	origin = srv.URL
	alias := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(alias.Close)
	reg := NewRegistry(&config.Config{}, testClient())
	res, ex, err := reg.Extract(t.Context(), alias.URL+"/embed/1234567/", Options{})
	if err != nil || ex == nil || ex.Name() != "txxx" || len(res.Files) != 1 {
		t.Fatalf("res=%+v ex=%v err=%v", res, ex, err)
	}
	f := res.Files[0]
	if res.Title != "An invented clip" || f.Name != "Original.mp4" || f.Headers[httpx.HeaderReferer] != srv.URL+"/embed/1234567/" || !strings.Contains(f.URL, "signature=1+sample") || f.Resolve == nil || f.Refresh == nil {
		t.Fatalf("file=%+v", f)
	}
	target, err := f.Resolve(t.Context())
	if err != nil || !strings.Contains(target.URL, "signature=2+sample") || target.Name != "Original.mp4" {
		t.Fatalf("refresh=%+v err=%v", target, err)
	}
}

func TestTxxxRefusesUnavailableMetadata(t *testing.T) {
	for _, metadata := range []string{`{"video":{"video_id":"1234567","is_private":"1"}}`, `{"video":{"video_id":"9876543"}}`, `{"error":"not_found"}`} {
		t.Run(metadata, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/embed/1234567/" {
					fmt.Fprint(w, txxxEmbedFixture)
					return
				}
				if strings.HasPrefix(r.URL.Path, "/api/json/video/") {
					fmt.Fprint(w, metadata)
					return
				}
				t.Errorf("unavailable video requested sources: %s", r.URL)
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)
			_, err := NewPlayer(testClient(), &txxxPlayer).Extract(t.Context(), mustParse(t, srv.URL+"/embed/1234567/"), Options{})
			if err == nil {
				t.Fatal("accepted unavailable video")
			}
		})
	}
}
