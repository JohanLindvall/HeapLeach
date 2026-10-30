// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"time"
)

// Accessors only the tests use, to look at state the code itself never
// needs to read back.

// storedNote is the note an item carries, for a view with nothing better.
// The manager has something better — see Manager.itemNoteLocked.
func storedNote(it *Item) string { return it.Note }

// status derives the job state from its phase and its items.
func (j *Job) status() Status { return j.statusFrom(j.tally()) }

// take blocks until the caller may move at least one byte, and reports how
// many. The answer is never larger than want and never zero without an
// error.
func (t *throttle) take(ctx context.Context, want int) (int, error) {
	n, gate, err := t.takeReservation(ctx, want)
	t.finishReservation(n, n, gate)
	return n, err
}

// limit reports a host's current allowance.
func (l *hostLimiter) limit(host string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.budgetLocked(host).limit
}

// quiet reports how long a host is to be left alone.
func (g *hostGate) quiet(host string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.hosts[host]
	if !ok {
		return 0
	}
	return max(time.Until(st.until), 0)
}
