// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestCookieJarIsolationIncludesRedirectsAndRoutes(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("routed=%t", routed), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if routed && r.UserAgent() != "route-agent" {
					t.Errorf("lost route user-agent: %q", r.UserAgent())
				}
				if r.URL.Path == "/start" {
					http.SetCookie(w, &http.Cookie{Name: "folder", Value: r.URL.Query().Get("id"), Path: "/"})
					http.Redirect(w, r, "/read", http.StatusSeeOther)
					return
				}
				cookie, err := r.Cookie("folder")
				if err != nil {
					http.Error(w, "no folder cookie", http.StatusUnauthorized)
					return
				}
				fmt.Fprint(w, cookie.Value)
			}))
			defer srv.Close()
			client, route := testClient(t, 0), testClient(t, 0)
			ctx := context.Background()
			var observations atomic.Int32
			if routed {
				ctx = WithRouteAgent(ctx, "test", route, func(RouteObservation) { observations.Add(1) }, "route-agent")
			}
			first, _ := cookiejar.New(nil)
			second, _ := cookiejar.New(nil)
			for _, step := range []struct {
				jar   http.CookieJar
				path  string
				value string
			}{
				{first, "/start?id=first", "first"},
				{second, "/start?id=second", "second"},
				{first, "/read", "first"},
				{second, "/read", "second"},
			} {
				body, err := client.Streaming().GetString(WithCookieJar(ctx, step.jar), srv.URL+step.path, nil)
				if err != nil || body != step.value {
					t.Fatalf("%s = %q, %v; want %q", step.path, body, err, step.value)
				}
			}
			u, _ := url.Parse(srv.URL)
			if len(client.hc.Jar.Cookies(u)) != 0 || len(route.hc.Jar.Cookies(u)) != 0 {
				t.Fatal("source cookies leaked into the shared client or route")
			}
			if routed && observations.Load() != 4 {
				t.Fatalf("lost route observations: %d", observations.Load())
			}
		})
	}
}
