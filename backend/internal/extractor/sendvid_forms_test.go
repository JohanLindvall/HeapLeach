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

func TestSendvidKnownHostMP4IDRange(t *testing.T) {
	for _, tc := range []struct {
		name, host, id string
		player         bool
		mayProbe       bool
	}{
		{"one character", "sendvid.com", "a", true, true},
		{"five characters", "sendvid.com", "abcde", true, true},
		{"eight characters", "sendvid.com", "abcd1234", true, true},
		{"thirteen characters", "sendvid.com", "abcdefghijklm", true, true},
		{"thirty-two characters", "sendvid.com", strings.Repeat("a", 32), true, true},
		{"www short id", "www.sendvid.com", "abcde", true, true},
		{"subdomain long id", "player.sendvid.com", "abcdefghijklm", true, true},
		{"empty id", "sendvid.com", "", false, false},
		{"too long", "sendvid.com", strings.Repeat("a", 33), false, false},
		{"hyphen", "sendvid.com", "ab-cd", false, false},
		{"underscore", "sendvid.com", "ab_cd", false, false},
		{"unrelated short file", "files.example.test", "abcde", false, false},
		{"unrelated long file", "files.example.test", "abcdefghijklm", false, false},
		{"unrelated existing shape", "files.example.test", "abcd1234", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if !tc.player {
					w.Header().Set(httpx.HeaderContentType, "video/mp4")
					return
				}
				switch r.URL.Path {
				case "/" + tc.id + ".mp4":
					http.Redirect(w, r, "/"+tc.id, http.StatusFound)
				case "/" + tc.id:
					w.Header().Set(httpx.HeaderContentType, "text/html")
					fmt.Fprint(w, `<html><title>Sendvid</title><meta property="og:site_name" content="Sendvid">`+
						`<meta property="og:title" content="First Clip"><video><source id="video_source" `+
						`src="https://media.example.test/first.mp4" type="video/mp4"></video></html>`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer proxy.Close()
			client, err := testClient().ThroughProxy(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			registry := NewRegistry(&config.Config{}, client)
			source := "http://" + tc.host + "/" + tc.id + ".mp4"
			res, ex, err := registry.Extract(t.Context(), source, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != 1 {
				t.Fatalf("got %d files, want one", len(res.Files))
			}
			if tc.player {
				if ex.Name() != "sendvid" || res.Files[0].URL != "https://media.example.test/first.mp4" {
					t.Fatalf("extractor=%s file=%s; want the player's media", ex.Name(), res.Files[0].URL)
				}
				return
			}
			if ex.Name() != "direct" || res.Files[0].URL != source {
				t.Fatalf("extractor=%s file=%s; want the direct file", ex.Name(), res.Files[0].URL)
			}
			// The existing eight-character shape permits a probe on
			// other hosts. Restoring the parser's wider range on its own
			// hosts must not expand that probing to unrelated file links.
			if !tc.mayProbe && reads.Load() != 0 {
				t.Fatalf("made %d requests for an unclaimed file", reads.Load())
			}
		})
	}
}
