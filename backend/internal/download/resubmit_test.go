// SPDX-License-Identifier: MIT

package download

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// Removing and re-adding a non-reusable source is a new acquisition. Even
// before its first transfer starts, a previous submission's media URL can expire.
func TestResubmittedSourceHonorsResponsePolicy(t *testing.T) {
	for _, policy := range []string{httpx.CacheControlNoStore, httpx.CacheControlNoCache, "max-age=0"} {
		t.Run(policy, func(t *testing.T) {
			var version, reads atomic.Int32
			version.Store(1)
			const payload = "synthetic-media-payload"
			var srv *httptest.Server
			srv = testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/synthetic-clip":
					reads.Add(1)
					w.Header().Set(httpx.HeaderContentType, "text/html")
					w.Header().Set(httpx.HeaderCacheControl, policy)
					fmt.Fprintf(w, `<html><title>First Clip</title><video src="%s/first.mp4?version=%d"></video></html>`, srv.URL, version.Load())
				case "/first.mp4":
					if r.URL.Query().Get("version") != strconv.Itoa(int(version.Load())) {
						http.Error(w, "expired synthetic media URL", http.StatusForbidden)
						return
					}
					w.Header().Set(httpx.HeaderContentType, "video/mp4")
					w.Header().Set(httpx.HeaderContentLength, strconv.Itoa(len(payload)))
					fmt.Fprint(w, payload)
				default:
					http.NotFound(w, r)
				}
			}))
			m := testManager(t, nil)
			first, err := m.Add(srv.URL+"/synthetic-clip", "")
			if err != nil {
				t.Fatal(err)
			}
			waitLocked(t, m, 5*time.Second, func() bool { return !m.jobs[first].resolving })
			if err := m.RemoveJob(first); err != nil {
				t.Fatal(err)
			}
			version.Store(2)
			second, err := m.Add(srv.URL+"/synthetic-clip", "")
			if err != nil {
				t.Fatal(err)
			}
			waitLocked(t, m, 5*time.Second, func() bool { return !m.jobs[second].resolving })
			m.Start()
			waitForStatus(t, m, StatusDone, 1, 5*time.Second)
			m.mu.Lock()
			path := m.jobs[second].Items[0].Path
			m.mu.Unlock()
			body, err := os.ReadFile(filepath.Join(m.DownloadDir(), path))
			if err != nil || string(body) != payload || reads.Load() != 2 {
				t.Fatalf("saved=%q err=%v source reads=%d; want fresh payload after two acquisitions", body, err, reads.Load())
			}
		})
	}
}
