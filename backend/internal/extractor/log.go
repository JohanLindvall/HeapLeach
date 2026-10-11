// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"log/slog"
)

type loggerKey struct{}

// WithLogger carries the caller's log level, output and job fields through
// concurrent and nested extraction without changing the process-wide logger.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, log)
}

func extractionLogger(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && log != nil {
		return log
	}
	return slog.Default()
}
