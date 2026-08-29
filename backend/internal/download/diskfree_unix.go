//go:build unix

package download

import "syscall"

// diskSpace reports the bytes a download may still write at path, and the
// size of the filesystem holding it.
//
// Bavail rather than Bfree: the blocks Bfree also counts are reserved for
// root, and reporting them would promise room no transfer here can use.
func diskSpace(path string) (free, total int64, err error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0, err
	}
	// Bsize is int64 on Linux and uint32 on Darwin, so it is widened rather
	// than used as it comes; the rest of the fields agree across both.
	size := uint64(fs.Bsize)
	return int64(size * fs.Bavail), int64(size * fs.Blocks), nil
}
