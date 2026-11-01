// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/download"
)

func TestSummaryAccountsForIncompleteListings(t *testing.T) {
	done := download.ItemView{Name: "saved.bin", Status: download.StatusDone, Downloaded: 5}
	failed := download.ItemView{Name: "missing.bin", Status: download.StatusFailed}
	for _, tc := range []struct {
		name       string
		jobs       []download.JobView
		prefix     string
		notices    []string
		absent     string
		incomplete bool
	}{
		{
			name:       "retained files completed",
			jobs:       []download.JobView{{Title: "Partial source", Status: download.StatusDone, Incomplete: true, Items: []download.ItemView{done}}},
			prefix:     "✗ 1 file, 5 B",
			notices:    []string{"1 source listing incomplete: Partial source"},
			incomplete: true,
		},
		{
			name:       "listing and transfer failures",
			jobs:       []download.JobView{{Title: "Partial source", Status: download.StatusFailed, Incomplete: true, Items: []download.ItemView{done, failed}}},
			prefix:     "✗ 1 file, 5 B",
			notices:    []string{"1 failed: missing.bin", "1 source listing incomplete: Partial source"},
			incomplete: true,
		},
		{
			name:       "failed resolution reported once",
			jobs:       []download.JobView{{Title: "Unread source", Status: download.StatusFailed, Incomplete: true}},
			prefix:     "✗ 0 files, 0 B",
			notices:    []string{"1 sources failed to resolve: Unread source"},
			absent:     "listing incomplete",
			incomplete: true,
		},
		{
			name:       "incomplete empty source",
			jobs:       []download.JobView{{Title: "Empty source", Status: download.StatusDone, Incomplete: true}},
			prefix:     "✗ 0 files, 0 B",
			notices:    []string{"1 source listing incomplete: Empty source"},
			incomplete: true,
		},
		{
			name:       "incomplete canceled source",
			jobs:       []download.JobView{{Title: "Canceled source", Status: download.StatusCanceled, Incomplete: true}},
			prefix:     "✗ 0 files, 0 B",
			notices:    []string{"1 source listing incomplete: Canceled source"},
			incomplete: true,
		},
		{
			name: "several incomplete sources",
			jobs: []download.JobView{
				{Title: "First source", Status: download.StatusDone, Incomplete: true, Items: []download.ItemView{done}},
				{Source: "https://example.test/second", Status: download.StatusDone, Incomplete: true, Items: []download.ItemView{done}},
			},
			prefix:     "✗ 2 files, 10 B",
			notices:    []string{"2 source listings incomplete: First source, https://example.test/second"},
			incomplete: true,
		},
		{
			name:   "deliberate cap remains successful",
			jobs:   []download.JobView{{Title: "Capped source — partial — file limit", Status: download.StatusDone, Items: []download.ItemView{done}}},
			prefix: "✓ 1 file, 5 B",
			absent: "listing incomplete",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			r := newRenderer(&out, 120, false, false)
			err := summary(download.Snapshot{Jobs: tc.jobs}, &out, r, time.Now())
			if tc.incomplete {
				if !errors.Is(err, ErrIncomplete) {
					t.Errorf("summary = %v, want ErrIncomplete", err)
				}
			} else if err != nil {
				t.Errorf("summary = %v, want success", err)
			}
			if !strings.HasPrefix(out.String(), tc.prefix) {
				t.Errorf("summary = %q, want prefix %q", out.String(), tc.prefix)
			}
			for _, notice := range tc.notices {
				if strings.Count(out.String(), notice) != 1 {
					t.Errorf("summary = %q, want exactly one %q", out.String(), notice)
				}
			}
			if tc.absent != "" && strings.Contains(out.String(), tc.absent) {
				t.Errorf("summary = %q, must not contain %q", out.String(), tc.absent)
			}
		})
	}
}
