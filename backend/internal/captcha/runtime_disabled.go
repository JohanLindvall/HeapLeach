// SPDX-License-Identifier: MIT

//go:build !cgo

package captcha

import "errors"

// Available explains why a deliberately cgo-free source build cannot read
// challenges. Official builds enable cgo and include the OCR assets.
func Available() error {
	return errors.New("captcha: this build has no OCR support; use an official release or rebuild with CGO_ENABLED=1")
}

func newEngine() (engine, error) { return nil, Available() }
