// SPDX-License-Identifier: MIT

package extractor

import "net/url"

// TODO(generic-dispatch): transitional helpers for tests still written
// against host matching. Removed with legacyMatcher.

// Find is Known without the flag, for tests written before identification.
func (r *Registry) Find(u *url.URL) Extractor {
	ex, _ := r.Known(u)
	return ex
}

func legacyMatch(e Extractor, u *url.URL) bool { return shapeOf(e, u) }
