package download

import (
	"errors"
	"github.com/JohanLindvall/HeapLeach/internal/config"
	"net/http"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A reading is only offered once the window has filled, so a decision is
// never made on one noisy interval.
func TestSpeedMeterWaitsForAFullWindow(t *testing.T) {
	meter := newSpeedMeter(3, 0)

	for i, total := range []int64{1_000, 2_000} {
		if _, stable := meter.observe(total, time.Second); stable {
			t.Fatalf("sample %d reported stable before the window filled", i+1)
		}
	}
	rate, stable := meter.observe(3_000, time.Second)
	if !stable {
		t.Fatal("still unstable after a full window")
	}
	if rate != 1000 {
		t.Errorf("rate = %.0f, want 1000", rate)
	}
}

// Averaging is the point: one wild interval must not swing the verdict.
func TestSpeedMeterSmoothsSpikes(t *testing.T) {
	meter := newSpeedMeter(4, 0)

	// Three steady seconds at 1 MB/s, then one that delivers 10 MB.
	var total int64
	for range 3 {
		total += 1_000_000
		meter.observe(total, time.Second)
	}
	total += 10_000_000
	rate, stable := meter.observe(total, time.Second)

	if !stable {
		t.Fatal("window should be full")
	}
	if rate < 3_000_000 || rate > 3_500_000 {
		t.Errorf("rate = %.0f, want the spike averaged to about 3.25 MB/s", rate)
	}
	// The instantaneous reading was 10 MB/s; the smoothed one is far lower,
	// which is what keeps the decision steady.
	if rate >= 10_000_000 {
		t.Error("a single spike dominated the reading")
	}
}

func TestSpeedMeterResetClearsHistory(t *testing.T) {
	meter := newSpeedMeter(2, 0)
	meter.observe(5_000_000, time.Second)
	meter.observe(10_000_000, time.Second)

	meter.reset(10_000_000)
	if _, stable := meter.observe(10_100_000, time.Second); stable {
		t.Error("reported stable immediately after a reset")
	}
}

func TestHostLimiterBoundsExtraConnections(t *testing.T) {
	limiter := newHostLimiter(2)

	if !limiter.reserve("example.test") || !limiter.reserve("example.test") {
		t.Fatal("could not take the two slots on offer")
	}
	if limiter.reserve("example.test") {
		t.Error("handed out a third slot against a ceiling of two")
	}
	// A different host has its own budget.
	if !limiter.reserve("other.test") {
		t.Error("one host's usage consumed another's budget")
	}

	limiter.release("example.test")
	if !limiter.reserve("example.test") {
		t.Error("a released slot was not reusable")
	}
}

// Being turned away lowers what is attempted against that host from then on.
func TestHostLimiterLearnsFromRefusal(t *testing.T) {
	limiter := newHostLimiter(4)
	for range 3 {
		limiter.reserve("busy.test")
	}

	if got := limiter.penalise("busy.test"); got >= 3 {
		t.Errorf("limit after refusal = %d, want it below the 3 in flight", got)
	}
	// Keep refusing and it should stop trying extras altogether.
	for range 5 {
		limiter.penalise("busy.test")
	}
	if got := limiter.limit("busy.test"); got != 0 {
		t.Errorf("limit = %d, want 0 after repeated refusals", got)
	}
	if limiter.reserve("busy.test") {
		t.Error("still opening extra connections to a host that keeps refusing")
	}
}

func TestRefusedExtraConnection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"too many requests", &httpx.StatusError{Code: http.StatusTooManyRequests}, true},
		{"service unavailable", &httpx.StatusError{Code: http.StatusServiceUnavailable}, true},
		{"connection refused", syscall.ECONNREFUSED, true},
		{"connection reset", syscall.ECONNRESET, true},
		{"not found", &httpx.StatusError{Code: http.StatusNotFound}, false},
		{"forbidden", &httpx.StatusError{Code: http.StatusForbidden}, false},
		{"other", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refusedExtraConnection(tc.err); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// A refused extra connection gives its range back, and the file stays fully
// covered so nothing is left without a reader.
func TestRetireReturnsRangeToNeighbour(t *testing.T) {
	const size = 64 << 20
	table := newSegmentTable(size, 0)

	tail := table.split(config.MinSegmentSize)
	if tail == nil {
		t.Fatal("split failed")
	}
	if !table.retire(tail) {
		t.Fatal("could not retire an untouched segment")
	}
	if got := table.count(); got != 1 {
		t.Fatalf("table holds %d segments, want 1", got)
	}
	assertTiles(t, table, size)
}

func TestRetireRefusesUnsafeCases(t *testing.T) {
	const size = 64 << 20

	t.Run("segment already has bytes", func(t *testing.T) {
		table := newSegmentTable(size, 0)
		tail := table.split(config.MinSegmentSize)
		tail.pos.Store(tail.start + 4096)
		if table.retire(tail) {
			t.Error("retired a segment that had already downloaded data")
		}
	})

	t.Run("neighbour has finished", func(t *testing.T) {
		table := newSegmentTable(size, 0)
		tail := table.split(config.MinSegmentSize)
		// The reader of the left segment has gone; extending it would
		// leave the range with nobody to fetch it.
		head := table.snapshot()[0]
		head.pos.Store(head.end.Load())
		if table.retire(tail) {
			t.Error("retired into a finished neighbour")
		}
	})

	t.Run("only segment", func(t *testing.T) {
		table := newSegmentTable(size, 0)
		if table.retire(table.snapshot()[0]) {
			t.Error("retired the only segment, leaving nothing to download")
		}
	})
}

// Gofile answers a request for a file on a busy storage server with a 302
// back to its own web page. Redirects are followed, so that arrives as a
// 200 of HTML — which, written to disk, would replace a multi-gigabyte
// video with a few kilobytes of page shell and, because Content-Length
// agrees with what was written, be recorded as a complete download.
func TestRejectWebPageCatchesBusyHost(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		filename    string
		wantErr     bool
	}{
		{"html in place of a video", http.StatusOK, "text/html; charset=utf-8", "movie.mp4", true},
		{"xhtml in place of a video", http.StatusOK, "application/xhtml+xml", "movie.mp4", true},
		{"a real ranged answer", http.StatusPartialContent, "text/html", "movie.mp4", false},
		{"an actual media body", http.StatusOK, "video/mp4", "movie.mp4", false},
		{"an octet stream", http.StatusOK, "application/octet-stream", "movie.mp4", false},
		{"a genuinely html file", http.StatusOK, "text/html", "page.html", false},
		{"no content type", http.StatusOK, "", "movie.mp4", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: tc.status,
				Header:     http.Header{"Content-Type": []string{tc.contentType}},
				Request:    &http.Request{URL: mustURL(t, "https://store9.example.test/download/web/x/movie.mp4")},
			}
			err := rejectWebPage(resp, tc.filename)

			if tc.wantErr && err == nil {
				t.Fatal("accepted a web page as file content")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("rejected a legitimate response: %v", err)
			}
			if err != nil && !retryableTransfer(err) {
				t.Error("a busy host should be retried, not treated as fatal")
			}
		})
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
