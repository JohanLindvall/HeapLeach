// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// A retry is announced once it is decided, before its backoff is waited
// out, so a transfer cancelled during the wait has still said a retry was
// pending; one that will not be retried announces nothing.
func TestRetryIsAnnouncedBeforeTheBackoff(t *testing.T) {
	m := testManager(t, func(c *config.Config) { c.MaxRetries = 3 })
	retryable := timeoutError("read timed out")

	ctx, cancel := context.WithCancel(context.Background())
	var announced time.Duration
	err := m.retryAnnounced(ctx, 0, retryable, func(wait time.Duration) {
		announced = wait
		cancel() // the user cancels during the wait
	})
	if announced <= 0 || announced > config.TransferRetryMax {
		t.Fatalf("announced a wait of %v, want the backoff", announced)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}

	called := false
	if err := m.retryAnnounced(context.Background(), 3, retryable, func(time.Duration) { called = true }); !errors.Is(err, retryable) || called {
		t.Fatalf("a spent budget: err %v, announced %v", err, called)
	}
}

// A segment's range answer is checked for its span alone: a total the
// server writes wrongly does not refuse the right bytes, and the wrong
// bytes are still refused.
func TestSegmentRangeAnsweredReadsTheSpan(t *testing.T) {
	answer := func(value string) *http.Response {
		return &http.Response{Header: http.Header{"Content-Range": {value}}}
	}
	for _, tc := range []struct {
		header string
		ok     bool
	}{
		{"bytes 0-99/1000", true},
		{"bytes 0-99/*", true},
		{"bytes 0-99/", true},
		{"bytes 0-99/99", true},
		{"bytes 0-99/50", true},
		{"bytes 1-99/1000", false},
		{"bytes 0-98/1000", false},
		{"bytes */1000", false},
		{"items 0-99/1000", false},
		{"bytes 99-0/1000", false},
	} {
		err := segmentRangeAnswered(answer(tc.header), 0, 99)
		if (err == nil) != tc.ok {
			t.Errorf("%q: err = %v, want ok=%v", tc.header, err, tc.ok)
		}
	}
}
