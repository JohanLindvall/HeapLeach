// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// platformJSON is an optional identification request, not an extraction.
// Guesses get a bounded attempt, and one more when the first met a rate
// limit, a server error or no answer (config.PlatformProbeAttempts): a busy
// moment is not the endpoint saying it is not the platform's. The caller
// must verify the platform's response shape before handing it to an
// extractor; a 200 or valid JSON alone says nothing. Once identified,
// extraction uses its normal retries, pacing and error handling.
func platformJSON(ctx context.Context, client *httpx.Client, endpoint string, headers httpx.Header, out any) bool {
	ok, _ := platformJSONAnswer(ctx, client, endpoint, headers, out)
	return ok
}

// platformJSONAnswer is platformJSON reporting also whether the endpoint
// answered (jsonAnswer.answered): a "no" that is the site's own rather than
// the absence of an answer.
func platformJSONAnswer(ctx context.Context, client *httpx.Client, endpoint string, headers httpx.Header, out any) (ok, answered bool) {
	got := platformJSONRead(ctx, client, endpoint, headers)
	return len(got.raw) > 0 && json.Unmarshal(got.raw, out) == nil, got.answered
}

// platformJSONRead also retains a transient failure after the bounded
// attempts. It is not a negative identification: a reader may need its
// normal retry budget after the generic fallback found nothing to read.
func platformJSONRead(ctx context.Context, client *httpx.Client, endpoint string, headers httpx.Header) jsonAnswer {
	for attempt := 0; ; attempt++ {
		var raw json.RawMessage
		ok, again, answered, retry := platformJSONOnce(ctx, client, endpoint, headers, &raw)
		if ok || !again || attempt+1 >= config.PlatformProbeAttempts {
			if ok {
				return jsonAnswer{raw: raw, answered: true}
			}
			return jsonAnswer{answered: answered, retry: retry}
		}
		if util.SleepCtx(ctx, min(httpx.RetryDelay(attempt), config.PlatformProbeRetryWait)) != nil {
			return jsonAnswer{}
		}
	}
}

// platformProbeTimeout is config.PlatformProbeTimeout, a variable so a test
// can shorten it.
var platformProbeTimeout = config.PlatformProbeTimeout

// probeContext is ctx for one bounded identification request: admitted
// first, through ctx's admission, and only then given its deadline, so the
// bound is spent on the network rather than in the queue behind an
// extraction's other requests. A probe that timed out waiting for a slot
// would otherwise be remembered on the shared page as the endpoint's answer.
// done gives the slot back and ends the deadline; ok is false when ctx
// ended while waiting.
func probeContext(ctx context.Context) (_ context.Context, done func(), ok bool) {
	release, err := httpx.Admit(ctx)
	if err != nil {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(httpx.WithoutAdmission(ctx), platformProbeTimeout)
	return ctx, func() { cancel(); release() }, true
}

// platformJSONOnce is one attempt of platformJSON; again reports a failure
// worth asking once more about, and answered one that is the endpoint's
// own answer: a success read whole, or a client error other than a rate
// limit, through no challenge and no route's refusal.
func platformJSONOnce(ctx context.Context, client *httpx.Client, endpoint string, headers httpx.Header, out any) (ok, again, answered bool, retry error) {
	ctx, done, admitted := probeContext(ctx)
	if !admitted {
		return false, false, false, nil
	}
	defer done()
	req, err := client.NewRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, false, false, nil
	}
	req.Header.Set(httpx.HeaderAccept, httpx.ContentTypeJSON)
	headers.Apply(req)
	resp, err := client.DoOnce(req)
	if err != nil {
		_, challenged := errors.AsType[*httpx.ChallengeError](err)
		_, routed := errors.AsType[*httpx.RouteError](err)
		status, isStatus := errors.AsType[*httpx.StatusError](err)
		transient := !isStatus || status.Code == http.StatusTooManyRequests || status.Code >= 500
		refused := isStatus && clientRefusal(status.Code)
		if transient && !challenged && !routed {
			retry = err
		}
		return false, retry != nil && ctx.Err() == nil, refused && !challenged && !routed, retry
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, false, clientRefusal(resp.StatusCode), nil
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get(httpx.HeaderContentType))
	if mediaType != "" && mediaType != httpx.ContentTypeJSON && !strings.HasSuffix(mediaType, "+json") &&
		mediaType != "text/plain" && mediaType != "text/css" {
		return false, false, true, nil
	}
	body, err := httpx.ReadBody(resp.Body)
	if err != nil {
		return false, false, false, nil
	}
	return json.Unmarshal(body, out) == nil, false, true, nil
}

