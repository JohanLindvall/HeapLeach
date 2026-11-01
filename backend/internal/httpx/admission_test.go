// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingServer answers after hold, counting the requests open at once.
func countingServer(t *testing.T, hold time.Duration, handle func(w http.ResponseWriter, r *http.Request) bool) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var active, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		if handle != nil && handle(w, r) {
			return
		}
		time.Sleep(hold)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv, &active, &peak
}

// Requests made in an admitted context are held to its slots, and finish.
func TestAdmissionBoundsRequestsInFlight(t *testing.T) {
	srv, _, peak := countingServer(t, 20*time.Millisecond, nil)
	c := New("test", "en", 0, 5*time.Second)
	ctx := WithAdmission(context.Background(), NewAdmission(3))
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, err := c.GetString(ctx, srv.URL, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if p := peak.Load(); p > 3 || p < 2 {
		t.Errorf("peak %d requests in flight, want at most 3 (and some overlap)", p)
	}
	// Without an admission the same requests are not held back.
	peak.Store(0)
	for range 12 {
		wg.Go(func() { _, _ = c.GetString(context.Background(), srv.URL, nil) })
	}
	wg.Wait()
	if peak.Load() <= 3 {
		t.Errorf("unadmitted peak %d, want more than 3", peak.Load())
	}
}

// A response's slot is held until its body is closed, however long the
// caller takes to read it, and given back exactly once.
func TestAdmissionHoldsTheSlotUntilTheBodyIsDone(t *testing.T) {
	srv, _, _ := countingServer(t, 0, nil)
	c := New("test", "en", 0, 5*time.Second)
	a := NewAdmission(1)
	ctx := WithAdmission(context.Background(), a)
	req, _ := c.NewRequest(ctx, http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.slots) != 1 {
		t.Fatalf("%d slots held with the body open, want 1", len(a.slots))
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := c.GetString(short, srv.URL, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a second request got through while the first body was open: %v", err)
	}
	_ = resp.Body.Close()
	_ = resp.Body.Close()
	if len(a.slots) != 0 {
		t.Fatalf("%d slots held after close, want 0", len(a.slots))
	}
	// Read to its end, a body gives its slot back without being closed.
	req, _ = c.NewRequest(ctx, http.MethodGet, srv.URL, nil)
	if resp, err = c.Do(req); err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	if len(a.slots) != 0 {
		t.Errorf("%d slots held after the body was read, want 0", len(a.slots))
	}
	_ = resp.Body.Close()
	if len(a.slots) != 0 {
		t.Errorf("%d slots after a second release, want 0", len(a.slots))
	}
}

// A retryable answer and a rate limit give their slot back before the wait,
// so one slot is enough for a request that retries, and a redirect is one
// admitted attempt, not two.
func TestAdmissionReleasesBeforeWaiting(t *testing.T) {
	var asked atomic.Int32
	srv, _, _ := countingServer(t, 0, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/moved":
			http.Redirect(w, r, "/flaky", http.StatusFound)
			return true
		case asked.Add(1) == 1:
			w.Header().Set(HeaderRetryAfter, "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		case asked.Load() == 2:
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	c := New("test", "en", 2, 5*time.Second)
	a := NewAdmission(1)
	ctx := WithAdmission(context.Background(), a)
	if body, err := c.GetString(ctx, srv.URL+"/moved", nil); err != nil || body != "ok" {
		t.Fatalf("body %q, err %v", body, err)
	}
	if len(a.slots) != 0 {
		t.Errorf("%d slots held after the request, want 0", len(a.slots))
	}
}

// A request waiting for a slot ends with its context, holding nothing.
func TestAdmissionWaitEndsWithTheContext(t *testing.T) {
	a := NewAdmission(1)
	ctx := WithAdmission(context.Background(), a)
	release, err := admit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := New("test", "en", 0, time.Second).GetString(waiting, "http://example.invalid/", nil)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation", err)
	}
	release()
	if len(a.slots) != 0 {
		t.Errorf("%d slots held, want 0", len(a.slots))
	}
	if AdmissionOf(WithoutAdmission(ctx)) != nil {
		t.Error("WithoutAdmission kept the admission")
	}
}
