// SPDX-License-Identifier: MIT

package httpx

import (
	"errors"
	"fmt"
	"io"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// ErrTooLarge is a buffered body longer than config.MaxResponseBytes.
var ErrTooLarge = fmt.Errorf("response exceeds %d bytes", config.MaxResponseBytes)

// ReadBody reads a body to be held in memory whole: a page, a JSON
// document. It reads one byte past config.MaxResponseBytes so that a body
// exactly at the limit is complete while a longer one is known to be cut.
//
// The error is the read's own, or ErrTooLarge for a body over the limit;
// either way what was read is returned too — at most the limit — for a
// caller that keeps a refusal's text or sniffs a prefix. A caller must not
// treat that prefix as the document: every path that reuses a body as the
// page it answered (Client.Bytes, the registry's probe, the platform and
// player probes) refuses it on any error.
func ReadBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, config.MaxResponseBytes+1))
	if err == nil && len(body) > config.MaxResponseBytes {
		return body[:config.MaxResponseBytes], ErrTooLarge
	}
	return body, err
}

// tooLarge is ErrTooLarge in the wording a fetch reports, naming the page.
func tooLarge(page string) error {
	return fmt.Errorf("%s: %w", page, ErrTooLarge)
}

// IsTooLarge reports whether err is a body that ran past the limit.
func IsTooLarge(err error) bool { return errors.Is(err, ErrTooLarge) }
