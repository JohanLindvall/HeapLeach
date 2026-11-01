// SPDX-License-Identifier: MIT

// Package httpx provides the shared outbound HTTP client: browser-like
// default headers, redirect following that keeps those headers across hops,
// and retry with backoff that honours Retry-After.
package httpx

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/util"
	"golang.org/x/net/publicsuffix"
)

// Client is a retrying HTTP client shared by every extractor and downloader.
// It is safe for concurrent use.
type Client struct {
	hc         *http.Client
	ua         string
	acceptLang string
	maxRetries int
	// Only explicit pool routes can attribute connection setup to a proxy.
	proxyScheme string
	// Streaming requests still need a deadline for receiving headers.
	headerTimeout time.Duration
	// quiet leaves out the debug line per response (logResponse).
	quiet bool

	// The intervals a rate limit is waited out with, kept as fields so a
	// test need not sit through the production values.
	rateLimitBase time.Duration
	rateLimitMax  time.Duration
}

// New builds a Client. timeout bounds a single non-streaming request; the
// downloader clears it for the body transfer and relies on the context.
func New(userAgent, acceptLanguage string, maxRetries int, timeout time.Duration) *Client {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	standard := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer(config.DialTimeout).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   config.DialTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		// Verified by chains rather than by crypto/tls alone, so a server
		// that leaves its intermediate out is completed the way a browser
		// completes it. The checks are the same ones; see aia.go.
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection
			VerifyConnection: func(cs tls.ConnectionState) error {
				return chains.verify(cs.ServerName, cs.PeerCertificates)
			},
		},
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

// WithTimeout caps a request's duration, including its response body, while
// retaining a stricter existing timeout. The copy shares transport and
// cookies. A client deadline, rather than a caller context deadline, lets a
// leased route recognise and score a network timeout as its own failure.
func (c *Client) WithTimeout(limit time.Duration) *Client {
	if c == nil || limit <= 0 {
		return c
	}
	hc := *c.hc
	if hc.Timeout <= 0 || hc.Timeout > limit {
		hc.Timeout = limit
	}
	cp := *c
	cp.hc = &hc
	cp.headerTimeout = 0
	return &cp
}

// Streaming returns a shallow copy with no client-level timeout, for response
// bodies that are read over minutes. Cancellation comes from the context.
//
// It logs no line per response either. Its requests are transfers, which
// for a segmented video number in the thousands, and a transfer reports its
// own failures; the per-response lines are for extraction.
func (c *Client) Streaming() *Client {
	hc := *c.hc
	headerTimeout := hc.Timeout
	hc.Timeout = 0
	cp := *c
	cp.hc = &hc
	cp.quiet = true
	if headerTimeout > 0 {
		cp.headerTimeout = headerTimeout
	}
	return &cp
}

// dialer opens connections bounded by timeout, kept alive the same way on
// every path: the standard transport, a proxy route and the browser-shaped
// handshake.
func dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
}

var errHeaderTimeout = errors.New("timed out waiting for response headers")

// do bounds connection setup and response headers without timing out a
// streaming body. The body owns cancellation after the timer is stopped.
func (c *Client) do(req *http.Request) (resp *http.Response, err error) {
	if first, ok := req.Context().Value(challengeCaptureKey{}).(*atomic.Pointer[capturedChallenge]); ok && first != nil {
		if got := first.Load(); got != nil {
			return nil, got.err
		}
	}
	defer func() {
		if _, challenged := errors.AsType[*ChallengeError](err); challenged {
			if first, ok := req.Context().Value(challengeCaptureKey{}).(*atomic.Pointer[capturedChallenge]); ok && first != nil {
				first.CompareAndSwap(nil, &capturedChallenge{err: err})
			}
		}
	}()
	if route, ok := req.Context().Value(routeContextKey{}).(*requestRoute); ok && route != nil {
		return route.do(c, req)
	}
	return c.doDirect(req)
}

