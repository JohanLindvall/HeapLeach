// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestPageChallengesAreCapturedBeforeRouteAccounting(t *testing.T) {
	for _, code := range []int{200, 403, 503} {
		for _, routed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/routed=%t", code, routed), func(t *testing.T) {
				var hits atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.Header().Set(HeaderContentType, "text/html; charset=UTF-8")
					w.WriteHeader(code)
					fmt.Fprint(w, `<html><div id="synthetic-gate">Verify</div></html>`)
				}))
				defer srv.Close()
				client := testClient(t, 3)
				defer client.CloseIdleConnections()
				ctx, captured := CaptureChallenges(context.Background())
				var observations []RouteObservation
				if routed {
					proxy, err := client.ThroughProxy(srv.URL)
					if err != nil {
						t.Fatal(err)
					}
					defer proxy.CloseIdleConnections()
					ctx = WithRoute(ctx, "route", proxy, func(o RouteObservation) { observations = append(observations, o) })
				}
				_, _, err := client.GetPage(ctx, srv.URL+"/gate", nil, func(doc string) bool {
					return strings.Contains(doc, `id="synthetic-gate"`)
				})
				challenge, ok := errors.AsType[*ChallengeError](err)
				if !ok || challenge.Err.Code != code || challenge.Err.URL != srv.URL+"/gate" {
					t.Fatalf("challenge=%+v err=%v", challenge, err)
				}
				if _, ok := errors.AsType[*RouteError](err); ok != routed {
					t.Fatalf("route error=%t, want %t", ok, routed)
				}
				if !errors.Is(captured(), challenge) {
					t.Fatalf("captured=%v, want %v", captured(), challenge)
				}
				// An extractor swallowing the first error must not continue
				// probing, or make the challenge disappear from the source.
				_, err = client.GetString(ctx, srv.URL+"/next", nil)
				if !errors.Is(err, challenge) || hits.Load() != 1 {
					t.Fatalf("requests=%d error=%v", hits.Load(), err)
				}
				if routed {
					if len(observations) != 1 {
						t.Fatalf("observations=%+v", observations)
					}
					o := observations[0]
					if o.Status != http.StatusForbidden || o.Bytes != 0 || !errors.Is(o.Err, challenge) {
						t.Fatalf("challenge counted as ordinary traffic: %+v", o)
					}
				}
			})
		}
	}
}

func TestPageCheckPreservesOrdinaryBodiesAndRedirects(t *testing.T) {
	for _, kind := range []string{"text/html", "application/xhtml+xml", "text/plain", "video/mp4"} {
		t.Run(kind, func(t *testing.T) {
			const payload = "<html><p>Ordinary page</p></html>"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/old" {
					http.Redirect(w, r, "/current", http.StatusFound)
					return
				}
				w.Header().Set(HeaderContentType, kind)
				fmt.Fprint(w, payload)
			}))
			defer srv.Close()
			client := testClient(t, 0)
			defer client.CloseIdleConnections()
			checks := 0
			body, final, err := client.GetPage(context.Background(), srv.URL+"/old", nil, func(doc string) bool {
				checks++
				if doc != payload {
					t.Errorf("check received %q", doc)
				}
				return false
			})
			if err != nil || body != payload || final == nil || final.String() != srv.URL+"/current" {
				t.Fatalf("body=%q final=%v err=%v", body, final, err)
			}
			want := 0
			if kind == "text/html" || kind == "application/xhtml+xml" {
				want = 1
			}
			if checks != want {
				t.Fatalf("checks=%d, want %d", checks, want)
			}
		})
	}
}
