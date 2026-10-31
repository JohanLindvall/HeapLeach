// SPDX-License-Identifier: MIT

package download

import (
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// testLogger is a logger that goes nowhere: a warning a test provokes is
// expected output rather than a failure.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testManager builds a manager writing into a directory of its own, with
// one worker, one stream and no retries, which tune changes before anything
// is built from the configuration. Its HTTP client retries as often as the
// manager does. It is neither restored nor started, and is closed when the
// test ends.
func testManager(t *testing.T, tune func(*config.Config)) *Manager {
	t.Helper()
	cfg := &config.Config{
		DownloadDir: t.TempDir(), Concurrency: 1, Streams: 1,
		UserAgent: config.DefaultUserAgent, Language: config.DefaultLanguage,
		GofileSecret: config.FallbackGofileSecret, Timeout: 30 * time.Second,
	}
	if tune != nil {
		tune(cfg)
	}
	client := httpx.New(cfg.UserAgent, cfg.AcceptLanguage(), cfg.MaxRetries, cfg.Timeout)
	m := New(cfg, extractor.NewRegistry(cfg, client), client, testLogger())
	t.Cleanup(m.Close)
	return m
}

// queueJobLocked registers job and queues the files given as its items, in
// order, as a resolved listing would be. Caller holds mu.
func (m *Manager) queueJobLocked(job *Job, files ...extractor.File) []*Item {
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	items := make([]*Item, 0, len(files))
	for i, f := range files {
		it := m.newItem(job, f, "", i)
		job.Items = append(job.Items, it)
		m.queue = append(m.queue, it)
		items = append(items, it)
	}
	return items
}

// randomPayload is n random bytes, a file's contents no transfer could get
// right by accident.
func randomPayload(t *testing.T, n int) []byte {
	t.Helper()
	payload := make([]byte, n)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// testServer starts a server for the test, closed when it ends.
func testServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// mustURL parses a URL a test wrote itself.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// timeoutError is a net.Error that timed out without wrapping
// context.DeadlineExceeded, as net/http's own unexported ones do.
type timeoutError string

func (e timeoutError) Error() string { return string(e) }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

const (
	// tlsHandshakeTimeout is net/http's TLS handshake timeout.
	tlsHandshakeTimeout timeoutError = "net/http: TLS handshake timeout"
	// dialTimeout is a dial's i/o timeout.
	dialTimeout timeoutError = "i/o timeout"
)

// waitFor polls until cond holds, failing the test if it has not within
// timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	if !waitForCond(timeout, cond) {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// waitLocked is waitFor with cond read under the manager's lock.
func waitLocked(t *testing.T, m *Manager, timeout time.Duration, cond func() bool) {
	t.Helper()
	waitFor(t, timeout, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return cond()
	})
}

// waitForCond polls until cond holds, reporting whether it did.
func waitForCond(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// jobView is a job as the snapshot shows it, zero if it is not there.
func jobView(m *Manager, id string) JobView {
	for _, v := range m.Snapshot().Jobs {
		if v.ID == id {
			return v
		}
	}
	return JobView{}
}

// findItem is an item as the snapshot shows it, in the job given or, with
// no job, in any; zero if it is not there.
func findItem(m *Manager, jobID, itemID string) ItemView {
	for _, job := range m.Snapshot().Jobs {
		if jobID != "" && job.ID != jobID {
			continue
		}
		for _, it := range job.Items {
			if it.ID == itemID {
				return it
			}
		}
	}
	return ItemView{}
}

// countStatus counts the items at status, in the job given or, with no
// job, in the whole queue.
func countStatus(m *Manager, jobID string, status Status) int {
	n := 0
	for _, job := range m.Snapshot().Jobs {
		if jobID != "" && job.ID != jobID {
			continue
		}
		for _, it := range job.Items {
			if it.Status == status {
				n++
			}
		}
	}
	return n
}

// waitForStatus waits until n items of the queue are at status, failing
// the test as soon as one fails instead, and when timeout passes first.
func waitForStatus(t *testing.T, m *Manager, status Status, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		at := 0
		for _, job := range m.Snapshot().Jobs {
			for _, it := range job.Items {
				if it.Status == StatusFailed && status != StatusFailed {
					t.Fatalf("%s failed: %s", it.Name, it.Error)
				}
				if it.Status == status {
					at++
				}
			}
		}
		if at >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d items %s after %s", at, n, status, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForItem waits for a job of one file to have listed it, and returns
// the file's id.
func waitForItem(t *testing.T, m *Manager, jobID string) string {
	t.Helper()
	var id string
	waitFor(t, 10*time.Second, func() bool {
		if job := jobView(m, jobID); len(job.Items) == 1 {
			id = job.Items[0].ID
			return true
		}
		return false
	})
	return id
}
