// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// Walking a share of nested folders.
//
// A folder share is a tree, and the job should be the tree: each file filed
// under the folders it sits in. The hosts differ only in how one folder is
// read; what bounds the walk is the same everywhere and lives here, so that
// no host is the one that follows a folder cycle or files a label with a
// slash in it as two folders:
//
//   - depth: folders deeper than config.MaxFolderDepth are not read;
//   - folders: a folder reached twice (by key) is read once, and no more than
//     the user's source limit are read in all;
//   - files: the walk stops at the user's file limit;
//   - a subfolder that cannot be read is left out and said so, rather than
//     sinking the whole job or vanishing without a word.
//
// Every cut is a "partial — …" note on the result. The order is depth-first
// and the host's own: a folder's entries in the order its list gave them,
// each subfolder's files where the subfolder was listed.

// treeItem is one entry of a folder: a file, or a subfolder to walk.
type treeItem[F any] struct {
	// File is the entry when it is a file. Its Dir is set by the walk.
	File *File

	// Otherwise the entry is a subfolder. Folder is what the list function
	// needs to read it, and Key its identity, so a folder reached twice is
	// read once.
	Folder F
	Key    string
	// Name is the subfolder's label; Fallback, usually its id, names the
	// directory when the label makes no name. With neither, its files stay
	// in the parent's directory.
	Name, Fallback string
}

// treeList reads one folder at depth (the root is 0) into its entries.
type treeList[F any] func(ctx context.Context, w *treeWalk, folder F, depth int) ([]treeItem[F], error)

// treeWalk is the state a walk shares with the list function reading its
// folders.
type treeWalk struct {
	opts Options
	res  *Result
	seen map[string]bool
	full bool
}

// note adds a reason the result is incomplete, once.
func (w *treeWalk) note(reason string) {
	switch {
	case w.res.Note == "":
		w.res.Note = "partial — " + reason
	case !strings.Contains(w.res.Note, reason):
		w.res.Note += "; " + reason
	}
}

// room is how many more files the walk may take. A list function that pages
// may stop reading once it holds more than this: the walk keeps what fits
// and says the rest was cut.
func (w *treeWalk) room() int { return w.opts.maxFiles() - len(w.res.Files) }

// treeStop marks a subfolder's error as the whole walk's, for a host where a
// refusal deeper in the tree means the share as a whole is refused.
type treeStop struct{ error }

func (e treeStop) Unwrap() error { return e.error }

// walkTree appends the files under root, keyed key, to res. An error reading
// the root is returned, as is a cancelled context or a treeStop; any other
// subfolder error becomes a note.
func walkTree[F any](ctx context.Context, opts Options, res *Result, key string, root F, list treeList[F]) error {
	w := &treeWalk{opts: opts, res: res, seen: map[string]bool{key: true}}
	err := walkFolder(ctx, w, list, root, "", 0)
	var stop treeStop
	if errors.As(err, &stop) {
		return stop.error
	}
	return err
}

func walkFolder[F any](ctx context.Context, w *treeWalk, list treeList[F], folder F, dir string, depth int) error {
	items, err := list(ctx, w, folder, depth)
	if err != nil {
		var stop treeStop
		if depth == 0 || ctx.Err() != nil || errors.As(err, &stop) {
			return err
		}
		w.note("unreadable folders")
		return nil
	}
	for _, item := range items {
		if w.full {
			return nil
		}
		if item.File != nil {
			if w.room() <= 0 {
				w.full = true
				w.note("file limit")
				return nil
			}
			f := *item.File
			f.Dir = dir
			w.res.Files = append(w.res.Files, f)
			continue
		}
		if w.seen[item.Key] {
			continue
		}
		if w.room() <= 0 {
			w.full = true
			w.note("file limit")
			return nil
		}
		if depth+1 >= config.MaxFolderDepth || len(w.seen) >= w.opts.maxSources() {
			w.note("folder limit")
			continue
		}
		w.seen[item.Key] = true
		if err := walkFolder(ctx, w, list, item.Folder, path.Join(dir, treeDir(item.Name, item.Fallback)), depth+1); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// treeDir is the directory a subfolder's files go in, under its parent's:
// its label as one component, or its fallback where the label makes no
// name.
func treeDir(name, fallback string) string {
	for _, label := range []string{name, fallback} {
		if dir := sourceFolder(label); dir != "" && dir != "." && dir != ".." {
			return dir
		}
	}
	return ""
}
