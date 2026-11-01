// SPDX-License-Identifier: MIT

package extractor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// A feed in a legacy charset is read as far as a UTF-8 one: past the
// response limit, up to the feed's own.
func TestFeedLongLegacyCharset(t *testing.T) {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss version=\"2.0\"><channel><title>Caf\xe9 Radio</title>")
	pad := strings.Repeat("x", 4096)
	n := 0
	for b.Len() <= config.MaxResponseBytes+(1<<20) {
		fmt.Fprintf(&b, "<item><title>Episode %d</title><description>%s</description>"+
			`<enclosure url="https://media.example.test/e/%d.mp3" length="9" type="audio/mpeg"/></item>`, n, pad, n)
		n++
	}
	b.WriteString("</channel></rss>")

	res, err := feedResult([]byte(b.String()), feedTestURL(t), config.MaxListingFiles)
	if err != nil {
		t.Fatalf("feedResult: %v", err)
	}
	if len(res.Files) != n {
		t.Errorf("%d files, want all %d episodes", len(res.Files), n)
	}
}
