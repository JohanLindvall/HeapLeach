// Package tools locates the optional external programs the downloader uses
// when they are available.
//
// A binary sitting next to downd wins over one on PATH, so the static builds
// `make dependencies` drops in ./bin are picked up without touching the
// system, and without depending on how the service was launched.
package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	mu       sync.Mutex
	resolved = map[string]string{}
)

// Find returns the path to a named tool, preferring one installed alongside
// the running binary. The result is cached: these do not appear mid-run.
func Find(name string) (string, bool) {
	mu.Lock()
	defer mu.Unlock()

	if path, ok := resolved[name]; ok {
		return path, path != ""
	}
	path := locate(name)
	resolved[name] = path
	return path, path != ""
}

// Reset clears the cache. Only tests need this.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	resolved = map[string]string{}
}

func locate(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	// Alongside the binary first.
	if exe, err := os.Executable(); err == nil {
		if resolvedExe, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolvedExe
		}
		candidate := filepath.Join(filepath.Dir(exe), name)
		if executable(candidate) {
			return candidate
		}
	}
	// Then the usual search path.
	if path, err := lookPath(name); err == nil {
		return path
	}
	return ""
}

// executable reports whether a path is a runnable file.
func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0
}

// lookPath is exec.LookPath, kept behind a variable so tests can steer it.
var lookPath = defaultLookPath
