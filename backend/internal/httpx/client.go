// Package httpx provides the shared outbound HTTP client: browser-like
// default headers, redirect following that keeps those headers across hops,
// and retry with backoff that honours Retry-After.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Client is a retrying HTTP client shared by every extractor and downloader.
// It is safe for concurrent use.
type Client struct {
	hc         *http.Client
	ua         string
	acceptLang string
	maxRetries int

	// The intervals a rate limit is waited out with, kept as fields so a
	// test need not sit through the production values.
	rateLimitBase time.Duration
	rateLimitMax  time.Duration
}

// New builds a Client. timeout bounds a single non-streaming request; the
// downloader clears it for the body transfer and relies on the context.
func New(userAgent, acceptLanguage string, maxRetries int, timeout time.Duration) *Client {
	jar, _ := cookiejar.New(nil)
	standard := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// A browser-shaped handshake by default; HEAPLEACH_UTLS=0 turns it off if
	// it ever causes trouble, since the standard transport still works.
	var transport http.RoundTripper = standard
	if config.LookupEnv("UTLS") != "0" {
		transport = newBrowserTransport(standard)
	}

	return &Client{
		hc: &http.Client{
			Jar:           jar,
			Timeout:       timeout,
			Transport:     transport,
			CheckRedirect: checkRedirect,
		},
		ua:            userAgent,
		acceptLang:    acceptLanguage,
		maxRetries:    maxRetries,
		rateLimitBase: config.RateLimitRetryBase,
		rateLimitMax:  config.RateLimitRetryMax,
	}
}

// Streaming returns a shallow copy with no client-level timeout, for response
// bodies that are read over minutes. Cancellation comes from the context.
func (c *Client) Streaming() *Client {
	hc := *c.hc
	hc.Timeout = 0
	cp := *c
	cp.hc = &hc
	return &cp
}

// UserAgent is the UA sent on every request. Gofile mixes it into its request
// signature, so callers that sign must use exactly this string.
func (c *Client) UserAgent() string { return c.ua }

// checkRedirect bounds the redirect chain and re-applies the browser-ish
// headers. Go already copies non-sensitive headers across hops and manages
// Referer itself, so this only fills gaps rather than overriding.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= config.MaxRedirects {
		return fmt.Errorf("stopped after %d redirects", config.MaxRedirects)
	}
	prev := via[len(via)-1]
	for _, h := range []string{HeaderUserAgent, HeaderAccept, HeaderAcceptLanguage, HeaderReferer} {
		if req.Header.Get(h) == "" {
			if v := prev.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
	}
	return nil
}

// NewRequest builds a request carrying the default browser headers. A non-nil
// body is buffered so retries and redirects can replay it.
func (c *Client) NewRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		buf := body
		req.ContentLength = int64(len(buf))
		req.GetBody = func() (io.ReadCloser, error) {
			// NoBody, not a reader over zero bytes: the transport treats a
			// plain empty reader as a length it does not know and switches
			// to chunked encoding, so an empty POST would go out with no
			// Content-Length — which picky endpoints refuse.
			if len(buf) == 0 {
				return http.NoBody, nil
			}
			return io.NopCloser(bytes.NewReader(buf)), nil
		}
	}
	req.Header.Set(HeaderUserAgent, c.ua)
	req.Header.Set(HeaderAcceptLanguage, c.acceptLang)
	req.Header.Set(HeaderAccept, AcceptAny)
	req.Header.Set(HeaderSecFetchDest, "empty")
	req.Header.Set(HeaderSecFetchMode, "cors")
	req.Header.Set(HeaderSecFetchSite, "same-origin")
	return req, nil
}

// StatusError reports a non-2xx response.
type StatusError struct {
	Code   int
	Status string
	URL    string
	Body   string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("%s: %s (%s)", e.URL, e.Status, util.Truncate(e.Body, 200))
	}
	return fmt.Sprintf("%s: %s", e.URL, e.Status)
}

// HasStatus reports whether err is, or wraps, a StatusError carrying one of
// the given codes — the typed alternative to grepping an error's text for a
// number.
func HasStatus(err error, codes ...int) bool {
	se, ok := errors.AsType[*StatusError](err)
	return ok && slices.Contains(codes, se.Code)
}

