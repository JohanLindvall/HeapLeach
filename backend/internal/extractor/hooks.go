// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// What the caller hands an extraction on its context: its job's logger, a
// way to show a resolver's progress, and the queue's challenge recovery.
// Each reaches nested and concurrent extraction the same way, without the
// extractor package owning what lies behind it.

// WithLogger carries the caller's log level, output and job fields through
// concurrent and nested extraction without changing the process-wide logger.
// It shares httpx's key, so the requests an extraction makes log the
// responses that went wrong (at debug) to the same job.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return httpx.WithLogger(ctx, log)
}

func extractionLogger(ctx context.Context) *slog.Logger {
	if log := httpx.Logger(ctx); log != nil {
		return log
	}
	return slog.Default()
}

type resolveNoteKey struct{}

// WithResolveNote lets a downloader show a resolver's progress while a host
// asks it to wait. The callback runs synchronously during Resolve.
func WithResolveNote(ctx context.Context, note func(string)) context.Context {
	return context.WithValue(ctx, resolveNoteKey{}, note)
}

// ResolveNote reports the current stage to the installed callback. Recovery
// can wrap that callback to include the proxy connection attempt.
func ResolveNote(ctx context.Context, text string) {
	if note, ok := ctx.Value(resolveNoteKey{}).(func(string)); ok && note != nil {
		note(text)
	}
}

// ChallengeRecovery lets the queue retry a complete extraction on another
// route after a challenge or an opted-in temporary failure. Keeping the
// hook on the context also reaches nested source links.
// The extractor package does not own proxy clients, leases or their budget.
type ChallengeRecovery func(context.Context, func(context.Context) (*Result, error), error) (*Result, error)

type challengeRecoveryKey struct{}

func WithChallengeRecovery(ctx context.Context, recover ChallengeRecovery) context.Context {
	return context.WithValue(ctx, challengeRecoveryKey{}, recover)
}

func refreshFile(read func(context.Context) (*Result, error), original File, single bool) func(context.Context) (*Target, error) {
	return func(ctx context.Context) (*Target, error) {
		res, err := read(ctx)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, fmt.Errorf("source no longer contains %q", original.Name)
		}
		var found *File
		for i := range res.Files {
			f := &res.Files[i]
			if (single && len(res.Files) == 1) ||
				(util.Unescape(f.Name) == original.Name && util.Unescape(f.Dir) == original.Dir) {
				if found != nil {
					return nil, fmt.Errorf("source now has multiple files named %q", original.Name)
				}
				found = f
			}
		}
		if found == nil {
			return nil, fmt.Errorf("source no longer contains %q", original.Name)
		}
		if found.External != "" {
			return nil, fmt.Errorf("WAF recovery requires native HTTP downloads; this source uses an external downloader")
		}
		if found.Resolve != nil {
			return found.Resolve(ctx)
		}
		link, size := found.URL, found.Size
		if link == "" && len(found.Segments) > 0 {
			link = found.Segments[0]
		}
		if found.SizeApprox {
			size = -1
		}
		return &Target{URL: link, Name: util.Unescape(found.Name), Headers: found.Headers,
			Size: size, Segments: found.Segments, SegmentKey: found.SegmentKey}, nil
	}
}
