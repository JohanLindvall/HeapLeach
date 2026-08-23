package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// testClient builds a client with retries but no browser-shaped transport,
// so tests exercise exactly the code under test against httptest servers.
func testClient(t *testing.T, retries int) *Client {
	t.Helper()
	t.Setenv("HEAPLEACH_UTLS", "0")
	return New("test-agent/1.0", "en-US,en;q=0.9", retries, 10*time.Second)
}

func TestDoRetriesTransientStatusThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "payload")
	}))
	defer srv.Close()

	c := testClient(t, 2)
	req, err := c.NewRequest(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.Bytes(req)
	if err != nil {
		t.Fatalf("Bytes after one 503: %v", err)
	}
	if string(body) != "payload" {
		t.Errorf("body = %q", body)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server saw %d calls, want 2", got)
	}
}

func TestDoGivesUpAfterTheRetryBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := testClient(t, 1)
	req, _ := c.NewRequest(context.Background(), http.MethodGet, srv.URL, nil)
	_, err := c.Do(req)
	if err == nil {
		t.Fatal("a host that only ever answers 502 should fail")
	}
	if !HasStatus(err, http.StatusBadGateway) {
		t.Errorf("error should carry the status: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server saw %d calls, want the original try plus one retry", got)
	}
}

// A definitive answer is returned to the caller, not retried: a 404 today is
// a 404 on the next attempt too.
func TestDoDoesNotRetryDefinitiveStatuses(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	c := testClient(t, 3)
	req, _ := c.NewRequest(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do should hand a 404 back, not fail: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || calls.Load() != 1 {
		t.Errorf("status=%d calls=%d, want one un-retried 404", resp.StatusCode, calls.Load())
	}
}

func TestDoHonoursRetryAfter(t *testing.T) {
	var calls atomic.Int32
	var gap time.Duration
	var first time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			first = time.Now()
			w.Header().Set(HeaderRetryAfter, "1")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			gap = time.Since(first)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := testClient(t, 2)
	req, _ := c.NewRequest(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gap < time.Second {
		t.Errorf("second attempt arrived after %s, want the 1s the server asked for", gap)
	}
}

func TestDoStopsWhenTheContextDoes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := testClient(t, 5)
	req, _ := c.NewRequest(ctx, http.MethodGet, srv.URL, nil)

	done := make(chan error, 1)
	go func() {
		_, err := c.Do(req)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !IsCanceled(err) {
			t.Errorf("cancelled request should report cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled request kept retrying")
	}
}

func TestDoReplaysTheBodyOnRetry(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		bodies = append(bodies, string(b[:n]))
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := testClient(t, 2)
	req, err := c.NewRequest(context.Background(), http.MethodPost, srv.URL, []byte(`{"k":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(bodies) != 2 || bodies[0] != bodies[1] || bodies[1] != `{"k":"v"}` {
		t.Errorf("retried request must carry the same body, got %q", bodies)
	}
}

func TestDoOnceNeverRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := testClient(t, 5)
	req, _ := c.NewRequest(context.Background(), http.MethodGet, srv.URL, nil)
	if _, err := c.DoOnce(req); err == nil {
		t.Fatal("a refused request should be an error at once")
	}
	if calls.Load() != 1 {
		t.Errorf("DoOnce made %d requests, want exactly 1", calls.Load())
	}
}

func TestBytesReportsTheBodyOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"reason":"quota exceeded"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	c := testClient(t, 0)
	req, _ := c.NewRequest(context.Background(), http.MethodGet, srv.URL, nil)
	_, err := c.Bytes(req)
	if err == nil {
		t.Fatal("a 403 body read should fail")
	}
	// The body is usually where these hosts put the real reason, so the
	// error has to carry it.
	if !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("error should quote the body: %v", err)
	}
	if !HasStatus(err, http.StatusForbidden) {
		t.Errorf("error should carry the typed status: %v", err)
	}
}

func TestGetJSONDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get(HeaderAccept), "application/json") {
			t.Errorf("Accept = %q, want a JSON accept", r.Header.Get(HeaderAccept))
		}
		fmt.Fprint(w, `{"name":"value"}`)
	}))
	defer srv.Close()

	c := testClient(t, 0)
	var out struct {
		Name string `json:"name"`
	}
	if err := c.GetJSON(context.Background(), srv.URL, nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "value" {
		t.Errorf("decoded %+v", out)
	}
}

func TestGetJSONQuotesUndecodableBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html>not json`)
	}))
	defer srv.Close()

	c := testClient(t, 0)
	var out map[string]any
	err := c.GetJSON(context.Background(), srv.URL, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "not json") {
		t.Errorf("the undecodable body should be quoted for diagnosis, got %v", err)
	}
}

