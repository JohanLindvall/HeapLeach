// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestExpandSourcesKeepsWhySourcesFailed carries the reasons sources did not
// resolve through an expansion, so a page none of whose sources resolved can
// say why; a link no extractor recognised was never a source, and is counted
// apart rather than as a failure.
func TestExpandSourcesKeepsWhySourcesFailed(t *testing.T) {
	e := expandSourcesWith(context.Background(), []string{"unrecognised", "refused", "broken", "fine"}, Options{},
		func(_ context.Context, link string) (*Result, error) {
			switch link {
			case "unrecognised":
				return nil, fmt.Errorf("follow: %w", errUnrecognised)
			case "refused":
				return nil, &broadcastRefusal{site: "host", reason: "shown only at home"}
			case "broken":
				return nil, errors.New("host: broken")
			}
			return &Result{Title: "Fine", Files: []File{{Name: "a.bin"}}}, nil
		})
	if e.used != 1 || len(e.files) != 1 || e.unrecognised != 1 {
		t.Errorf("used %d, %d files, %d unrecognised; want 1, 1, 1", e.used, len(e.files), e.unrecognised)
	}
	if strings.Join(e.refusals, ",") != "shown only at home" || e.failed == nil || e.failed.Error() != "host: broken" {
		t.Errorf("refusals = %v, failed = %v", e.refusals, e.failed)
	}
	err := noneRead("links", 3, "supported links", "a page", e.refusals, e.failed)
	if want := "links: none of the 3 supported links on a page could be read: shown only at home"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}
