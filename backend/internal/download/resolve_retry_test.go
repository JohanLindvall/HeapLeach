// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// net/http's TLS handshake timeout is an unexported net.Error that does
// not wrap context.DeadlineExceeded. Keep that distinction in the fixture.
type resolveHandshakeTimeout struct{}

func (resolveHandshakeTimeout) Error() string   { return "net/http: TLS handshake timeout" }
func (resolveHandshakeTimeout) Timeout() bool   { return true }
func (resolveHandshakeTimeout) Temporary() bool { return true }

func resolveRequestError(err error) error {
	return fmt.Errorf("sign download link: %w", &url.Error{
		Op: "Get", URL: "https://api.example.test/sign?path=%2Fsample.jpg", Err: err,
	})
}

func resolveRetryManager(retries int) *Manager {
	return &Manager{
		cfg: &config.Config{MaxRetries: retries},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestResolveRetriesTemporaryFailures(t *testing.T) {
	for name, failure := range map[string]error{
		"TLS handshake":       resolveRequestError(resolveHandshakeTimeout{}),
		"dial":                resolveRequestError(&net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}),
		"client deadline":     resolveRequestError(context.DeadlineExceeded),
		"connection reset":    resolveRequestError(syscall.ECONNRESET),
		"connection closed":   resolveRequestError(io.EOF),
		"truncated body":      fmt.Errorf("read metadata: %w", io.ErrUnexpectedEOF),
		"request timeout":     &httpx.StatusError{Code: http.StatusRequestTimeout},
		"rate limit":          &httpx.StatusError{Code: http.StatusTooManyRequests},
		"server error":        &httpx.StatusError{Code: http.StatusInternalServerError},
		"gateway error":       &httpx.StatusError{Code: http.StatusBadGateway},
		"unavailable":         &httpx.StatusError{Code: http.StatusServiceUnavailable},
		"gateway timeout":     &httpx.StatusError{Code: http.StatusGatewayTimeout},
		"extractor transient": &extractor.TransientError{Err: errors.New("try again")},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := resolveRetryManager(2)
				var calls int
				start := time.Now()
				it := &Item{Name: "sample.jpg", resolve: func(ctx context.Context) (*extractor.Target, error) {
					calls++
					extractor.ResolveNote(ctx, "Obtaining download link")
					if calls <= 2 {
						return nil, failure
					}
					return &extractor.Target{URL: "https://storage.example.test/sample.jpg"}, nil
				}}
				if err := m.resolveTarget(t.Context(), it); err != nil {
					t.Fatalf("temporary failure stopped resolution: %v", err)
				}
				if calls != 3 || it.URL == "" || it.Note != "" {
					t.Fatalf("calls=%d URL=%q note=%q", calls, it.URL, it.Note)
				}
				// Backoff has +/-25% jitter: the first two waits total 2.25-3.75s.
				if elapsed := time.Since(start); elapsed < 2250*time.Millisecond || elapsed > 3750*time.Millisecond {
					t.Fatalf("retry backoff took %s", elapsed)
				}
			})
		})
	}
}

func TestResolveRetryBudgetIsBounded(t *testing.T) {
	for _, retries := range []int{0, 2} {
		t.Run(fmt.Sprint(retries), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := resolveRetryManager(retries)
				failure := resolveRequestError(context.DeadlineExceeded)
				var calls int
				it := &Item{resolve: func(context.Context) (*extractor.Target, error) {
					calls++
					return nil, failure
				}}
				err := m.resolveTarget(t.Context(), it)
				if err != failure || calls != retries+1 || it.Note != "" {
					t.Fatalf("error=%v calls=%d note=%q; want last error after %d calls", err, calls, it.Note, retries+1)
				}
			})
		})
	}
}

