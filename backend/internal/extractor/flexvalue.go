// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"encoding/json"
)

// flexValue accepts a JSON field that some hosts send as a string and others
// as a number — ids, byte counts, pagination cursors. It began life in the
// booru families and is shared by every parser here that reads documents
// written by more than one implementation of the same software. A number
// keeps the text it was written as, and so its every digit. A field that
// repeats — an array, as a schemaless store writes a value edited twice —
// keeps its first non-empty value, and an object, which no field here is
// meant to be, reads as empty.
type flexValue string

func (f *flexValue) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	*f = ""
	switch {
	case len(raw) == 0 || string(raw) == "null" || raw[0] == '{':
	case raw[0] == '"':
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		*f = flexValue(text)
	case raw[0] == '[':
		var values []flexValue
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, v := range values {
			if v != "" {
				*f = v
				break
			}
		}
	default:
		*f = flexValue(raw)
	}
	return nil
}

func (f flexValue) String() string { return string(f) }
