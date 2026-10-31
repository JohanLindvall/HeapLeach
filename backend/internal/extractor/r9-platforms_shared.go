// SPDX-License-Identifier: MIT

package extractor

import (
	"slices"
	"strings"
	"sync"

	"golang.org/x/net/html"
)

// hasAnyClass reports whether n carries any of classes.
func hasAnyClass(n *html.Node, classes ...string) bool {
	return slices.ContainsFunc(classes, func(class string) bool { return hasClass(n, class) })
}

// hostMemo remembers one answer per host for the extractor's lifetime: what
// a platform install established about itself — the software it runs, where
// it keeps its API — which no install changes between one paste and the next.
// The zero value is ready to use.
type hostMemo[T any] struct {
	mu sync.Mutex
	m  map[string]T
}

// lookup answers for host from memory, or with compute, remembering what it
// answered. A failure is not remembered: the next paste asks again.
func (h *hostMemo[T]) lookup(host string, compute func() (T, error)) (T, error) {
	host = strings.ToLower(host)
	h.mu.Lock()
	v, ok := h.m[host]
	h.mu.Unlock()
	if ok {
		return v, nil
	}
	v, err := compute()
	if err != nil {
		return v, err
	}
	h.mu.Lock()
	if h.m == nil {
		h.m = make(map[string]T)
	}
	h.m[host] = v
	h.mu.Unlock()
	return v, nil
}
