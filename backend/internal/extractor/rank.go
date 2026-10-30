// SPDX-License-Identifier: MIT

package extractor

import (
	"slices"
	"strings"
)

// rankIn is name's place in order, compared without regard to case, or
// unknown when order does not hold it. Where an unknown name lands is each
// caller's to say: last (len(order)) for an ordering that still takes it,
// or -1 for one read as an allow-list or with the best at the end.
func rankIn(order []string, name string, unknown int) int {
	if i := slices.IndexFunc(order, func(known string) bool { return strings.EqualFold(name, known) }); i >= 0 {
		return i
	}
	return unknown
}
