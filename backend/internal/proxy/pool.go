// SPDX-License-Identifier: MIT

// Package proxy leases egress routes to downloads. The inventory, selection
// and health rules follow JohanLindvall/amzscrape's proxy pool; bbolt replaces
// its SQLite store. A lease lasts through CAPTCHA, ticket and file transfer.
package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	bolt "go.etcd.io/bbolt"
)

const Direct = "direct"

type siteStat struct {
	OK, Bad, Duration float64
	BytesPerSecond    float64
	SetupSeconds      float64
	SetupSamples      int
	LastSuccess       time.Time
	Tries, Fails      int
	Until             time.Time
	active            int
}

type entry struct {
	URL                                          string
	Origins                                      []string
	FirstSeen, LastSeen, LastTested, LastSuccess time.Time
	Fails                                        int
	Until                                        time.Time
	Sites                                        map[string]*siteStat
	WAF                                          wafStat
	// Exit is the public address the route was last found to leave by, and
	// ExitAt when; see exit.go.
	Exit   string    `json:",omitempty"`
	ExitAt time.Time `json:",omitzero"`
	client *httpx.Client
	// hostname is identity(URL), parsed once: see host. hostID numbers it
	// within the pool: see Pool.hostID. route is routeID(URL), hashed once:
	// see id. inMeasured says it is in the index's measured routes.
	hostname   string
	hostID     int
	route      string
	inMeasured bool
}

// host is the address a proxy is known by, without scheme or port, since
// several ports or protocols on one address are not extra public IPs. The URL
// never changes, so it is parsed once. Route selection asks for it of every
// entry on every lease, and parsing it each time was most of the service's
// CPU with a public feed's inventory. Caller holds the pool's lock.
func (e *entry) host() string {
	if e.hostname == "" {
		e.hostname = identity(e.URL)
	}
	return e.hostname
}

// id is the route's routeID, hashed once. Caller holds the pool's lock.
func (e *entry) id() string {
	if e.route == "" {
		e.route = routeID(e.URL)
	}
	return e.route
}

// WAF refusals do not change transport health or ordinary service scores.
type wafStat struct {
	OK, Bad      float64
	Tries, Fails int
	Until        time.Time
}

func (e *entry) stat(site string) *siteStat {
	if e.Sites == nil {
		e.Sites = make(map[string]*siteStat)
	}
	if e.Sites[site] == nil {
		e.Sites[site] = &siteStat{}
	}
	return e.Sites[site]
}

type source struct {
	Members            []string
	Attempted, Fetched time.Time
	Failed             bool
	refreshing         bool
}

type prior struct {
	Attempts, Timed int
	Valid, Seconds  float64
}

// Pool owns the Bolt database and never stores active leases. A restart may
// reuse an idle route, but must still honour its persisted host cooldown.
type Pool struct {
	mu            sync.Mutex
	entries       map[string]*entry
	sources       map[string]*source
	priors        map[string]*prior
	demand        map[string]int
	lastProbe     map[string]time.Time
	dirty         map[string]bool
	static, feeds []string
	discovery     bool
	configDirty   bool
	wake          chan struct{}
	client        *httpx.Client
	log           *slog.Logger
	db            *bolt.DB
	closed        bool
	stop          context.CancelFunc
	wg            sync.WaitGroup

	// hostIDs numbers the addresses entries are known by (entry.host), so
	// that available can total them in a slice rather than hashing every
	// entry's host into a map on every lease. addresses is that slice,
	// reused under mu. Numbers start at 1; an entry's 0 means not yet seen.
	hostIDs   map[string]int
	addresses []address

	// idx indexes the routes for ranking (index.go). leased counts the
	// leases each route holds, across sites, so that selection can find the
	// routes in use without walking the inventory.
	idx    inventory
	leased map[*entry]int
	// exitLeases counts the leases holding each exit per site, and
	// exitCooling holds an exit back for a site until a time; see exit.go.
	// Both live in memory: exitCooling is seeded from the routes on load.
	exitLeases  map[exitSite]int
	exitCooling map[exitSite]time.Time
}

// address is what available totals for one proxy address: the leases on
// all its ports and protocols, and whether any of them is cooling.
type address struct {
	active  int
	cooling bool
}

// hostID is the number of the address an entry is known by. Caller holds mu.
func (p *Pool) hostID(e *entry) int {
	if e.hostID == 0 {
		if p.hostIDs == nil {
			p.hostIDs = make(map[string]int)
		}
		id, ok := p.hostIDs[e.host()]
		if !ok {
			id = len(p.hostIDs) + 1
			p.hostIDs[e.host()] = id
		}
		e.hostID = id
	}
	return e.hostID
}

