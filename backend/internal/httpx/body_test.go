// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

func TestReadBodyBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		n       int
		tooBig  bool
		wantLen int
	}{
		{"under", config.MaxResponseBytes - 1, false, config.MaxResponseBytes - 1},
		{"exact", config.MaxResponseBytes, false, config.MaxResponseBytes},
		{"over", config.MaxResponseBytes + 1, true, config.MaxResponseBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := ReadBody(strings.NewReader(strings.Repeat("x", tc.n)))
			if IsTooLarge(err) != tc.tooBig || (!tc.tooBig && err != nil) || len(body) != tc.wantLen {
				t.Fatalf("len=%d err=%v", len(body), err)
			}
		})
	}
	failure := errors.New("connection reset")
	body, err := ReadBody(io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(failure)))
	if !errors.Is(err, failure) || string(body) != "partial" {
		t.Fatalf("interrupted: body=%q err=%v", body, err)
	}
}

// Fetch refuses a body past the limit and accepts one exactly at it.
func TestFetchRefusesOversizedBodies(t *testing.T) {
	for _, tc := range []struct {
		n  int
		ok bool
	}{{config.MaxResponseBytes, true}, {config.MaxResponseBytes + 64, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, strings.Repeat("x", tc.n))
		}))
		body, _, err := New("test", "en", 0, 0).GetPage(context.Background(), srv.URL, nil, nil)
		srv.Close()
		if tc.ok != (err == nil) || (tc.ok && len(body) != tc.n) || (!tc.ok && !IsTooLarge(err)) {
			t.Errorf("n=%d len=%d err=%v", tc.n, len(body), err)
		}
	}
}
