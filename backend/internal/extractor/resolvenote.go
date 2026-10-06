// SPDX-License-Identifier: MIT

package extractor

import "context"

type resolveNoteKey struct{}

// WithResolveNote lets a downloader show a resolver's progress while a host
// asks it to wait. The callback runs synchronously during Resolve.
func WithResolveNote(ctx context.Context, note func(string)) context.Context {
	return context.WithValue(ctx, resolveNoteKey{}, note)
}

func resolveNote(ctx context.Context, text string) {
	if note, ok := ctx.Value(resolveNoteKey{}).(func(string)); ok && note != nil {
		note(text)
	}
}
