package extractor

import (
	"bytes"
	"encoding/json"
)

// flexValue accepts a JSON field that some hosts send as a string and others
// as a number — ids, byte counts, pagination cursors. It began life in the
// booru families and is shared by every parser here that reads documents
// written by more than one implementation of the same software.
type flexValue string

func (f *flexValue) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		*f = ""
		return nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		*f = flexValue(text)
		return nil
	}
	*f = flexValue(raw)
	return nil
}

func (f flexValue) String() string { return string(f) }
