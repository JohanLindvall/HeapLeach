// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
)

// ChallengeError distinguishes a Cloudflare challenge from a missing or
// forbidden file. Retrying on the same connection cannot solve the challenge.
type ChallengeError struct{ Err *StatusError }

func (e *ChallengeError) Error() string { return e.Err.URL + ": Cloudflare challenge required" }
func (e *ChallengeError) Unwrap() error { return e.Err }

type challengeCaptureKey struct{}
type capturedChallenge struct{ err error }

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
	if err == nil && strings.EqualFold(strings.TrimSpace(resp.Header.Get(HeaderCFMitigated)), "challenge") {
		resp.Body.Close()
		return nil, &ChallengeError{Err: &StatusError{
			Code: resp.StatusCode, Status: resp.Status, URL: resp.Request.URL.Redacted(),
		}}
	}
	return resp, err
}