func TestResolveDoesNotRetryPermanentOrRoutedFailures(t *testing.T) {
	for name, failure := range map[string]error{
		"missing":         &httpx.StatusError{Code: http.StatusNotFound},
		"forbidden":       &httpx.StatusError{Code: http.StatusForbidden},
		"unsupported":     &httpx.StatusError{Code: http.StatusNotImplemented},
		"parser":          errors.New("metadata is missing the media location"),
		"certificate":     resolveRequestError(x509.UnknownAuthorityError{Cert: &x509.Certificate{}}),
		"DNS not found":   resolveRequestError(&net.DNSError{Err: "no such host", IsNotFound: true}),
		"cancelled":       resolveRequestError(context.Canceled),
		"host wait":       &extractor.WaitError{Until: time.Now().Add(time.Hour), Reason: "quota used"},
		"address refused": &extractor.RefusedError{Err: context.DeadlineExceeded},
		"route":           &httpx.RouteError{Err: context.DeadlineExceeded},
		"challenge":       &httpx.ChallengeError{Err: &httpx.StatusError{Code: http.StatusForbidden}},
	} {
		t.Run(name, func(t *testing.T) {
			m := resolveRetryManager(3)
			var calls int
			it := &Item{resolve: func(context.Context) (*extractor.Target, error) {
				calls++
				return nil, failure
			}}
			if err := m.resolveTarget(t.Context(), it); err != failure || calls != 1 {
				t.Fatalf("error=%v calls=%d; want original error after one call", err, calls)
			}
		})
	}
}

func TestResolveLeavesLeasedRetriesToRouteRecovery(t *testing.T) {
	for _, route := range []string{"proxy-test", httpx.DirectRoute} {
		t.Run(route, func(t *testing.T) {
			m := resolveRetryManager(3)
			failure := &extractor.TransientError{Err: errors.New("temporary resolver failure")}
			var calls int
			it := &Item{resolve: func(context.Context) (*extractor.Target, error) {
				calls++
				return nil, failure
			}}
			ctx := httpx.WithRoute(t.Context(), route, nil, nil)
			if err := m.resolveTarget(ctx, it); err != failure || calls != 1 {
				t.Fatalf("error=%v calls=%d; want route recovery after one call", err, calls)
			}
		})
	}
}

func TestResolveRetryCancellation(t *testing.T) {
	for _, when := range []string{"before resolution", "during backoff", "caller deadline"} {
		t.Run(when, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				want := context.Canceled
				if when == "before resolution" {
					cancel()
				} else if when == "caller deadline" {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
					defer stop()
					want = context.DeadlineExceeded
				}
				m := resolveRetryManager(3)
				var calls int
				it := &Item{resolve: func(context.Context) (*extractor.Target, error) {
					calls++
					return nil, resolveRequestError(context.DeadlineExceeded)
				}}
				done := make(chan error, 1)
				go func() { done <- m.resolveTarget(ctx, it) }()
				synctest.Wait()
				if when != "before resolution" {
					m.mu.Lock()
					note := it.Note
					m.mu.Unlock()
					if !strings.Contains(note, "retrying") {
						t.Fatalf("retry wait is not visible: %q", note)
					}
					if when == "during backoff" {
						cancel()
					}
				}
				if err := <-done; !errors.Is(err, want) {
					t.Fatalf("error=%v; want %v", err, want)
				}
				wantCalls := 1
				if when == "before resolution" {
					wantCalls = 0
				}
				if calls != wantCalls || it.Note != "" {
					t.Fatalf("calls=%d note=%q; want %d calls and no stale note", calls, it.Note, wantCalls)
				}
			})
		})
	}
}

func TestResolveRetriesBeforeDownloadAndWhileResuming(t *testing.T) {
	payload := randomPayload(t, 100_000)
	origin := &droppingServer{payload: payload, chunks: []int{30_000, len(payload)}}
	srv := httptest.NewServer(origin.handler())
	defer srv.Close()
	m := progressManager(t, 1)
	var calls int
	it := &Item{ID: newID(), Name: "sample.bin", Size: -1,
		resolve: func(context.Context) (*extractor.Target, error) {
			calls++
			if calls == 1 || calls == 3 {
				return nil, resolveRequestError(resolveHandshakeTimeout{})
			}
			return &extractor.Target{URL: srv.URL + "/sample.bin"}, nil
		},
	}
	if err := m.transfer(t.Context(), it); err != nil {
		t.Fatalf("temporary signing failure stopped the transfer: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(m.DownloadDir(), "sample.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("resumed file differs from the source: %v", err)
	}
	// The final request confirms the end of a response whose total was unknown.
	if calls != 5 || origin.hits.Load() != 3 {
		t.Fatalf("resolutions=%d transfers=%d; want 5 resolutions and 3 transfers", calls, origin.hits.Load())
	}
}
