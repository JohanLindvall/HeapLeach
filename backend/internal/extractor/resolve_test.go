// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestResolveFilesKeepsOrderAndReasons pins the multi-file resolve: each
// item's files in the listing's order, whichever finished first, the
// refusals in order beside them, and the first other failure.
func TestResolveFilesKeepsOrderAndReasons(t *testing.T) {
	resolve := func(_ context.Context, item string) ([]File, error) {
		switch item {
		case "refused-1", "refused-2":
			return nil, &broadcastRefusal{"site", item, "withheld " + item}
		case "failed-1", "failed-2":
			return nil, errors.New("site: " + item)
		case "empty":
			return nil, nil
		}
		return []File{{Name: item + "-a"}, {Name: item + "-b"}}, nil
	}
	items := []string{"one", "failed-1", "refused-1", "empty", "two", "refused-2", "failed-2"}
	files, refusals, failed := resolveFiles(context.Background(), items, resolve)
	if got := fmt.Sprint(fileNames(files)); got != "[one-a one-b two-a two-b]" {
		t.Errorf("files = %s", got)
	}
	if got := fmt.Sprint(refusals); got != "[withheld refused-1 withheld refused-2]" {
		t.Errorf("refusals = %s", got)
	}
	if failed == nil || failed.Error() != "site: failed-1" {
		t.Errorf("failed = %v", failed)
	}
}

// TestNoneReadSaysWhy pins the error a listing gives when none of its
// items could be read: a refusal first, then a failure less the site's
// name, then that they held nothing.
func TestNoneReadSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		refusals []string
		failed   error
		want     string
	}{
		{[]string{"it is private"}, errors.New("site: transport"), "site: none of the 3 releases on the link could be read: it is private"},
		{nil, errors.New("site: transport"), "site: none of the 3 releases on the link could be read: transport"},
		{nil, nil, "site: none of the 3 releases on the link could be read: they hold nothing to download"},
	} {
		if got := noneRead("site", 3, "releases", "the link", tc.refusals, tc.failed).Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}
