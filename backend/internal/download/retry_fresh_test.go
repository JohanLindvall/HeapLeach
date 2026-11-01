// SPDX-License-Identifier: MIT

package download

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// A retry the user asks for reads the source again. The page a job failed
// on a moment ago is still in the extractors' cache, and handing it back
// would repeat the failure without asking the site; adding the same link
// as a new job may share it, but a retry may not.
func TestRetryJobReadsTheSourceAgain(t *testing.T) {
	var hits atomic.Int32
	srv := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		switch r.URL.Path {
		case "/page":
			// The recognised feed has no enclosures at first. Unlike an
			// unrecognised HTML page, it does not get a Referer retry, so
			// only the user's Retry can discover the added enclosure.
			if hits.Add(1) == 1 {
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
	if failed == "" || items != 0 {
		t.Fatalf("the empty page: err %q with %d items, want a failure", failed, items)
	}

	if err := m.RetryJob(id); err != nil {
		t.Fatal(err)
	}
	waitLocked(t, m, 5*time.Second, func() bool { return !m.jobs[id].resolving })
	m.mu.Lock()
	failed, items = m.jobs[id].Err, len(m.jobs[id].Items)
	m.mu.Unlock()
	if hits.Load() != 2 || failed != "" || items != 1 {
		t.Fatalf("retry: %d reads of the page, err %q, %d items; want a second read and its file", hits.Load(), failed, items)
	}
}
