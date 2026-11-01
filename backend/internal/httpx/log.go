// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"log/slog"
	"net/http"
)

type loggerKey struct{}

// attachedLog is a context's logger and how much of the traffic it records.
type attachedLog struct {
	log *slog.Logger
	// all records every response; otherwise only those that went wrong —
	// a refusal, an error status or a challenge — which is what a job's log
	// is read for. A listing reads hundreds of pages, and the Logs panel
	// holds a thousand lines across every job.
	all bool
}

// WithLogger attaches the caller's job and stage to ctx. It is the one
// logger key: the extractor package keeps its own diagnostics under it too
// (extractor.WithLogger), so a job's extraction log carries a debug line for
// each response that went wrong. Bodies, cookies and signed paths are not
// included in these records.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, attachedLog{log: log})
}

// WithResponseLog is WithLogger recording every response, for a stage being
// diagnosed request by request.
func WithResponseLog(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, attachedLog{log: log, all: true})
}

// Logger returns the logger WithLogger attached to ctx, or nil.
func Logger(ctx context.Context) *slog.Logger {
	a, _ := ctx.Value(loggerKey{}).(attachedLog)
	return a.log
}

func logResponse(resp *http.Response) {
	ctx := resp.Request.Context()
	a, _ := ctx.Value(loggerKey{}).(attachedLog)
	if a.log == nil || !a.log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	mitigated := resp.Header.Get(HeaderCFMitigated)
	if !a.all && resp.StatusCode < 400 && mitigated == "" {
		return
	}
	a.log.DebugContext(ctx, "http: response",
		"host", resp.Request.URL.Host, "status", resp.StatusCode,
		"content_type", resp.Header.Get(HeaderContentType), "content_length", resp.ContentLength,
		"server", resp.Header.Get("Server"), "cf_mitigated", mitigated,
		"cf_ray", resp.Header.Get("CF-Ray"))
}
