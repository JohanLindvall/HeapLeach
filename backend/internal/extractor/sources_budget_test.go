// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"testing"
)

func TestExpansionTrimsFirstNonemptySource(t *testing.T) {
	populated := &Result{Title: "Populated", Files: []File{{Name: "first.jpg"}, {Name: "second.jpg"}}}
	e := expandSourcesWith(context.Background(), []string{"empty", "populated"}, Options{limits: Limits{Files: 1}},
		func(_ context.Context, link string) (*Result, error) {
			if link == "empty" {
				return &Result{Title: "Empty"}, nil
			}
			return populated, nil
		})
	if len(e.files) != 1 || e.files[0].Name != "first.jpg" || e.used != 1 || e.cut != 1 || e.of != 2 || !e.full {
		t.Fatalf("expansion = %+v; want first file of first nonempty source", e)
	}
	if e.listed != 2 || e.attempted != 2 || e.empty != 1 || e.succeeded != 1 {
		t.Errorf("summary = %+v", e.outcomeSummary)
	}
	if len(populated.Files) != 2 {
		t.Errorf("source result was modified: %+v", populated)
	}
}

func TestSourceExpansionSharesAdmissionAcrossPages(t *testing.T) {
	sources := map[string]*Result{
		"empty": {Title: "Empty"},
		"one":   {Title: "One", Files: []File{{Name: "first.jpg"}, {Name: "second.jpg"}}},
		"two":   {Title: "Two", Files: []File{{Name: "third.jpg"}, {Name: "fourth.jpg"}}},
		"small": {Title: "Small", Files: []File{{Name: "fifth.jpg"}}},
	}
	read := func(_ context.Context, link string) (*Result, error) {
		if link == "broken" {
			return nil, errors.New("source could not be read")
		}
		return sources[link], nil
	}
	e := sourceExpansion{limit: 3}
	e.add(context.Background(), []string{"empty", "broken"}, read)
	if e.done() || e.total != 0 {
		t.Fatalf("empty and failed sources spent the budget: %+v", e)
	}
	e.add(context.Background(), []string{"one"}, read)
	e.add(context.Background(), []string{"two", "small"}, read)
	if !e.done() || !e.full || e.total != 2 {
		t.Fatalf("whole-source admission reset across pages: %+v", e)
	}
	e.add(context.Background(), []string{"later"}, func(context.Context, string) (*Result, error) {
		t.Error("opened another source after a whole source could not fit")
		return nil, nil
	})
	got := e.result(sourceFolders)
	if got.used != 1 || len(got.files) != 2 || got.files[0].Name != "first.jpg" || got.cut != 0 {
		t.Fatalf("result = %+v; want only the first whole nonempty source", got)
	}
	if got.listed != 6 || got.attempted != 5 || got.succeeded != 3 || got.empty != 1 || got.failures != 1 {
		t.Errorf("summary = %+v", got.outcomeSummary)
	}
}

func TestSourceExpansionFinalizesAcrossPages(t *testing.T) {
	e := sourceExpansion{limit: 2}
	read := func(_ context.Context, link string) (*Result, error) {
		return &Result{Title: "Shared title", Files: []File{{Name: "image.jpg"}}, Note: "partial — could not fetch page"}, nil
	}
	e.add(context.Background(), []string{"https://example.test/one"}, read)
	e.add(context.Background(), []string{"https://example.test/two"}, read)
	e.add(context.Background(), nil, read)
	if !e.done() || e.full {
		t.Fatalf("exact exhaustion invented an omitted source: %+v", e)
	}
	got := e.result(sourceFolders)
	if got.used != 2 || got.partial != 2 || len(got.files) != 2 {
		t.Fatalf("result = %+v", got)
	}
	for i, want := range []string{"Shared title [one]", "Shared title [two]"} {
		if got.files[i].Dir != want {
			t.Errorf("file %d folder = %q, want %q", i, got.files[i].Dir, want)
		}
	}
	e.add(context.Background(), []string{"https://example.test/three"}, func(context.Context, string) (*Result, error) {
		t.Error("opened another source after exactly filling the file cap")
		return nil, nil
	})
	got = e.result(sourceFolders)
	if !got.full || got.listed != 3 || got.attempted != 2 || len(got.files) != 2 || got.partial != 2 {
		t.Errorf("result after a known omitted source = %+v", got)
	}
}
