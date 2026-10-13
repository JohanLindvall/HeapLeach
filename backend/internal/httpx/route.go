// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// ThroughProxy creates an isolated transport and cookie jar. Explicit routes
// ignore NO_PROXY and never fall back to a direct connection. "direct" keeps
// the normal environment-proxy policy. Destination TLS is always verified.
func (c *Client) ThroughProxy(raw string) (*Client, error) {
	cp := New(c.ua, c.acceptLang, c.maxRetries, c.hc.Timeout)
	var tr *http.Transport
	switch t := cp.hc.Transport.(type) {
	case *http.Transport:
		tr = t
	case *browserTransport:
		tr = t.standard
	}
	if raw != "direct" {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.Port() == "" {
			return nil, errors.New("invalid proxy endpoint")
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
		tr.Proxy = http.ProxyURL(u)
		tr.DialContext = (&net.Dialer{Timeout: config.ProxyDialTimeout, KeepAlive: 30 * time.Second}).DialContext
		cp.proxyScheme = u.Scheme
		tr.OnProxyConnectResponse = func(ctx context.Context, _ *url.URL, _ *http.Request, resp *http.Response) error {
			if pending, ok := ctx.Value(proxyConnectKey{}).(*atomic.Bool); ok {
				pending.Store(false)
			}
			if resp.StatusCode != http.StatusOK {
				// net/http would return only the status text, "Bad Request",
				// which says neither that it came from the proxy nor which
				// status it was.
				return &ProxyRefusedError{Code: resp.StatusCode, Status: resp.Status}
			}
			return nil
		}
		if u.Scheme == "https" {
			// DialTLSContext handles only the first hop to the fixed proxy.
			// TLS inside CONNECT still uses TLSClientConfig and verifies the
			// destination. Public HTTPS proxies often use self-signed certs.
			tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if addr != u.Host {
					return nil, errors.New("unexpected HTTPS proxy address")
				}
				conn, err := tr.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				secured := tls.Client(conn, &tls.Config{
					InsecureSkipVerify: true, //nolint:gosec // only the proxy's outer TLS hop
					ServerName:         u.Hostname(), NextProtos: []string{"http/1.1"},
				})
				handshake, cancel := context.WithTimeout(ctx, tr.TLSHandshakeTimeout)
				defer cancel()
				if err := secured.HandshakeContext(handshake); err != nil {
					conn.Close()
					return nil, err
				}
				return secured, nil
			}
		}
	}
	return cp, nil
}

// CloseIdleConnections releases a route's cached connections on retirement.
func (c *Client) CloseIdleConnections() { c.hc.CloseIdleConnections() }

type routeContextKey struct{}
type proxyConnectKey struct{}

// DirectRoute keeps an IP-bound ticket's identity across live proxy toggles.
const DirectRoute = "direct"

type requestRoute struct {
	id      string
	client  *Client
	observe func(RouteObservation)
	// agent, when set, is the User-Agent every request on the route sends
	// in place of the client's own (WithRouteAgent).
	agent string
}

// RouteObservation measures complete response reads, including failed reads.
// Cancellation and intentionally closed partial bodies are not failures.
type RouteObservation struct {
	Duration   time.Duration
	Bytes      int64
	Status     int
	Err        error
	RetryAfter time.Time
}

// WithRoute pins every request, redirect and retry in ctx to the same client.
// The ID is opaque; it lets IP-bound resolvers keep tickets with their route.
func WithRoute(ctx context.Context, id string, client *Client, observe func(RouteObservation)) context.Context {
	return WithRouteAgent(ctx, id, client, observe, "")
}

// WithRouteAgent is WithRoute for a route that presents a User-Agent of its
// own on every request, whichever client builds it: one browser behind one
// address, rather than the same browser behind all of them.
func WithRouteAgent(ctx context.Context, id string, client *Client, observe func(RouteObservation), agent string) context.Context {
	return context.WithValue(ctx, routeContextKey{}, &requestRoute{id: id, client: client, observe: observe, agent: agent})
}

func RouteID(ctx context.Context) string {
	if route, ok := ctx.Value(routeContextKey{}).(*requestRoute); ok {
		return route.id
	}
	return ""
}

// RouteError asks the owner of a lease to retry on a different route. It is
// separate from file, OCR and disk errors, which say nothing about a proxy.
type RouteError struct{ Err error }

