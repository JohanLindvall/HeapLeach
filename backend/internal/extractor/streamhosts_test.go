package extractor

import (
	"net/url"
	"strings"
	"testing"
)

// TestStreamtapeAssemble covers the one thing that actually breaks about
// this host: the link is delivered in two halves, and how much of the second
// half is padding is decided by the page, not by us.
func TestStreamtapeAssemble(t *testing.T) {
	const page = "https://streamtape.example.test/e/SYNTHETIC"
	doc := `<div id="robotlink" style="display:none;">/get_video?id=1&amp;token=abc</div>
<script>document.getElementById('robotlink').innerHTML = ` +
		`'//streamtape.example.test/get_video?id=1&token=abc' + ('xxxdef456').substring(3);</script>`

	got, err := streamtapeAssemble(doc, page)
	if err != nil {
		t.Fatalf("streamtapeAssemble: %v", err)
	}
	const want = "https://streamtape.example.test/get_video?id=1&token=abcdef456&stream=1"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestStreamtapeAssembleFollowsTheOffsetOnThePage is the same fixture with a
// different offset, which is exactly the change the host makes when it wants
// to break scrapers that hard-coded one.
func TestStreamtapeAssembleFollowsTheOffsetOnThePage(t *testing.T) {
	const page = "https://streamtape.example.test/e/SYNTHETIC"
	doc := `<script>document.getElementById('robotlink').innerHTML = ` +
		`'//streamtape.example.test/get_video?id=1&token=abc' + ('xxxxxxdef456').substring(6);</script>`

	got, err := streamtapeAssemble(doc, page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSuffix(got, "&stream=1"), "abcdef456") {
		t.Errorf("got %s, want the tail joined from offset 6", got)
	}
}

func TestStreamtapeAssembleReportsAMissingLink(t *testing.T) {
	_, err := streamtapeAssemble("<html><body>File not found</body></html>",
		"https://streamtape.example.test/e/SYNTHETIC")
	if err == nil {
		t.Fatal("a page with no link was accepted")
	}
	if !strings.Contains(err.Error(), "streamtape") {
		t.Errorf("error %q does not name the host", err)
	}
}

// TestUnpackJS covers the p,a,c,k,e,d packer these players ship instead of
// readable source. The fixture is hand-built: the payload's tokens are
// indices into the word list, written in the base the call states.
func TestUnpackJS(t *testing.T) {
	const packed = `eval(function(p,a,c,k,e,d){while(c--)if(k[c])` +
		`p=p.replace(new RegExp('\\b'+c.toString(a)+'\\b','g'),k[c]);return p}` +
		`('0.1="2://4.5/6.7"',36,8,'MDCore|wurl|https|unused|example|test|clip|mp4'.split('|'),0,{}))`

	got, ok := unpackJS(packed)
	if !ok {
		t.Fatal("a packed script was not recognised")
	}
	const want = `MDCore.wurl="https://example.test/clip.mp4"`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestUnpackJSLeavesUnpackedScriptsAlone(t *testing.T) {
	if _, ok := unpackJS(`<script>var MDCore = {}; MDCore.wurl = "//example.test/clip.mp4";</script>`); ok {
		t.Error("an ordinary script was taken for a packed one")
	}
}

// TestPackerIndex covers the alphabet, whose third range only appears once a
// word list outgrows base 36 — the case that is easy to get wrong and rare
// enough not to notice.
func TestPackerIndex(t *testing.T) {
	tests := []struct {
		word  string
		base  int
		want  int
		valid bool
	}{
		{word: "0", base: 36, want: 0, valid: true},
		{word: "9", base: 36, want: 9, valid: true},
		{word: "a", base: 36, want: 10, valid: true},
		{word: "z", base: 36, want: 35, valid: true},
		{word: "10", base: 36, want: 36, valid: true},
		{word: "A", base: 62, want: 36, valid: true},
		{word: "Z", base: 62, want: 61, valid: true},
		{word: "z", base: 20, valid: false}, // a digit the base does not have
		{word: "-", base: 36, valid: false}, // not a digit at all
		{word: "", base: 36, valid: false},
	}
	for _, tc := range tests {
		got, ok := packerIndex(tc.word, tc.base)
		if ok != tc.valid || (ok && got != tc.want) {
			t.Errorf("packerIndex(%q, %d) = %d, %v; want %d, %v",
				tc.word, tc.base, got, ok, tc.want, tc.valid)
		}
	}
}

func TestStreamHostMatching(t *testing.T) {
	tests := []struct {
		host  string
		match func(*url.URL) bool
		want  bool
	}{
		{host: "streamtape.com", match: NewStreamtape(nil).Match, want: true},
		{host: "streamtape.to", match: NewStreamtape(nil).Match, want: true},
		{host: "streamtape.example.test", match: NewStreamtape(nil).Match, want: false},
		{host: "dood.to", match: NewDoodStream(nil).Match, want: true},
		{host: "d0o0d.com", match: NewDoodStream(nil).Match, want: true},
		{host: "mixdrop.co", match: NewMixDrop(nil).Match, want: true},
		{host: "www.mixdrop.ag", match: NewMixDrop(nil).Match, want: true},
		{host: "mixdrop.example.test", match: NewMixDrop(nil).Match, want: false},
	}
	for _, tc := range tests {
		u := &url.URL{Scheme: "https", Host: tc.host, Path: "/e/x"}
		if got := tc.match(u); got != tc.want {
			t.Errorf("%s matched = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestRandomTokenLength(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		token := randomToken(doodTailLength)
		if len(token) != doodTailLength {
			t.Fatalf("token %q is %d characters, want %d", token, len(token), doodTailLength)
		}
		seen[token] = true
	}
	if len(seen) < 90 {
		t.Errorf("only %d of 100 tokens were distinct", len(seen))
	}
}