func TestNewRequestCarriesTheBrowserHeaders(t *testing.T) {
	c := testClient(t, 0)
	req, err := c.NewRequest(context.Background(), http.MethodGet, "https://example.test/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get(HeaderUserAgent); got != "test-agent/1.0" {
		t.Errorf("UA = %q", got)
	}
	if got := req.Header.Get(HeaderAcceptLanguage); got != "en-US,en;q=0.9" {
		t.Errorf("Accept-Language = %q", got)
	}
	if req.Header.Get(HeaderAccept) == "" {
		t.Error("no Accept header")
	}
}

// Redirects must keep the browser-ish headers: some hosts vary on the UA and
// a hop that dropped it would be served a different page.
func TestRedirectsKeepHeaders(t *testing.T) {
	var landed http.Header
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		landed = r.Header.Clone()
	})

	c := testClient(t, 0)
	req, _ := c.NewRequest(context.Background(), http.MethodGet, srv.URL+"/start", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := landed.Get(HeaderUserAgent); got != "test-agent/1.0" {
		t.Errorf("UA after redirect = %q", got)
	}
	if got := landed.Get(HeaderAcceptLanguage); got != "en-US,en;q=0.9" {
		t.Errorf("Accept-Language after redirect = %q", got)
	}
}

func TestRedirectChainIsBounded(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound) // forever
	})

	c := testClient(t, 0)
	req, _ := c.NewRequest(context.Background(), http.MethodGet, srv.URL+"/", nil)
	_, err := c.Do(req)
	if err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("an endless redirect chain should be stopped and named, got %v", err)
	}
}

func TestHasStatus(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &StatusError{Code: 401, Status: "401 Unauthorized"})
	if !HasStatus(err, 401) {
		t.Error("a wrapped StatusError should match its code")
	}
	if HasStatus(err, 404, 500) {
		t.Error("matched a code the error does not carry")
	}
	if HasStatus(errors.New("plain"), 401) {
		t.Error("a plain error has no status to match")
	}
	if HasStatus(nil, 401) {
		t.Error("nil has no status to match")
	}
}

func TestRetryAfterForms(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}

	// A header that is not there at all is a different answer from one that
	// says zero, which is what a rate limit's schedule turns on.
	if wait, stated := retryAfter(resp); stated || wait != 0 {
		t.Errorf("absent header = (%s, %v), want (0, false)", wait, stated)
	}
	resp.Header.Set(HeaderRetryAfter, "0")
	if wait, stated := retryAfter(resp); !stated || wait != 0 {
		t.Errorf("zero seconds = (%s, %v), want (0, true) — the host said come back now", wait, stated)
	}

	resp.Header.Set(HeaderRetryAfter, "3")
	if got, _ := retryAfter(resp); got != 3*time.Second {
		t.Errorf("seconds form = %s", got)
	}

	resp.Header.Set(HeaderRetryAfter, time.Now().Add(2*time.Second).UTC().Format(http.TimeFormat))
	if got, _ := retryAfter(resp); got <= 0 || got > 2*time.Second {
		t.Errorf("date form = %s, want a positive wait up to 2s", got)
	}

	// A hostile or broken value cannot park a worker for an hour.
	resp.Header.Set(HeaderRetryAfter, "9999999")
	if got, _ := retryAfter(resp); got > 2*time.Minute {
		t.Errorf("cap ignored: %s", got)
	}

	resp.Header.Set(HeaderRetryAfter, "soon")
	if got, _ := retryAfter(resp); got != 0 {
		t.Errorf("unparseable value should mean no wait, got %s", got)
	}
}

func TestStatusErrorMessage(t *testing.T) {
	long := strings.Repeat("x", 500)
	err := &StatusError{Code: 500, Status: "500 Internal Server Error", URL: "https://example.test/a", Body: long}
	msg := err.Error()
	if len(msg) > 300 {
		t.Errorf("a huge body must be truncated in the message, got %d bytes", len(msg))
	}
	if !strings.Contains(msg, "500") {
		t.Errorf("message should carry the status: %q", msg)
	}
}

