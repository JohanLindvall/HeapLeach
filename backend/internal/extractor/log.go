// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"log/slog"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// WithLogger carries the caller's log level, output and job fields through
// concurrent and nested extraction without changing the process-wide logger.
// It shares httpx's key, so the requests an extraction makes log their
// responses (at debug) to the same job.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return httpx.WithLogger(ctx, log)
}

func extractionLogger(ctx context.Context) *slog.Logger {
	if log := httpx.Logger(ctx); log != nil {
		return log
	}
	return slog.Default()
}
