//go:build !unix && !windows

package download

import "errors"

// diskSpace has no portable answer here, and says so rather than reporting a
// zero the UI would draw as a full disk.
func diskSpace(string) (free, total int64, err error) {
	return 0, 0, errors.New("free space is unavailable on this platform")
}
