// SPDX-License-Identifier: MIT

package extractor

import (
	"slices"
	"strings"
)

// What the program supports, asked of the program rather than written down.
//
// The host table in README.md explains how a handful of interesting hosts
// resolve, and that is editorial: it is worth a person's judgement about what
// deserves explaining. The *inventory* is not. A list of everything supported
// maintained by hand is a list that is wrong within two commits, and wrong in
// the direction that matters — it under-reports, so people conclude a page is
// unsupported when it is not.
//
// So the registry answers the question itself, `cmd/hostlist` renders the
// answer as Markdown, and `make hosts` writes it into README.md between two
// markers. Adding an extractor to NewRegistry is then the only step; the
// documentation follows from the code, and CI fails if the two have drifted.
//
// No extractor claims a domain, so the inventory names none: each row is an
// extractor and its own account of the pages it recognises.

// Describer is how an extractor says, for the inventory, what it takes: the
// pages, by the forms and markers it recognises them by.
type Describer interface {
	Recognises() string
}

// HostInfo is one row of the catalogue.
type HostInfo struct {
	// Name is the label the UI shows.
	Name string
	// Recognises is the extractor's own account of the pages it takes.
	Recognises string
}

// Catalogue reports every registered extractor and the fallback, with what
// each recognises, ordered by name so the generated documentation has a stable diff.
func (r *Registry) Catalogue() []HostInfo {
	out := make([]HostInfo, 0, len(r.extractors)+1)
	for _, e := range append(slices.Clip(r.extractors), r.fallback) {
		out = append(out, HostInfo{Name: e.Name(), Recognises: e.Recognises()})
	}
	slices.SortFunc(out, func(a, b HostInfo) int { return strings.Compare(a.Name, b.Name) })
	return out
}
