// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// ChallengeRecovery lets the queue retry a complete extraction on another
// route. Keeping the hook on the context also reaches nested source links.
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