func (e *RouteError) Error() string { return e.Err.Error() }
func (e *RouteError) Unwrap() error { return e.Err }

// ProxyConnectError preserves a timeout before an explicit proxy supplied
// a connection. net/http's client timeout otherwise hides the dial/CONNECT
// error, making an unanswered proxy look like a slow origin response.
type ProxyConnectError struct{ Err error }

func (e *ProxyConnectError) Error() string { return e.Err.Error() }
func (e *ProxyConnectError) Unwrap() error { return e.Err }

// ProxyRefusedError is an HTTP proxy declining to open a tunnel: its answer
// to CONNECT was not 200.
type ProxyRefusedError struct {
	Code   int
	Status string
}

func (e *ProxyRefusedError) Error() string { return "proxy refused the tunnel: " + e.Status }

func (r *requestRoute) do(original *Client, req *http.Request) (*http.Response, error) {
	if r.agent != "" {
		req.Header.Set(HeaderUserAgent, r.agent)
	}
	// Preserve the caller's streaming/header timeout while borrowing only
	// the selected route's transport and cookies.
	cp, hc := *original, *r.client.hc
	hc.Timeout = original.hc.Timeout
	cp.hc = &hc
	var pending atomic.Bool
	if r.client.proxyScheme != "" {
		trace := &httptrace.ClientTrace{
			// Reset on every redirect or transport reconnect: a completed
			// earlier hop says nothing about the next connection.
			GetConn: func(string) { pending.Store(true) },
			GotConn: func(httptrace.GotConnInfo) { pending.Store(false) },
		}
		if r.client.proxyScheme == "socks5" || r.client.proxyScheme == "socks5h" {
			// SOCKS negotiation finishes before destination TLS starts.
			// HTTP(S) proxies signal this via OnProxyConnectResponse; an
			// HTTPS proxy's own TLS handshake is still proxy setup.
			trace.TLSHandshakeStart = func() { pending.Store(false) }
		}
		ctx := context.WithValue(req.Context(), proxyConnectKey{}, &pending)
		req = req.WithContext(httptrace.WithClientTrace(ctx, trace))
	}
	start := time.Now()
	resp, err := cp.doDirect(req)
	report := func(status int, n int64, err error, until time.Time) {
		if req.Context().Err() == nil && r.observe != nil {
			r.observe(RouteObservation{Duration: time.Since(start), Bytes: n, Status: status, Err: err, RetryAfter: until})
		}
	}
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		if ne, timeout := errors.AsType[net.Error](err); pending.Load() &&
			((timeout && ne.Timeout()) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errHeaderTimeout)) {
			err = &ProxyConnectError{Err: err}
		}
		status := 0
		if _, challenged := errors.AsType[*ChallengeError](err); challenged {
			// Even a challenge served with 200 is an address refusal.
			// Record it separately from ordinary proxy health.
			status = http.StatusForbidden
		}
		report(status, 0, err, time.Time{})
		return nil, &RouteError{err}
	}
	code := resp.StatusCode
	if code == http.StatusForbidden || code == http.StatusProxyAuthRequired || code == http.StatusTooManyRequests || code >= 500 {
		var until time.Time
		if wait, stated := retryAfter(resp); stated {
			until = time.Now().Add(wait)
		}
		err := &StatusError{Code: code, Status: resp.Status, URL: req.URL.Redacted()}
		report(code, 0, err, until)
		resp.Body.Close()
		return nil, &RouteError{err}
	}
	resp.Body = &routeBody{ReadCloser: resp.Body, expected: resp.ContentLength, report: func(n int64, err error) {
		if err != nil || (code >= 200 && code < 300) {
			report(code, n, err, time.Time{})
		}
	}}
	return resp, nil
}

type routeBody struct {
	io.ReadCloser
	once            sync.Once
	bytes, expected int64
	report          func(int64, error)
}

func (b *routeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes += int64(n)
	if err != nil {
		b.once.Do(func() {
			if errors.Is(err, io.EOF) {
				b.report(b.bytes, nil)
			} else {
				b.report(b.bytes, err)
			}
		})
		if !errors.Is(err, io.EOF) {
			err = &RouteError{err}
		}
	}
	return n, err
}

func (b *routeBody) Close() error {
	if b.expected >= 0 && b.bytes >= b.expected {
		b.once.Do(func() { b.report(b.bytes, nil) })
	}
	return b.ReadCloser.Close()
}
