// SPDX-License-Identifier: MIT

package extractor

import (
	"strings"
	"testing"
)

func TestKeep2ShareOCRChecksInstallationBeforeRequestingAChallenge(t *testing.T) {
	t.Setenv("HEAPLEACH_OCR_DIR", t.TempDir())
	solver, err := keep2ShareOCR()
	if solver != nil || err == nil || !strings.Contains(err.Error(), "OCR") {
		t.Fatalf("solver=%v, error=%v; want an installation error before requesting a CAPTCHA", solver != nil, err)
	}
}
