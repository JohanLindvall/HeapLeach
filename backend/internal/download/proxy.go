// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/JohanLindvall/proxytron"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

// PerRoute applies the host's file cap to each address. Only supported
// download protocols may opt in; unrelated hosts keep their normal client.
func proxyEligible(it *Item) bool {
	return it.pace != nil && it.pace.PerRoute && config.IsProxyGroup(it.pace.Group)
}

func (m *Manager) usesProxies(it *Item) bool {
	return m.proxyEnabled && m.proxies != nil && proxyEligible(it)
}

// updateProxyDemandLocked lets discovery replenish only when the runnable
// queue needs routes. Other hosts, deferred items, a pause or full disk
// must not turn a quiet pool into a five-minute feed poller. Active proxy
// transfers count toward demand; workers occupied by other hosts do not.
func (m *Manager) updateProxyDemandLocked() {
	if m.proxies != nil {
		m.proxies.SetDemand(m.proxyDemandLocked())
	}
}

func (m *Manager) proxyDemandLocked() map[string]int {
	wanted := make(map[string]int)
	if !m.proxyEnabled || m.closing || m.throttle.isPaused() || m.lowOnSpace() {
		return wanted
	}
	running := 0
	for _, n := range m.proxyRunning {
		running += n
	}
	capacity := max(0, m.limit-(m.running-running))
	blocked := make(map[string]bool)
	for _, site := range config.ProxyGroups {
		active := min(m.proxyRunning[site], capacity)
		capacity -= active
		blocked[site] = m.hostActive[groupKey(site)] > m.proxyRunning[site]
		if !blocked[site] {
			wanted[site] = active
		}
	}
	now := time.Now()
	for _, it := range m.queue {
		if capacity == 0 {
			break
		}
		// A file its job holds back wants no route yet (routeWantedLocked).
		if proxyEligible(it) && !blocked[it.pace.Group] && it.Status == StatusQueued && !it.inFlight &&
			!it.notBefore.After(now) && !m.jobHeldLocked(it) {
			wanted[it.pace.Group]++
			capacity--
		}
	}
	// Source extraction runs before there are files to dispatch. It still
	// needs usable routes, even when this is the first job in an empty queue.
	wanted[config.GroupWAF] = max(wanted[config.GroupWAF], min(m.proxyResolving, m.limit))
	return wanted
}

// ProxyPage samples active transfer speeds under mu, then reads the large
// inventory without holding the queue lock.
func (m *Manager) ProxyPage(q proxy.Query) proxy.Page {
	if q.Site == "" {
		q.Site = config.GroupKeep2Share
	}
	m.mu.Lock()
	pool := m.proxies
	speeds := make(map[string]float64)
	for _, job := range m.jobs {
		for _, it := range job.Items {
			if it.inFlight && it.route != nil && it.pace.Group == q.Site {
				speeds[it.route.ID()] += it.speed
			}
		}
	}
	m.mu.Unlock()
	if pool == nil {
		return proxy.EmptyPage(q)
	}
	page := pool.Page(q.Site, q)
	for i := range page.Rows {
		page.Rows[i].CurrentSpeed = speeds[page.Rows[i].ID]
	}
	return page
}

// noteWaitingForRoute is the note of a file that waits for a proxy route to
// come free, and the reason given for a wait that only a route ends.
const noteWaitingForRoute = "Waiting for an available download route"

type routeGroup struct {
	name  string
	files int
	waf   bool
}

