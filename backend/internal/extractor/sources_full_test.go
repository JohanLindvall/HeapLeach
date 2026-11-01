// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// An expansion filled exactly to the file cap resolves no further batch:
// nothing after it could be taken whole, so it would be asked for only to
// be left out.
func TestExpansionFullToTheFileStops(t *testing.T) {
	batch := config.SourceBatchRounds * config.PageFetchConcurrency
	sources := make([]string, 2*batch)
	for i := range sources {
		sources[i] = fmt.Sprintf("https://example.test/album/%d", i)
	}
	var asked atomic.Int32
	e := expandSourcesWith(context.Background(), sources, Options{limits: Limits{Files: batch}},
		func(_ context.Context, link string) (*Result, error) {
			asked.Add(1)
			return &Result{Title: link, Files: []File{{Name: "a.jpg"}}}, nil
		})
	if n := int(asked.Load()); n != batch {
		t.Errorf("resolved %d sources, want the first batch of %d alone", n, batch)
	}
	if len(e.files) != batch || !strings.Contains(e.note("", sourceWords{noun: "albums"}), "file limit") {
		t.Errorf("%d files, note %q; want %d and the file limit", len(e.files), e.note("", sourceWords{noun: "albums"}), batch)
	}
}
