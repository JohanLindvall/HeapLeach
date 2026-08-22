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
	// A host at its connection limit commonly just drops or refuses the
	// TCP connection rather than answering.
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}
