// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// lineWith is the first line of out holding every one of parts.
func lineWith(out string, parts ...string) string {
	for line := range strings.SplitSeq(out, "\n") {
		found := true
		for _, part := range parts {
			found = found && strings.Contains(line, part)
		}
		if found {
			return line
		}
	}
	return ""
}

// Each routed attempt says how it ended: the route it went by, what the host
// answered, and what the file does next. Sending the file on to another route
// is not a wait the host asked for, and is not logged as one: that line, with
// the time it was logged as the time to wait until, was all there was.
func TestARoutedAttemptLogsHowItEndedAndWhatComesNext(t *testing.T) {
	m := busyManager(t)
	var logs lockedBuffer
	m.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "file")
	}
	a := proxyServer(t, "127.0.0.2", handler)
	b := proxyServer(t, "127.0.0.3", handler)
	attachPool(t, m, []string{a.URL, b.URL})
	var attempts atomic.Int32
	job := addProxyFiles(m, 1, func(ctx context.Context) (*extractor.Target, error) {
		if attempts.Add(1) == 1 {
			return nil, &extractor.RefusedError{Err: errors.New("service: Download is not available: free downloads are refused from this address")}
		}
		return &extractor.Target{URL: "http://storage.example.test/file", Size: 4}, nil
	})
	m.Start()
	m.signal()
	waitFor(t, 10*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return job.Items[0].Status == StatusDone || job.Items[0].Status == StatusFailed
	})
	out := logs.String()
	refused := lineWith(out, `msg="route attempt ended"`, "outcome=refused")
	for _, want := range []string{`next="another route"`, "route=http://127.0.0.", "refused from this address", "attempt=1"} {
		if !strings.Contains(refused, want) {
			t.Errorf("refused attempt logged as %q; want %s in it", refused, want)
		}
	}
	if finished := lineWith(out, `msg="route attempt ended"`, "outcome=finished"); !strings.Contains(finished, "next=done") || !strings.Contains(finished, "attempt=2") {
		t.Errorf("finished attempt logged as %q", finished)
	}
	if strings.Contains(out, "host asked for a wait") {
		t.Errorf("a file sent on to another route was logged as waiting:\n%s", out)
	}
}

// A wait the host asked for is logged with its reason; a requeue that waits
// for nothing is not logged as a wait at all.
func TestOnlyARealWaitIsLoggedAsTheHostsWait(t *testing.T) {
	m := busyManager(t)
	var logs lockedBuffer
	m.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	job := addProxyFiles(m, 2, func(context.Context) (*extractor.Target, error) { return nil, errors.New("unused") })
	m.mu.Lock()
	m.deferWaitLocked(job.Items[0], &extractor.WaitError{Until: time.Now().Add(10 * time.Minute), Reason: "Service: waiting between free downloads"}, false)
	m.deferWaitLocked(job.Items[1], &extractor.WaitError{Until: time.Now(), Reason: "Retrying through another download route"}, false)
	m.mu.Unlock()
	out := logs.String()
	if line := lineWith(out, "host asked for a wait", "item="+job.Items[0].ID); !strings.Contains(line, `reason="Service: waiting between free downloads"`) {
		t.Errorf("the host's wait was logged as %q; want its reason", line)
	}
	if line := lineWith(out, "host asked for a wait", "item="+job.Items[1].ID); line != "" {
		t.Errorf("a requeue waiting for nothing was logged as %q", line)
	}
}
