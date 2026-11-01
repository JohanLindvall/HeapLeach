// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidatedResponseLengthRecordsOnlyCompletedBodies(t *testing.T) {
	for _, mode := range []string{"complete", "unread", "partly read", "cancelled", "short", "excess"} {
		for _, admitted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admitted=%v", mode, admitted), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set(HeaderContentRange, "bytes 0-3/4")
					w.WriteHeader(http.StatusPartialContent)
					w.(http.Flusher).Flush()
					payload := "data"
					if mode == "short" {
						payload = "da"
					} else if mode == "excess" {
						payload += "unread tail"
					}
					fmt.Fprint(w, payload)
					w.(http.Flusher).Flush()
					if mode != "short" {
						<-r.Context().Done()
					}
				}))
				defer srv.Close()
				base := New("test", "en", 0, time.Second).Streaming()
				route, err := base.ThroughProxy(srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				defer route.CloseIdleConnections()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var observations []RouteObservation
				ctx = WithRoute(ctx, "fixture", route, func(o RouteObservation) { observations = append(observations, o) })
				admission := NewAdmission(1)
				if admitted {
					ctx = WithAdmission(ctx, admission)
				}
				req, _ := base.NewRequest(ctx, http.MethodGet, "http://example.test/file", nil)
				resp, err := base.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				LimitResponseBody(resp, 4)
				var body []byte
				switch mode {
				case "unread":
				case "partly read", "cancelled":
					body = make([]byte, 2)
					_, err = io.ReadFull(resp.Body, body)
				default:
					body, err = io.ReadAll(resp.Body)
				}
				if mode == "cancelled" {
					cancel()
				}
				resp.Body.Close()
				resp.Body.Close() // Completion is accounted exactly once.
				if !admission.Idle() {
					t.Fatal("the bounded body leaked its admission slot")
				}
				switch mode {
				case "complete", "excess":
					if err != nil || string(body) != "data" || len(observations) != 1 || observations[0].Err != nil || observations[0].Bytes != 4 {
						t.Fatalf("body=%q err=%v observations=%+v", body, err, observations)
					}
				case "short":
					if !errors.Is(err, io.ErrUnexpectedEOF) || string(body) != "da" || len(observations) != 1 || !errors.Is(observations[0].Err, io.ErrUnexpectedEOF) {
						t.Fatalf("short body=%q err=%v observations=%+v", body, err, observations)
					}
				default:
					if err != nil || len(observations) != 0 || !strings.HasPrefix("data", string(body)) {
						t.Fatalf("unfinished body=%q err=%v observations=%+v", body, err, observations)
					}
				}
			})
		}
	}
}

func TestRouteHeadWithResourceLengthStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderContentLength, "4096")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	base := New("test", "en", 0, time.Second)
	route, err := base.ThroughProxy(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer route.CloseIdleConnections()
	var observations []RouteObservation
	ctx := WithRoute(t.Context(), "fixture", route, func(o RouteObservation) { observations = append(observations, o) })
	req, _ := base.NewRequest(ctx, http.MethodHead, "http://example.test/file", nil)
	body, err := base.Bytes(req)
	if err != nil || len(body) != 0 || len(observations) != 1 || observations[0].Err != nil || observations[0].Bytes != 0 {
		t.Fatalf("HEAD: body=%q err=%v observations=%+v", body, err, observations)
	}
}
