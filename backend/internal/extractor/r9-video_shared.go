// SPDX-License-Identifier: MIT

package extractor

import "context"

// readListing resolves a listing's items into their files, in the
// listing's order, with the note counting the items refused beside others
// that resolved. When none resolves, the error is the first reason why
// (noneRead) — or, once the caller has cancelled, that.
//
// For the integrator: it belongs in resolve.go beside resolveFiles, and
// the same shape is written out by hand in bandcamp, coomerfans, imgur and
// the other listings that call noneRead after resolveFiles.
func readListing[T any](ctx context.Context, site, noun, where string, items []T,
	resolve func(context.Context, T) ([]File, error)) ([]File, string, error) {
	files, refusals, failed := resolveFiles(ctx, items, resolve)
	if len(files) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		return nil, "", noneRead(site, len(items), noun, where, refusals, failed)
	}
	return files, refusedNote(refusals), nil
}
