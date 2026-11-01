// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// fakeTree serves folders from a map: a folder's entries are files named
// "f:<name>" and subfolders "d:<key>:<label>". A folder missing from the map
// cannot be read.
func fakeTree(tree map[string][]string, read *[]string) treeList[string] {
	return func(_ context.Context, _ *treeWalk, key string, _ int) ([]treeItem[string], error) {
		*read = append(*read, key)
		entries, ok := tree[key]
		if !ok {
			return nil, fmt.Errorf("folder %s unreadable", key)
		}
		var items []treeItem[string]
		for _, e := range entries {
			parts := strings.SplitN(e, ":", 3)
			if parts[0] == "f" {
				items = append(items, treeItem[string]{File: &File{Name: parts[1]}})
			} else {
				items = append(items, treeItem[string]{Folder: parts[1], Key: parts[1], Name: parts[2], Fallback: parts[1]})
			}
		}
		return items, nil
	}
}

func TestWalkTreeKeepsOrderAndFolders(t *testing.T) {
	tree := map[string][]string{
		"root": {"f:a.mp4", "d:one:First / Part", "f:b.mp4", "d:two:..", "d:root:Loop"},
		"one":  {"f:c.mp4", "d:root:Back"},
		"two":  {"f:d.mp4", "d:gone:Gone"},
	}
	var read []string
	res := &Result{}
	if err := walkTree(context.Background(), Options{}, res, "root", "root", fakeTree(tree, &read)); err != nil {
		t.Fatal(err)
	}
	want := []string{"a.mp4", "First _ Part/c.mp4", "b.mp4", "two/d.mp4"}
	if got := filePaths(res.Files); !slices.Equal(got, want) {
		t.Fatalf("files = %q, want %q", got, want)
	}
	if !slices.Equal(read, []string{"root", "one", "two", "gone"}) {
		t.Fatalf("read %q: a folder was read twice or out of order", read)
	}
	if res.Note != "partial — unreadable folders" {
		t.Fatalf("note = %q", res.Note)
	}
}

func TestWalkTreeLimits(t *testing.T) {
	deep := map[string][]string{}
	for i := range config.MaxFolderDepth + 2 {
		deep[fmt.Sprint(i)] = []string{fmt.Sprintf("f:%d.mp4", i), fmt.Sprintf("d:%d:Level", i+1)}
	}
	wide := map[string][]string{"root": {"d:a:A", "d:b:B", "d:c:C"}, "a": {"f:a.mp4"}, "b": {"f:b.mp4"}, "c": {"f:c.mp4"}}
	for _, tc := range []struct {
		name  string
		tree  map[string][]string
		opts  Options
		files int
		note  string
	}{
		// The root is depth 0, and depths 0 through MaxFolderDepth are read.
		{"depth", deep, Options{}, config.MaxFolderDepth + 1, "partial — folder limit"},
		// A folder is one request, not a followed link: the source cap
		// does not bound how many are read.
		{"source cap", wide, Options{limits: Limits{Sources: 2}}, 3, ""},
		{"files", wide, Options{limits: Limits{Files: 2}}, 2, "partial — file limit"},
		{"exact", wide, Options{limits: Limits{Files: 3}}, 3, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var read []string
			root := "root"
			if tc.tree["root"] == nil {
				root = "0"
			}
			res := &Result{}
			if err := walkTree(context.Background(), tc.opts, res, root, root, fakeTree(tc.tree, &read)); err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != tc.files || res.Note != tc.note {
				t.Fatalf("files=%q note=%q", filePaths(res.Files), res.Note)
			}
		})
	}
}

func TestWalkTreeErrors(t *testing.T) {
	var read []string
	if err := walkTree(context.Background(), Options{}, &Result{}, "root", "root", fakeTree(nil, &read)); err == nil {
		t.Fatal("an unreadable root was not an error")
	}
	stop := func(_ context.Context, _ *treeWalk, key string, _ int) ([]treeItem[string], error) {
		if key == "root" {
			return []treeItem[string]{{Folder: "locked", Key: "locked", Name: "Locked"}}, nil
		}
		return nil, treeStop{ErrPasswordRequired}
	}
	if err := walkTree(context.Background(), Options{}, &Result{}, "root", "root", stop); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("a stopping subfolder error = %v", err)
	}
}
