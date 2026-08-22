package httpx

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// Browser-shaped TLS.
//
// A TLS ClientHello is itself a fingerprint: the cipher suites, extensions
// and curves it offers, and the order they appear in, differ between Chrome,
// Firefox and Go's own crypto/tls. Bot-protection services hash that
// ordering (JA3/JA4) and compare it against known browsers, which no amount
// of header setting can influence — the decision is made a layer below.
//
// So the handshake is made to look like Chrome's. In testing this changed
// nothing on the hosts here, which answered identically either way, but the
// cost is one dependency and the protection is against a class of block
// that headers cannot address at all.
//
// Any failure at the transport level falls back to the standard library, so
// a host that dislikes the impersonated handshake still works.

// browserTransport speaks HTTP/2 over a Chrome-shaped handshake, falling
// back to the standard transport when that does not work out.
type browserTransport struct {
	impersonated *http2.Transport
	standard     *http.Transport

	mu       sync.Mutex
	fellBack map[string]bool // hosts the impersonated path failed for
}

// newBrowserTransport builds the impersonating transport around a standard
// one, which stays in use as the fallback.
func newBrowserTransport(standard *http.Transport) http.RoundTripper {
	return &browserTransport{
		standard: standard,
		fellBack: make(map[string]bool),
		impersonated: &http2.Transport{
			// Only h2 is offered: every host this talks to supports it, and
			// negotiating per-connection would mean dialing twice to learn
			// which transport to hand the request to.
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return dialChrome(ctx, network, addr)
			},
			ReadIdleTimeout: 30 * time.Second,
			PingTimeout:     15 * time.Second,
		},
	}
}

// RoundTrip prefers the browser-shaped path, remembering hosts where it did
// not work so they are not retried through it on every request.
func (t *browserTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host

	t.mu.Lock()
	skip := t.fellBack[host]
	t.mu.Unlock()

	if skip || req.URL.Scheme != "https" {
		return t.standard.RoundTrip(req)
	}

	resp, err := t.impersonated.RoundTrip(req)
	if err == nil {
		// A handshake can be accepted and the request still turned away.
		// Furbooru, for one, answers the impersonated connection with a
		// challenge page and a 501 while serving the standard client
		// normally, so a challenge counts as a failure of this path too.
		challenged, restored := looksChallenged(resp)
		if !challenged {
			return restored, nil
		}
		_ = restored.Body.Close()
	} else if req.Context().Err() != nil {
		// A cancelled request is the caller's decision, not a transport
		// problem, and must not condemn the host.
		return nil, err
	}

	t.mu.Lock()
	t.fellBack[host] = true
	t.mu.Unlock()

	if req.GetBody != nil {
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return nil, bodyErr
		}
		req.Body = body
	}
	return t.standard.RoundTrip(req)
}

// challengeStatuses are the replies a bot check hides behind.
var challengeStatuses = map[int]bool{
	http.StatusForbidden: true, http.StatusTooManyRequests: true,
	http.StatusUnavailableForLegalReasons: true,
	http.StatusNotImplemented:             true, http.StatusServiceUnavailable: true,
}

// challengeMarkers appear in the bodies such checks serve.
var challengeMarkers = []string{
	"just a moment", "attention required", "challenge-platform",
	"cf-browser-verification", "challenge.css", "_cf_chl", "captcha",
	"ddos-guard",
}

// looksChallenged reports whether a response is a bot check rather than the
// thing that was asked for. The body is returned intact either way, since a
// response that turns out to be genuine still has to be readable.
func looksChallenged(resp *http.Response) (bool, *http.Response) {
	if !challengeStatuses[resp.StatusCode] {
		return false, resp
	}
	const peek = 64 << 10
	head, err := io.ReadAll(io.LimitReader(resp.Body, peek))
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(head))
		return false, resp
	}
	rest := resp.Body
	resp.Body = &joinedBody{Reader: io.MultiReader(bytes.NewReader(head), rest), closer: rest}

	lowered := strings.ToLower(string(head))
	for _, marker := range challengeMarkers {
		if strings.Contains(lowered, marker) {
			return true, resp
		}
	}
	return false, resp
}

// joinedBody re-attaches a peeked prefix to the rest of a response body.
type joinedBody struct {
	io.Reader
	closer io.Closer
}

func (b *joinedBody) Close() error { return b.closer.Close() }

// dialChrome performs a handshake shaped like Chrome's.
func dialChrome(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	raw, err := (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).
		DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	conn := utls.UClient(raw, &utls.Config{
		ServerName: host,
		NextProtos: []string{"h2"},
	}, utls.HelloChrome_Auto)

	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}