// leaseRouteLocked is nonblocking: a busy/cooling route keeps the item in the
// queue, leaving workers free for other hosts. Called only at dispatch.
func (m *Manager) leaseRouteLocked(it *Item, unavailable map[routeGroup]bool) bool {
	if !m.usesProxies(it) {
		return true
	}
	if it.route != nil {
		return true
	}
	group := routeGroup{it.pace.Group, it.pace.Files, it.pace.WAF}
	if unavailable[group] {
		it.Note, it.notBefore = noteWaitingForRoute, time.Time{}
		return false
	}
	it.route = m.proxies.AcquireFor(it.pace.Group, it.preferredRoute, it.pace.Files, it.Size-it.downloaded.Load(), it.pace.WAF)
	if it.route == nil {
		// A large queue must not rescan the whole inventory for every file
		// while every route in the same service is already busy/cooling.
		unavailable[group] = true
		it.Note = noteWaitingForRoute
		// The wait that brought it here has ended (nextLocked checked), and
		// one left in the past hides the note: a queue of files waiting
		// for routes read as a queue of files waiting for nothing.
		it.notBefore = time.Time{}
		return false
	}
	it.preferredRoute = it.route.ID()
	return true
}

func (m *Manager) transferRouted(ctx context.Context, it *Item) error {
	m.mu.Lock()
	lease := it.route
	ctx = extractor.WithLogger(ctx, m.log.With("job", it.JobID, "item", it.ID))
	m.mu.Unlock()
	if lease == nil {
		start := time.Now()
		err := m.transfer(ctx, it)
		if ctx.Err() == nil && m.moveOntoRoutes(it, err) {
			return &extractor.WaitError{Until: time.Now(), Reason: "Going on through a proxy"}
		}
		_, challenged := errors.AsType[*httpx.ChallengeError](err)
		if ctx.Err() != nil || (!challenged && (!it.proxyFallback || !retryRouteFailure(err))) {
			return err
		}
		m.mu.Lock()
		pool := m.proxies
		fallback := m.proxyEnabled && pool != nil
		if fallback {
			it.pace, it.preferredRoute = challengePace(it.pace), ""
			it.routeAttempts = 1 // the failed direct attempt precedes the first proxy
			if it.refresh != nil {
				it.resolve = it.refresh
			}
		}
		m.mu.Unlock()
		if fallback {
			pool.RefusedDirect(it.pace.Group, err, time.Since(start))
			return &extractor.WaitError{Until: time.Now(), Reason: "Retrying through a proxy"}
		}
		return err
	}
	if it.check != nil {
		// A question about the file, over this machine's own connection,
		// before the route is used: a file the host will not let anyone
		// have fails here, at no cost to the route.
		if err := it.check(ctx); err != nil {
			lease.Release()
			return err
		}
	}
	start := time.Now()
	var err error
	for {
		attempt, abort := context.WithCancel(ctx)
		m.mu.Lock()
		it.routeAttempts++
		it.routeCancel, it.routeResumable = abort, false
		if m.throttling(it) {
			lease.Constrain()
		}
		m.mu.Unlock()
		if err = m.claimExit(attempt, it, lease); err == nil {
			err = m.transfer(lease.Context(attempt), it)
		}
		abort()
		m.mu.Lock()
		upgrade := it.routeUpgrade
		it.routeUpgrade, it.routeCancel = nil, nil
		retry := upgrade != nil && ctx.Err() == nil && errors.Is(err, context.Canceled)
		switchRoute := retry && m.proxyEnabled && upgrade.Configured() &&
			!m.throttle.isPaused() && m.throttle.currentLimit() == 0 && it.limiter.currentLimit() == 0
		if switchRoute {
			it.route, it.preferredRoute = upgrade, upgrade.ID()
			it.speed, it.proxyRetries = 0, 0
			it.lastBytes, it.lastSample = it.downloaded.Load(), time.Now()
		}
		m.mu.Unlock()
		if !switchRoute {
			if upgrade != nil {
				upgrade.Release()
			}
			if retry {
				// Settings changed after sampling reserved a replacement.
				// Resume on the original lease instead of turning our own
				// optimization cancellation into a user-visible failure.
				continue
			}
			break
		}
		// transfer has closed the response and flushed the part file before
		// releasing the old address or minting a ticket on the new one.
		lease.Release()
		lease, start = upgrade, time.Now()
	}
	if ctx.Err() == nil {
		cause := err
		m.noteRouteOutcome(it, err)
		if _, challenged := errors.AsType[*httpx.ChallengeError](err); challenged {
			m.mu.Lock()
			it.pace = challengePace(it.pace)
			if it.refresh != nil {
				it.resolve = it.refresh
			}
			m.mu.Unlock()
		}
		if stall, ok := errors.AsType[*stalledError](err); ok {
			lease.Failed(err, time.Since(start), true)
			err = &routeTransferError{err: &httpx.RouteError{Err: err}, moved: stall.moved}
		} else if _, busy := errors.AsType[*busyHostError](err); busy {
			lease.Failed(err, time.Since(start), false)
		} else if _, refused := errors.AsType[*extractor.RefusedError](err); refused {
			// The host turned this address away, not the file: hold the
			// route back from it, its exit with it, and let the file try
			// another. The API's own answer came back as an ordinary
			// response, so nothing below HTTP has counted it.
			lease.Failed(err, time.Since(start), false)
			lease.Cooldown(time.Now().Add(config.ProxyAddressRefusedWait))
			err = &httpx.RouteError{Err: err}
		} else if transientRouteFailure(err) {
			// A deadline, a timeout or a dropped connection says the
			// attempt went badly, not that the file has gone, and through
			// public proxies it is the commonest way for one to end. So the
			// file tries again on the best route free. Nothing is charged
			// to this one: what the network did the route layer has already
			// scored, and a CAPTCHA reader out of time is not its doing.
			err = &httpx.RouteError{Err: err}
		}
		if wait, ok := errors.AsType[*extractor.WaitError](err); ok {
			lease.Cooldown(wait.Until)
			// Only this address is waiting. The next dispatch may use a
			// different one immediately, or wait in the queue for the pool.
			err = &extractor.WaitError{Until: time.Now(), Reason: noteWaitingForRoute}
		} else if _, routed := errors.AsType[*httpx.RouteError](err); routed {
			m.mu.Lock()
			it.preferredRoute = ""
			progress, _ := errors.AsType[*routeTransferError](err)
			moved := progress != nil && progress.moved
			if moved {
				it.proxyRetries = 0
			}
			// Some failures are the proxy's and nothing to do with the
			// file, and public lists are full of proxies that have them; see
			// routeAtFault. They cost the file nothing: the pool has already
			// put the route away, and charging them spent a file's whole
			// budget before it met a proxy that worked.
			free := routeAtFault(err)
			if moved || free || it.proxyRetries < m.cfg.ProxyRetries {
				if !moved && !free {
					it.proxyRetries++
				}
				err = &extractor.WaitError{Until: time.Now(), Reason: "Retrying through another download route"}
			}
			m.mu.Unlock()
		}
		// Before the lease goes: releasing it gives up the exit this names.
		m.logRouteAttempt(it, lease, cause, err, time.Since(start))
	}
	lease.Release()
	// runItem clears the route together with its dispatch accounting.
	return err
}

