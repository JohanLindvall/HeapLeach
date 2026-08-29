//go:build windows

package download

import (
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// diskSpace reports the bytes a download may still write at path, and the
// size of the volume holding it.
//
// The first out-parameter is the one taken, not the second: it honours a
// per-user quota where the volume carries one, which is the figure a
// transfer is actually allowed to write. Called through the DLL rather than
// through golang.org/x/sys/windows, which would be a new dependency for one
// call this program can make from the standard library.
func diskSpace(path string) (free, total int64, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var avail, size, unused uint64
	r, _, callErr := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&size)),
		uintptr(unsafe.Pointer(&unused)),
	)
	if r == 0 {
		return 0, 0, callErr
	}
	return int64(avail), int64(size), nil
}
