// SPDX-License-Identifier: MIT

package extractor

import (
	"context"
	"encoding/json"
	"testing"
)

// Helpers shared by the package's tests: the boilerplate around decoding a
// fixture, running one extraction and reading what came back, written once
// so a test states only what it checks.

// decode reads a JSON fixture into a T, failing the test when it does not
// parse.
func decode[T any](t *testing.T, doc string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("decode %T: %v", v, err)
	}
	return v
}

// extract runs one extraction of raw, which must parse as a URL.
func extract(t *testing.T, ex Extractor, raw string, opts Options) (*Result, error) {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ex.Extract(context.Background(), u, opts)
}

// extractOK is extract for an extraction that must succeed.
func extractOK(t *testing.T, ex Extractor, raw string, opts Options) *Result {
	t.Helper()
	res, err := extract(t, ex, raw, opts)
	if err != nil {
		t.Fatalf("extract %s: %v", raw, err)
	}
	return res
}

// fileNames lists a result's file names, in order.
func fileNames(files []File) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return names
}

// filePaths lists a result's files as dir/name, in order.
func filePaths(files []File) []string {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Name
		if f.Dir != "" {
			paths[i] = f.Dir + "/" + f.Name
		}
	}
	return paths
}
