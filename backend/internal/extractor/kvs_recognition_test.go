// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Plain-media installs need no license permutation. Their generator and a
// playable player configuration suffice even when the video route is custom.
func TestRegistryKVSGeneratorWithPlainMediaOnCustomPath(t *testing.T) {
	const media = "https://cdn.example.test/first.mp4?token=synthetic"
	for _, tc := range []struct {
		name, generator, fields string
	}{
		{"main rendition", "Kernel Video Sharing", "video_url: '" + media + "'"},
		{"alternate rendition", "KVS", "video_alt_url2: '" + media + "'"},
		{"versioned generator", "Kernel Video Sharing 6.0", "video_url: '" + media + "'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/watch/synthetic" {
					t.Errorf("unexpected request: %s", r.URL)
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprintf(w, `<html><meta name="generator" content="%s"><script>
var flashvars = {video_title: 'First Clip', %s};</script></html>`, tc.generator, tc.fields)
			}))
			defer srv.Close()
			client := testClient()
			defer client.CloseIdleConnections()
			reg := NewRegistry(&config.Config{}, client)
			res, ex, err := reg.Extract(context.Background(), srv.URL+"/watch/synthetic", Options{})
			if err != nil {
				t.Fatal(err)
			}
			if ex.Name() != "kvs" || res.Title != "First Clip" || len(res.Files) != 1 {
				t.Fatalf("extractor %s, result %+v; want one KVS video", ex.Name(), res)
			}
			if file := res.Files[0]; file.URL != media || file.Name != "First Clip.mp4" {
				t.Fatalf("file %+v; want the plain rendition with its query intact", file)
			}
		})
	}
}

func TestKVSGeneratorNeedsPlayableFlashvarsOffVideoPaths(t *testing.T) {
	const generator = `<meta name="generator" content="Kernel Video Sharing">`
	const direct = `video_url: 'https://cdn.example.test/first.mp4'`
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"generator only", generator, false},
		{"generic flashvars", `<script>var flashvars = {` + direct + `};</script>`, false},
		{"unrelated generator", `<meta name="generator" content="KVS CMS"><script>var flashvars = {` + direct + `};</script>`, false},
		{"empty player", generator + `<script>var flashvars = {};</script>`, false},
		{"title only", generator + `<script>var flashvars = {video_title: 'First Clip'};</script>`, false},
		{"unclosed player", generator + `<script>var flashvars = {` + direct + `;</script>`, false},
		{"empty media", generator + `<script>var flashvars = {video_url: ''};</script>`, false},
		{"relative media", generator + `<script>var flashvars = {video_url: '/media/first.mp4'};</script>`, false},
		{"invalid media", generator + `<script>var flashvars = {video_url: 'https://%zz/first.mp4'};</script>`, false},
		{"non-web media", generator + `<script>var flashvars = {video_url: 'javascript:play()'};</script>`, false},
		{"adaptive only", generator + `<script>var flashvars = {video_url: 'https://cdn.example.test/first.m3u8'};</script>`, false},
		{"uppercase HLS", generator + `<script>var flashvars = {video_url: 'https://cdn.example.test/first.M3U8?token=synthetic'};</script>`, false},
		{"DASH only", generator + `<script>var flashvars = {video_url: 'https://cdn.example.test/first.mpd?token=synthetic'};</script>`, false},
		{"scrambled without license", generator + `<script>var flashvars = {video_url: 'function/0/https://cdn.example.test/get_file/1/first.mp4'};</script>`, false},
		{"invalid preferred rendition", generator + `<script>var flashvars = {video_url: 'javascript:play()', video_url_text: '1080p', video_alt_url: 'https://cdn.example.test/first.mp4', video_alt_url_text: '720p'};</script>`, false},
		{"plain player", generator + `<script>var flashvars = {` + direct + `};</script>`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := NewKVS(nil)
			expectRecognised(t, k, htmlPage(t, "https://tube.example.test/watch/synthetic", tc.body), tc.want)
		})
	}
	// Page recognition must not make custom watch routes a link-scanning
	// Shape: those routes also belong to unrelated players and navigation.
	expectShape(t, NewKVS(nil), "https://tube.example.test/watch/synthetic", false)
}
