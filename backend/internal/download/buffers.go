// SPDX-License-Identifier: MIT

package download

import (
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// bytePool recycles buffers of one fixed size, so a long queue of small
// files stops asking the allocator for identical blocks over and over.
type bytePool struct{ pool sync.Pool }

func newBytePool(size int) *bytePool {
	p := &bytePool{}
	p.pool.New = func() any {
		buf := make([]byte, size)
		return &buf
	}
	return p
}

// borrow lends a buffer and the function that returns it. The buffer is not
// zeroed; every caller writes before reading.
func (p *bytePool) borrow() ([]byte, func()) {
	buf := p.pool.Get().(*[]byte)
	return *buf, func() { p.pool.Put(buf) }
}

// The pools are kept apart because their buffers differ in size and
// lifetime.
var (
	// borrowChunk lends a CopyBufferSize buffer, the size the hot paths copy
	// and decrypt through: a connection borrows one for the life of an
	// attempt, a decrypting write borrows one per call.
	borrowChunk = newBytePool(config.CopyBufferSize).borrow

	// borrowWriteBuffer lends a WriteBufferSize buffer, the larger one a
	// connection accumulates into before touching the file, for the length
	// of one connection's run at one segment.
	borrowWriteBuffer = newBytePool(config.WriteBufferSize).borrow

	// borrowStartBuffer lends the WriteBufferStart buffer every connection
	// begins with; see bufferedWriterAt.grow.
	borrowStartBuffer = newBytePool(config.WriteBufferStart).borrow
)