// Do executes req, retrying transient failures with exponential backoff.
// The returned response body is still open and belongs to the caller.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	var (
		lastErr error
		// A rate limit keeps a budget of its own. It is not a failure but a
		// request to come back, so it must not spend the retries that exist
		// for things going wrong — a request that waits out a limiter and
		// then meets a dropped connection still has its own left.
		limited int
	)
	for attempt := 0; ; attempt++ {
		attemptReq := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			attemptReq.Body = body
		}

		resp, err := c.hc.Do(attemptReq)
		switch {
		case err != nil:
			// A cancelled context is the user's decision, never a retry.
			if ctxErr := req.Context().Err(); ctxErr != nil {
				return nil, ctxErr
			}
			lastErr = err
		case resp.StatusCode == http.StatusTooManyRequests:
			// The host is counting requests and has had enough of ours.
			// Waited out on the rate limit's own schedule: the ordinary
			// backoff starts at a few hundred milliseconds, which asks
			// again well inside whatever window is being counted and
			// exhausts the budget before the limit has begun to lift.
			// A host that names its own interval is believed, in either
			// direction: it is the authority on the limiter, and waiting
			// longer than it asked serves nobody. The escalating schedule
			// below is for the common case of a 429 that names nothing.
			wait, stated := retryAfter(resp)
			drain(resp)
			lastErr = &StatusError{Code: resp.StatusCode, Status: resp.Status, URL: req.URL.Redacted()}
			if limited >= config.RateLimitRetries {
				return nil, lastErr
			}
			limited++
			if !stated {
				wait = util.Backoff(limited-1, c.rateLimitBase, c.rateLimitMax)
			}
			if err := util.SleepCtx(req.Context(), wait); err != nil {
				return nil, err
			}
			attempt-- // waiting out a limiter is not a failed attempt
			continue
		case retryableStatus(resp.StatusCode):
			wait, _ := retryAfter(resp)
			drain(resp)
			lastErr = &StatusError{Code: resp.StatusCode, Status: resp.Status, URL: req.URL.Redacted()}
			if attempt < c.maxRetries {
				if err := util.SleepCtx(req.Context(), max(wait, retryDelay(attempt))); err != nil {
					return nil, err
				}
				continue
			}
			return nil, lastErr
		default:
			return resp, nil
		}

		if attempt >= c.maxRetries {
			// Not wrapped with the URL: a StatusError already leads with it,
			// and http.Client reports a transport failure as `Get "<url>":
			// EOF`, so a prefix here printed every address twice.
			return nil, lastErr
		}
		if err := util.SleepCtx(req.Context(), retryDelay(attempt)); err != nil {
			return nil, err
		}
	}
}

// DoOnce performs a single attempt with no retrying, for a request whose
// failure is itself the useful answer — an optional extra connection that a
// host turns away should be recognised at once, not after backing off
// through the retry budget first.
func (c *Client) DoOnce(req *http.Request) (*http.Response, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if retryableStatus(resp.StatusCode) {
		drain(resp)
		return nil, &StatusError{Code: resp.StatusCode, Status: resp.Status, URL: req.URL.Redacted()}
	}
	return resp, nil
}

// Bytes performs the request and reads the whole body, failing on non-2xx.
func (c *Client) Bytes(req *http.Request) ([]byte, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, config.MaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%s: read body: %w", req.URL.Redacted(), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &StatusError{
			Code: resp.StatusCode, Status: resp.Status,
			URL: req.URL.Redacted(), Body: string(body),
		}
	}
	return body, nil
}

// JSON performs the request and decodes a JSON body into out.
func (c *Client) JSON(req *http.Request, out any) error {
	body, err := c.Bytes(req)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decode json: %w (body: %s)", req.URL.Redacted(), err, util.Truncate(string(body), 200))
	}
	return nil
}

// GetString fetches a URL as text, applying any extra headers.
func (c *Client) GetString(ctx context.Context, url string, headers Header) (string, error) {
	req, err := c.NewRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(HeaderAccept, AcceptHTML)
	req.Header.Set(HeaderSecFetchDest, "document")
	req.Header.Set(HeaderSecFetchMode, "navigate")
	applyHeaders(req, headers)
	body, err := c.Bytes(req)
	return string(body), err
}

// GetJSON fetches and decodes a JSON document.
func (c *Client) GetJSON(ctx context.Context, url string, headers Header, out any) error {
	req, err := c.NewRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set(HeaderAccept, AcceptJSON)
	applyHeaders(req, headers)
	return c.JSON(req, out)
}

// PostJSON posts a JSON document and decodes the JSON response.
func (c *Client) PostJSON(ctx context.Context, url string, headers Header, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	} else {
		body = []byte{}
	}
	req, err := c.NewRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	req.Header.Set(HeaderContentType, ContentTypeJSON)
	req.Header.Set(HeaderAccept, AcceptJSON)
	applyHeaders(req, headers)
	if out == nil {
		_, err := c.Bytes(req)
		return err
	}
	return c.JSON(req, out)
}

// retryableStatus reports whether a status warrants another attempt.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryAfter reads the Retry-After header in either of its two legal forms,
// and reports whether the host stated one at all.
//
// That second answer is what a rate limit turns on. A host naming an
// interval is the authority on its own limiter and is believed in either
// direction, a shorter one included; a host naming nothing has said only
// "too many", and is left to the escalating schedule. Without the flag the
// two are indistinguishable, since both come out as a zero wait.
func retryAfter(resp *http.Response) (time.Duration, bool) {
	v := strings.TrimSpace(resp.Header.Get(HeaderRetryAfter))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return min(time.Duration(secs)*time.Second, config.MaxRetryAfter), true
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return min(d, config.MaxRetryAfter), true
		}
		// A moment already past says the wait is over rather than absent.
		return 0, true
	}
	return 0, false
}

// retryDelay is the backoff between attempts at a single request.
func retryDelay(attempt int) time.Duration {
	return util.Backoff(attempt, config.RequestRetryBase, config.RequestRetryMax)
}

// applyHeaders sets each entry on the request, overriding client defaults.
func applyHeaders(req *http.Request, headers Header) {
	for k, v := range headers {
		req.Header.Set(k, v)
	}
}

// drain empties and closes a body so the connection returns to the pool.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// IsCanceled reports whether err is the result of a cancelled context.
func IsCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
