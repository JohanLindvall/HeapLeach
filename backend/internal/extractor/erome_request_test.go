// SPDX-License-Identifier: MIT

package extractor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A navigation refusal belongs to that request. The album's reader has
// its own Referer policy, including when the server omits Vary entirely.
func TestEromeAlbumUsesReaderStatus(t *testing.T) {
	for _, vary := range []string{"", "Referer"} {
		t.Run("vary="+vary, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				status int
			}{
				{"available", http.StatusOK},
				{"deleted", http.StatusGone},
				{"forbidden", http.StatusForbidden},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var navigations, readers atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/a/SYNTHETIC42" {
							http.NotFound(w, r)
							return
						}
						w.Header().Set(httpx.HeaderContentType, "text/html")
						if vary != "" {
							w.Header().Set(httpx.HeaderVary, vary)
						}
						if r.Referer() != eromeReferer+"/" {
							navigations.Add(1)
							w.WriteHeader(http.StatusGone)
							_, _ = w.Write([]byte(`<html><title>Gone - EroMe</title></html>`))
							return
						}
						readers.Add(1)
						w.WriteHeader(tc.status)
						_, _ = w.Write([]byte(eromeAlbumPage))
					}))
					defer srv.Close()
					client := testClient()
					defer client.CloseIdleConnections()
					reg := NewRegistry(&config.Config{}, client)
					res, ex, err := reg.Extract(t.Context(), srv.URL+"/a/SYNTHETIC42", Options{})
					if ex == nil || ex.Name() != "erome" {
						t.Fatalf("extractor=%v, error=%v; want album reader", ex, err)
					}
					switch tc.status {
					case http.StatusOK:
						if err != nil || res == nil || len(res.Files) != 3 || res.Files[0].URL != "https://media.example.test/v/clip1.mp4" {
							t.Errorf("result=%+v, error=%v; want original album media", res, err)
						}
					case http.StatusGone:
						if res != nil || err == nil || !strings.Contains(err.Error(), "deleted") || strings.Contains(err.Error(), "<") {
							t.Errorf("result=%+v, error=%v; want plain deletion error", res, err)
						}
					default:
						if res != nil || !httpx.HasStatus(err, tc.status) || strings.Contains(err.Error(), "deleted") {
							t.Errorf("result=%+v, error=%v; want reader status %d", res, err, tc.status)
						}
					}
					if navigations.Load() != 1 || readers.Load() != 1 {
						t.Errorf("navigation requests=%d, reader requests=%d; want one of each", navigations.Load(), readers.Load())
					}
				})
			}
		})
	}
}

func TestEromeAlbumReusesMatchingDeletion(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Referer() != eromeReferer+"/" {
			t.Errorf("Referer=%q, want the album reader's policy", r.Referer())
		}
		w.Header().Set(httpx.HeaderContentType, "text/html")
		if reads.Add(1) != 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`<html><title>Gone - EroMe</title></html>`))
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	u := mustParse(t, srv.URL+"/a/SYNTHETIC42")
	p := newProbePage(t.Context(), client, u)
	probeGet(t.Context(), client, p, httpx.Referer(eromeReferer+"/"))
	p.finishProbe(t.Context())
	if p.Status != http.StatusGone || p.Err != nil {
		t.Fatalf("probe status=%d, error=%v; want the completed reader refusal", p.Status, p.Err)
	}
	res, err := NewErome(client).Extract(withPage(t.Context(), p), u, Options{})
	if res != nil || err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Errorf("result=%+v, error=%v; want deletion from the matching probe", res, err)
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("requests=%d, want the matching refusal reused", got)
	}
}
