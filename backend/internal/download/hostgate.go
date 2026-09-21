package download

import (
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// A global admission queue for a host that cannot take what it is given.
//
// The evidence is a 503, which is the host saying so itself. What it is
// *not* is a statement about one transfer: the storage backend behind a
// large album serves a few files, falls over, and answers everything else
// 503 — so holding one item back while its siblings carry on arriving
// achieves nothing, and each of those siblings waiting out its own private
// retry loop is the same crowd coming back in step.
//
// So this is one queue per host, across every job and every worker. A
// transfer waits here for a slot before it opens anything, and the number of
// slots is what the host has shown it can take: one fewer at each refusal,
// one more for each transfer that runs to the end.
//
// Two things decide the key, and both were got wrong first. It is the host
// that actually answered — the storage server the link resolved to, not the
// site the job was submitted from. A search on an index site is the case
// that proves it: every file in that queue comes from somewhere else
// entirely, and throttling the index would slow a queue without touching
// the host that is struggling, under a message naming the wrong machine.
// And a host is only throttled once it has served something: a 503 from a
// host that has never got a transfer going is a host that is down, where
// giving it less to do neither helps it nor gets the file.
type hostGate struct {
	mu    sync.Mutex
	hosts map[string]*hostAdmission
}

type hostAdmission struct {
	// limit is how many transfers this host will be given at once. Zero
	// means it has not asked to be held back.
	limit int
	// active is how many are in flight against it right now.
	active int
	// served records that a transfer to this host has got going, which is
	// what separates an overloaded host from an absent one.
	served bool
	// refusals counts 503s in a row, and sets how long the host is left
	// alone after each. A transfer that gets going clears it.
	refusals int
	// until is when the host may be asked again. A cap on its own does not
	// slow anything down when the queue behind it is long enough: the next
	// item takes the freed slot the instant it is given back, so the host
	// is asked again as fast as the dispatcher can turn round.
	until time.Time
	// wake is closed when a slot frees or the limit rises, and replaced.
	// Waiters select on it alongside their own cancellation.
	wake chan struct{}
}

func newHostGate() *hostGate {
	return &hostGate{hosts: make(map[string]*hostAdmission)}
}

// stateLocked returns a host's admission state, creating it on first use.
func (g *hostGate) stateLocked(host string) *hostAdmission {
	st, ok := g.hosts[host]
	if !ok {
		st = &hostAdmission{wake: make(chan struct{})}
		g.hosts[host] = st
	}
	return st
}

// tryAdmit takes a slot at host if there is one, and returns the function
// that gives it back. It never waits, and that is the whole point: a worker
// blocked here is a worker not downloading from anywhere else, so an item
// that cannot have a slot goes back to the queue instead and the worker
// picks up whatever is next. The queue is where it waits; the dispatcher
// skips it until this host has room (Manager.hostFullLocked).
//
// The limit returned alongside a refusal is what the host is taking, so the
// item can say what it is waiting for.
func (g *hostGate) tryAdmit(host string) (release func(), limit int, ok bool) {
	if host == "" {
		return func() {}, 0, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	st := g.stateLocked(host)
	if st.limit > 0 && st.active >= st.limit {
		return nil, st.limit, false
	}
	if time.Now().Before(st.until) {
		return nil, st.limit, false
	}
	st.active++
	return func() { g.release(host) }, st.limit, true
}

// full reports whether a host is already giving out every slot it has. The
// dispatcher asks before starting an item, so an item whose turn has not
// come waits in the queue rather than in a worker.
func (g *hostGate) full(host string) bool {
	if host == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.hosts[host]
	if !ok {
		return false
	}
	return (st.limit > 0 && st.active >= st.limit) || time.Now().Before(st.until)
}

// waiting reports how many transfers are queued for a host and what it is
// allowing, for a note that would otherwise say only "waiting".
func (g *hostGate) waiting(host string) (limit, active int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if st, ok := g.hosts[host]; ok {
		return st.limit, st.active
	}
	return 0, 0
}

// release gives a slot back and wakes whoever is queued for it.
func (g *hostGate) release(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.hosts[host]
	if !ok {
		return
	}
	if st.active > 0 {
		st.active--
	}
	g.wakeLocked(st)
}

// serving records that a transfer to this host got going: a response
// accepted as the file itself rather than a refusal or a page. It is the
// evidence the throttle waits for.
func (g *hostGate) serving(host string) {
	if host == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	st := g.stateLocked(host)
	st.served = true
	// A host that is serving is not in the middle of refusing, so the
	// escalating wait starts over from here rather than from whatever it
	// had climbed to the last time this host had a bad minute.
	st.refusals = 0
	st.until = time.Time{}
}

// overloaded records a refusal and returns what the host is now allowed at
// once, or zero when it was not throttled — which means it has never served
// anything, and is unavailable rather than overloaded.
//
// The cap becomes one fewer than was in flight when the host said so, and
// one fewer again on each further refusal: several siblings failing together
// walk it down to what the host can actually sustain within a few refusals
// rather than in one guess. It never reaches zero, because a host that can
// serve nothing at all is a failure to report rather than a rate to find.
//
// Transfers already running are left alone, for the same reason the
// free-space floor leaves them alone: their connection is open and the host
// has accepted it. The cap governs what starts next.
func (g *hostGate) overloaded(host string, base, max time.Duration) (limit int, wait time.Duration) {
	if host == "" {
		return 0, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	st := g.stateLocked(host)

	// How long the host is left alone, which is the half that a cap cannot
	// do on its own. With a thousand items behind it, a freed slot is taken
	// again immediately, so without this the host is asked as fast as the
	// dispatcher can turn round however small the cap is.
	st.refusals++
	wait = util.Backoff(st.refusals-1, base, max)
	if until := time.Now().Add(wait); until.After(st.until) {
		st.until = until
	}

	if !st.served {
		return 0, wait
	}
	// One at a time, from the first refusal. Walking the cap down a step
	// per refusal sounds gentler and is not: while it walks, the transfers
	// admitted before it started are all still going, so the host being
	// asked for less is also the host still being asked by six things at
	// once. A host that has said it cannot cope gets one attempt until it
	// says otherwise, and eased puts the slots back one at a time as
	// transfers actually finish.
	st.limit = 1
	return st.limit, wait
}

// quiet reports how long a host is to be left alone, for a note that would
// otherwise have nothing to say about when anything will happen.
func (g *hostGate) quiet(host string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.hosts[host]
	if !ok {
		return 0
	}
	if wait := time.Until(st.until); wait > 0 {
		return wait
	}
	return 0
}

// eased gives a throttled host one slot back, having just served a transfer
// to the end.
//
// Down on evidence and up on evidence: a completed transfer is the only
// thing that says the host is coping, and stepping up one at a time means
// the next refusal costs one slot rather than undoing the whole recovery.
// Once a host is allowed as many as the queue would ever run at once the cap
// has stopped meaning anything, and it is forgotten rather than kept at a
// number it can never reach.
func (g *hostGate) eased(host string, ceiling int) {
	if host == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	st, ok := g.hosts[host]
	if !ok || st.limit <= 0 {
		return
	}
	if ceiling > 0 && st.limit+1 >= ceiling {
		st.limit = 0
	} else {
		st.limit++
	}
	g.wakeLocked(st)
}

// wakeLocked releases everyone queued for a host so they can re-check.
// Caller holds mu.
func (g *hostGate) wakeLocked(st *hostAdmission) {
	close(st.wake)
	st.wake = make(chan struct{})
}
