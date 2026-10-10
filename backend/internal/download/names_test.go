// SPDX-License-Identifier: MIT

package download

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

func TestSameNamedListingEntriesKeepDistinctBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="sample.bin"`)
		_, _ = fmt.Fprint(w, "bytes"+r.URL.Path)
	}))
	defer srv.Close()
	m := progressManager(t, 0)
	res := &extractor.Result{Title: "Synthetic album", Files: []extractor.File{
		{Name: "sample.bin", URL: srv.URL + "/1", Size: 7},
		{Name: "sample.bin", URL: srv.URL + "/2", Size: 7},
		{Name: "sample (2).bin", URL: srv.URL + "/3", Size: 7},
	}}
	job := &Job{ID: "job", Source: "https://example.test/album"}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.applyResultLocked(job, "fixture", res, nil)
	m.mu.Unlock()
	for i, it := range job.Items {
		// A late name must not undo the destination allocated from the listing.
		if i == 1 {
			it.resolve = func(context.Context) (*extractor.Target, error) {
				return &extractor.Target{Name: "sample.bin", URL: srv.URL + "/2"}, nil
			}
		}
		if err := m.transfer(t.Context(), it); err != nil {
			t.Fatal(err)
		}
		if it.Skipped {
			t.Fatalf("entry %d mistook another entry's bytes for its own", i)
		}
		got, err := os.ReadFile(filepath.Join(m.DownloadDir(), it.Path))
		if want := fmt.Sprintf("bytes/%d", i+1); err != nil || string(got) != want {
			t.Fatalf("entry %d = %q, %v, want %q", i, got, err, want)
		}
	}
	if job.Items[1].Name != "sample (3).bin" || job.Items[2].Name != "sample (2).bin" {
		t.Fatalf("numbered original was displaced: %q, %q", job.Items[1].Name, job.Items[2].Name)
	}
	// The allocation is deterministic, so submitting the same listing again
	// skips its own bytes at each distinct path instead of duplicating them.
	again := &Job{ID: "again", Source: job.Source}
	m.mu.Lock()
	m.jobs[again.ID] = again
	m.applyResultLocked(again, "fixture", res, nil)
	m.mu.Unlock()
	for _, it := range again.Items {
		if err := m.transfer(t.Context(), it); err != nil || !it.Skipped {
			t.Fatalf("repeat entry = %v, skipped = %t", err, it.Skipped)
		}
	}
}

func TestSeparateNamesUsesPortableSanitizedPaths(t *testing.T) {
	long := strings.Repeat("界", 70) + ".bin"
	items := []*Item{
		{Name: "one?.bin", Dir: "Folder"}, {Name: "one*.bin", Dir: "folder"},
		{Name: "one?.bin", Dir: "different"}, {Name: long}, {Name: long},
	}
	separateNames(items)
	seen := map[string]bool{}
	for _, it := range items {
		key := strings.ToLower(filepath.Join(it.Dir, SafeName(it.Name)))
		if seen[key] {
			t.Fatalf("duplicate portable destination: %s", key)
		}
		seen[key] = true
	}
	if items[2].fixedName || len(items[4].Name) > maxNameBytes || !strings.HasSuffix(items[4].Name, " (2).bin") {
		t.Fatalf("unrelated folder or long name mishandled: %+v, %q", items[2], items[4].Name)
	}
}
