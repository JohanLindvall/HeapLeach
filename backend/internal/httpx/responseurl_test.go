// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestResponseURLKeepsRedirectedStatusPrivate(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusGone} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var final string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/submitted" {
					http.Redirect(w, r, final, http.StatusFound)
					return
				}
				if r.URL.Path != "/destination" {
					t.Errorf("unexpected request path %s", r.URL.Path)
				}
				w.Header().Set(HeaderContentType, "text/plain")
				w.Header().Set(HeaderRetryAfter, "0")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "Unavailable")
			}))
			defer srv.Close()
			final = srv.URL + "/destination?signature=synthetic-secret&part=2&part=1"
			submitted, err := url.Parse(srv.URL + "/submitted")
			if err != nil {
				t.Fatal(err)
			}
			submitted.User = url.UserPassword("reader", "synthetic-password")
			client := testClient(t, 0)
			defer client.CloseIdleConnections()
			req, err := client.NewRequest(t.Context(), http.MethodGet, submitted.String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.DoOnce(req)
			if code == http.StatusGone {
				// Permanent responses belong to the caller, which constructs
				// its status error; DoOnce consumes retryable ones itself.
				if err != nil {
					t.Fatal(err)
				}
				err = NewStatusError(resp)
				resp.Body.Close()
			} else {
				if resp != nil {
					resp.Body.Close()
					t.Fatal("retryable status unexpectedly returned an open response")
				}
			}
			status, ok := errors.AsType[*StatusError](err)
			if !ok || status.Code != code {
				t.Fatalf("got %v, want status %d", err, code)
			}
			for _, wrapped := range []error{err, fmt.Errorf("wrapped: %w", err), &RouteError{Err: err}} {
				if got := ResponseURL(wrapped); got != final {
					t.Errorf("ResponseURL = %q, want final destination %q", got, final)
				}
				if !HasStatus(wrapped, code) || !errors.Is(wrapped, status) {
					t.Error("reading the destination changed the status error chain")
				}
			}
			wantMessage := submitted.Redacted() + ": " + fmt.Sprintf("%d %s", code, http.StatusText(code)) + " (Unavailable)"
			if status.URL != submitted.Redacted() || status.Error() != wantMessage {
				t.Errorf("diagnostic changed: URL=%q error=%q, want %q", status.URL, status.Error(), wantMessage)
			}
			encoded, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			for _, diagnostic := range []string{status.Error(), fmt.Sprintf("%+v", status), string(encoded)} {
				if strings.Contains(diagnostic, "synthetic-secret") || strings.Contains(diagnostic, "synthetic-password") || strings.Contains(diagnostic, "/destination") {
					t.Errorf("diagnostic exposed private request metadata: %s", diagnostic)
				}
			}
		})
	}
}

type responseURLTransport func(*http.Request) (*http.Response, error)

func (f responseURLTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestResponseURLTracksRedirectedTransportFailure(t *testing.T) {
	const submitted = "https://origin.example.test/submitted"
	const final = "https://target.example.test/destination%2Fopaque?signature=synthetic-secret"
	failure := errors.New("synthetic connection failure")
	client := testClient(t, 0)
	client.hc.Transport = responseURLTransport(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case submitted:
			w := httptest.NewRecorder()
			http.Redirect(w, req, final, http.StatusFound)
			resp := w.Result()
			resp.Request = req
			return resp, nil
		case final:
			return nil, failure
		default:
			return nil, fmt.Errorf("unexpected offline request %s", req.URL)
		}
	})
	req, err := client.NewRequest(t.Context(), http.MethodGet, submitted, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.DoOnce(req)
	transport, ok := errors.AsType[*url.Error](err)
	if !ok || !errors.Is(err, failure) {
		t.Fatalf("got %v, want the transport's wrapped failure", err)
	}
	message := err.Error()
	for _, wrapped := range []error{err, fmt.Errorf("wrapped: %w", err), &RouteError{Err: err}, &ProxyConnectError{Err: err}} {
		if got := ResponseURL(wrapped); got != final {
			t.Errorf("ResponseURL = %q, want failed destination %q", got, final)
		}
		got, ok := errors.AsType[*url.Error](wrapped)
		if !ok || got != transport || !errors.Is(wrapped, failure) {
			t.Error("reading the destination changed the transport error chain")
		}
	}
	if err.Error() != message {
		t.Error("reading the destination changed the transport diagnostic")
	}
}

func TestResponseURLWithoutMetadataAndCancellation(t *testing.T) {
	for _, err := range []error{nil, errors.New("plain failure"), context.Canceled, context.DeadlineExceeded,
		&StatusError{Code: http.StatusServiceUnavailable, URL: "https://origin.example.test/submitted"}} {
		if got := ResponseURL(err); got != "" {
			t.Errorf("ResponseURL(%v) = %q without destination metadata", err, got)
		}
	}
	const final = "https://target.example.test/destination"
	canceled := fmt.Errorf("wrapped: %w", &url.Error{Op: "Get", URL: final, Err: context.Canceled})
	if got := ResponseURL(canceled); got != final || !errors.Is(canceled, context.Canceled) {
		t.Errorf("cancellation changed: destination=%q error=%v", got, canceled)
	}
}
