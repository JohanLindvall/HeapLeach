// SPDX-License-Identifier: MIT

package download

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// Decrypting HLS segments (METHOD=AES-128).
//
// Every segment is encrypted on its own: AES-128 in CBC mode, padded with
// PKCS#7, under one key the playlist names by URL. The playlist path already
// fetches each segment whole before writing it, so decrypting is done to
// that buffer, in place, before it joins the file — the part file then holds
// the plain stream, and the checkpoint's byte count goes on meaning what is
// on disk, which is what resume depends on.
//
// The IV is the one the playlist names, or, when it names none, the
// segment's media sequence number as a sixteen-byte big-endian integer.

// errSegmentPadding is what a wrong key looks like: CBC decrypts anything,
// and only the padding at the end says whether it was the right key.
var errSegmentPadding = errors.New("decrypted segment has invalid padding (wrong key?)")

// segmentIV is the IV segment index of a playlist is decrypted with.
func segmentIV(key *extractor.SegmentKey, index int) []byte {
	if len(key.IV) == aes.BlockSize {
		return key.IV
	}
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], uint64(key.Sequence)+uint64(index))
	return iv
}

// decryptSegment decrypts one segment in place and returns it without its
// padding. data must be the caller's own buffer.
func decryptSegment(data, key, iv []byte) ([]byte, error) {
	if len(key) != aes.BlockSize {
		return nil, fmt.Errorf("segment key is %d bytes, want %d", len(key), aes.BlockSize)
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted segment is %d bytes, not whole AES blocks", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)

	pad := int(data[len(data)-1])
	if pad < 1 || pad > aes.BlockSize ||
		!bytes.Equal(data[len(data)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errSegmentPadding
	}
	return data[:len(data)-pad], nil
}
