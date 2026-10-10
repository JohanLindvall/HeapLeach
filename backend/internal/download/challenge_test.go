// SPDX-License-Identifier: MIT

package download

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

func TestChallengeProxyRecoveryForUnregisteredPagesAndFiles(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		directFile  bool
		disabled    bool
		refusals    int32
		wantFailed  bool
	}{
		{name: "direct-page"},
		{name: "direct-file", directFile: true},
		{name: "page-challenge", stage: "page"},
		{name: "media-challenge", stage: "media"},
		{name: "file-challenge", stage: "media", directFile: true},
		{name: "rotate-page", stage: "page", refusals: 1},
		{name: "rotate-file", stage: "media", directFile: true, refusals: 1},
		{name: "exhaust-page", stage: "page", refusals: 3, wantFailed: true},
		{name: "exhaust-file", stage: "media", directFile: true, refusals: 3, wantFailed: true},
		{name: "disabled-page", stage: "page", disabled: true, wantFailed: true},
		{name: "disabled-file", stage: "media", directFile: true, disabled: true, wantFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := busyManager(t)
			var origin string
			var directChallenges, proxyChallenges, proxyRequests, refusals atomic.Int32
			refusals.Store(tc.refusals)
			challenge := func(w http.ResponseWriter) {
				w.Header().Set(httpx.HeaderCFMitigated, "challenge")
				w.Header().Set(httpx.HeaderContentType, "text/html")
				fmt.Fprint(w, "<html>Challenge</html>")
			}
			const payload = "synthetic media bytes"
			handler := func(route string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					if route != "direct" {
						proxyRequests.Add(1)
						if refusals.Add(-1) >= 0 {
							proxyChallenges.Add(1)
							challenge(w)
							return
						}
					}
					if r.URL.Path == "/watch" {
						if route == "direct" && tc.stage == "page" {
							directChallenges.Add(1)
							challenge(w)
							return
						}
						http.SetCookie(w, &http.Cookie{Name: "route", Value: route, Path: "/"})
						w.Header().Set(httpx.HeaderContentType, "text/html")
						fmt.Fprintf(w, `<title>Test Clip</title><video src="%s/media/%s/clip.mp4"></video>`, origin, route)
						return
					}
					if r.URL.Path == "/asset.bin" || strings.HasPrefix(r.URL.Path, "/media/") {
						if route == "direct" && tc.stage == "media" {
							directChallenges.Add(1)
							challenge(w)
							return
						}
						if !tc.directFile {
							cookie, err := r.Cookie("route")
							if err != nil || cookie.Value != route || r.URL.Path != "/media/"+route+"/clip.mp4" {
								t.Errorf("media reused another route's cookies or URL: %s %v %v on %s", r.URL.Path, cookie, err, route)
							}
						}
						w.Header().Set(httpx.HeaderContentType, "video/mp4")
						http.ServeContent(w, r, "clip.mp4", time.Time{}, strings.NewReader(payload))
						return
					}
					http.NotFound(w, r)
				}
			}
			srv := httptest.NewServer(handler("direct"))
			defer srv.Close()
			origin = srv.URL
			endpoints := []string{proxy.Direct}
			for i, address := range []string{"127.0.0.2", "127.0.0.3", "127.0.0.4"} {
				srv := proxyServer(t, address, handler(fmt.Sprintf("proxy-%d", i)))
				endpoints = append(endpoints, srv.URL)
			}
			pool := attachPool(t, m, endpoints)
			if tc.disabled {
				enabled := false
				if err := m.ApplySettings(Settings{Proxies: &enabled}); err != nil {
					t.Fatal(err)
				}
			}
			m.Start()
			path, name := "/watch", "Test Clip.mp4"
			if tc.directFile {
				path, name = "/asset.bin", "asset.bin"
			}
			id, err := m.Add(origin+path, "")
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, 5*time.Second, func() bool {
				m.mu.Lock()
				defer m.mu.Unlock()
				job := m.jobs[id]
				return job.Err != "" || (len(job.Items) == 1 && job.Items[0].Status.Terminal())
			})
			m.mu.Lock()
			job := m.jobs[id]
			gotErr := job.Err
			if len(job.Items) == 1 && job.Items[0].Status == StatusFailed {
				gotErr = job.Items[0].Err
			}
			m.mu.Unlock()
			if (gotErr != "") != tc.wantFailed || (tc.wantFailed && !strings.Contains(gotErr, "Cloudflare challenge")) {
				t.Fatalf("error=%s", gotErr)
			}
			if tc.stage == "" || tc.disabled {
				if proxyRequests.Load() != 0 {
					t.Fatal("used proxies before a challenge or while disabled")
				}
			} else {
				if directChallenges.Load() != 1 || proxyRequests.Load() == 0 || proxyChallenges.Load() != tc.refusals {
					t.Fatalf("direct challenges=%d proxy requests=%d proxy challenges=%d", directChallenges.Load(), proxyRequests.Load(), proxyChallenges.Load())
				}
				if pool.Page("cloudflare", proxy.Query{}).Summary.Cooling != 1+int(tc.refusals) {
					t.Fatal("challenged addresses were not cooled")
				}
				if pool.Page("keep2share", proxy.Query{}).Summary.Cooling != 0 {
					t.Fatal("Cloudflare challenge penalised another service")
				}
			}
			if !tc.wantFailed {
				body, err := os.ReadFile(filepath.Join(m.DownloadDir(), name))
				if err != nil || string(body) != payload {
					t.Fatalf("download=%q err=%v", body, err)
				}
			}
		})
	}
}
