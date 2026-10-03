package httpx

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestCookiesCannotCrossAPublicSuffix(t *testing.T) {
	c := testClient(t, 0)
	source, _ := url.Parse("https://first.example.co.uk/")
	other, _ := url.Parse("https://second.example.co.uk/")
	c.hc.Jar.SetCookies(source, []*http.Cookie{{Name: "injected", Value: "yes", Domain: "co.uk", Path: "/"}})
	if got := c.hc.Jar.Cookies(other); len(got) != 0 {
		t.Fatalf("a public-suffix cookie reached another site: %v", got)
	}
	c.hc.Jar.SetCookies(source, []*http.Cookie{{Name: "session", Value: "yes", Domain: "example.co.uk", Path: "/"}})
	if got := c.hc.Jar.Cookies(other); len(got) != 1 || got[0].Name != "session" {
		t.Fatalf("legitimate same-site cookies no longer work: %v", got)
	}
}

func TestBrowserTransportHonoursProxy(t *testing.T) {
	var connects, direct atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			connects.Add(1)
		}
		http.Error(w, "proxy refused the tunnel", http.StatusForbidden)
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	standard := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer standard.CloseIdleConnections()
	bt := newBrowserTransport(standard).(*browserTransport)
	bt.impersonated.DialTLSContext = func(context.Context, string, string, *tls.Config) (net.Conn, error) {
		direct.Add(1)
		return nil, errors.New("unexpected direct connection")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://origin.example.test/", nil)
	if resp, err := bt.RoundTrip(req); err == nil {
		resp.Body.Close()
		t.Fatal("the configured proxy's refusal was ignored")
	}
	if connects.Load() != 1 || direct.Load() != 0 {
		t.Fatalf("proxy CONNECTs = %d, direct attempts = %d; want 1, 0", connects.Load(), direct.Load())
	}
}

func TestRedirectDoesNotRestoreAReferrerOnTLSDowngrade(t *testing.T) {
	var referer string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		referer = r.Referer()
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer secure.Close()
	c := testClient(t, 0)
	c.hc.Transport = secure.Client().Transport
	req, _ := c.NewRequest(context.Background(), http.MethodGet, secure.URL, nil)
	req.Header.Set(HeaderReferer, "https://private.example.test/")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if referer != "" {
		t.Fatalf("HTTPS referrer leaked to HTTP: %q", referer)
	}
}

func TestStreamingErrorBodiesHaveADeadline(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			c := testClient(t, 0)
			c.hc.Timeout = 50 * time.Millisecond
			c = c.Streaming()
			req, _ := c.NewRequest(ctx, http.MethodGet, srv.URL, nil)
			resp, err := c.Do(req)
			if resp != nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if err == nil || ctx.Err() != nil {
				t.Fatalf("error response was not bounded by the client's timeout: %v (parent: %v)", err, ctx.Err())
			}
		})
	}
}