// Open restores inventory before any network request. Invalid configuration
// and a locked/corrupt database fail once at startup, without replacing it.
func Open(path string, endpoints, feeds []string, client *httpx.Client, log *slog.Logger) (*Pool, error) {
	if len(endpoints) == 0 && len(feeds) == 0 {
		return nil, errors.New("proxy pool needs endpoints or discovery feeds")
	}
	c, err := ParseConfiguration(endpoints, feeds)
	if err != nil {
		return nil, err
	}
	p := &Pool{entries: make(map[string]*entry), sources: make(map[string]*source),
		priors: make(map[string]*prior), demand: make(map[string]int), client: client, log: log,
		lastProbe: make(map[string]time.Time), dirty: make(map[string]bool),
		static: c.Endpoints, feeds: c.Feeds, discovery: true, wake: make(chan struct{}, 1)}
	if err := p.openStore(path); err != nil {
		return nil, err
	}
	now := time.Now()
	for _, raw := range p.static {
		p.ensure(raw, now)
	}
	if err := p.saveAll(); err != nil {
		p.db.Close()
		return nil, err
	}
	return p, nil
}

func (p *Pool) ensure(raw string, now time.Time) *entry {
	e := p.entries[raw]
	if e == nil {
		e = &entry{URL: raw, FirstSeen: now, LastSeen: now, Sites: make(map[string]*siteStat)}
		p.entries[raw] = e
		p.idx.valid = false
	}
	return e
}

func (p *Pool) enabled(e *entry) bool {
	if slices.Contains(p.static, e.URL) {
		return true
	}
	for _, src := range e.Origins {
		if slices.Contains(p.feeds, src) {
			return true
		}
	}
	return false
}

// Start refreshes in the background. Restored routes can be used immediately;
// slow or unavailable feeds do not delay startup or other download hosts.
func (p *Pool) Start(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.stop != nil {
		return
	}
	ctx, p.stop = context.WithCancel(ctx)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		tick := time.NewTicker(config.ProxyTick)
		defer tick.Stop()
		for {
			p.Refresh(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			case <-p.wake:
			}
		}
	}()
}

// Close is called after workers release their leases.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	if p.stop != nil {
		p.stop()
	}
	p.mu.Unlock()
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.client != nil {
			e.client.CloseIdleConnections()
		}
	}
	var err error
	if p.configDirty {
		err = p.saveAll()
	} else {
		err = p.flushMeasurements()
	}
	return errors.Join(err, p.db.Close())
}

// Lease reserves one slot for a service on one route, across aliases and jobs.
type Lease struct {
	pool  *Pool
	entry *entry
	site  string
	waf   bool
	// files is how many leases the site allows one address, and exit the
	// route's exit while the lease holds it (ClaimExit). Guarded by pool.mu.
	files int
	exit  string
	// client is the route's client when the lease was taken, which the
	// lease's requests go on using whatever becomes of the entry's.
	client *httpx.Client
	once   sync.Once
	// Measurement and upgrade state is guarded by pool.mu. Progress
	// samples are in memory; the background refresh flushes dirty rows.
	started                  time.Time
	released, sawBytes       bool
	measured, constrained    bool
	windowBytes, received    int64
	windowTime, transferTime time.Duration
	lastUpgrade              time.Time
	upgradeWins              int
}

