// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Discovery already admits feed-host links. A successful file response must
// survive strict expansion, while unrelated raw links and navigation stay out.
func TestHarvestRetainsConfirmedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, mode, host, path, kind, body string
		status                             int
		wantFile, wantProbe                bool
	}{
		{"explicit audio", "explicit", "feeds.example.test", "/episode.mp3", "audio/mpeg", "", 200, true, true},
		{"automatic audio", "automatic", "feeds.example.test", "/episode.mp3", "audio/mpeg", "", 200, true, true},
		{"extensionless audio", "explicit", "feeds.example.test", "/episode", "audio/mpeg", "", 200, true, true},
		{"nonfeed XML file", "explicit", "feeds.example.test", "/catalogue.xml", "application/xml", "<catalogue/>", 200, true, true},
		{"JSON file", "explicit", "feeds.example.test", "/catalogue.json", "application/json", `{}`, 200, true, true},
		{"pasted feed-host file", "pasted", "feeds.example.test", "/episode.mp3", "audio/mpeg", "", 200, true, true},
		{"pasted unrelated file", "pasted", "media.example.test", "/episode.mp3", "audio/mpeg", "", 200, true, false},
		{"unrelated harvested file", "explicit", "media.example.test", "/episode.mp3", "audio/mpeg", "", 200, false, false},
		{"HTML navigation", "explicit", "feeds.example.test", "/browse", "text/html", `<html><video src="https://media.example.test/preview.mp4"></video></html>`, 200, false, true},
		{"HTML at a file URL", "explicit", "feeds.example.test", "/episode.mp3", "text/html", `<html>Sign in</html>`, 200, false, true},
		{"plain text", "explicit", "feeds.example.test", "/episode.mp3", "text/plain", "not confirmed media", 200, false, true},
		{"missing type", "explicit", "feeds.example.test", "/episode.mp3", "", "", 200, false, true},
		{"invalid type", "explicit", "feeds.example.test", "/episode.mp3", "invalid", "", 200, false, true},
		{"extensionless JSON", "explicit", "feeds.example.test", "/catalogue", "application/json", `{}`, 200, false, true},
		{"extensionless XML", "explicit", "feeds.example.test", "/catalogue", "application/xml", `<catalogue/>`, 200, false, true},
		{"no content", "explicit", "feeds.example.test", "/episode.mp3", "audio/mpeg", "", 204, false, true},
		{"reset content", "explicit", "feeds.example.test", "/episode.mp3", "audio/mpeg", "", 205, false, true},
		{"gone file", "explicit", "feeds.example.test", "/episode.mp3", "audio/mpeg", "", 410, false, true},
		{"invalid playlist", "explicit", "feeds.example.test", "/episode.m3u8", "application/vnd.apple.mpegurl", "not a playlist", 200, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fileRequests atomic.Int32
			fileURL := "http://" + tc.host + tc.path
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host == "publisher.example.test" {
					w.Header().Set(httpx.HeaderContentType, "text/html")
					fmt.Fprintf(w, `<html><title>Example Files</title><body><a href="%s">Example File</a></body></html>`, fileURL)
					return
				}
				if r.Host == tc.host && r.URL.Path == tc.path {
					fileRequests.Add(1)
					// An explicit empty header prevents net/http from guessing.
					w.Header()[httpx.HeaderContentType] = []string{tc.kind}
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
					return
				}
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Redacted())
				http.NotFound(w, r)
			}))
			defer server.Close()
			base := httpx.New("test", "en-US", 0, time.Second)
			defer base.CloseIdleConnections()
			client, err := base.ThroughProxy(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			source := "links:http://publisher.example.test/page"
			if tc.mode == "automatic" {
				source = "http://publisher.example.test/page"
			}
			if tc.mode == "pasted" {
				source = fileURL
			}
			res, _, err := NewRegistry(&config.Config{}, client).Extract(context.Background(), source, Options{})
			if tc.wantFile {
				if err != nil || res == nil || len(res.Files) != 1 || res.Files[0].URL != fileURL {
					t.Fatalf("got %+v, %v; want the original file URL %s", res, err, fileURL)
				}
			} else if err == nil || res != nil {
				t.Fatalf("unsupported response became a file: %+v, %v", res, err)
			}
			if got := fileRequests.Load() > 0; got != tc.wantProbe {
				t.Errorf("file probed = %v, want %v", got, tc.wantProbe)
			}
		})
	}
}
