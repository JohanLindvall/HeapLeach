// SPDX-License-Identifier: MIT

package httpx

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRoutePinsRedirectsCookiesAndBodyMeasurements(t *testing.T) {
	base := New("test", "en", 3, time.Second)
	payload := strings.Repeat("x", 128<<10)
	var direct atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1) }))
	defer origin.Close()
	makeRoute := func(id string) *Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				if r.Header.Get("Cookie") != "" {
					t.Error("cookies crossed route boundaries")
				}
				http.SetCookie(w, &http.Cookie{Name: "route", Value: id})
				http.Redirect(w, r, "/file", http.StatusFound)
				return
			}
			cookie, err := r.Cookie("route")
			if err != nil || cookie.Value != id {
				t.Error("redirect lost its route cookie")
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			fmt.Fprint(w, payload)
		}))
		t.Cleanup(srv.Close)
		client, err := base.ThroughProxy(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.CloseIdleConnections)
		return client
	}
	for _, id := range []string{"one", "two"} {
		var observations []RouteObservation
		ctx := WithRoute(context.Background(), id, makeRoute(id), func(o RouteObservation) { observations = append(observations, o) })
		req, _ := base.NewRequest(ctx, http.MethodGet, origin.URL+"/start", nil)
		body, err := base.Bytes(req)
		if err != nil || string(body) != payload {
			t.Fatalf("body length=%d err=%v", len(body), err)
		}
		if len(observations) != 1 || observations[0].Bytes != int64(len(payload)) || observations[0].Duration <= 0 {
			t.Fatalf("body observation: %+v", observations)
		}
	}
	if direct.Load() != 0 {
		t.Fatal("explicit proxy was bypassed for a loopback destination")
	}
}

func TestRouteRefusalReturnsToOwnerWithoutHTTPRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	base := New("test", "en", 4, time.Second)
	route, _ := base.ThroughProxy(srv.URL)
	defer route.CloseIdleConnections()
	var observation RouteObservation
	ctx := WithRoute(context.Background(), "route", route, func(o RouteObservation) { observation = o })
	req, _ := base.NewRequest(ctx, http.MethodGet, "http://example.test/file", nil)
	_, err := base.Bytes(req)
	if _, ok := errors.AsType[*RouteError](err); !ok || calls.Load() != 1 || time.Until(observation.RetryAfter) < 119*time.Second {
		t.Fatalf("calls=%d observation=%+v err=%v", calls.Load(), observation, err)
	}
}

func TestRouteCONNECTVerifiesDestinationAndNeverFallsBack(t *testing.T) {
	for _, mode := range []string{"0", "1"} {
		for _, secure := range []bool{false, true} {
			t.Run(fmt.Sprintf("utls=%s/https-proxy=%t", mode, secure), func(t *testing.T) {
				t.Setenv("HEAPLEACH_UTLS", mode)
				origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "verified") }))
				defer origin.Close()
				var connects atomic.Int32
				srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodConnect {
						t.Error("not a CONNECT")
						w.WriteHeader(400)
						return
					}
					connects.Add(1)
					upstream, err := net.Dial("tcp", r.Host)
					if err != nil {
						w.WriteHeader(502)
						return
					}
					defer upstream.Close()
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					fmt.Fprint(rw, "HTTP/1.1 200 Connection Established\r\n\r\n")
					rw.Flush()
					done := make(chan struct{})
					go func() { io.Copy(upstream, rw); close(done) }()
					io.Copy(conn, upstream)
					conn.Close()
					<-done
				}))
				if secure {
					srv.StartTLS()
				} else {
					srv.Start()
				}
				defer srv.Close()
				base := New("test", "en", 0, time.Second)
				route, _ := base.ThroughProxy(srv.URL)
				defer route.CloseIdleConnections()
				ctx := WithRoute(context.Background(), "route", route, nil)
				req, _ := base.NewRequest(ctx, http.MethodGet, origin.URL, nil)
				if _, err := base.Bytes(req); err == nil {
					t.Fatal("accepted an untrusted destination certificate")
				}
				var standard *http.Transport
				switch tr := route.hc.Transport.(type) {
				case *http.Transport:
					standard = tr
				case *browserTransport:
					standard = tr.standard
				}
				// Trust only the fixture certificate and repeat the same CONNECT path.
				standard.CloseIdleConnections()
				standard.TLSClientConfig = &tls.Config{RootCAs: x509.NewCertPool()}
				standard.TLSClientConfig.RootCAs.AddCert(origin.Certificate())
				if body, err := base.Bytes(req); err != nil || string(body) != "verified" {
					t.Fatalf("CONNECT body=%q err=%v", body, err)
				}
				if connects.Load() != 2 {
					t.Fatalf("CONNECT count=%d", connects.Load())
				}
			})
		}
	}
}

func TestHTTPSProxyAllowsSelfSignedCertificateForHTTP(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "origin.example.test" {
			t.Errorf("lost the forwarding target: %s", r.URL.Host)
		}
		fmt.Fprint(w, "proxied")
	}))
	defer srv.Close()
	base := New("test", "en", 0, time.Second)
	route, err := base.ThroughProxy(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer route.CloseIdleConnections()
	ctx := WithRoute(context.Background(), "https-proxy", route, nil)
	if body, err := base.GetString(ctx, "http://origin.example.test/file", nil); err != nil || body != "proxied" {
		t.Fatalf("self-signed HTTPS proxy: body=%q error=%v", body, err)
	}
}

