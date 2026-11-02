// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A transforming intermediary can return a complete page with 203. Its
// embedded media must be selected from that first response, just as with
// 200; a binary response remains a direct file under either status.
func TestRegistrySelectsMediaFromSuccessfulHTML(t *testing.T) {
	const media = "https://media.example.test/example-clip.mp4"
	for _, tc := range []struct {
		name   string
		status int
		kind   string
	}{
		{"html 200", http.StatusOK, "text/html"},
		{"html 203", http.StatusNonAuthoritativeInfo, "text/html"},
		{"binary 200", http.StatusOK, "video/mp4"},
		{"binary 203", http.StatusNonAuthoritativeInfo, "video/mp4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/article" {
					http.NotFound(w, r)
					return
				}
				if reads.Add(1) > 1 {
					http.Error(w, "page already consumed", http.StatusGone)
					return
				}
				w.Header().Set(httpx.HeaderContentType, tc.kind)
				w.WriteHeader(tc.status)
				if tc.kind == "text/html" {
					fmt.Fprintf(w, `<html><head><title>Example Clip</title></head><body><video src="%s"></video></body></html>`, media)
				}
			}))
			t.Cleanup(srv.Close)
			client := testClient()
			t.Cleanup(client.CloseIdleConnections)
			reg := NewRegistry(&config.Config{}, client)
			source := srv.URL + "/article"
			res, _, err := reg.Extract(t.Context(), source, Options{})
			if err != nil {
				t.Fatal(err)
			}
			want := source
			if tc.kind == "text/html" {
				want = media
			}
			if res == nil || len(res.Files) != 1 || res.Files[0].URL != want {
				t.Fatalf("status %d selected the wrong file: %+v", tc.status, res)
			}
			if got := reads.Load(); got != 1 {
				t.Errorf("read the source %d times, want its first complete response", got)
			}
		})
	}
}