// moveOntoRoutes moves a file that starts on the normal connection
// (extractor.Pace.DirectFirst) onto its group's routes once the host has made
// that connection wait, and reports whether it did. Only another address can
// go on before the wait is over. Without proxies the file stays where it is
// and waits in the queue instead.
func (m *Manager) moveOntoRoutes(it *Item, err error) bool {
	wait, ok := errors.AsType[*extractor.WaitError](err)
	if !ok {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if it.routePace == nil || !m.proxyEnabled || m.proxies == nil {
		return false
	}
	it.takeRoutePace()
	m.log.Info("the normal connection must wait; going on through proxies", "item", it.ID, "name", it.Name,
		"service", it.pace.Group, "until", wait.Until.Format(time.DateTime), "reason", wait.Reason)
	return true
}

// takeRoutePace moves an item onto the proxy pace it was holding back
// (Item.routePace), forgetting any route it preferred before. Caller holds
// mu.
func (it *Item) takeRoutePace() {
	it.pace, it.routePace, it.preferredRoute = it.routePace, nil, ""
}

// retryRouteFailure is shared by source recovery and opted-in direct files.
// Local disk/parser errors and missing files never trigger a proxy search.
func retryRouteFailure(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*httpx.RouteError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*httpx.ChallengeError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*stalledError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*busyHostError](err); ok {
		return true
	}
	if transientRouteFailure(err) || routeAtFault(err) {
		return true
	}
	if status, ok := errors.AsType[*httpx.StatusError](err); ok {
		return status.Code == http.StatusForbidden || status.Code == http.StatusRequestTimeout ||
			status.Code == http.StatusTooManyRequests || status.Code >= 500
	}
	return false
}

