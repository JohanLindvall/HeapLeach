// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"log/slog"
	"net/http"
)

type loggerKey struct{}

// WithLogger attaches the caller's job and stage to ctx. It is the one
// logger key: the extractor package keeps its own diagnostics under it too
// (extractor.WithLogger), so a job's extraction log carries a debug line for
// each response it read. Bodies, cookies and signed paths are not included
// in these records.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, log)
}

// Logger returns the logger WithLogger attached to ctx, or nil.
func Logger(ctx context.Context) *slog.Logger {
	log, _ := ctx.Value(loggerKey{}).(*slog.Logger)
	return log
}

func logResponse(resp *http.Response) {
	ctx := resp.Request.Context()
	log := Logger(ctx)
	if log == nil || !log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	log.DebugContext(ctx, "http: response",
		"host", resp.Request.URL.Host, "status", resp.StatusCode,
		"content_type", resp.Header.Get(HeaderContentType), "content_length", resp.ContentLength,
		"server", resp.Header.Get("Server"), "cf_mitigated", resp.Header.Get(HeaderCFMitigated),
		"cf_ray", resp.Header.Get("CF-Ray"))
}
