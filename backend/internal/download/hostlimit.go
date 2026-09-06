package download

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// hostLimiter bounds how many *extra* connections are opened to any one
// host, and lowers that bound when the host pushes back.
//
// Only the additional connections from splitting are governed. Each file
// keeps its own primary connection unconditionally, so a tight budget can
// never starve a download or deadlock the pool — it only decides whether
// going faster is worth attempting.
type hostLimiter struct {
	mu      sync.Mutex
	ceiling int
	hosts   map[string]*hostBudget
}

type hostBudget struct {
	active int
	// limit is what this host has been observed to tolerate. It starts at
	// the configured ceiling and only ever drops, on evidence.
	limit int
}

func newHostLimiter(ceiling int) *hostLimiter {
	if ceiling < 0 {
		ceiling = 0
	}
	return &hostLimiter{ceiling: ceiling, hosts: make(map[string]*hostBudget)}
}

// budget returns the state for a host, creating it on first use.
// Caller holds mu.
func (l *hostLimiter) budgetLocked(host string) *hostBudget {
	b, ok := l.hosts[host]
	if !ok {
		b = &hostBudget{limit: l.ceiling}
		l.hosts[host] = b
	}
	return b
}

// reserve takes an extra-connection slot if the host has room. It never
// blocks: an extra stream is an optimisation, so if there is no room the
// caller simply carries on with what it has.
func (l *hostLimiter) reserve(host string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.budgetLocked(host)
	if b.active >= b.limit {
		return false
	}
	b.active++
	return true
}

// release returns a slot.
func (l *hostLimiter) release(host string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if b, ok := l.hosts[host]; ok && b.active > 0 {
		b.active--
	}
}

// penalise records that a host pushed back against an extra connection,
// lowering what will be attempted against it from now on.
//
// It only lowers the limit. The caller still holds the slot it reserved —
// every reservation is released exactly once, by whoever took it — so the
// active count here still includes the connection that was refused, and
// settling one below it is settling just below what was in flight when the
// refusal came. A further refusal at the same level steps the limit down
// once more, which is what walks a host that keeps refusing to zero.
func (l *hostLimiter) penalise(host string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.budgetLocked(host)
	if b.limit > b.active {
		b.limit = b.active
	}
	if b.limit > 0 {
		b.limit--
	}
	return b.limit
}

// saturated records a host refusing a connection this transfer actually
// needed, for having too many open already.
//
// That is a stronger statement than the refused extra penalise acts on, and
// it deserves a stronger answer: the refusal was not of a speculative
// addition but of the one connection the file cannot proceed without, so
// there is certainly no room for extras. Dropping the allowance to nothing
// spares every other transfer to that host a refusal apiece to learn the
// same thing. Like every other move here it only ever lowers the limit.
func (l *hostLimiter) saturated(host string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.budgetLocked(host).limit = 0
}

// limit reports a host's current allowance, for logging.
func (l *hostLimiter) limit(host string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.budgetLocked(host).limit
}

// hostOf is the key a limit applies to.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return strings.ToLower(u.Hostname())
}

// refusedExtraConnection reports whether an error looks like a host turning
// away a connection because there are already too many, rather than a
// problem with the request itself.
func refusedExtraConnection(err error) bool {
	if httpx.HasStatus(err, http.StatusTooManyRequests, http.StatusServiceUnavailable) {
		return true
	}
	if refusedForConnectionCount(err) {
		return true
	}
	// A host at its connection limit commonly just drops or refuses the
	// TCP connection rather than answering.
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}

// connectionLimitMarkers are how a host says, in the body of a refusal, that
// this caller already has as many downloads open as it is allowed.
//
// The body is what has to be read, because the status alone cannot carry
// this. A host that means "come back" has 429 to say it with and these do
// not use it: pixeldrain answers 403 with a machine-readable value beside
// the sentence, and 403 otherwise means forbidden and must stay fatal —
// reading every 403 as a queueing problem would retry a genuinely private
// file ten times before failing it.
//
// So each entry is a host's own error identifier rather than a phrase from
// its prose, which is what keeps this from matching a page that merely
// mentions downloads. A host that stops sending its identifier stops being
// recognised and fails the way it did before, which is the safe direction.
var connectionLimitMarkers = []string{
	// pixeldrain, and nova.storage which runs the same software:
	// {"success":false,"value":"max_concurrent_downloads", ...}
	"max_concurrent_downloads",
}

// refusedForConnectionCount reports whether a host turned a request away
// because this caller has too many downloads open at once.
//
// This is not a failure and not a rate limit. It is a statement about a
// queue we are ourselves filling, and it clears when one of our own
// transfers finishes — so the answer is to wait, and meanwhile to stop
// asking this host for more connections than it will grant.
func refusedForConnectionCount(err error) bool {
	se, ok := errors.AsType[*httpx.StatusError](err)
	if !ok || se.Body == "" {
		return false
	}
	body := strings.ToLower(se.Body)
	for _, marker := range connectionLimitMarkers {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}
