// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
)

// A page that carries nothing the walk takes but says the listing goes on
// (skip) is passed over, not read as a repeat, even as the first page; a run
// of them is bounded, and a page that only repeats still ends the walk.
func TestWalkPagesSkipsPagesThatSayTheListingGoesOn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages []pageOf[string]
		want  []string
		note  string
	}{
		{"empty first page with a pager", []pageOf[string]{{skip: true, next: true}, {items: []string{"a"}}},
			[]string{"a"}, ""},
		{"empty middle page", []pageOf[string]{{items: []string{"a"}, next: true}, {skip: true, next: true}, {items: []string{"b"}}},
			[]string{"a", "b"}, ""},
		{"repeats are no skip", []pageOf[string]{{items: []string{"a"}, next: true}, {items: []string{"a"}, next: true}, {items: []string{"b"}}},
			[]string{"a"}, "partial — repeated or empty page"},
		{"empty first page without the signal", []pageOf[string]{{next: true}, {items: []string{"a"}}},
			nil, "partial — repeated or empty page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, note, err := walkPages(context.Background(), pageWalk[string]{
				key: func(s string) string { return s }, limit: -1,
				fetch: func(_ context.Context, n int) (pageOf[string], error) { return tc.pages[n], nil },
			})
			if err != nil || strings.Join(items, ",") != strings.Join(tc.want, ",") || note != tc.note {
				t.Fatalf("items %v note %q err %v; want %v %q", items, note, err, tc.want, tc.note)
			}
		})
	}
	asked := 0
	_, note, _ := walkPages(context.Background(), pageWalk[string]{
		key: func(s string) string { return s }, limit: -1,
		fetch: func(_ context.Context, n int) (pageOf[string], error) {
			asked++
			if n == 0 {
				return pageOf[string]{items: []string{"a"}, next: true}, nil
			}
			return pageOf[string]{skip: true, next: true}, nil
		},
	})
	if want := 2 + config.MaxSkippedListingPages; asked != want || note != "partial — repeated or empty page" {
		t.Fatalf("an endless run of empty pages: asked %d (want %d), note %q", asked, want, note)
	}
}

// A walk that accepts an undecodable later page as the end takes a body
// that does not decode — the empty answer some APIs give past their last
// page — as the listing's end; any other failure stays a cut.
func TestWalkPagesUndecodableEnd(t *testing.T) {
	var v []int
	decodeErr := fmt.Errorf("page 2: decode json: %w", json.Unmarshal([]byte(""), &v))
	for _, tc := range []struct {
		name   string
		ends   walkAccepts
		failAt error
		want   string
	}{
		{"accepted", walkAccepts{undecodable: true}, decodeErr, ""},
		{"not accepted", walkAccepts{}, decodeErr, couldNotFetchNote},
		{"other failures stay a cut", walkAccepts{undecodable: true}, &httpx.StatusError{Code: http.StatusBadGateway}, couldNotFetchNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, note, err := walkPages(context.Background(), pageWalk[string]{
				key: func(s string) string { return s }, limit: -1, ends: tc.ends,
				fetch: func(_ context.Context, n int) (pageOf[string], error) {
					if n > 0 {
						return pageOf[string]{}, tc.failAt
					}
					return pageOf[string]{items: []string{"a"}, next: true}, nil
				},
			})
			if err != nil || note != tc.want {
				t.Fatalf("note %q err %v; want %q", note, err, tc.want)
			}
		})
	}
}

// entryLimit counts a listing of files against the file cap, never the
// source cap, with the host's own lower cap said as the listing limit.
func TestEntryLimitIsTheFileCap(t *testing.T) {
	for _, tc := range []struct {
		opts    Options
		hostCap int
		limit   int
		note    string
	}{
		{Options{limits: Limits{Sources: 5}}, 0, config.MaxListingFiles, fileLimitNote},
		{Options{limits: Limits{Files: 7}}, 0, 7, fileLimitNote},
		{Options{limits: Limits{Files: 7}}, 100, 7, fileLimitNote},
		{Options{}, 100, 100, listingLimitNote},
	} {
		if limit, note := entryLimit(tc.opts, tc.hostCap); limit != tc.limit || note != tc.note {
			t.Errorf("entryLimit(%+v, %d) = %d, %q; want %d, %q", tc.opts.limits, tc.hostCap, limit, note, tc.limit, tc.note)
		}
	}
}
