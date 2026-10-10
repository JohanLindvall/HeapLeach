// SPDX-License-Identifier: MIT

package httpx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// ChallengeError distinguishes a Cloudflare challenge from a missing or
// forbidden file. Retrying on the same connection cannot solve the challenge.
type ChallengeError struct{ Err *StatusError }

func (e *ChallengeError) Error() string { return e.Err.URL + ": Cloudflare challenge required" }
func (e *ChallengeError) Unwrap() error { return e.Err }

type challengeCaptureKey struct{}
type capturedChallenge struct{ err error }
type pageChallengeKey struct{}

// CaptureChallenges keeps a challenge visible even when an extractor treats
// a failed optional probe as "not this platform". Each extraction has its
// own capture; nested sources can recover independently.
func CaptureChallenges(ctx context.Context) (context.Context, func() error) {
	var first atomic.Pointer[capturedChallenge]
	return context.WithValue(ctx, challengeCaptureKey{}, &first), func() error {
		if got := first.Load(); got != nil {
			return got.err
		}
		return nil
	}
}

// Cloudflare documents this header for every interstitial challenge type:
// https://developers.cloudflare.com/cloudflare-challenges/challenge-types/challenge-pages/detect-response/
// Check before reading the body or classifying the status, including a 200
// carrying challenge HTML where media should have been.
func challengeResponse(resp *http.Response, err error) (*http.Response, error) {
	if err != nil {
		return resp, err
	}
	challenged := strings.EqualFold(strings.TrimSpace(resp.Header.Get(HeaderCFMitigated)), "challenge")
	// Some hosts put Turnstile in their own player page, which has neither
	// the interstitial header nor an error status. Only an explicit page
	// fetch can inspect that host's gate. Do it before route accounting so
	// the HTML is not first recorded as a successful download.
	if check, _ := resp.Request.Context().Value(pageChallengeKey{}).(func(string) bool); !challenged && check != nil {
		kind, _, _ := mime.ParseMediaType(resp.Header.Get(HeaderContentType))
		if kind == "text/html" || kind == "application/xhtml+xml" {
			body, err := io.ReadAll(io.LimitReader(resp.Body, config.MaxResponseBytes+1))
			resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("%s: read page: %w", resp.Request.URL.Redacted(), err)
			}
			if len(body) > config.MaxResponseBytes {
				return nil, fmt.Errorf("%s: response exceeds %d bytes", resp.Request.URL.Redacted(), config.MaxResponseBytes)
			}
			challenged = check(string(body))
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
	}
	if challenged {
		resp.Body.Close()
		return nil, &ChallengeError{Err: &StatusError{
			Code: resp.StatusCode, Status: resp.Status, URL: resp.Request.URL.Redacted(),
		}}
	}
	return resp, nil
}