// sampleProxyLocked feeds useful-byte windows into the route score. A
// replacement is reserved before aborting the current attempt, and only a
// confirmed range response permits moving a healthy transfer mid-file.
//
// bytes are the route's own: what extra streams read through routes of
// their own is credited to those (multiroute.go). Nor does a transfer with
// such streams running move: aborting the attempt would end them too, and
// throw away the free downloads they waited for.
func (m *Manager) sampleProxyLocked(it *Item, now time.Time, bytes int64, elapsed time.Duration, limited bool) {
	if it.route == nil {
		return
	}
	if !it.route.Progress(now, bytes, elapsed, limited) || limited || !m.proxyEnabled ||
		!it.routeResumable || it.routeCancel == nil || it.routeUpgrade != nil || m.lowOnSpace() ||
		it.routeExtras.Load() > 0 {
		return
	}
	if next := it.route.Upgrade(now, it.Size-it.downloaded.Load(), it.speed); next != nil {
		it.routeUpgrade = next
		it.Note = "Switching to a faster download route"
		it.routeCancel()
	}
}

func (m *Manager) constrainProxyMeasurementsLocked() {
	if !m.throttle.isPaused() && m.throttle.currentLimit() == 0 {
		return
	}
	for _, job := range m.jobs {
		for _, it := range job.Items {
			if it.route != nil {
				it.route.Constrain()
			}
		}
	}
}

// A resumed prefix already on disk is not progress from this attempt.
type routeTransferError struct {
	err   error
	moved bool
}

func (e *routeTransferError) Error() string { return e.err.Error() }
func (e *routeTransferError) Unwrap() error { return e.err }