func (c *Client) doDirect(req *http.Request) (*http.Response, error) {
	if jar, ok := req.Context().Value(cookieJarContextKey{}).(http.CookieJar); ok {
		cp, hc := *c, *c.hc
		hc.Jar = jar
		cp.hc = &hc
		c = &cp
	}
	if c.headerTimeout <= 0 {
		return c.answered(c.hc.Do(req))
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	timedOut := make(chan struct{})
	timer := time.AfterFunc(c.headerTimeout, func() { cancel(errHeaderTimeout); close(timedOut) })
	resp, err := c.hc.Do(req.Clone(ctx))
	if !timer.Stop() {
		<-timedOut
	}
	if errors.Is(context.Cause(ctx), errHeaderTimeout) {
		if resp != nil {
			resp.Body.Close()
		}
		err = errHeaderTimeout
	}
	if err != nil {
		cancel(nil)
		return nil, err
	}
	// Only successful media bodies are allowed to stream indefinitely.
	// Error bodies are read before the transfer watchdog starts, both here
	// (draining a retryable response) and by the downloader for its message.
	// Headers followed by a silent 403/503 must not pin a worker forever.
	var bodyTimer *time.Timer
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		bodyTimer = time.AfterFunc(c.headerTimeout, func() { cancel(context.DeadlineExceeded) })
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: func() {
		if bodyTimer != nil {
			bodyTimer.Stop()
		}
		cancel(nil)
	}}
	return c.answered(resp, nil)
}

// answered logs a response and checks it for a challenge.
func (c *Client) answered(resp *http.Response, err error) (*http.Response, error) {
	if err == nil && !c.quiet {
		logResponse(resp)
	}
	return challengeResponse(resp, err)
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}

// UserAgent is the UA sent on every request. Gofile mixes it into its request
// signature, so callers that sign must use exactly this string.
func (c *Client) UserAgent() string { return c.ua }

// SameIdentity reports whether c and o ask as the same visitor: the same
// User-Agent and language, and the same cookie jar, which is the session. A
// response to one stands for a response to the other; a copy made by
// WithTimeout or Streaming is the same visitor.
func (c *Client) SameIdentity(o *Client) bool {
	if c == nil || o == nil {
		return c == o
	}
	return c == o || (c.ua == o.ua && c.acceptLang == o.acceptLang && c.hc.Jar == o.hc.Jar)
}

// checkRedirect bounds the redirect chain and re-applies the browser-ish
// headers. Go already copies non-sensitive headers across hops and manages
// Referer itself, so this only fills gaps rather than overriding.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= config.MaxRedirects {
		return fmt.Errorf("stopped after %d redirects", config.MaxRedirects)
	}
	prev := via[len(via)-1]
	// net/http suppresses its generated referrer on a TLS downgrade, but
	// copies an explicitly supplied one. Apply that protection to the
	// extractor's referrer too, and never restore it from an earlier hop.
	if prev.URL.Scheme == "https" && req.URL.Scheme == "http" {
		req.Header.Del(HeaderReferer)
	}
	for _, h := range []string{HeaderUserAgent, HeaderAccept, HeaderAcceptLanguage} {
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
	// Header is the response's headers, kept where a host may say more in
	// them than the status does: mega names the time an address's quota has
	// left in one of its own.
	Header http.Header
	// responseURL is the address that answered, which may be a signed
	// redirect destination. Keep it out of diagnostics and serialization;
	// ResponseURL exposes it only to callers deciding request identity.
	responseURL string
}

// NewStatusError reports resp's status. It names the address the caller
// asked for, without a password, rather than one a redirect led to, which is
// often a signed CDN link; and it keeps the response's headers and the first
// config.ErrorBodySample bytes of its body, which is where hosts tend to say
// why. It reads from the body but leaves closing it to the caller.
func NewStatusError(resp *http.Response) *StatusError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, config.ErrorBodySample))
	asked := resp.Request
	// http.Client links each redirected request to the response that caused it.
	for asked != nil && asked.Response != nil && asked.Response.Request != nil {
		asked = asked.Response.Request
	}
	return statusError(resp, asked, body)
}

// statusError reports resp's status as an answer to req.
func statusError(resp *http.Response, req *http.Request, body []byte) *StatusError {
	se := &StatusError{Code: resp.StatusCode, Status: resp.Status, Body: string(body), Header: resp.Header}
	if req != nil {
		se.URL = req.URL.Redacted()
	}
	if resp.Request != nil && resp.Request.URL != nil {
		se.responseURL = resp.Request.URL.String()
	}
	return se
}

// ResponseURL returns the address that answered a StatusError, or the failed
// destination carried by a transport's url.Error, through error wrappers.
// It returns an empty string when no destination is available. Unlike the
// diagnostic StatusError.URL, this may be a signed redirect URL and must not
// be logged; it is for callers deciding which request failed.
func ResponseURL(err error) string {
	if status, ok := errors.AsType[*StatusError](err); ok && status != nil && status.responseURL != "" {
		return status.responseURL
	}
	if transport, ok := errors.AsType[*url.Error](err); ok && transport != nil {
		return transport.URL
	}
	return ""
}

