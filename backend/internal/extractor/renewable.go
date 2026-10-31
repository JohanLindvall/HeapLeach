// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"sync"
	"time"
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
//   - Waiting is cancelable. The renewal itself runs detached from any one
//     caller's cancellation (keeping its context's values, the route and the
//     logger), so a caller that gives up neither fails the others nor
//     throws away a value about to arrive.
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
	done  chan struct{}
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
	r.mu.Lock()
	if stale != nil && r.valid && r.value == *stale {
		r.valid = false
	}
	if r.valid && r.clock().Before(r.expires) {
		v := r.value
		r.mu.Unlock()
		return v, nil
	}
	p := r.pending
	if p == nil {
		p = &renewal[T]{done: make(chan struct{})}
		r.pending = p
		go r.run(context.WithoutCancel(ctx), fetch, p)
	}
	r.mu.Unlock()

	select {
	case <-p.done:
		return p.value, p.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func (r *renewable[T]) run(ctx context.Context, fetch renewFunc[T], p *renewal[T]) {
	value, expires, err := fetch(ctx)
	r.mu.Lock()
	if err == nil {
		r.value, r.valid, r.expires = value, true, expires
	}
	r.pending = nil
	r.mu.Unlock()
	p.value, p.err = value, err
	close(p.done)
}
