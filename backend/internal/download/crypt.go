package download

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// Hosts that serve ciphertext.
//
// A file from such a host has to be decrypted before it is any use, and the
// obvious way — decrypt the finished download in a second pass — would mean
// reading and rewriting every byte again. Decrypting as the bytes arrive is
// better, and possible here because the transfer engine already writes
// positionally: every write states the absolute offset it lands at, whether
// it came from the sequential path or from one of eight connections filling
// in ranges out of order.
//
// That is exactly what a counter-mode stream cipher needs. The keystream at
// an offset depends on nothing but the offset, so slotting a decrypting
// io.WriterAt under the file leaves the whole engine untouched: ranges,
// splitting, resuming a part file from a previous run. What lands in the
// part file is plaintext, which is what makes resume work at all — the
// sidecar's byte counts and the file's length go on meaning the same thing.

// ctrNonceLen is how much of the counter block the nonce occupies. What is
// left is the block index, which is what makes an arbitrary offset
// addressable without having decrypted anything before it.
const ctrNonceLen = 8

// decryptingWriter decrypts each write before passing it to the file below.
// It is safe for concurrent use, as every WriteAt derives its own keystream.
type decryptingWriter struct {
	dst   io.WriterAt
	block cipher.Block
	nonce [ctrNonceLen]byte
}

// writerFor returns the destination for a transfer's bytes: the part file
// itself, or a decrypting layer over it when the host serves ciphertext.
func writerFor(f *os.File, sc *extractor.StreamCipher) (io.WriterAt, error) {
	if sc == nil {
		return f, nil
	}
	if len(sc.Nonce) > ctrNonceLen {
		return nil, fmt.Errorf("payload nonce is %d bytes, at most %d fit the counter block",
			len(sc.Nonce), ctrNonceLen)
	}
	block, err := aes.NewCipher(sc.Key)
	if err != nil {
		return nil, fmt.Errorf("payload key: %w", err)
	}
	w := &decryptingWriter{dst: f, block: block}
	copy(w.nonce[:], sc.Nonce)
	return w, nil
}

// WriteAt decrypts p as the bytes at off and writes the plaintext there.
func (w *decryptingWriter) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("decrypt at negative offset %d", off)
	}

	// The counter block is the nonce over the index of the block this
	// offset falls in; a write starting mid-block skips that many bytes of
	// keystream first.
	var counter [aes.BlockSize]byte
	copy(counter[:], w.nonce[:])
	binary.BigEndian.PutUint64(counter[ctrNonceLen:], uint64(off/aes.BlockSize))
	stream := cipher.NewCTR(w.block, counter[:])
	if skip := int(off % aes.BlockSize); skip > 0 {
		var discard [aes.BlockSize]byte
		stream.XORKeyStream(discard[:skip], discard[:skip])
	}

	plain, release := cryptScratch(len(p))
	defer release()
	stream.XORKeyStream(plain, p)
	return w.dst.WriteAt(plain, off)
}

// cryptBuffers recycles the scratch space a write decrypts into.
//
// Decrypting in place is not an option even though it would be free:
// io.WriterAt may not modify the slice it is handed, and both callers here
// mean it — each reuses one read buffer for the whole transfer, so scribbling
// on it would corrupt the next read rather than merely being impolite.
var cryptBuffers = sync.Pool{
	New: func() any {
		buf := make([]byte, config.CopyBufferSize)
		return &buf
	},
}

// cryptScratch lends a buffer of exactly n bytes and a function to return it.
func cryptScratch(n int) ([]byte, func()) {
	if n > config.CopyBufferSize {
		return make([]byte, n), func() {}
	}
	buf := cryptBuffers.Get().(*[]byte)
	return (*buf)[:n], func() { cryptBuffers.Put(buf) }
}
