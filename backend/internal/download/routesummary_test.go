// SPDX-License-Identifier: MIT

package download

import (
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// How routed attempts end is counted per service and logged once an
// interval, with the files moving bytes and those still looking for a route.
func TestRouteAttemptsAreSummarizedOncePerInterval(t *testing.T) {
	m := busyManager(t)
	k2s, other := &extractor.Pace{Group: "keep2share", PerRoute: true}, &extractor.Pace{Group: "emload", PerRoute: true}
	for _, tc := range []struct {
		pace *extractor.Pace
		err  error
	}{
		{k2s, fmt.Errorf("keep2share: requestCaptcha: %w", io.ErrUnexpectedEOF)},
		{k2s, &extractor.RefusedError{Err: errors.New("free downloads are refused from this address")}},
		{k2s, &extractor.WaitError{Until: time.Now().Add(time.Hour)}},
		{k2s, &extractor.RefusedError{Err: errors.New("free downloads are refused from this address")}},
		{k2s, &httpx.RouteError{Err: errExitInUse}},
		{other, nil},
	} {
		m.noteRouteOutcome(&Item{pace: tc.pace}, tc.err)
	}
	now := time.Now()
	m.mu.Lock()
	m.routeSummaryAt = now.Add(-config.RouteSummaryInterval / 2)
	early := m.routeSummaryLocked(now)
	lines := m.routeSummaryLocked(now.Add(config.RouteSummaryInterval))
	again := m.routeSummaryLocked(now.Add(2 * config.RouteSummaryInterval))
	m.mu.Unlock()
	if early != nil {
		t.Fatalf("summarized before the interval was up: %v", early)
	}
	want := []string{
		"[service emload dead 0 shared 0 challenged 0 refused 0 waited 0 transient 0 stalled 0 failed 0 finished 1 moving 0 trying 0]",
		"[service keep2share dead 1 shared 1 challenged 0 refused 2 waited 1 transient 0 stalled 0 failed 0 finished 0 moving 0 trying 0]",
	}
	if len(lines) != len(want) {
		t.Fatalf("lines %v; want %v", lines, want)
	}
	for i, line := range lines {
		if got := fmt.Sprint(line); got != want[i] {
			t.Errorf("line %d: %s; want %s", i, got, want[i])
		}
	}
	if again != nil {
		t.Fatalf("the counts were not cleared: %v", again)
	}
}