func (e *StatusError) Error() string {
	if quoted := e.quoted(); quoted != "" {
		return fmt.Sprintf("%s: %s (%s)", e.URL, e.Status, util.Truncate(quoted, 200))
	}
	return fmt.Sprintf("%s: %s", e.URL, e.Status)
}

// quoted is what of the body the message quotes: a plain or JSON body as
// it came, where hosts say why, but of an HTML error page only its title.
// The rest of such a page is a WAF's or a CDN's markup and scripts, which
// read in a job's error as noise; the Body field keeps it all for an
// extractor that parses it. A body labelled HTML that holds no markup at
// all is plain text under a server's default content type, and says why
// as it came.
func (e *StatusError) quoted() string {
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return ""
	}
	kind, _, _ := mime.ParseMediaType(e.Header.Get(HeaderContentType))
	head := strings.ToLower(body[:min(len(body), 512)])
	if kind != "text/html" && kind != "application/xhtml+xml" &&
		!strings.HasPrefix(head, "<!doctype html") && !strings.HasPrefix(head, "<html") && !strings.Contains(head, "<head") {
		return body
	}
	if !strings.Contains(body, "<") {
		return body
	}
	if m := htmlTitle.FindStringSubmatch(body); m != nil {
		return strings.Join(strings.Fields(html.UnescapeString(m[1])), " ")
	}
	return ""
}

// htmlTitle is an HTML page's title element.
var htmlTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

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
	return c.send(req, c.maxRetries, config.RateLimitRetries)
}

// DoOnce performs a single attempt with no retrying, for a request whose
// failure is itself the useful answer — an optional extra connection that a
// host turns away should be recognised at once, not after backing off
// through the retry budget first. A retryable status is returned as a
// StatusError, as Do returns one when its budget is spent.
func (c *Client) DoOnce(req *http.Request) (*http.Response, error) {
	return c.send(req, 0, 0)
}

