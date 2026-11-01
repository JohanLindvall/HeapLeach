// SPDX-License-Identifier: MIT

package download

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// transferFiles confines native transfers and checkpoints to the directory
// selected when the transfer started. os.Root also checks symlinks at the
// actual filesystem operation, avoiding a check-then-open traversal race.
// The zero value handles trusted local paths for helper tools and tests.
type transferFiles struct{ root *os.Root }

func (f transferFiles) relative(path string) (string, error) {
	return filepath.Rel(f.root.Name(), path)
}

func (f transferFiles) open(path string, flags int, mode os.FileMode) (*os.File, error) {
	if f.root == nil {
		return os.OpenFile(path, flags, mode)
	}
	rel, err := f.relative(path)
	if err != nil {
		return nil, err
	}
	return f.root.OpenFile(rel, flags, mode)
}

func (f transferFiles) stat(path string) (os.FileInfo, error) {
	if f.root == nil {
		return os.Stat(path)
	}
	rel, err := f.relative(path)
	if err != nil {
		return nil, err
	}
	return f.root.Stat(rel)
}

func (f transferFiles) lstat(path string) (os.FileInfo, error) {
	if f.root == nil {
		return os.Lstat(path)
	}
	rel, err := f.relative(path)
	if err != nil {
		return nil, err
	}
	return f.root.Lstat(rel)
}

func (f transferFiles) read(path string) ([]byte, error) {
	file, err := f.open(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func (f transferFiles) write(path string, data []byte) error {
	file, err := f.open(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func (f transferFiles) remove(path string) error {
	if f.root == nil {
		return os.Remove(path)
	}
	rel, err := f.relative(path)
	if err != nil {
		return err
	}
	return f.root.Remove(rel)
}

func (f transferFiles) rename(from, to string) error {
	if f.root == nil {
		return os.Rename(from, to)
	}
	old, err := f.relative(from)
	if err != nil {
		return err
	}
	next, err := f.relative(to)
	if err != nil {
		return err
	}
	return f.root.Rename(old, next)
}

func (f transferFiles) truncate(path string) error {
	file, err := f.open(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Truncate(0)
}

// reserve creates the destination exclusively before it is renamed or
// remuxed into. Merely checking with Lstat allows concurrent completions to
// choose the same name and overwrite each other.
func (f transferFiles) reserve(dir, name string) (string, error) {
	for i := 1; i < 10000; i++ {
		candidate := filepath.Join(dir, numberedName(name, i))
		file, err := f.open(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := file.Close(); err != nil {
			f.remove(candidate)
			return "", err
		}
		return candidate, nil
	}
	return "", fmt.Errorf("could not reserve a filename for %q in %s", name, dir)
}

// writeFile writes a whole file under a temporary name and renames it into
// place, so that a file found there is always a whole one. A write that
// fails removes its temporary file.
func writeFile(files transferFiles, path string, data []byte) error {
	tmp := path + ".tmp"
	err := files.write(tmp, data)
	if err == nil {
		err = files.rename(tmp, path)
	}
	if err != nil {
		_ = files.remove(tmp)
	}
	return err
}

// A sidecar is the JSON checkpoint written beside a part file so that an
// interrupted transfer resumes rather than starts over. It is written whole
// (writeFile), so an interrupted write cannot leave a half-parsed one, and a
// missing or broken one simply means starting over.

// writeSidecar records a checkpoint.
func writeSidecar(files transferFiles, path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFile(files, path, raw)
}

// readSidecar reads a checkpoint, nil when it is absent or unreadable.
func readSidecar[T any](files transferFiles, path string) *T {
	raw, err := files.read(path)
	if err != nil {
		return nil
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return &v
}

// clearSidecar removes a checkpoint, and any write of one left unfinished.
func clearSidecar(files transferFiles, path string) {
	_ = files.remove(path)
	_ = files.remove(path + ".tmp")
}
