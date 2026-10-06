// SPDX-License-Identifier: MIT

package util

// BoundedBuffer retains the first Limit bytes while accepting and discarding
// the rest. A verbose helper must not grow memory indefinitely, nor block on
// a full pipe because its diagnostic reader stopped early.
type BoundedBuffer struct {
	Limit     int
	Truncated bool
	data      []byte
}

func (b *BoundedBuffer) Write(p []byte) (int, error) {
	n := min(len(p), max(0, b.Limit-len(b.data)))
	b.data = append(b.data, p[:n]...)
	b.Truncated = b.Truncated || n < len(p)
	return len(p), nil
}

func (b *BoundedBuffer) Bytes() []byte  { return b.data }
func (b *BoundedBuffer) String() string { return string(b.data) }
