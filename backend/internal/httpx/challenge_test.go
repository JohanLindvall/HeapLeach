// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestChallengeStopsRetriesAndKeepsRouteRefusalSeparateFromTransportFailure(t *testing.T) {
	for _, code := range []int{200, 403, 429, 503} {
		for _, streaming := range []bool{false, true} {
			for _, routed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/streaming=%t/routed=%t", code, streaming, routed), func(t *testing.T) {
					var hits atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						hits.Add(1)
						w.Header().Set(HeaderCFMitigated, "challenge")
						w.Header().Set(HeaderContentType, "text/html")
						w.WriteHeader(code)
						w.(http.Flusher).Flush()
						// Detection must close the response without waiting for its body.
						<-r.Context().Done()
					}))
					defer srv.Close()
					client := New("test", "en", 3, time.Second)
					defer client.CloseIdleConnections()
					if streaming {
						client = client.Streaming()
					}
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					var observations []RouteObservation
					if routed {
						route, err := client.ThroughProxy(srv.URL)
						if err != nil {
							t.Fatal(err)
						}
						defer route.CloseIdleConnections()
						ctx = WithRoute(ctx, "test-route", route, func(o RouteObservation) { observations = append(observations, o) })
					}
					for _, do := range []func(*http.Request) (*http.Response, error){client.Do, client.DoOnce} {
						req, _ := client.NewRequest(ctx, http.MethodGet, srv.URL+"/file", nil)
						resp, err := do(req)
						if _, ok := errors.AsType[*ChallengeError](err); !ok || resp != nil || !HasStatus(err, code) {
							t.Fatalf("response=%v err=%v", resp, err)
						}
						if _, ok := errors.AsType[*RouteError](err); ok != routed {
							t.Fatalf("route error=%t, want %t", ok, routed)
						}
					}
					if hits.Load() != 2 || ctx.Err() != nil {
						t.Fatalf("requests=%d, context=%v", hits.Load(), ctx.Err())
					}
					if routed {
						if len(observations) != 2 {
							t.Fatalf("observations=%v", observations)
						}
						for _, o := range observations {
							if o.Status != http.StatusForbidden || o.Bytes != 0 || o.Err == nil {
								t.Fatalf("challenge was not a service refusal: %+v", o)
							}
						}
					}
				})
			}
		}
	}
}

func TestCloudflareServerHeaderAloneDoesNotMeanChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "cloudflare")
		http.Error(w, "File unavailable", http.StatusForbidden)
	}))
	defer srv.Close()
	client := New("test", "en", 0, time.Second)
	defer client.CloseIdleConnections()
	_, err := client.GetString(context.Background(), srv.URL, nil)
	if _, ok := errors.AsType[*ChallengeError](err); ok || !HasStatus(err, http.StatusForbidden) {
		t.Fatalf("ordinary forbidden response: %v", err)
	}
}
