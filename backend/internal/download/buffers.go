package download

import (
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// chunkPool recycles the fixed-size buffers the hot paths copy and decrypt
// through. Every user wants exactly CopyBufferSize: a connection borrows one
// for the life of an attempt, a decrypting write borrows one per call. They
// are all the same size, so one pool serves the lot — and a long queue of
// small files stops asking the allocator for identical 256 KiB blocks over
// and over.
var chunkPool = sync.Pool{
	New: func() any {
		buf := make([]byte, config.CopyBufferSize)
		return &buf
	},
}

// borrowChunk lends a CopyBufferSize buffer and the function that returns
// it. The buffer is not zeroed; every caller writes before reading.
func borrowChunk() ([]byte, func()) {
	buf := chunkPool.Get().(*[]byte)
	return *buf, func() { chunkPool.Put(buf) }
}

// writePool recycles the larger buffers connections accumulate into before
// touching the file. They are kept apart from the chunk pool because they
// are a different size and have a different lifetime: a read buffer is
// borrowed for the length of an attempt, a write buffer for the length of
// one connection's run at one segment.
var writePool = sync.Pool{
	New: func() any {
		buf := make([]byte, config.WriteBufferSize)
		return &buf
	},
}

// borrowWriteBuffer lends a WriteBufferSize buffer and the function that
// returns it.
func borrowWriteBuffer() ([]byte, func()) {
	buf := writePool.Get().(*[]byte)
	return *buf, func() { writePool.Put(buf) }
}
