// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingFetch mints "v1", "v2", ... valid for an hour of clock, after
// release is closed when one is given.
func countingFetch(clock func() time.Time, calls *atomic.Int32, release <-chan struct{}) renewFunc[string] {
	return func(context.Context) (string, time.Time, error) {
		if release != nil {
			<-release
		}
		n := calls.Add(1)
		return fmt.Sprintf("v%d", n), clock().Add(time.Hour), nil
	}
}

// Concurrent callers of an expired or refused value share one renewal, and
// a refusal of a value already replaced keeps the replacement.
func TestRenewableCoalescesAGeneration(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	var r renewable[string]
	r.now = func() time.Time { return now }
	var calls atomic.Int32
	fetch := countingFetch(r.now, &calls, nil)

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if v, err := r.get(context.Background(), fetch); err != nil || v != "v1" {
				t.Errorf("get = %q, %v", v, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("first fetch ran %d times", calls.Load())
	}

	for range 32 {
		wg.Go(func() {
			if v, err := r.renew(context.Background(), "v1", fetch); err != nil || v != "v2" {
				t.Errorf("renew = %q, %v", v, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("one refused generation renewed %d times", calls.Load()-1)
	}
	// A refusal that arrives late, of v1, must not discard v2.
	if v, _ := r.renew(context.Background(), "v1", fetch); v != "v2" || calls.Load() != 2 {
		t.Fatalf("late refusal: %q after %d fetches", v, calls.Load())
	}

	// Expiry follows the clock exactly.
	now = now.Add(time.Hour - time.Nanosecond)
	if v, _ := r.get(context.Background(), fetch); v != "v2" {
		t.Fatalf("renewed before expiry: %q", v)
	}
	now = now.Add(time.Nanosecond)
	if v, _ := r.get(context.Background(), fetch); v != "v3" || calls.Load() != 3 {
		t.Fatalf("at expiry: %q after %d fetches", v, calls.Load())
	}
}

// A failed renewal caches nothing, so the next caller tries again.
func TestRenewableFailureIsRetryable(t *testing.T) {
	var r renewable[string]
	var calls atomic.Int32
	fetch := func(context.Context) (string, time.Time, error) {
		if calls.Add(1) == 1 {
			return "", time.Time{}, errors.New("token service down")
		}
		return "fresh", time.Now().Add(time.Hour), nil
	}
	if _, err := r.get(context.Background(), fetch); err == nil {
		t.Fatal("a failed fetch succeeded")
	}
	if v, err := r.get(context.Background(), fetch); err != nil || v != "fresh" {
		t.Fatalf("retry = %q, %v", v, err)
	}
	// A failed renewal of a refused value leaves no value to hand out.
	failing := func(context.Context) (string, time.Time, error) {
		return "", time.Time{}, errors.New("still down")
	}
	if _, err := r.renew(context.Background(), "fresh", failing); err == nil {
		t.Fatal("renewal failure was hidden")
	}
	if _, err := r.get(context.Background(), failing); err == nil {
		t.Fatal("the refused value was handed out again")
	}
}

// A waiter that gives up returns at once; the renewal it was waiting for
// still completes, and its value serves the next caller without a fetch.
func TestRenewableCanceledWaiterStops(t *testing.T) {
	var r renewable[string]
	var calls atomic.Int32
	release := make(chan struct{})
	fetch := countingFetch(time.Now, &calls, release)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.get(ctx, fetch)
		done <- err
	}()
	other := make(chan string, 1)
	go func() {
		// Started after the first so it joins the same renewal.
		time.Sleep(10 * time.Millisecond)
		v, _ := r.get(context.Background(), fetch)
		other <- v
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not stop")
	}
	close(release)
	if v := <-other; v != "v1" {
		t.Fatalf("other waiter got %q", v)
	}
	if v, err := r.get(context.Background(), fetch); err != nil || v != "v1" || calls.Load() != 1 {
		t.Fatalf("after: %q, %v, %d fetches", v, err, calls.Load())
	}
}
