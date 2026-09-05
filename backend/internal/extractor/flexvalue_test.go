package extractor

import (
	"encoding/json"
	"testing"
)

// The field is shared by every parser reading documents written by more than
// one implementation of the same software, and bunkr's signing service is
// the case that prompted it: its expiry arrives as a number from one build
// and a string from the next.
func TestFlexValueReadsEitherSpelling(t *testing.T) {
	cases := map[string]string{
		`{"ex":"1755000000"}`: "1755000000",
		`{"ex":1755000000}`:   "1755000000",
		`{"ex":null}`:         "",
		`{}`:                  "",
	}
	for raw, want := range cases {
		var out struct {
			Ex flexValue `json:"ex"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := out.Ex.String(); got != want {
			t.Errorf("%s: ex = %q, want %q", raw, got, want)
		}
	}
}

func TestFlexValueRejectsAMalformedString(t *testing.T) {
	var out struct {
		ID flexValue `json:"id"`
	}
	if err := json.Unmarshal([]byte(`{"id":"unterminated}`), &out); err == nil {
		t.Error("a broken document decoded without complaint")
	}
}
