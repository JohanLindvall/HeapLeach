package download

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

func TestChooseName(t *testing.T) {
	cases := []struct {
		current, fromServer, want string
	}{
		// The extractor's name wins by default.
		{"video.mp4", "cdn-4823.mp4", "video.mp4"},
		// Unless ours is a placeholder...
		{"download", "real-name.zip", "real-name.zip"},
		{"", "real-name.zip", "real-name.zip"},
		// ...or the server's is strictly more informative.
		{"video", "video.mp4", "video.mp4"},
		// A server name with nothing to add changes nothing.
		{"video.mp4", "", "video.mp4"},
		{"video", "servername", "video"},
	}
	for _, tc := range cases {
		if got := chooseName(tc.current, tc.fromServer); got != tc.want {
			t.Errorf("chooseName(%q, %q) = %q, want %q", tc.current, tc.fromServer, got, tc.want)
		}
	}
}

func TestFilenameFromDisposition(t *testing.T) {
	cases := map[string]string{
		`attachment; filename="report.pdf"`:                  "report.pdf",
		`attachment; filename*=UTF-8''na%C3%AFve%20file.txt`: "naïve file.txt",
		`attachment; filename="percent%20encoded.mp4"`:       "percent encoded.mp4",
		`inline`:                              "",
		`garbage;;;`:                          "",
		`attachment; filename=" padded.bin "`: "padded.bin",
	}
	for in, want := range cases {
		if got := filenameFromDisposition(in); got != want {
			t.Errorf("filenameFromDisposition(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseContentRangeTotal(t *testing.T) {
	if total, ok := parseContentRangeTotal("bytes 0-1023/4096"); !ok || total != 4096 {
		t.Errorf("plain form = %d, %v", total, ok)
	}
	if _, ok := parseContentRangeTotal("bytes 0-1023/*"); ok {
		t.Error("an unknown total is not a total")
	}
	if _, ok := parseContentRangeTotal("nonsense"); ok {
		t.Error("garbage parsed as a total")
	}
}

func TestTotalSize(t *testing.T) {
	withRange := &http.Response{Header: http.Header{"Content-Range": {"bytes 100-199/500"}}, ContentLength: 100}
	if got := totalSize(withRange, 100); got != 500 {
		t.Errorf("Content-Range total = %d, want 500", got)
	}
	plain := &http.Response{Header: http.Header{}, ContentLength: 400}
	if got := totalSize(plain, 100); got != 500 {
		t.Errorf("offset+length = %d, want 500", got)
	}
	unknown := &http.Response{Header: http.Header{}, ContentLength: -1}
	if got := totalSize(unknown, 0); got != -1 {
		t.Errorf("unknown length = %d, want -1", got)
	}
}

func TestRetryableTransfer(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"cancelled", context.Canceled, false},
		{"busy host", &busyHostError{url: "https://x.test/f"}, true},
		{"404", &httpx.StatusError{Code: 404}, false},
		{"403", &httpx.StatusError{Code: 403}, false},
		{"429", &httpx.StatusError{Code: 429}, true},
		{"500", &httpx.StatusError{Code: 500}, true},
		{"connection reset", syscall.ECONNRESET, true},
		{"plain io", errors.New("unexpected EOF"), true},
	}
	for _, tc := range cases {
		if got := retryableTransfer(tc.err); got != tc.want {
			t.Errorf("%s: retryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStreamsFor(t *testing.T) {
	partial := &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{}}
	if got := streamsFor(8, config.MinSplitFileSize, partial); got != 8 {
		t.Errorf("rangeable large file = %d, want 8", got)
	}
	// Too small to be worth dividing.
	if got := streamsFor(8, config.MinSplitFileSize-1, partial); got != 1 {
		t.Errorf("small file = %d, want 1", got)
	}
	// The server ignored the range, so it will ignore the rest.
	plain := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	if got := streamsFor(8, 1<<30, plain); got != 1 {
		t.Errorf("no range support = %d, want 1", got)
	}
	none := &http.Response{StatusCode: http.StatusPartialContent,
		Header: http.Header{"Accept-Ranges": {"none"}}}
	if got := streamsFor(8, 1<<30, none); got != 1 {
		t.Errorf("Accept-Ranges: none = %d, want 1", got)
	}
}

func TestAnnotateTransferNamesAStall(t *testing.T) {
	var stalled atomic.Bool
	stalled.Store(true)
	err := annotateTransfer("clip.mp4", context.Canceled, &stalled, 90*time.Second)
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("a stall must be reported as one, got %v", err)
	}
	// The stall deliberately does not wrap the cancellation it manifests
	// as, or it would read as the user cancelling.
	if errors.Is(err, context.Canceled) {
		t.Error("a stall must not be mistakable for a cancellation")
	}
}

func TestRejectWebPageAllowsPagesWhenAsked(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Request:    &http.Request{URL: mustParse(t, "https://x.test/page.html")},
	}
	if err := rejectWebPage(resp, "page.html"); err != nil {
		t.Errorf("an HTML file asked for by name is fine: %v", err)
	}
	if err := rejectWebPage(resp, "video.mp4"); err == nil {
		t.Error("HTML in place of a video must be rejected")
	}
	ranged := &http.Response{StatusCode: http.StatusPartialContent,
		Header: http.Header{"Content-Type": {"text/html"}}}
	if err := rejectWebPage(ranged, "video.mp4"); err != nil {
		t.Errorf("a ranged answer is the real thing: %v", err)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
