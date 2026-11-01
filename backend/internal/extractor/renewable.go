// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// renewable is a credential an extractor holds for every job: a guest
// account's token, a bearer token, a player token. It keeps the last value
// fetch validated until it expires or a caller reports it refused, and
// renews it once however many callers find it stale together.
//
// The rules it exists to keep, which each host's copy used to get slightly
// differently:
//
//   - Only a successful, validated value is cached. A failed renewal leaves
//     nothing behind, so the next caller tries again.
//   - Callers that saw the same value refused share one renewal. A refusal
//     of a value that has already been replaced is old news: renew returns
//     the replacement rather than discarding it.
//   - Only a success is shared. A renewal runs with the values of the
//     caller that started it, its route among them, and its failure may be
//     that route's: a proxy's error, an address refused, or the moment's.
//     A caller that shared a failed renewal renews once more with its own
//     context, joining only a renewal already under way on its own route,
//     and that answer is its own, success or failure.
//   - Waiting is cancelable. The renewal itself runs detached from any one
//     caller's cancellation (keeping its context's values, the route and the
//     logger), so a caller that gives up neither fails the others nor
//     throws away a value about to arrive, and from its admission, so the
//     other callers do not wait behind its job's queue of requests.
//
// What counts as refused, how a credential is requested and decoded, and
// whether to retry stay with the extractor: a 401, a status in a JSON
// envelope and a geo or DRM refusal are different conditions.
//
// The zero value is ready to use, so an extractor holds one as a field.
type renewable[T comparable] struct {
	// now is the clock, nil for time.Now; a field so tests can move it.
	now func() time.Time

	mu      sync.Mutex
	value   T
	valid   bool
	expires time.Time
	pending *renewal[T]
}

// renewFunc obtains a fresh, validated value and when it expires: the
// provider's own expiry less a margin where it states one, else a TTL from
// config/limits.go.
type renewFunc[T any] func(context.Context) (T, time.Time, error)

// renewal is one fetch in flight, shared by everyone waiting for it.
type renewal[T any] struct {
	done chan struct{}
	// route is the route the renewal went out on (httpx.RouteID).
	route string
	value T
	err   error
}

func (r *renewable[T]) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// get is the current value, fetching one when there is none or it expired.
func (r *renewable[T]) get(ctx context.Context, fetch renewFunc[T]) (T, error) {
	return r.obtain(ctx, fetch, nil)
}

// renew is a value other than stale, which the caller was refused with:
// the replacement another caller already obtained, or a fresh one.
func (r *renewable[T]) renew(ctx context.Context, stale T, fetch renewFunc[T]) (T, error) {
	return r.obtain(ctx, fetch, &stale)
}

func (r *renewable[T]) obtain(ctx context.Context, fetch renewFunc[T], stale *T) (T, error) {
	route := httpx.RouteID(ctx)
	r.mu.Lock()
	if stale != nil && r.valid && r.value == *stale {
		r.valid = false
	}
	retried := false
	for {
		if r.valid && r.clock().Before(r.expires) {
			v := r.value
			r.mu.Unlock()
			return v, nil
		}
		// A retry joins only a renewal on its own route: the one it shared
		// failed, perhaps through another caller's proxy.
		p, mine := r.pending, false
		if p == nil || (retried && p.route != route) {
			p, mine = r.start(ctx, fetch, p == nil), true
		}
		r.mu.Unlock()

		select {
		case <-p.done:
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
		// A failed renewal is only the answer of the caller whose context
		// made it. One that shared another's tries once more, with its own
		// context, as it would have had nobody else asked.
		if p.err == nil || mine || retried {
			return p.value, p.err
		}
		retried = true
		r.mu.Lock()
	}
}

// start begins a renewal with ctx's values, registering it as the one
// callers share when shared is set. Caller holds mu.
func (r *renewable[T]) start(ctx context.Context, fetch renewFunc[T], shared bool) *renewal[T] {
	p := &renewal[T]{done: make(chan struct{}), route: httpx.RouteID(ctx)}
	if shared {
		r.pending = p
	}
	// Detached from the first caller's admission too: the value is
	// every job's, and the jobs waiting on it are not to queue behind
	// that one job's listing (httpx.Admission).
	go r.run(httpx.WithoutAdmission(context.WithoutCancel(ctx)), fetch, p)
	return p
}

func (r *renewable[T]) run(ctx context.Context, fetch renewFunc[T], p *renewal[T]) {
	value, expires, err := fetch(ctx)
	r.mu.Lock()
	if err == nil {
		r.value, r.valid, r.expires = value, true, expires
	}
	if r.pending == p {
		r.pending = nil
	}
	r.mu.Unlock()
	p.value, p.err = value, err
	close(p.done)
}