// transientRouteFailure reports a failure that another attempt may well
// not meet: a resolver's own word for it, an expired deadline (a CAPTCHA
// reader's included), a timeout, or a connection refused, reset or cut
// short. The caller has already excluded the item's own cancellation.
func transientRouteFailure(err error) bool {
	if _, ok := errors.AsType[*extractor.TransientError](err); ok {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return true
	}
	for _, errno := range []syscall.Errno{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED,
		syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.EPIPE} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// routeAtFault reports a route failure that is the proxy's and not the
// file's, which does not spend the file's retry budget: a proxy that could
// not be reached, a connection it dropped (a bare or unexpected EOF, a
// reset), or an address the host refuses outright. With these exempt, a host
// that refused every proxy would have files trying routes for as long as the
// pool has any to offer, and waiting in the queue for one after that,
// rather than failing; the refusal backoff keeps each route from being
// asked again for a while. Timeouts after proxy setup, HTTP refusals and
// stalls still count.
func routeAtFault(err error) bool {
	if deadProxy(err) || proxyUnusable(err) || errors.Is(err, errExitInUse) {
		return true
	}
	if errors.Is(err, proxy.ErrExitProbeFailed) {
		return true
	}
	// A proxy that answered and could not carry this request on: it
	// refused the tunnel (a 502 or 504 is a proxy that cannot reach the host
	// from where it is), or the host's TLS never completed through it. Or
	// the host challenged the address it saw, which is that address's
	// standing with the host, recorded apart (entry.WAF). Each is the route
	// alone: through public proxies a file met twenty in a row, and failed,
	// on a host other routes were serving. Its route is scored for the
	// service, and the file moves on at no cost. A host that is down for
	// every route leaves its files waiting for one, rather than failing.
	if _, refused := errors.AsType[*httpx.ProxyRefusedError](err); refused {
		return true
	}
	if _, challenged := errors.AsType[*httpx.ChallengeError](err); challenged {
		return true
	}
	// A proxy that answered the tunnel in another protocol altogether, as a
	// SOCKS proxy listed as an HTTP one does, comes back from net/http as a
	// malformed response: it carries nothing, whatever is asked of it.
	if _, routed := errors.AsType[*httpx.RouteError](err); routed &&
		(strings.Contains(err.Error(), "TLS handshake timeout") || strings.Contains(err.Error(), "malformed HTTP response")) {
		return true
	}
	if _, refused := errors.AsType[*extractor.RefusedError](err); refused {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)
}

// routeOutcome is one service's routed attempts that ended one way.
type routeOutcome struct{ service, kind string }

// routeOutcomeKinds are the ways a routed attempt ends, in the order the
// summary lists them.
var routeOutcomeKinds = []string{"dead", "shared", "challenged", "refused", "waited", "transient", "stalled", "failed", "finished"}

// routeOutcomeKind names how a routed attempt ended: dead, a proxy that
// could not carry the request at all; shared, a route whose exit another
// download held or the host had cooling; refused, an address the host
// turned away; waited, a host's timer for the address; then transient
// failures, stalls, other failures, and attempts that finished their file.
func routeOutcomeKind(err error) string {
	if err == nil || errors.Is(err, errAlreadyComplete) {
		return "finished"
	}
	if errors.Is(err, errExitInUse) {
		return "shared"
	}
	if _, challenged := errors.AsType[*httpx.ChallengeError](err); challenged {
		return "challenged"
	}
	if _, ok := errors.AsType[*extractor.WaitError](err); ok {
		return "waited"
	}
	if _, ok := errors.AsType[*extractor.RefusedError](err); ok {
		return "refused"
	}
	if _, ok := errors.AsType[*stalledError](err); ok {
		return "stalled"
	}
	if routeBroken(err) {
		return "dead"
	}
	if transientRouteFailure(err) {
		return "transient"
	}
	return "failed"
}

// logRouteAttempt says how a routed attempt ended and what the file does
// next: the route and the exit it left by, what the host or the network
// answered, and whether the file goes on through another route, waits or
// fails. A dead route, or one whose exit is taken, is the commonest end
// through a public feed and says nothing about the host, so those are
// logged at debug level and every answer of the host's at info. A
// requeue used to be logged as the host asking for a wait until that very
// moment, which named neither the route nor the reason.
func (m *Manager) logRouteAttempt(it *Item, lease *proxy.Lease, cause, next error, took time.Duration) {
	m.mu.Lock()
	name, attempt, retries, service := it.Name, it.routeAttempts, it.proxyRetries, ""
	if it.pace != nil {
		service = it.pace.Group
	}
	m.mu.Unlock()
	outcome := routeOutcomeKind(cause)
	then := "fail"
	if next == nil {
		then = "done"
	} else if wait, ok := errors.AsType[*extractor.WaitError](next); ok {
		then = "another route"
		if time.Until(wait.Until) > time.Second {
			then = "wait until " + wait.Until.Format(time.DateTime)
		}
	}
	level := slog.LevelInfo
	if outcome == "dead" || outcome == "shared" {
		level = slog.LevelDebug
	}
	args := []any{"item", it.ID, "name", name, "service", service, "route", lease.Address(), "exit", lease.Exit(),
		"outcome", outcome, "next", then, "attempt", attempt,
		"retries", fmt.Sprintf("%d/%d", retries, m.cfg.ProxyRetries), "took", took.Round(100 * time.Millisecond)}
	if cause != nil {
		args = append(args, "cause", cause.Error())
	}
	m.log.Log(context.Background(), level, "route attempt ended", args...)
}

// noteRouteOutcome counts how a routed attempt for it ended.
func (m *Manager) noteRouteOutcome(it *Item, err error) {
	kind := routeOutcomeKind(err)
	m.mu.Lock()
	defer m.mu.Unlock()
	if it.pace == nil {
		return
	}
	if m.routeOutcomes == nil {
		m.routeOutcomes = make(map[routeOutcome]int)
	}
	m.routeOutcomes[routeOutcome{it.pace.Group, kind}]++
}

// routeSummaryLocked returns, once a RouteSummaryInterval, a log line's
// fields per service: how its routed attempts ended since the last one, and
// how many of its files are moving bytes now and how many are still looking
// for a route that will. One line a minute says what a queue is spending its
// routes on, where a line per attempt said it twenty times a second and
// pushed everything else out of the log. Caller holds mu.
func (m *Manager) routeSummaryLocked(now time.Time) [][]any {
	if m.routeSummaryAt.IsZero() {
		m.routeSummaryAt = now
		return nil
	}
	if now.Sub(m.routeSummaryAt) < config.RouteSummaryInterval {
		return nil
	}
	m.routeSummaryAt = now
	moving, trying := map[string]int{}, map[string]int{}
	for _, job := range m.jobs {
		for _, it := range job.Items {
			if it.Status != StatusRunning || it.route == nil || it.pace == nil {
				continue
			}
			if it.speed > 0 {
				moving[it.pace.Group]++
			} else {
				trying[it.pace.Group]++
			}
		}
	}
	services := map[string]bool{}
	for o := range m.routeOutcomes {
		services[o.service] = true
	}
	for s := range moving {
		services[s] = true
	}
	for s := range trying {
		services[s] = true
	}
	var lines [][]any
	for _, s := range slices.Sorted(maps.Keys(services)) {
		args := []any{"service", s}
		for _, kind := range routeOutcomeKinds {
			args = append(args, kind, m.routeOutcomes[routeOutcome{s, kind}])
		}
		lines = append(lines, append(args, "moving", moving[s], "trying", trying[s]))
	}
	clear(m.routeOutcomes)
	return lines
}

// errExitInUse lets admission and retry accounting recognize shared exit claims.
var errExitInUse = proxy.ErrExitInUse

// claimExit delegates probing and reservation to the shared lease client. The
// manager retains its UI note, retry classification and cached outage policy.
func (m *Manager) claimExit(ctx context.Context, it *Item, route *proxy.Lease) error {
	if m.cfg == nil || len(m.cfg.ProxyExitProbes) == 0 || route.Claimed() {
		return nil
	}
	if it != nil {
		m.mu.Lock()
		it.Note = "Checking the download route"
		m.mu.Unlock()
	}
	prober := proxy.ExitProber{
		URLs:    m.cfg.ProxyExitProbes,
		Timeout: config.ProxyExitProbeTimeout,
		IsResponseError: func(err error) bool {
			// Adapter HTTP refusals try the next probe, including statuses
			// wrapped in RouteError. Other route failures stop immediately.
			if _, answered := errors.AsType[*httpx.StatusError](err); answered {
				return true
			}
			_, routed := errors.AsType[*httpx.RouteError](err)
			return !routed && !routeAtFault(err)
		},
		Reachable: m.echoReachable,
	}
	identity, err := prober.Claim(ctx, route)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, proxy.ErrExitInUse) || errors.Is(err, proxy.ErrExitProbeFailed) || errors.Is(err, context.DeadlineExceeded) {
			return &httpx.RouteError{Err: err}
		}
		return err
	}
	if !identity.IP.IsValid() {
		m.log.DebugContext(ctx, "echo services unreachable; going ahead without the route's exit", "route", route.ID())
	}
	return nil
}

