// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"io"
	"sync"
)

// Admission bounds how many requests one piece of work has open at once:
// one extraction, however its listings nest. Bounding each concurrent map
// on its own multiplies the bound when maps nest, and a semaphore around a
// map or an extraction would deadlock, a parent holding every slot while it
// waits on children that need one. So a slot is held only for an actual
// request: from the attempt being sent until its response body is read to
// the end or closed, or until the attempt fails. Nothing waiting — a retry
// backoff, a Retry-After, a parent waiting on its children, a caller waiting
// on another's fetch — holds one.
//
// A context carries at most one (WithAdmission); a client sending a request
// in such a context admits each attempt through it (Client.send). Media
// transfers and transfer-time resolvers run in contexts of their own and
// are not admitted.
type Admission struct {
	slots chan struct{}
}

// NewAdmission is an admission of n slots; n below 1 is one.
func NewAdmission(n int) *Admission {
	return &Admission{slots: make(chan struct{}, max(n, 1))}
}

type admissionKey struct{}

// WithAdmission is ctx with every request made in it admitted through a.
func WithAdmission(ctx context.Context, a *Admission) context.Context {
	return context.WithValue(ctx, admissionKey{}, a)
}

// AdmissionOf is the admission ctx carries, or nil.
func AdmissionOf(ctx context.Context) *Admission {
	a, _ := ctx.Value(admissionKey{}).(*Admission)
	return a
}

// WithoutAdmission is ctx with no admission, for work started in an
// admitted context that is not part of it: a resolver run when a transfer
// starts, long after the extraction that listed it.
func WithoutAdmission(ctx context.Context) context.Context {
	if AdmissionOf(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, admissionKey{}, (*Admission)(nil))
}

// admit waits for a slot of ctx's admission, if it has one, and returns
// the function that gives it back, which does so once however often it is
// called. Waiting ends with ctx.
func admit(ctx context.Context) (release func(), err error) {
	a := AdmissionOf(ctx)
	if a == nil {
		return func() {}, nil
	}
	select {
	case a.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() { once.Do(func() { <-a.slots }) }, nil
}

// admittedBody gives its request's slot back once the body is done with:
// read to its end or to an error, or closed.
type admittedBody struct {
	io.ReadCloser
	release func()
}

func (b *admittedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.release()
	}
	return n, err
}

func (b *admittedBody) Close() error {
	defer b.release()
	return b.ReadCloser.Close()
}

// Idle reports whether no slot is held.
func (a *Admission) Idle() bool { return len(a.slots) == 0 }
