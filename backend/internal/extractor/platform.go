// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// platformJSON is an optional identification request, not an extraction.
// Guesses get one bounded attempt, including on 429. The caller must verify
// the platform's response shape before handing it to an extractor; a 200 or
// valid JSON alone says nothing. Once identified, extraction uses its normal
// retries, pacing and error handling.
func platformJSON(ctx context.Context, client *httpx.Client, endpoint string, headers httpx.Header, out any) bool {
	ctx, cancel := context.WithTimeout(ctx, config.PlatformProbeTimeout)
	defer cancel()
	req, err := client.NewRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	req.Header.Set(httpx.HeaderAccept, httpx.ContentTypeJSON)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.DoOnce(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get(httpx.HeaderContentType))
	if mediaType != "" && mediaType != httpx.ContentTypeJSON && !strings.HasSuffix(mediaType, "+json") &&
		mediaType != "text/plain" && mediaType != "text/css" {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, config.MaxResponseBytes+1))
	return err == nil && len(body) <= config.MaxResponseBytes && json.Unmarshal(body, out) == nil
}

// pageJSON is platformJSON asked once per probed page: every recogniser,
// and the extractor after it, that puts the same question to the same
// endpoint shares one answer, keyed by the endpoint. The raw answer is kept
// rather than a decoded one, so each caller may read it into its own shape.
// It reports false when the endpoint gave no JSON.
func pageJSON(ctx context.Context, p *Page, client *httpx.Client, endpoint string, headers httpx.Header, out any) bool {
	if client == nil || p == nil {
		return false
	}
	raw := memo(p, "json "+endpoint, func() json.RawMessage {
		var raw json.RawMessage
		if !platformJSON(ctx, client, endpoint, headers, &raw) {
			return nil
		}
		return raw
	})
	return len(raw) > 0 && json.Unmarshal(raw, out) == nil
}
