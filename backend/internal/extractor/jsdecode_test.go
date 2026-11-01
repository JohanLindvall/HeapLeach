// SPDX-License-Identifier: MIT

package extractor

import (
	"strings"
	"testing"
)

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

// A payload word long enough to overflow the index accumulator — a hex token
// inside a URL is the ordinary case — must pass through verbatim rather than
// wrap negative and panic the word lookup.
func TestUnpackJSSurvivesLongPayloadWords(t *testing.T) {
	const packed = `eval(function(p,a,c,k,e,d){}` +
		`('0="deadbeefdeadbeefdeadbeefdeadbeef"',36,2,'MDCore|unused'.split('|'),0,{}))`

	got, ok := unpackJS(packed)
	if !ok {
		t.Fatal("a packed script was not recognised")
	}
	if !strings.Contains(got, "deadbeefdeadbeefdeadbeefdeadbeef") {
		t.Errorf("the long token did not survive: %s", got)
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
		// A long run of base digits is an ordinary word — a hex token in a
		// URL, say — not an index. Unbounded it overflows the accumulator,
		// and a wrapped-negative value would panic the caller's word lookup.
		{word: "deadbeefdeadbeefdeadbeefdeadbeef", base: 36, valid: false},
		{word: strings.Repeat("z", 64), base: 62, valid: false},
	}
	for _, tc := range tests {
		got, ok := packerIndex(tc.word, tc.base)
		if ok != tc.valid || (ok && got != tc.want) {
			t.Errorf("packerIndex(%q, %d) = %d, %v; want %d, %v",
				tc.word, tc.base, got, ok, tc.want, tc.valid)
		}
	}
}
