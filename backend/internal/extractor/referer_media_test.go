// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A successful response must identify a file, not just stop being HTML.
// Its body stays unread: the fixture sends headers and then waits for the
// request to close, so trying to read media would time out the extraction.
func TestDirectRefererRetryClassifiesFiles(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		status     int
		file       bool
	}{
		{"video", "video/mp4", 200, true},
		{"audio", "audio/mpeg", 200, true},
		{"image", "image/png", 200, true},
		{"binary", "application/octet-stream", 200, true},
		{"archive", "application/zip", 200, true},
		{"document file", "application/pdf", 200, true},
		{"type parameters", "Video/MP4; codecs=avc1", 200, true},
		{"partial media", "video/mp4", 206, true},
		{"no content", "video/mp4", 204, false},
		{"reset content", "video/mp4", 205, false},
		{"refused media", "video/mp4", 404, false},
		{"JSON refusal", "application/problem+json", 200, false},
		{"XML refusal", "application/xml", 200, false},
		{"script", "application/javascript", 200, false},
		{"plain text", "text/plain", 200, false},
		{"missing type", "", 200, false},
		{"invalid type", "not-a-media-type", 200, false},
		{"invalid parameters", "video/mp4; broken", 200, false},
		{"HLS manifest", "application/vnd.apple.mpegurl", 200, false},
		{"audio manifest", "audio/x-mpegurl", 200, false},
		{"DASH manifest", "application/dash+xml", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			closed := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RequestURI() != "/feature?signature=synthetic%2Ftoken+raw" {
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
					return
				}
				if reads.Add(1) == 1 {
					w.Header().Set(httpx.HeaderContentType, "text/html")
					fmt.Fprint(w, `<html><title>Open from this site</title></html>`)
					return
				}
				if r.Referer() != "http://"+r.Host+"/" {
					t.Errorf("retry Referer=%q", r.Referer())
				}
				w.Header()[httpx.HeaderContentType] = []string{tc.kind}
				w.WriteHeader(tc.status)
				w.(http.Flusher).Flush()
				// These statuses cannot carry a body, so a client need not
				// close its connection after receiving their headers.
				if tc.status == http.StatusNoContent {
					closed <- struct{}{}
					return
				}
				<-r.Context().Done()
				closed <- struct{}{}
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			input := srv.URL + "/feature?signature=synthetic%2Ftoken+raw"
			res, _, err := markedRegistry(client, markedSite{}).Extract(ctx, input, Options{})
			if tc.file {
				if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != input {
					t.Fatalf("result=%+v, err=%v; want the original signed URL", res, err)
				}
				if res.Title != "feature" || res.Files[0].Name != "feature" || res.Files[0].Size != -1 {
					t.Errorf("direct-file metadata changed: %+v", res)
				}
				if res.Files[0].Headers[httpx.HeaderReferer] != srv.URL+"/" {
					t.Errorf("working Referer lost: %v", res.Files[0].Headers)
				}
			} else if res != nil || !errors.Is(err, errPlainPage) {
				t.Fatalf("result=%+v, err=%v; want the original page refusal", res, err)
			}
			if reads.Load() != 2 {
				t.Errorf("requests=%d; want one probe and one Referer retry", reads.Load())
			}
			select {
			case <-closed:
			case <-ctx.Done():
				t.Fatal("the retry did not close its unread body")
			}
		})
	}
}

func TestDirectMediaRetryPreservesRedirectAndRequestHeaders(t *testing.T) {
	var entryReads, mediaReads atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/entry":
			if r.URL.RawQuery != "signature=synthetic%2Ftoken+raw" {
				// A recogniser may ask about a canonical page separately.
				// Only the signed submitted URL can serve this fixture.
				http.NotFound(w, r)
				return
			}
			entryReads.Add(1)
			if r.Referer() == "" {
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprint(w, `<html><title>Open from this site</title></html>`)
				return
			}
			http.Redirect(w, r, "/media?token=temporary%2Fvalue+raw", http.StatusFound)
		case "/media":
			mediaReads.Add(1)
			if r.URL.RawQuery != "token=temporary%2Fvalue+raw" || r.Referer() != srv.URL+"/" {
				t.Errorf("redirect lost identity: URL=%s Referer=%q", r.URL, r.Referer())
			}
			for name, value := range httpx.Navigate {
				if r.Header.Get(name) != value {
					t.Errorf("request lost %s=%q", name, value)
					http.Error(w, "missing navigation header", http.StatusForbidden)
					return
				}
			}
			w.Header().Set(httpx.HeaderContentType, "video/mp4")
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := testClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ctx = httpx.WithAdmission(ctx, httpx.NewAdmission(1))
	input := srv.URL + "/entry?signature=synthetic%2Ftoken+raw"
	res, _, err := NewRegistry(&config.Config{}, client).Extract(ctx, input, Options{})
	if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != input {
		t.Fatalf("result=%+v, err=%v; want the submitted URL, not its temporary redirect", res, err)
	}
	if entryReads.Load() != 2 || mediaReads.Load() != 1 {
		t.Fatalf("entry requests=%d, media headers=%d; want 2 and 1", entryReads.Load(), mediaReads.Load())
	}
	// Replay only the returned file's request metadata. The fixture sends
	// no media bytes, and extraction must not need another request first.
	req, err := client.NewRequest(ctx, http.MethodGet, res.Files[0].URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Files[0].Headers.Apply(req)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get(httpx.HeaderContentType) != "video/mp4" {
		t.Errorf("file request: status=%d, type=%s", resp.StatusCode, resp.Header.Get(httpx.HeaderContentType))
	}
}