// clientRefusal is a status by which an endpoint says no rather than "not
// now": a 4xx other than a rate limit.
func clientRefusal(code int) bool {
	return code >= 400 && code < 500 && code != http.StatusTooManyRequests
}

// hostLabel names an install by its host's first label, the part that
// distinguishes one install of a platform from another, whether the host is
// a bare domain or a subdomain of a shared one.
func hostLabel(host string) string {
	label, _, _ := strings.Cut(strings.TrimPrefix(strings.ToLower(host), "www."), ".")
	return label
}

// pageJSON is platformJSON asked once per probed page: every recogniser,
// and the extractor after it, that puts the same question to the same
// endpoint shares one answer, keyed by the request: the endpoint and the
// headers that change its answer (jsonKey), on the route asking (memoAsked);
// a client of another identity (httpx.Client.SameIdentity) neither shares
// the page's answers nor stores its own among them. The raw answer is kept
// rather than a decoded one, so each caller may read it into its own shape.
// It reports false when the endpoint gave no JSON.
func pageJSON(ctx context.Context, p *Page, client *httpx.Client, endpoint string, headers httpx.Header, out any) bool {
	ok, _ := pageJSONAnswer(ctx, p, client, endpoint, headers, out)
	return ok
}

// jsonAnswer is what pageJSON keeps of a question: the raw JSON, nil when
// there was none, and whether the endpoint answered at all
// (platformJSONAnswer).
type jsonAnswer struct {
	raw      json.RawMessage
	answered bool
	// retry is the actual transient failure; nil for a definite refusal,
	// invalid document, challenge, route failure or a skipped question.
	retry error
}

// pageJSONAnswer is pageJSON reporting also whether its "no" is the
// endpoint's own answer: false for a check that was skipped or that the
// endpoint did not answer, which a caller remembering verdicts must not
// keep (verdictMemo).
func pageJSONAnswer(ctx context.Context, p *Page, client *httpx.Client, endpoint string, headers httpx.Header, out any) (ok, answered bool) {
	got := pageJSONRead(ctx, p, client, endpoint, headers)
	return len(got.raw) > 0 && json.Unmarshal(got.raw, out) == nil, got.answered
}

// pageJSONRead is the memoized answer, including a transient failure when
// the bounded question ran out of attempts. Identity and route checks are
// the same for successful and failed answers.
func pageJSONRead(ctx context.Context, p *Page, client *httpx.Client, endpoint string, headers httpx.Header) jsonAnswer {
	if client == nil || p == nil || (p.via != nil && !p.via.SameIdentity(client)) {
		return jsonAnswer{}
	}
	return memoAsked(ctx, p, jsonKey(endpoint, headers), func() jsonAnswer {
		return platformJSONRead(ctx, client, endpoint, headers)
	})
}

// askedJSON is pageJSON for an extractor: the answer a recogniser already
// had from the endpoint with the same headers, when one asked. When none did
// it asks nothing and reports false, so the extractor makes its own request,
// with its own retries, rather than a bounded probe ahead of it.
func askedJSON(ctx context.Context, p *Page, client *httpx.Client, endpoint string, headers httpx.Header, out any) bool {
	got, asked := askedJSONRead(ctx, p, client, endpoint, headers)
	return asked && len(got.raw) > 0 && json.Unmarshal(got.raw, out) == nil
}

// askedJSONRead never starts a new question. Deferred recognition uses it
// to inspect a failed acquisition without guessing on a new route or client.
func askedJSONRead(ctx context.Context, p *Page, client *httpx.Client, endpoint string, headers httpx.Header) (jsonAnswer, bool) {
	if p == nil || client == nil || (p.via != nil && !p.via.SameIdentity(client)) {
		return jsonAnswer{}, false
	}
	p.memoMu.Lock()
	_, asked := p.memo[memoKey(ctx, jsonKey(endpoint, headers))]
	p.memoMu.Unlock()
	if !asked {
		return jsonAnswer{}, false
	}
	return pageJSONRead(ctx, p, client, endpoint, headers), true
}

// jsonKey names a JSON question for a page's memo: the endpoint and every
// header asked with but the referer, which is taken not to change an answer
// (Page.answers), sorted so that one request has one name.
func jsonKey(endpoint string, headers httpx.Header) string {
	var parts []string
	for name, value := range headers {
		if !strings.EqualFold(name, httpx.HeaderReferer) {
			parts = append(parts, strings.ToLower(name)+"="+value)
		}
	}
	slices.Sort(parts)
	return "json " + strings.Join(append([]string{endpoint}, parts...), "\n")
}
