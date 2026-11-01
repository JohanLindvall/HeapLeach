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

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
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

// The renewal is every caller's, so it is not held to the admission of the
// job whose caller happened to start it.
func TestRenewableLeavesTheCallersAdmission(t *testing.T) {
	var r renewable[string]
	ctx := httpx.WithAdmission(context.Background(), httpx.NewAdmission(1))
	v, err := r.get(ctx, func(ctx context.Context) (string, time.Time, error) {
		if httpx.AdmissionOf(ctx) != nil {
			return "", time.Time{}, errors.New("renewal ran under the caller's admission")
		}
		return "v1", time.Now().Add(time.Hour), nil
	})
	if err != nil || v != "v1" {
		t.Fatalf("got %q, %v", v, err)
	}
}

// A renewal that failed on one caller's route is that caller's failure
// alone: callers that shared it renew once more on their own route, and
// those on one route share that second renewal.
func TestRenewableSharesOnlyASuccess(t *testing.T) {
	var r renewable[string]
	release := make(chan struct{})
	var proxied, direct atomic.Int32
	fetch := func(ctx context.Context) (string, time.Time, error) {
		if httpx.RouteID(ctx) == "proxy" {
			proxied.Add(1)
			<-release
			return "", time.Time{}, errors.New("proxy refused")
		}
		direct.Add(1)
		return "direct", time.Now().Add(time.Hour), nil
	}
	viaProxy := httpx.WithRoute(context.Background(), "proxy", testClient(), nil)
	first := make(chan error, 1)
	go func() {
		_, err := r.get(viaProxy, fetch)
		first <- err
	}()
	for proxied.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if v, err := r.get(context.Background(), fetch); err != nil || v != "direct" {
				t.Errorf("a direct waiter got %q, %v; want its own renewal", v, err)
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if err := <-first; err == nil {
		t.Error("the caller whose route failed was handed a success")
	}
	if proxied.Load() != 1 || direct.Load() != 1 {
		t.Errorf("renewals: %d proxied, %d direct; want one each", proxied.Load(), direct.Load())
	}
}

// A caller that shared a failed renewal tries once more, not again and
// again: a second failure is its answer.
func TestRenewableRetriesASharedFailureOnce(t *testing.T) {
	var r renewable[string]
	release := make(chan struct{})
	var calls atomic.Int32
	fetch := func(context.Context) (string, time.Time, error) {
		if calls.Add(1) == 1 {
			<-release
		}
		return "", time.Time{}, errors.New("down")
	}
	first := make(chan error, 1)
	go func() {
		_, err := r.get(context.Background(), fetch)
		first <- err
	}()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	second := make(chan error, 1)
	go func() {
		_, err := r.get(context.Background(), fetch)
		second <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-first; err == nil {
		t.Fatal("first caller: a failure was hidden")
	}
	if err := <-second; err == nil {
		t.Fatal("waiter: a failure was hidden")
	}
	if calls.Load() != 2 {
		t.Errorf("%d renewals, want the shared one and the waiter's own", calls.Load())
	}
}
