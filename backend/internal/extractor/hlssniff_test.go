// SPDX-License-Identifier: MIT

package extractor

import (
	"net/http"
	"testing"
)

// TestHLSSniffableOnlyForBinaryLabels: only a generic binary or segment type
// at an extensionless path is asked for its opening bytes; a picture or a
// counter a page links without an extension is not asked again.
func TestHLSSniffableOnlyForBinaryLabels(t *testing.T) {
	for kind, want := range map[string]bool{
		"application/octet-stream": true, "binary/octet-stream": true, "video/mp2t": true,
		"image/gif": false, "image/jpeg": false, "video/mp4": false,
	} {
		if got := hlsSniffable(fixturePage(t, "https://cdn.example.test/watch/123", http.StatusOK, kind, "")); got != want {
			t.Errorf("%s: sniffable %v, want %v", kind, got, want)
		}
	}
	if hlsSniffable(fixturePage(t, "https://cdn.example.test/a-clip.bin", http.StatusOK, "application/octet-stream", "")) {
		t.Error("a link naming a file was sniffed")
	}
}
