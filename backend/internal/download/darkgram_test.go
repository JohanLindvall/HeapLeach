// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Exercise the deferred playlist through the transfer engine, including its
// late container name and the referer on every part. Every byte is synthetic
// and every request stays on this test server.
func TestDarkGramAlbumDownloadsVideoAndOriginalPhoto(t *testing.T) {
	parts := map[string][]byte{
		"/init.mp4": {0, 0, 0, 24, 'f', 't', 'y', 'p'},
		"/one.m4s":  bytes.Repeat([]byte{1, 2, 3}, 100),
		"/two.m4s":  bytes.Repeat([]byte{4, 5, 6}, 100),
		"/photo":    {0xff, 0xd8, 1, 2, 3, 0xff, 0xd9},
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/post.html" {
			w.Header().Set(httpx.HeaderContentType, "text/html")
			fmt.Fprint(w, `<html><title>Morning Walk</title><div class="darkgram-album" data-album='{"items":[
				{"id":"video-one","kind":"video","hls_url":"/master.m3u8","thumb_url":"/thumbnail.jpg"},
				{"id":"photo-one","kind":"photo","photo_url":"/photo","thumb_url":"/thumbnail.jpg"}
			]}'></div></html>`)
			return
		}
		if r.Header.Get(httpx.HeaderReferer) != srv.URL+"/post.html" {
			t.Errorf("lost page referer on %s", r.URL.Path)
		}
		if r.URL.Path == "/master.m3u8" {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4,\none.m4s\n#EXTINF:4,\ntwo.m4s\n#EXT-X-ENDLIST\n")
			return
		}
		if payload, ok := parts[r.URL.Path]; ok {
			w.Header().Set(httpx.HeaderContentType, "application/octet-stream")
			if r.URL.Path == "/photo" {
				w.Header().Set(httpx.HeaderContentType, "image/jpeg")
			}
			_, _ = w.Write(payload)
			return
		}
		t.Errorf("unexpected request: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	m := busyManager(t)
	res, ex, err := m.reg.Extract(t.Context(), srv.URL+"/post.html", extractor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("album resolved to %d files", len(res.Files))
	}
	job := &Job{ID: "album-job", Source: srv.URL + "/post.html"}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.applyResultLocked(job, ex.Name(), res)
	m.mu.Unlock()
	wants := []struct {
		name string
		body []byte
	}{
		{"Morning Walk - 001.mp4", bytes.Join([][]byte{parts["/init.mp4"], parts["/one.m4s"], parts["/two.m4s"]}, nil)},
		{"Morning Walk - 002.jpg", parts["/photo"]},
	}
	for i, item := range job.Items {
		if err := m.transfer(t.Context(), item); err != nil {
			t.Fatal(err)
		}
		want := wants[i]
		got, err := os.ReadFile(filepath.Join(m.DownloadDir(), "Morning Walk", want.name))
		if err != nil || !bytes.Equal(got, want.body) {
			t.Fatalf("%s: got %d bytes, error %v; want %d bytes in order", want.name, len(got), err, len(want.body))
		}
	}
}
