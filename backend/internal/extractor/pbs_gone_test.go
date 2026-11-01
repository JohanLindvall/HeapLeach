// SPDX-License-Identifier: MIT

package extractor

import (
	"strings"
	"testing"
)

// A player or video link to an expired or unknown title answers with PBS's
// error page, which is PBS's to explain: it is recognised, and the job fails
// in PBS's words rather than as a page that is not a file.
func TestPBSRecognisesItsOwnErrorPage(t *testing.T) {
	p := NewPBS(nil)
	expectRecognised(t, p, htmlPage(t, "https://player.example.test/portalplayer/3100000001/", pbsErrorPage), true)
	expectRecognised(t, p, htmlPage(t, "https://www.example.test/video/a-title-abc123/", pbsErrorPage), true)
	// The paragraph without the site's name is somebody else's error.
	other := strings.Replace(pbsErrorPage, "| PBS", "| Elsewhere", 1)
	expectRecognised(t, p, htmlPage(t, "https://player.example.test/portalplayer/3100000001/", other), false)

	_, err := pbsBridgeOf(pbsErrorPage)
	if err == nil || !strings.Contains(err.Error(), "this video is not available") {
		t.Errorf("err = %v, want PBS's own sentence", err)
	}
}
