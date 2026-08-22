package webui

import (
	"strings"
	"testing"
)

// TestBuiltRecognisesThePlaceholder guards the one thing Built exists to do.
//
// The marker it looks for and the marker the Makefile writes have to agree.
// When they drifted apart once, Built reported a real UI while the binary
// served "Frontend not built" — no warning, and nothing to suggest the
// frontend had never been compiled in. This holds whichever of the two is
// embedded: with a real build the condition simply does not apply.
func TestBuiltRecognisesThePlaceholder(t *testing.T) {
	page, err := dist.ReadFile("dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "Frontend not built") && Built() {
		t.Error("the placeholder is embedded, but Built() says a real UI is")
	}
}
