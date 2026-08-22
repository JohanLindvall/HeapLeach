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
