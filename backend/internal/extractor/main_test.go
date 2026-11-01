// SPDX-License-Identifier: MIT

//go:build !live && !corpus_audit

package extractor

import (
	"os"
	"testing"
)

// TestMain keeps the unit tests off the real sites. Extractors built by
// NewRegistry carry their production API bases, and a recogniser may ask
// one about a page its probe could not read (probeWithheld) — which a
// fixture on an unresolvable host always is. Every request but those to the
// loopback test servers is sent to a closed proxy port and fails at once,
// as an unreachable API would, rather than reaching the site unrecorded.
func TestMain(m *testing.M) {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		_ = os.Setenv(name, "http://127.0.0.1:1")
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		_ = os.Setenv(name, "127.0.0.1,localhost,::1")
	}
	os.Exit(m.Run())
}