func TestHeaderHelpers(t *testing.T) {
	base := Header{"A": "1", "B": "2"}
	merged := base.Merge(Header{"B": "3", "C": "4"})
	if merged["A"] != "1" || merged["B"] != "3" || merged["C"] != "4" {
		t.Errorf("Merge = %v", merged)
	}
	if base["B"] != "2" {
		t.Error("Merge must not mutate its receiver")
	}

	if got := Referer("https://example.test/")[HeaderReferer]; got != "https://example.test/" {
		t.Errorf("Referer = %q", got)
	}
	ro := RefererOrigin("https://example.test/page", "https://example.test")
	if ro[HeaderReferer] != "https://example.test/page" || ro[HeaderOrigin] != "https://example.test" {
		t.Errorf("RefererOrigin = %v", ro)
	}
}

func TestStreamingClientSharesEverythingButTheTimeout(t *testing.T) {
	c := testClient(t, 2)
	s := c.Streaming()
	if s.hc.Timeout != 0 {
		t.Errorf("streaming timeout = %s, want none", s.hc.Timeout)
	}
	if c.hc.Timeout == 0 {
		t.Error("the original client's timeout must be untouched")
	}
	if s.UserAgent() != c.UserAgent() {
		t.Error("streaming client changed the UA")
	}
	if s.hc.Jar != c.hc.Jar {
		t.Error("streaming client must share the cookie jar")
	}
}

// rateLimitedClient is testClient with the rate limit's waits shortened, so
// the behaviour is exercised without sitting out the production intervals.
func rateLimitedClient(t *testing.T, retries int) *Client {
	t.Helper()
	c := testClient(t, retries)
	c.rateLimitBase = 2 * time.Millisecond
	c.rateLimitMax = 10 * time.Millisecond
	return c
}

// A 429 is the host asking us to come back, so it is waited out rather than
// counted as a failure — several times over, and past the point where the
// ordinary retry budget would have given up.
func TestDoWaitsOutARateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, "payload")
	}))
	defer srv.Close()

	// No ordinary retries at all: the rate limit must not be drawing on them.
	c := rateLimitedClient(t, 0)
	req, err := c.NewRequest(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.Bytes(req)
	if err != nil {
		t.Fatalf("Bytes after three 429s: %v", err)
	}
	if string(body) != "payload" {
		t.Errorf("body = %q, want the payload once the limit lifted", body)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("made %d attempts, want 4 — three refused and the one that worked", got)
	}
}

// Waiting out a limiter must leave the ordinary budget untouched: a request
// that is rate-limited and then meets a dropped connection still has its own
// retries in hand.
func TestRateLimitDoesNotSpendTheRetryBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1, 2:
			w.WriteHeader(http.StatusTooManyRequests)
		case 3:
			w.WriteHeader(http.StatusServiceUnavailable) // a genuine transient failure
		default:
			fmt.Fprint(w, "payload")
		}
	}))
	defer srv.Close()

	// One ordinary retry, which the 503 needs and the two 429s must not have
	// taken.
	c := rateLimitedClient(t, 1)
	req, err := c.NewRequest(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Bytes(req); err != nil {
		t.Fatalf("Bytes: %v — the rate limit spent the retry the 503 needed", err)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("made %d attempts, want 4", got)
	}
}

// Patience is bounded: a host that never lets up is reported as the rate
// limit it is, rather than being asked forever.
func TestDoGivesUpOnAPermanentRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := rateLimitedClient(t, 0)
	req, err := c.NewRequest(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Bytes(req)
	if err == nil {
		t.Fatal("a host that always refuses was never reported")
	}
	if !HasStatus(err, http.StatusTooManyRequests) {
		t.Errorf("err = %v, want it to carry the 429 the host actually sent", err)
	}
	// The first attempt plus the rate limit's own budget, and nothing from
	// the ordinary one.
	if want := int32(config.RateLimitRetries + 1); calls.Load() != want {
		t.Errorf("made %d attempts, want %d", calls.Load(), want)
	}
}

// A host naming its own interval is believed over the schedule here, which
// is why Retry-After is read at all.
func TestRateLimitHonoursRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set(HeaderRetryAfter, "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, "payload")
	}))
	defer srv.Close()

	c := rateLimitedClient(t, 0) // its own waits are milliseconds; the host asks for a second
	req, err := c.NewRequest(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Bytes(req); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Errorf("waited %s, want at least the second the host asked for", waited.Round(time.Millisecond))
	}
}