// echoReachable caches the availability of the explicitly chosen normal route.
// A caller cancellation does not cache an outage or penalize a leased route.
func (m *Manager) echoReachable(ctx context.Context) (bool, error) {
	m.echoMu.Lock()
	defer m.echoMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !m.echoCheckedAt.IsZero() && time.Since(m.echoCheckedAt) < config.ProxyEchoRecheck {
		return m.echoUp, nil
	}
	client := m.client.WithTimeout(config.ProxyExitProbeTimeout)
	up := false
	for _, echo := range m.cfg.ProxyExitProbes {
		detector := proxytron.Detector{
			URL:     echo,
			Timeout: config.ProxyExitProbeTimeout,
			Client:  func(string) (proxytron.HTTPDoer, error) { return echoClient{client}, nil },
		}
		if _, err := detector.Detect(httpx.WithoutRoute(ctx), proxytron.Direct); err == nil {
			up = true
			break
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	m.echoUp, m.echoCheckedAt = up, time.Now()
	return up, nil
}

// echoClient keeps the normal connection's request headers and transport while
// Detector owns the bounded IP response parsing.
type echoClient struct{ *httpx.Client }

func (c echoClient) Do(req *http.Request) (*http.Response, error) {
	req, err := c.NewRequest(req.Context(), req.Method, req.URL.String(), nil)
	if err != nil {
		return nil, err
	}
	return c.DoOnce(req)
}

// routeBroken is routeAtFault short of a refusal: a proxy that could not
// carry the request at all, which the host either never saw or saw cut off.
// Most untested ports on a public gateway are such proxies.
func routeBroken(err error) bool {
	if _, refused := errors.AsType[*extractor.RefusedError](err); refused {
		return false
	}
	return routeAtFault(err)
}

// proxyUnusable reports a proxy that answered and showed it cannot carry these
// requests at all, whatever their destination:
//   - a SOCKS proxy that wants credentials, or methods, it was not given;
//   - an HTTP proxy declining to open a tunnel, below 500. At 502 or 504 it
//     tried and could not reach the host, which is the host's business as
//     much as its own, as deadProxy explains;
//   - one speaking plain HTTP where TLS was expected, from its own hop (an
//     https proxy that is not one) or from inside the tunnel;
//   - a certificate for the destination that does not verify. The proxy's
//     own TLS hop accepts any certificate, so this one came through the
//     tunnel: through a proxy, that is the proxy reading the traffic. A
//     certificate expired in 2021 was presented for a host whose own is
//     current.
//
// Public lists are full of all four. Charged to the file, twenty of them in
// a row failed it at its first step, before a link existed.
func proxyUnusable(err error) bool {
	if refused, ok := errors.AsType[*httpx.ProxyRefusedError](err); ok {
		return refused.Code < http.StatusInternalServerError
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok || errors.Is(err, http.ErrSchemeMismatch) {
		return true
	}
	if _, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return true
	}
	return socksAuthRefused(err)
}

// socksAuthRefused reports a SOCKS proxy that would not let these requests
// past its authentication. net/http's SOCKS client returns that as a bare
// string inside the "socks connect" error; the wording is the standard
// library's own, fixed with the toolchain, and none of its replies about
// reaching the host ("host unreachable", "general SOCKS server failure")
// mentions authentication.
func socksAuthRefused(err error) bool {
	for err != nil {
		op, ok := errors.AsType[*net.OpError](err)
		if !ok {
			return false
		}
		if op.Op == "socks connect" {
			return op.Err != nil && strings.Contains(op.Err.Error(), "authentication")
		}
		err = op.Err
	}
	return false
}

// deadProxy reports a failure to reach the proxy itself, before anything
// was asked of the host behind it: an HTTP proxy's "proxyconnect" or a
// SOCKS proxy's "socks connect" around a dial that failed, or an unanswered
// proxy handshake. A proxy that answered and then could not reach the host
// is not one; that answer is about the host as much as the proxy, and an
// outage there would otherwise walk one file through the entire pool.
func deadProxy(err error) bool {
	if _, ok := errors.AsType[*httpx.ProxyConnectError](err); ok {
		return true
	}
	for err != nil {
		op, ok := errors.AsType[*net.OpError](err)
		if !ok {
			return false
		}
		if op.Op == "proxyconnect" || op.Op == "socks connect" {
			dial, ok := errors.AsType[*net.OpError](op.Err)
			return ok && dial.Op == "dial"
		}
		err = op.Err
	}
	return false
}