func TestRouteRequestTimeoutBoundsProxyCONNECT(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("https-proxy=%t", secure), func(t *testing.T) {
			var connects atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					t.Errorf("expected CONNECT, got %s", r.Method)
				}
				connects.Add(1)
				<-r.Context().Done()
			}))
			if secure {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			base := New("test", "en", 3, time.Minute)
			defer base.CloseIdleConnections()
			route, err := base.ThroughProxy(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer route.CloseIdleConnections()
			var observations []RouteObservation
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			ctx = WithRoute(ctx, "silent-proxy", route, func(o RouteObservation) { observations = append(observations, o) })
			req, _ := base.NewRequest(ctx, http.MethodPost, "https://service.example.test/api/v2/requestCaptcha", []byte(`{}`))
			_, err = base.WithTimeout(100 * time.Millisecond).Bytes(req)
			if _, ok := errors.AsType[*RouteError](err); !ok || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatalf("proxy timeout=%v caller=%v", err, ctx.Err())
			}
			if connects.Load() != 1 || len(observations) != 1 || observations[0].Err == nil {
				t.Fatalf("connects=%d observations=%+v; want one scored timeout with no HTTP retry", connects.Load(), observations)
			}
		})
	}
}

func TestRouteCancellationDoesNotPenaliseProxyAndStreamingKeepsItsTimeout(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	base := New("test", "en", 0, time.Second)
	route, _ := base.ThroughProxy(srv.URL)
	defer route.CloseIdleConnections()
	var observations atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithRoute(ctx, "route", route, func(RouteObservation) { observations.Add(1) })
	done := make(chan error, 1)
	go func() {
		req, _ := base.NewRequest(ctx, http.MethodGet, "http://example.test/file", nil)
		_, err := base.Bytes(req)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || observations.Load() != 0 {
		t.Fatalf("cancellation: %v observations=%d", err, observations.Load())
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(80 * time.Millisecond)
		fmt.Fprint(w, "file")
	}))
	defer slow.Close()
	base = New("test", "en", 0, 40*time.Millisecond)
	route, _ = base.ThroughProxy(slow.URL)
	defer route.CloseIdleConnections()
	ctx = WithRoute(context.Background(), "route", route, nil)
	req, _ := base.NewRequest(ctx, http.MethodGet, "http://example.test/file", nil)
	if body, err := base.Streaming().Bytes(req); err != nil || string(body) != "file" {
		t.Fatalf("streaming body=%q err=%v", body, err)
	}
}

func TestSOCKSRouteTimeoutPenalisesAnUnansweredHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	defer func() {
		listener.Close()
		if conn := <-accepted; conn != nil {
			conn.Close()
		}
	}()
	base := New("test", "en", 3, time.Minute)
	defer base.CloseIdleConnections()
	route, err := base.ThroughProxy("socks5h://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer route.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var observations []RouteObservation
	ctx = WithRoute(ctx, "silent-socks-proxy", route, func(o RouteObservation) { observations = append(observations, o) })
	req, _ := base.NewRequest(ctx, http.MethodPost, "https://service.example.test/api/v2/requestCaptcha", []byte(`{}`))
	_, err = base.WithTimeout(100 * time.Millisecond).Bytes(req)
	if _, ok := errors.AsType[*RouteError](err); !ok || !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("SOCKS timeout=%v caller=%v", err, ctx.Err())
	}
	if len(observations) != 1 || observations[0].Err == nil {
		t.Fatalf("observations=%+v; want one scored timeout", observations)
	}
}

func TestSOCKSRouteResolvesNamesAtTheProxy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		read := func(n int) ([]byte, error) { b := make([]byte, n); _, err := io.ReadFull(conn, b); return b, err }
		greeting, err := read(2)
		if err != nil {
			done <- err
			return
		}
		if _, err := read(int(greeting[1])); err != nil {
			done <- err
			return
		}
		conn.Write([]byte{5, 0})
		request, err := read(5)
		if err != nil {
			done <- err
			return
		}
		if request[0] != 5 || request[1] != 1 || request[3] != 3 {
			done <- fmt.Errorf("SOCKS request is not a domain CONNECT: %v", request)
			return
		}
		host, err := read(int(request[4]))
		if err != nil {
			done <- err
			return
		}
		if string(host) != "unresolvable.example.test" {
			done <- fmt.Errorf("SOCKS host=%q", host)
			return
		}
		if _, err := read(2); err != nil {
			done <- err
			return
		}
		conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			done <- err
			return
		}
		req.Body.Close()
		_, err = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 4\r\nConnection: close\r\n\r\nfile")
		done <- err
	}()
	base := New("test", "en", 0, time.Second)
	route, err := base.ThroughProxy("socks5h://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer route.CloseIdleConnections()
	ctx := WithRoute(context.Background(), "socks", route, nil)
	req, _ := base.NewRequest(ctx, http.MethodGet, "http://unresolvable.example.test/file", nil)
	if body, err := base.Bytes(req); err != nil || string(body) != "file" {
		t.Fatalf("SOCKS body=%q err=%v", body, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