func routeID(raw string) string {
	if raw == Direct {
		return httpx.DirectRoute
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
}

func (l *Lease) ID() string { return routeID(l.entry.URL) }

// Configured checks a reserved replacement again after a live source edit.
// Existing requests can finish on removed routes; a not-yet-used upgrade
// should not start a new request there.
func (l *Lease) Configured() bool {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	return !l.pool.closed && l.pool.enabled(l.entry)
}

func (l *Lease) Context(ctx context.Context) context.Context {
	return httpx.WithRoute(ctx, l.ID(), l.client, l.observe)
}

// ProbeContext is Context for a question put to a third party through the
// route, such as which address it leaves by. The route answers for carrying
// the request: failing to, a dial or a dropped connection, counts against it
// as on any request, and an answer shows it alive. What the third party says
// back does not count, a refusal or an outage of its own, since that is not
// the service's host speaking about the route's address.
func (l *Lease) ProbeContext(ctx context.Context) context.Context {
	return httpx.WithRoute(ctx, l.ID(), l.client, func(o httpx.RouteObservation) {
		if o.Status == 0 || (o.Status >= 200 && o.Status < 300) {
			l.observe(o)
		}
	})
}

func (l *Lease) Release() {
	l.once.Do(func() {
		l.pool.mu.Lock()
		defer l.pool.mu.Unlock()
		p, e := l.pool, l.entry
		e.stat(l.site).active--
		p.unleaseAddress(e, l.site)
		p.unclaimExit(l)
		if p.leased[e] > 1 {
			p.leased[e]--
		} else {
			delete(p.leased, e)
			// A route shelved as dead or broken is not leased again until its
			// cooldown is over, a day for one that never worked, and its
			// client is only memory until then. Route hunting tries thousands
			// of such routes an hour, and each one kept its client for good.
			// A lease taken later makes a new one.
			if e.client != nil && e.Until.After(time.Now()) {
				e.client.CloseIdleConnections()
				e.client = nil
			}
		}
		l.released = true
	})
}

// Failed reports failures recognised above HTTP: a stalled media stream or a
// web page served in place of the file. Callers exclude user cancellation.
func (l *Lease) Failed(err error, elapsed time.Duration, transport bool) {
	status := http.StatusForbidden
	if transport {
		status = 0
	}
	l.observe(httpx.RouteObservation{Err: err, Status: status, Duration: elapsed})
}

// RefusedDirect records a challenge encountered before a file opted into
// routing. The pool must not immediately select the same refused address.
// No lease was held, so this observation does not alter active counts.
func (p *Pool) RefusedDirect(site string, err error, elapsed time.Duration) {
	p.mu.Lock()
	e := p.entries[Direct]
	p.mu.Unlock()
	if e != nil {
		l := &Lease{pool: p, entry: e, site: site}
		l.Failed(err, elapsed, false)
	}
}

// Acquire never waits or writes to disk. A preferred route lets a paused or
// interrupted file reuse its unexpired, IP-bound ticket when it is healthy.
func (p *Pool) Acquire(site, preferred string, files int) *Lease {
	return p.AcquireFor(site, preferred, files, 0, site == "cloudflare")
}

// AcquireFor ranks routes for the bytes this file still needs. A short
// remaining tail values setup latency more than a long transfer does.
// Selection and reservation share one lock: unrated and preferred routes
// obey the same per-address limit, starting before the first request.
func (p *Pool) AcquireFor(site, preferred string, files int, remaining int64, waf bool) *Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	now, ix := time.Now(), p.index()
	var chosen *entry
	if e := ix.byID[preferred]; preferred != "" && e != nil && p.eligible(ix, e, site, files, now, waf) {
		chosen = e
	}
	if chosen == nil {
		var measured []*entry
		for _, e := range ix.measured {
			if e.speed(site) != 0 && p.eligible(ix, e, site, files, now, waf) {
				measured = append(measured, e)
			}
		}
		chosen = p.choose(site, remaining, waf, measured, func() *entry {
			return p.drawUnmeasured(ix, site, files, now, waf)
		})
	}
	if chosen == nil {
		return nil
	}
	l := p.lease(chosen, site)
	if l != nil {
		l.waf, l.files = waf, files
	}
	return l
}

// available lists every route that could take a transfer for site now.
// Ranking asks eligible of the routes it considers instead (index.go); this
// is for listing them all at once.
func (p *Pool) available(site string, files int, now time.Time, waf bool) []*entry {
	// Different ports/protocols on the same proxy address are not extra
	// public IPs: their leases count together, conservatively, and a
	// cooldown on one holds back all of them. Nor may two leases share an
	// exit (exit.go).
	size := len(p.hostIDs) + 1
	p.addresses = slices.Grow(p.addresses[:0], size)[:size]
	clear(p.addresses)
	for _, e := range p.entries {
		id := p.hostID(e)
		for id >= len(p.addresses) { // an address not seen before
			p.addresses = append(p.addresses, address{})
		}
		a := &p.addresses[id]
		if s := e.Sites[site]; s != nil {
			a.active += s.active
			a.cooling = a.cooling || s.Until.After(now)
		}
		a.cooling = a.cooling || (waf && e.WAF.Until.After(now))
	}
	var candidates []*entry
	for _, e := range p.entries {
		// Cheapest first: a long-running public pool is mostly dead
		// addresses sitting out a day, which never get to the settings.
		if e.Until.After(now) || !p.exitFree(e, site, files, now) {
			continue
		}
		if e.untested(site, waf) { // see untested
			if p.enabled(e) && e.freeOnItsOwn(site, files, now, waf) {
				candidates = append(candidates, e)
			}
			continue
		}
		if a := p.addresses[e.hostID]; a.cooling || a.active >= max(1, files) || !p.enabled(e) {
			continue
		}
		candidates = append(candidates, e)
	}
	return candidates
}

