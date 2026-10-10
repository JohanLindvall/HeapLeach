// SPDX-License-Identifier: MIT

package extractor

import "context"

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
