// SPDX-License-Identifier: MIT

package download

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/proxy"
)

// fakeProxy serves each connection to address with serve, standing in for a
// public proxy that misbehaves in one particular way.
func fakeProxy(t *testing.T, address string, serve func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", address+":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				serve(c)
			}()
		}
	}()
	return l.Addr().String()
}

// answerConnect answers an HTTP CONNECT with reply, then hands what follows
// to rest, if any.
func answerConnect(reply string, rest func(net.Conn, *bufio.Reader)) func(net.Conn) {
	return func(c net.Conn) {
		br := bufio.NewReader(c)
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		_, _ = io.WriteString(c, reply)
		if rest != nil {
			rest(c, br)
		}
	}
}

// socksAnswering negotiates SOCKS5 the way a server does: method is its
// choice of authentication (0xff for none acceptable), and reply its answer
// to the CONNECT that follows.
func socksAnswering(method, reply byte) func(net.Conn) {
	return func(c net.Conn) {
		greeting := make([]byte, 2)
		if _, err := io.ReadFull(c, greeting); err != nil {
			return
		}
		if _, err := io.ReadFull(c, make([]byte, greeting[1])); err != nil {
			return
		}
		_, _ = c.Write([]byte{5, method})
		if method == 0xff {
			return
		}
		request := make([]byte, 5) // version, command, reserved, address type, name length
		if _, err := io.ReadFull(c, request); err != nil {
			return
		}
		if _, err := io.ReadFull(c, make([]byte, int(request[4])+2)); err != nil { // name and port
			return
		}
		_, _ = c.Write([]byte{5, reply, 0, 1, 0, 0, 0, 0, 0, 0})
	}
}

