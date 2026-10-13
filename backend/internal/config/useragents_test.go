// SPDX-License-Identifier: MIT

package config

import (
	"regexp"
	"testing"
)

// Routes draw from at least twenty browsers, each a User-Agent in the form
// a real desktop Chrome, Edge, Firefox or Safari sends.
func TestRoutesDrawFromAtLeastTwentyRealBrowsers(t *testing.T) {
	real := regexp.MustCompile(`^Mozilla/5\.0 \((Windows NT 10\.0; Win64; x64|Macintosh; Intel Mac OS X 10[._]15(_7)?|X11; (Ubuntu; )?Linux x86_64)(; rv:\d+\.0)?\) ` +
		`(AppleWebKit/537\.36 \(KHTML, like Gecko\) Chrome/\d+\.0\.0\.0 Safari/537\.36( Edg/\d+\.0\.0\.0)?` +
		`|Gecko/20100101 Firefox/\d+\.0` +
		`|AppleWebKit/605\.1\.15 \(KHTML, like Gecko\) Version/\d+\.\d+ Safari/605\.1\.15)$`)
	seen := map[string]bool{}
	for _, agent := range BrowserUserAgents {
		if !real.MatchString(agent) {
			t.Errorf("not a real browser's User-Agent: %q", agent)
		}
		if seen[agent] {
			t.Errorf("listed twice: %q", agent)
		}
		seen[agent] = true
	}
	if len(seen) < 20 {
		t.Fatalf("%d browsers; want at least twenty", len(seen))
	}
}
