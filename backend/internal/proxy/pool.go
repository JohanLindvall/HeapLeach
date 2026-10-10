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
	client                                       *httpx.Client
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
	last          map[string]string
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
		priors: make(map[string]*prior), last: make(map[string]string), demand: make(map[string]int), client: client, log: log,
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
	once  sync.Once
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
	return httpx.WithRoute(ctx, l.ID(), l.entry.client, l.observe)
}

func (l *Lease) Release() {
	l.once.Do(func() {
		l.pool.mu.Lock()
		defer l.pool.mu.Unlock()
		l.entry.stat(l.site).active--
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
func (p *Pool) AcquireFor(site, preferred string, files int, remaining int64, waf bool) *Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	candidates := p.available(site, files, time.Now(), waf)
	if len(candidates) == 0 {
		return nil
	}
	slices.SortFunc(candidates, func(a, b *entry) int { return compareURL(a.URL, b.URL) })
	var chosen *entry
	for _, e := range candidates {
		if preferred == "" {
			break
		}
		if routeID(e.URL) == preferred {
			chosen = e
			break
		}
	}
	if chosen == nil {
		chosen = p.selectFor(site, candidates, remaining, waf)
	}
	l := p.lease(chosen, site)
	if l != nil {
		l.waf = waf
	}
	return l
}

func (p *Pool) available(site string, files int, now time.Time, waf bool) []*entry {
	var candidates []*entry
	active := make(map[string]int)
	cooling := make(map[string]bool)
	for _, e := range p.entries {
		active[identity(e.URL)] += e.stat(site).active
		if e.stat(site).Until.After(now) || (waf && e.WAF.Until.After(now)) {
			cooling[identity(e.URL)] = true
		}
	}
	for _, e := range p.entries {
		if !p.enabled(e) || e.Until.After(now) || cooling[identity(e.URL)] {
			continue
		}
		// Different ports/protocols on the same proxy address are not extra
		// public IPs. Count their leases together, conservatively.
		if active[identity(e.URL)] < max(1, files) {
			candidates = append(candidates, e)
		}
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
	p.last[site] = chosen.URL
	return &Lease{pool: p, entry: chosen, site: site, started: time.Now()}
}

func compareURL(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
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
func (l *Lease) Cooldown(until time.Time) {
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	for _, e := range p.entries {
		if identity(e.URL) == identity(l.entry.URL) && until.After(e.stat(l.site).Until) {
			e.stat(l.site).Until = until
			p.persist(e, l.site)
		}
	}
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
	}
	p.persist(e, l.site)
}