func (p *Pool) lease(chosen *entry, site string) *Lease {
	if chosen.client == nil {
		// URLs were validated before entering the inventory.
		var err error
		chosen.client, err = p.client.ThroughProxy(chosen.URL)
		if err != nil {
			return nil
		}
	}
	chosen.stat(site).active++
	p.leaseAddress(chosen, site)
	if p.leased == nil {
		p.leased = make(map[*entry]int)
	}
	p.leased[chosen]++
	return &Lease{pool: p, entry: chosen, site: site, client: chosen.client, started: time.Now()}
}

func identity(raw string) string {
	if raw == Direct {
		return Direct
	}
	u, _ := url.Parse(raw)
	return u.Hostname()
}

// Cooldown records an authoritative per-address host timer without treating
// it as a dead proxy. Other services can keep using the endpoint.
//
// It is written on this route alone. The address's other tried ports take it
// up where routes are chosen (addressCooling), and those never tried may
// leave through another public address altogether (untested). Writing it on
// each port was a synced transaction per port, under the pool's lock: on a
// gateway of thousands, seconds in which no route could be leased.
func (l *Lease) Cooldown(until time.Time) {
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	if s := l.entry.stat(l.site); until.After(s.Until) {
		s.Until = until
		p.persist(l.entry, l.site)
	}
	p.coolExit(l, until)
}

func (l *Lease) observe(o httpx.RouteObservation) {
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	e, now := l.entry, time.Now()
	if _, challenged := errors.AsType[*httpx.ChallengeError](o.Err); challenged {
		e.WAF.Tries++
		e.WAF.OK *= config.ProxyOutcomeDecay
		e.WAF.Bad = e.WAF.Bad*config.ProxyOutcomeDecay + 1
		e.WAF.Fails++
		e.WAF.Until = now.Add(util.Backoff(min(e.WAF.Fails-1, 30), config.ProxyRefusalBase, config.ProxyRefusalMax))
		p.persist(e, l.site)
		return
	}
	if l.waf && o.Err == nil {
		e.WAF.Tries++
		e.WAF.Bad *= config.ProxyOutcomeDecay
		e.WAF.OK = e.WAF.OK*config.ProxyOutcomeDecay + 1
		e.WAF.Fails = 0
	}
	s := e.stat(l.site)
	ok := o.Err == nil
	seconds := o.Duration.Seconds()
	if p.priors[l.site] == nil {
		p.priors[l.site] = &prior{}
	}
	first := p.priors[l.site]
	if s.Tries == 0 {
		first.Attempts++
		first.Timed++
		first.Seconds += seconds
		if ok {
			first.Valid++
		}
	}
	s.Tries++
	s.OK *= config.ProxyOutcomeDecay
	s.Bad *= config.ProxyOutcomeDecay
	if ok {
		s.OK++
	} else {
		s.Bad++
	}
	if s.Tries == 1 {
		s.Duration = seconds
	} else {
		s.Duration = config.ProxyDurationAlpha*seconds + (1-config.ProxyDurationAlpha)*s.Duration
	}
	e.LastTested = now
	if ok {
		// API replies and CAPTCHA images measure responsiveness, not file
		// throughput. Only substantial response bodies train bandwidth.
		if o.Bytes >= config.ProxyThroughputMinBytes && !l.measured && !l.constrained {
			rate := float64(o.Bytes) / max(seconds, config.ProxyMinSeconds)
			if s.BytesPerSecond == 0 {
				s.BytesPerSecond = rate
				p.noteMeasured(e)
			} else {
				s.BytesPerSecond = config.ProxyDurationAlpha*rate + (1-config.ProxyDurationAlpha)*s.BytesPerSecond
			}
		}
		e.LastSuccess = now
		s.LastSuccess = now
		e.Fails, s.Fails = 0, 0
		// An observed success does not cancel a server's still-running timer.
	} else if o.Status == 0 || o.Status == http.StatusProxyAuthRequired || (o.Status >= 200 && o.Status < 300) {
		e.Fails++
		wait := config.ProxyDeadCooldown
		if !e.LastSuccess.IsZero() || slices.Contains(p.static, e.URL) {
			wait = util.Backoff(min(e.Fails-1, 30), config.ProxyBrokenCooldown, config.ProxyDeadCooldown)
		}
		e.Until = now.Add(wait)
	} else {
		s.Fails++
		s.Until = now.Add(util.Backoff(min(s.Fails-1, 30), config.ProxyRefusalBase, config.ProxyRefusalMax))
		if !o.RetryAfter.IsZero() {
			s.Until = o.RetryAfter
		}
		// A refusal is the host's answer to the address it saw.
		p.coolExit(l, s.Until)
	}
	p.persist(e, l.site)
}
