// SPDX-License-Identifier: MIT

package extractor

import (
	"context"

	"github.com/JohanLindvall/HeapLeach/internal/captcha"
)

var errKeep2ShareOCR = captcha.ErrUnreadable

// Check the runtime and model before requesting a challenge. The shared reader
// owns concurrency, session reuse, inference cancellation and ranked decoding.
func keep2ShareOCR() (func(context.Context, []byte) ([]string, error), error) {
	if err := captcha.Available(); err != nil {
		return nil, err
	}
	return func(ctx context.Context, raw []byte) ([]string, error) {
		return captcha.Read(ctx, raw, func(note string) { ResolveNote(ctx, note) })
	}, nil
}
