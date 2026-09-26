package download

import (
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

func fileScope(scopes []transferFiles) transferFiles {
	if len(scopes) > 0 {
		return scopes[0]
	}
	return transferFiles{}
}

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
