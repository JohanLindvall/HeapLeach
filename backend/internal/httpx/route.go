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
	"net/url"
	"sync"
	"time"
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

// DirectRoute keeps an IP-bound ticket's identity across live proxy toggles.
const DirectRoute = "direct"

type requestRoute struct {
	id      string
	client  *Client
	observe func(RouteObservation)
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
	return context.WithValue(ctx, routeContextKey{}, &requestRoute{id, client, observe})
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

func (r *requestRoute) do(original *Client, req *http.Request) (*http.Response, error) {
	// Preserve the caller's streaming/header timeout while borrowing only
	// the selected route's transport and cookies.
	cp, hc := *original, *r.client.hc
	hc.Timeout = original.hc.Timeout
	cp.hc = &hc
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