// send is Do with the budgets given: retries for failed attempts, and
// limits for rate limits waited out.
func (c *Client) send(req *http.Request, retries, limits int) (*http.Response, error) {
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
		if err := rewind(attemptReq); err != nil {
			return nil, err
		}

		// Each attempt is admitted on its own: a slot is held while the
		// request is out and its answer unread, and given back before any
		// wait, so a backoff or a Retry-After holds none (Admission).
		release, err := admit(req.Context())
		if err != nil {
			return nil, err
		}
		resp, err := c.do(attemptReq)
		if err != nil {
			release()
		} else {
			// The body carries the slot: a retryable answer gives it back
			// when drained, below, before the wait; the caller's when it
			// reads the body to its end or closes it.
			resp.Body = &admittedBody{ReadCloser: resp.Body, release: release}
		}
		switch {
		case err != nil:
			// A cancelled context is the user's decision, never a retry.
			if ctxErr := req.Context().Err(); ctxErr != nil {
				return nil, ctxErr
			}
			// A leased route gets one attempt. Its owner returns the file to
			// the queue to try another route, without changing IP mid-ticket.
			if _, routed := errors.AsType[*RouteError](err); routed {
				return nil, err
			}
			if _, challenged := errors.AsType[*ChallengeError](err); challenged {
				return nil, err
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
			lastErr = NewStatusError(resp)
			drain(resp)
			if limited >= limits {
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
			lastErr = NewStatusError(resp)
			drain(resp)
			if attempt < retries {
				if err := util.SleepCtx(req.Context(), max(wait, RetryDelay(attempt))); err != nil {
					return nil, err
				}
				continue
			}
			return nil, lastErr
		default:
			return resp, nil
		}

		if attempt >= retries {
			// Not wrapped with the URL: a StatusError already leads with it,
			// and http.Client reports a transport failure as `Get "<url>":
			// EOF`, so a prefix here printed every address twice.
			return nil, lastErr
		}
		if err := util.SleepCtx(req.Context(), RetryDelay(attempt)); err != nil {
			return nil, err
		}
	}
}

// StatusOnce asks url once, with no retrying, and answers with the status
// alone, the body unread: for a probe of whether a host serves something to
// this address at all. An error is a request that could not be made or got
// no usable answer, which says nothing either way.
func (c *Client) StatusOnce(ctx context.Context, method, url string, h Header) (int, error) {
	req, err := c.request(ctx, method, url, nil, h)
	if err != nil {
		return 0, err
	}
	resp, err := c.DoOnce(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// Bytes performs the request and reads the whole body, failing on non-2xx.
func (c *Client) Bytes(req *http.Request) ([]byte, error) {
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	return responseBytes(resp)
}

func responseBytes(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	page := resp.Request.URL.Redacted()

	body, err := ReadBody(resp.Body)
	if err != nil && !IsTooLarge(err) {
		return nil, fmt.Errorf("%s: read body: %w", page, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The whole of a refusal is kept, not a sample: some hosts answer it
		// with a JSON document their extractor decodes.
		return nil, statusError(resp, resp.Request, body)
	}
	if err != nil {
		return nil, tooLarge(page)
	}
	return body, nil
}

// JSON performs the request and decodes a JSON body into out. A nil out
// only checks the answer's status.
func (c *Client) JSON(req *http.Request, out any) error {
	body, err := c.Bytes(req)
	if err != nil || out == nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decode json: %w (body: %s)", req.URL.Redacted(), err, util.Truncate(string(body), 200))
	}
	return nil
}

// GetString fetches a URL as text, applying any extra headers.
func (c *Client) GetString(ctx context.Context, url string, headers Header) (string, error) {
	body, _, err := c.GetPage(ctx, url, headers, nil)
	return body, err
}

// GetPage fetches a page the way a browser navigating to it would (Navigate,
// then headers over it), and retains the URL after redirects for relative
// links and referrers. An optional host-specific check recognises challenge
// HTML before retries, challenge capture and route scoring, just like the
// cf-mitigated header.
func (c *Client) GetPage(ctx context.Context, rawURL string, headers Header, isChallenge func(string) bool) (string, *url.URL, error) {
	if isChallenge != nil {
		ctx = context.WithValue(ctx, pageChallengeKey{}, isChallenge)
	}
	body, resp, err := c.Fetch(ctx, rawURL, Navigate.Merge(headers))
	if resp == nil {
		return "", nil, err
	}
	return string(body), resp.Request.URL, err
}

// Fetch GETs rawURL with the client's defaults and headers over them,
// retrying as Do does, and reads the whole body. It fails on a non-2xx
// status (a StatusError) and on a body over config.MaxResponseBytes, rather
// than handing back a truncated one. The response comes back with its body
// read and closed, for its headers and for the address that answered after
// redirects (resp.Request.URL); it is nil only where none arrived.
func (c *Client) Fetch(ctx context.Context, rawURL string, headers Header) ([]byte, *http.Response, error) {
	req, err := c.request(ctx, http.MethodGet, rawURL, nil, headers)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	body, err := responseBytes(resp)
	return body, resp, err
}

// GetJSON fetches and decodes a JSON document.
func (c *Client) GetJSON(ctx context.Context, url string, headers Header, out any) error {
	req, err := c.request(ctx, http.MethodGet, url, nil, askJSON, headers)
	if err != nil {
		return err
	}
	return c.JSON(req, out)
}

// PostJSON posts a JSON document, an empty body for a nil one, and decodes
// the JSON response into out unless out is nil.
func (c *Client) PostJSON(ctx context.Context, url string, headers Header, in, out any) error {
	body := []byte{}
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := c.request(ctx, http.MethodPost, url, body, sendJSON, headers)
	if err != nil {
		return err
	}
	return c.JSON(req, out)
}

// The headers a JSON exchange carries under the caller's own.
var (
	askJSON  = Header{HeaderAccept: AcceptJSON}
	sendJSON = Header{HeaderAccept: AcceptJSON, HeaderContentType: ContentTypeJSON}
)

// request is NewRequest with each of headers applied over the defaults, in
// order.
func (c *Client) request(ctx context.Context, method, url string, body []byte, headers ...Header) (*http.Request, error) {
	req, err := c.NewRequest(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	for _, h := range headers {
		h.Apply(req)
	}
	return req, nil
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
		// Clamp before converting to nanoseconds so a large delay cannot
		// overflow into a negative duration and cause an immediate retry.
		return time.Duration(min(secs, int(config.MaxRetryAfter/time.Second))) * time.Second, true
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

// RetryDelay is the wait after a single request's failed try, counted from
// 0. Extractors that retry an answer rather than a request back off by it
// too, so both share one schedule.
func RetryDelay(attempt int) time.Duration {
	return util.Backoff(attempt, config.RequestRetryBase, config.RequestRetryMax)
}

// rewind gives req a fresh copy of its body, for another attempt at it.
func rewind(req *http.Request) error {
	if req.GetBody == nil {
		return nil
	}
	body, err := req.GetBody()
	if err != nil {
		return err
	}
	req.Body = body
	return nil
}

// drain empties and closes a body so the connection returns to the pool.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