// routeFailure is the error a request to an HTTPS destination meets through
// proxyURL, from the client's real transport.
func routeFailure(t *testing.T, proxyURL string) error {
	t.Helper()
	c, err := httpx.New("test", "en", 0, 3*time.Second).ThroughProxy(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := httpx.WithRoute(context.Background(), "test", c, func(httpx.RouteObservation) {})
	req, err := c.NewRequest(ctx, http.MethodGet, "https://destination.example.test/api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c.DoOnce(req); err == nil {
		resp.Body.Close()
		t.Fatal("the request went through")
	} else {
		return err
	}
	return nil
}

// A proxy that answered and showed it cannot carry these requests is the
// route's fault, and costs the file nothing. One that could not reach the
// host still counts: that is the host's business as much as its own.
func TestAProxyThatCannotCarryTheRequestIsTheRoutesFault(t *testing.T) {
	destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(destination.Close)
	// The destination's certificate is for example.com and from no trusted
	// root, as an intercepting proxy's would be.
	tunnel := func(c net.Conn, br *bufio.Reader) {
		up, err := net.Dial("tcp", destination.Listener.Addr().String())
		if err != nil {
			return
		}
		defer up.Close()
		go func() { _, _ = io.Copy(up, br) }()
		_, _ = io.Copy(c, up)
	}
	// Says it opened the tunnel, then answers the TLS handshake in HTTP.
	httpInTunnel := func(c net.Conn, br *bufio.Reader) {
		if _, err := br.Peek(1); err != nil { // the client's hello
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
	}
	for _, tc := range []struct {
		name, proxy string
		atFault     bool
	}{
		{"socks proxy wanting credentials", "socks5h://" + fakeProxy(t, "127.0.0.1", socksAnswering(0xff, 0)), true},
		{"tunnel refused", "http://" + fakeProxy(t, "127.0.0.1", answerConnect("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n", nil)), true},
		{"tunnel needing proxy credentials", "http://" + fakeProxy(t, "127.0.0.1", answerConnect("HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n", nil)), true},
		{"https proxy speaking plain http", "https://" + fakeProxy(t, "127.0.0.1", func(c net.Conn) {
			_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
		}), true},
		{"plain http inside the tunnel", "http://" + fakeProxy(t, "127.0.0.1", answerConnect("HTTP/1.1 200 OK\r\n\r\n", httpInTunnel)), true},
		{"destination certificate that does not verify", "http://" + fakeProxy(t, "127.0.0.1", answerConnect("HTTP/1.1 200 OK\r\n\r\n", tunnel)), true},
		{"tunnel the proxy could not open to the host", "http://" + fakeProxy(t, "127.0.0.1", answerConnect("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n", nil)), true},
		{"tunnel timed out at the proxy", "http://" + fakeProxy(t, "127.0.0.1", answerConnect("HTTP/1.1 504 Gateway Time-out\r\nContent-Length: 0\r\n\r\n", nil)), true},
		{"socks proxy that could not reach the host", "socks5h://" + fakeProxy(t, "127.0.0.1", socksAnswering(0, 4)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := routeFailure(t, tc.proxy)
			if got := routeAtFault(err); got != tc.atFault {
				t.Fatalf("routeAtFault = %v, want %v, for %v", got, tc.atFault, err)
			}
		})
	}
}

// Through the queue: a route that cannot carry the file's first request is
// held back and the file goes on to another, at no cost to its retries.
func TestAFileMovesOnFromAProxyThatCannotCarryIt(t *testing.T) {
	m := busyManager(t)
	route := "socks5h://" + fakeProxy(t, "127.0.0.2", socksAnswering(0xff, 0))
	pool := attachPool(t, m, []string{route})
	job := addProxyFiles(m, 1, func(ctx context.Context) (*extractor.Target, error) {
		req, err := m.client.NewRequest(ctx, http.MethodPost, "https://api.example.test/v2/start", nil)
		if err != nil {
			return nil, err
		}
		resp, err := m.client.DoOnce(req)
		if err != nil {
			return nil, fmt.Errorf("test service: start: %w", err)
		}
		resp.Body.Close()
		return nil, errors.New("test service: unexpected answer")
	})
	it := job.Items[0]
	row := pool.Page(config.GroupKeep2Share, proxy.Query{Search: route}).Rows[0]
	if it.route = pool.Acquire(config.GroupKeep2Share, row.ID, 1); it.route == nil {
		t.Fatal("no route")
	}
	retries := it.proxyRetries
	err := m.transferRouted(t.Context(), it)
	if wait, ok := errors.AsType[*extractor.WaitError](err); !ok || !strings.Contains(wait.Reason, "another download route") {
		t.Fatalf("error = %v; want the file sent to another route", err)
	}
	if it.proxyRetries != retries {
		t.Fatalf("retries %d -> %d; a proxy that cannot carry the file must cost it nothing", retries, it.proxyRetries)
	}
	if row = pool.Page(config.GroupKeep2Share, proxy.Query{Search: route}).Rows[0]; row.Status != "cooling" {
		t.Fatalf("route status %q, want it held back", row.Status)
	}
}

// A route of a per-route service presents one browser of its own, the same
// on every request it carries; WAF recovery keeps the client's own, which
// some hosts sign requests with.
func TestARouteKeepsOneBrowserUserAgent(t *testing.T) {
	m := busyManager(t)
	agents := make(chan string, 8)
	route := proxyServer(t, "127.0.0.2", func(w http.ResponseWriter, r *http.Request) {
		agents <- r.Header.Get(httpx.HeaderUserAgent)
		_, _ = io.WriteString(w, "ok")
	}).URL
	pool := attachPool(t, m, []string{route})
	id := pool.Page(config.GroupKeep2Share, proxy.Query{Search: route}).Rows[0].ID
	ask := func(lease *proxy.Lease) string {
		t.Helper()
		if _, err := m.client.WithTimeout(5*time.Second).GetString(lease.Context(t.Context()), "http://service.example.test/api", nil); err != nil {
			t.Fatal(err)
		}
		return <-agents
	}
	lease := pool.Acquire(config.GroupKeep2Share, id, 1)
	if lease == nil {
		t.Fatal("no route")
	}
	first, second := ask(lease), ask(lease)
	lease.Release()
	if first != second || !slices.Contains(config.BrowserUserAgents, first) {
		t.Fatalf("the route sent %q, then %q; want one browser from the list", first, second)
	}
	waf := pool.Acquire(config.GroupWAF, id, 1)
	if waf == nil {
		t.Fatal("no route for WAF recovery")
	}
	defer waf.Release()
	if got := ask(waf); got != m.cfg.UserAgent {
		t.Fatalf("WAF recovery sent %q; want the client's own %q", got, m.cfg.UserAgent)
	}
}

// A proxy that answers and cannot reach the host has refused that host, not
// died: the route is held back for the service asking, and stays on offer to
// the others. Scored as dead, one CDN out of public proxies' reach shelved
// them for every service.
func TestATunnelRefusedAtTheProxyHoldsTheRouteBackForThatServiceAlone(t *testing.T) {
	m := busyManager(t)
	route := "http://" + fakeProxy(t, "127.0.0.2", answerConnect("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n", nil))
	pool := attachPool(t, m, []string{route})
	id := pool.Page(config.GroupKeep2Share, proxy.Query{Search: route}).Rows[0].ID
	lease := pool.Acquire(config.GroupKeep2Share, id, 1)
	if lease == nil {
		t.Fatal("no route")
	}
	_, err := m.client.WithTimeout(3*time.Second).GetString(lease.Context(t.Context()), "https://cdn.example.test/media", nil)
	lease.Release()
	if refused, ok := errors.AsType[*httpx.ProxyRefusedError](err); !ok || refused.Code != 502 {
		t.Fatalf("error = %v; want the proxy's 502", err)
	}
	if row := pool.Page(config.GroupKeep2Share, proxy.Query{Search: route}).Rows[0]; row.Status != "cooling" || time.Until(row.CooldownUntil) > 5*time.Minute {
		t.Fatalf("for the service asking: %+v; want it held back briefly, as a refusal", row)
	}
	if row := pool.Page(config.GroupEmload, proxy.Query{Search: route}).Rows[0]; row.Status == "cooling" {
		t.Fatalf("for another service: %+v; want the route still on offer", row)
	}
}

// A proxy that refuses the tunnel for any other reason than failing to reach
// the host is no proxy for anyone, and is set aside for every service, by
// the exit probe as by any other request. Taken for the host's refusals, Tor
// ports and servers that know no CONNECT came back after a minute's rest and
// took nearly every extra stream's turn: the probe scores only a success or a
// dead connection, so it never set them aside.
func TestAProxyThatCannotTunnelIsSetAsideForEveryService(t *testing.T) {
	for _, answer := range []string{
		"HTTP/1.1 501 Tor is not an HTTP Proxy\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.1 503 unknown method\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\n\r\n",
	} {
		t.Run(strings.Fields(answer)[1], func(t *testing.T) {
			m := busyManager(t)
			route := "http://" + fakeProxy(t, "127.0.0.2", answerConnect(answer, nil))
			pool := attachPool(t, m, []string{route})
			id := pool.Page(config.GroupKeep2Share, proxy.Query{Search: route}).Rows[0].ID
			lease := pool.Acquire(config.GroupKeep2Share, id, 1)
			if lease == nil {
				t.Fatal("no route")
			}
			_, err := m.client.WithTimeout(3*time.Second).GetString(lease.ProbeContext(t.Context()), "https://echo.example.test/", nil)
			lease.Release()
			if _, ok := errors.AsType[*httpx.ProxyRefusedError](err); !ok {
				t.Fatalf("error = %v; want the proxy's refusal", err)
			}
			for _, service := range []string{config.GroupKeep2Share, config.GroupEmload} {
				if row := pool.Page(service, proxy.Query{Search: route}).Rows[0]; row.Status != "cooling" {
					t.Errorf("for %s: %+v; want it set aside", service, row)
				}
			}
		})
	}
}
