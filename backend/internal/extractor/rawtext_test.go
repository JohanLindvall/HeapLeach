// SPDX-License-Identifier: MIT

package extractor

import (
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
