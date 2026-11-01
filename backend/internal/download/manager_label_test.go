// SPDX-License-Identifier: MIT

package download

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A job is queued under its host while its link's form is ambiguous, and
// a job that failed keeps the name of the extractor that recognised its
// page and failed, rather than the guess it was queued under (R-009).
func TestFailedJobIsLabelledByItsExtractor(t *testing.T) {
	srv := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// An ordinary page nothing recognises: the fallback refuses it.
		w.Write([]byte(`<html><title>Nothing here</title></html>`))
	}))
	m := testManager(t, nil)
	id, err := m.Add(srv.URL+"/e/abcdef123456", "")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	host := m.jobs[id].Host
	m.mu.Unlock()
	if want := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[0]; host != want {
		t.Errorf("queued as %q, want the host %q while several forms claim the link", host, want)
	}
	waitFor(t, 10*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.jobs[id].Err != ""
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	if job := m.jobs[id]; job.Host != host {
		t.Errorf("failed job relabelled %q by the fallback; err %s", job.Host, job.Err)
	}
}
