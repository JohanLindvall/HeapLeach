// SPDX-License-Identifier: MIT

package download

import (
	"io"
	"log/slog"
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

// waitFor polls until cond holds, failing the test if it has not within
// timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	if !waitForCond(timeout, cond) {
		t.Fatalf("condition not met within %s", timeout)
	}
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
