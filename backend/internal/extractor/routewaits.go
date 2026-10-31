// SPDX-License-Identifier: MIT

package extractor

import (
	"cmp"
	"context"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// routeWaits keeps, by egress route, until when a host has made that route's
// address wait: a used-up quota, a cooldown between free downloads. The rest
// of a queue learns a wait from the first file that met it rather than each
// asking to be told again, and one route's wait says nothing of another's.
// The normal connection is kept under httpx.DirectRoute.
type routeWaits struct {
	mu    sync.Mutex
	until map[string]time.Time
}

// waitRoute is the route a request made with ctx leaves by.
func waitRoute(ctx context.Context) string {
	return cmp.Or(httpx.RouteID(ctx), httpx.DirectRoute)
}

// hold records that ctx's route waits for d, keeping a longer wait it
// already has, and returns that route's wait.
func (w *routeWaits) hold(ctx context.Context, d time.Duration, reason string) error {
	route := waitRoute(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.until == nil {
		w.until = make(map[string]time.Time)
	}
	if until := time.Now().Add(d); until.After(w.until[route]) {
		w.until[route] = until
	}
	return &WaitError{Until: w.until[route], Reason: reason}
}

// waiting returns ctx's route's wait while it lasts, and nil after.
func (w *routeWaits) waiting(ctx context.Context, reason string) error {
	route := waitRoute(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	if until := w.until[route]; time.Now().Before(until) {
		return &WaitError{Until: until, Reason: reason}
	}
	return nil
}
