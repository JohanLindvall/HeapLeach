// SPDX-License-Identifier: MIT

package download

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// A retry the user asks for obtains a fresh probe even when the cached page
// already recognises the feed. The feed reader also makes its own request,
// so verify both fresh reads as well as the newly available enclosure.
func TestRetryJobReadsTheSourceAgain(t *testing.T) {
	var hits atomic.Int32
	var ready atomic.Bool
	srv := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		switch r.URL.Path {
		case "/page":
			hits.Add(1)
			// The browser probe and feed reader see the same empty source
			// until it changes immediately before the user's retry.
			if !ready.Load() {
				fmt.Fprint(w, `<rss version="2.0"><channel><title>Page</title></channel></rss>`)
				return
			}
			fmt.Fprint(w, `<rss version="2.0"><channel><title>Page</title><item><enclosure url="/clip.mp4" type="video/mp4"/></item></channel></rss>`)
		default:
			http.NotFound(w, r)
		}
	}))
	m := testManager(t, nil)
	id, err := m.Add(srv.URL+"/page", "")
	if err != nil {
		t.Fatal(err)
	}
	waitLocked(t, m, 5*time.Second, func() bool { return !m.jobs[id].resolving })
	m.mu.Lock()
	failed, items := m.jobs[id].Err, len(m.jobs[id].Items)
	m.mu.Unlock()
	if hits.Load() != 2 || failed == "" || items != 0 {
		t.Fatalf("the empty page: %d reads, err %q with %d items; want probe and feed reads, and a failure", hits.Load(), failed, items)
	}

	ready.Store(true)
	if err := m.RetryJob(id); err != nil {
		t.Fatal(err)
	}
	waitLocked(t, m, 5*time.Second, func() bool { return !m.jobs[id].resolving })
	m.mu.Lock()
	failed, items = m.jobs[id].Err, len(m.jobs[id].Items)
	m.mu.Unlock()
	if hits.Load() != 4 || failed != "" || items != 1 {
		t.Fatalf("retry: %d reads of the page, err %q, %d items; want fresh probe and feed reads, and its file", hits.Load(), failed, items)
	}
}
