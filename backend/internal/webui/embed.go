// Package webui carries the compiled TypeScript frontend, embedded into the
// binary so the server ships as a single artefact.
package webui

import (
	"embed"
	"io/fs"
)

// dist is populated by `make frontend`, which builds frontend/ into this
// directory. The checked-in placeholder keeps `go build` working before the
// first frontend build.
//
//go:embed all:dist
var dist embed.FS

// FS returns the built site rooted at its index.html.
func FS() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}

// Built reports whether a real frontend build is embedded, as opposed to the
// placeholder page.
func Built() bool {
	b, err := dist.ReadFile("dist/index.html")
	return err == nil && len(b) > 0 && !containsPlaceholder(b)
}

func containsPlaceholder(b []byte) bool {
	const marker = "data-heapleach-placeholder"
	return len(b) < 4096 && contains(b, marker)
}

func contains(haystack []byte, needle string) bool {
	n := len(needle)
	for i := 0; i+n <= len(haystack); i++ {
		if string(haystack[i:i+n]) == needle {
			return true
		}
	}
	return false
}
