// SPDX-License-Identifier: MIT

package extractor

import (
	"strings"
	"testing"

	"golang.org/x/net/html/atom"
)

// Script payloads are decoded as written: a doubled space, a tab and
// non-ASCII text inside a JSON string survive, where display text
// (textOf) collapses them.
func TestScriptPayloadsKeepTheirSpacing(t *testing.T) {
	root, err := parseHTML(`<html><head>
		<script type="application/ld+json">{"@type":"BreadcrumbList","name":"Home  Page"}</script>
		<script type="application/ld+json">
			{"@type":"VideoObject","name":"Synthetic  Clip\twith Ünïcode — dash"}
		</script>
	</head><body><p>Synthetic   display   text</p></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if got := epornerLinkedName(root); got != "Synthetic  Clip\twith Ünïcode — dash" {
		t.Errorf("JSON-LD name = %q", got)
	}
	if docs := linkedData(root); len(docs) != 2 {
		t.Errorf("linkedData found %d payloads", len(docs))
	}
	if got := firstText(root, atom.P); got != "Synthetic display text" {
		t.Errorf("display text = %q", got)
	}
}

// JSON-LD whose strings carry raw line breaks and tabs, as page templates
// write them, still decodes: the media it names is found, and the name
// keeps the characters, escaped. Whitespace between tokens is untouched,
// and an escaped quote or backslash does not end a string early.
func TestLinkedDataToleratesRawControlCharacters(t *testing.T) {
	for _, ctl := range []string{"\n", "\t", "\r\n", "\x01"} {
		root, err := parseHTML("<html><head><script type=\"application/ld+json\">\n\t{\"@type\":\"VideoObject\"," +
			"\"name\":\"First" + ctl + "Clip \\\"quoted\\\\\\\" \",\n\t\"contentUrl\":\"/media/clip.mp4\"," +
			"\"description\":\"line one" + ctl + "line two\"}</script></head><body></body></html>")
		if err != nil {
			t.Fatal(err)
		}
		found, ok := mediaPageLinkedData(root, mustParse(t, "https://example.test/watch/clip"))
		if !ok || found.URL != "https://example.test/media/clip.mp4" {
			t.Fatalf("%q: media not found: %+v", ctl, found)
		}
		// The parser reads a CRLF as a line feed, and the reader trims.
		if want := "First" + strings.ReplaceAll(ctl, "\r\n", "\n") + "Clip \"quoted\\\""; found.Title != want {
			t.Errorf("%q: title = %q, want %q", ctl, found.Title, want)
		}
	}
	if got := jsonControlsEscaped("{\n\t\"a\": \"b\"\n}"); got != "{\n\t\"a\": \"b\"\n}" {
		t.Errorf("whitespace between tokens changed: %q", got)
	}
}

// Whitespace JSON does not allow between tokens — a no-break space, a
// line or paragraph separator, an ideographic space, a form feed or a
// vertical tab — reads as a space there, as a browser reads it, and is
// kept as it is inside a string.
func TestLinkedDataToleratesUnicodeWhitespaceBetweenTokens(t *testing.T) {
	for _, ws := range []string{" ", " ", " ", "　", "\f", "\v", "\u0085"} {
		root, err := parseHTML("<html><head><script type=\"application/ld+json\">{" + ws +
			"\"@type\":" + ws + "\"VideoObject\"," + ws + ws + "\"name\":\"First" + ws + "Clip\"," +
			"\"contentUrl\":\"/media/clip.mp4\"" + ws + "}</script></head><body></body></html>")
		if err != nil {
			t.Fatal(err)
		}
		found, ok := mediaPageLinkedData(root, mustParse(t, "https://example.test/watch/clip"))
		if !ok || found.URL != "https://example.test/media/clip.mp4" {
			t.Fatalf("%q: media not found: %+v", ws, found)
		}
	}
	if got := jsonControlsEscaped("{ \"a\":\"b c\"}"); got != "{ \"a\":\"b c\"}" {
		t.Errorf("got %q", got)
	}
}

// A JSON attribute is read through the parser, so its quoting and its
// entities are decoded once, exactly.
func TestJSONAttrDecodesQuotingAndEntities(t *testing.T) {
	for _, doc := range []string{
		`<div data-page="{&quot;name&quot;:&quot;A &amp;amp; B  &#x2014; C&quot;}"></div>`,
		`<div data-page='{"name":"A &amp;amp; B  &#x2014; C"}'></div>`,
	} {
		root, err := parseHTML(doc)
		if err != nil {
			t.Fatal(err)
		}
		var v struct{ Name string }
		found, err := jsonAttr(root, "data-page", &v)
		if !found || err != nil || v.Name != "A &amp; B  — C" {
			t.Errorf("%s: found=%v err=%v name=%q", doc, found, err, v.Name)
		}
	}
	root, _ := parseHTML(`<div data-other="{}"></div>`)
	if found, _ := jsonAttr(root, "data-page", new(any)); found {
		t.Error("found an attribute that is not there")
	}
}
