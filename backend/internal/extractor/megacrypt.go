package extractor

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Mega's client-side cryptography, kept apart from the API plumbing in
// mega.go the way kvs.go is kept apart from its hosts.
//
// Mega stores ciphertext and never holds a key, so everything readable about
// a link — its filenames, its sizes, its bytes — is decrypted here from the
// key in the URL fragment. Three primitives are involved:
//
//   - Keys travel wrapped in AES-ECB. Each node's key is unwrapped with the
//     key of the share it belongs to and nothing chains further, so there is
//     no IV and no ordering to get right.
//   - Attributes, the filename among them, are AES-CBC under a zero IV,
//     zero-padded, and prefixed with the literal "MEGA". That prefix is what
//     makes a wrong key recognisable instead of merely producing nonsense.
//   - File payload is AES-CTR, and that is the one that matters to the rest
//     of the program: CTR is a stream cipher, so every byte range decrypts
//     independently of every other, and an encrypted download keeps ranged
//     resume and the multi-connection engine exactly as they are.

const (
	// megaFileKeyLen is the key in a file link: an AES key and a nonce,
	// folded together, followed by the plaintext's condensed MAC.
	megaFileKeyLen = 32
	// megaFolderKeyLen is the key in a folder link, which unwraps the keys
	// of the nodes inside it.
	megaFolderKeyLen = 16
)

// megaAttrMagic prefixes decrypted attributes. Mega checks for it and so do
// we: with a wrong key the CBC output is noise, and noise that happens to
// parse as JSON would otherwise be taken for a filename.
var megaAttrMagic = []byte("MEGA")

// megaB64 decodes mega's unpadded URL-safe base64.
func megaB64(s string) ([]byte, error) { return util.DecodeBase64(s) }

// megaFileKey splits the key from a file link into its three parts: the AES
// key that unlocks both the attributes and the payload, the nonce seeding
// the payload's counter, and the condensed MAC of the plaintext.
//
// The MAC is returned for completeness and not verified. Checking it means
// running a chunked CBC-MAC over the plaintext in order, which a transfer
// that writes several ranges at once and resumes across runs cannot produce
// as it goes; it would have to re-read the finished file. Mega's transport
// is already TLS with a length the API states in advance, so the practical
// gain is small.
func megaFileKey(k []byte) (key, nonce, mac []byte, err error) {
	if len(k) != megaFileKeyLen {
		return nil, nil, nil, fmt.Errorf("file key is %d bytes, expected %d", len(k), megaFileKeyLen)
	}
	key = make([]byte, aes.BlockSize)
	for i := range key {
		key[i] = k[i] ^ k[i+aes.BlockSize]
	}
	return key, k[16:24], k[24:32], nil
}

// megaUnwrapKey decrypts a node's key with the key of the share holding it.
//
// ECB deserves the suspicion it usually attracts, but what it encrypts here
// is a key rather than a message: one block, uniformly random, never
// repeated across nodes. The pattern leakage ECB is avoided for has nothing
// to work with.
func megaUnwrapKey(wrapped, share []byte) ([]byte, error) {
	if len(share) != megaFolderKeyLen {
		return nil, fmt.Errorf("share key is %d bytes, expected %d", len(share), megaFolderKeyLen)
	}
	if len(wrapped) == 0 || len(wrapped)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("wrapped key is %d bytes, not a whole number of blocks", len(wrapped))
	}
	block, err := aes.NewCipher(share)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(wrapped))
	for i := 0; i < len(wrapped); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], wrapped[i:i+aes.BlockSize])
	}
	return out, nil
}

// megaAttrs is the decrypted attribute blob. Mega puts several fields in
// there; the name is the only one this program has a use for.
type megaAttrs struct {
	Name string `json:"n"`
}

// megaDecryptAttrs decrypts a node's attributes with its own key. A key that
// does not belong to this node is reported as such rather than returning
// whatever the plaintext happened to look like.
func megaDecryptAttrs(encoded string, key []byte) (megaAttrs, error) {
	raw, err := megaB64(encoded)
	if err != nil {
		return megaAttrs{}, fmt.Errorf("attributes are not valid base64: %w", err)
	}
	if len(raw)%aes.BlockSize != 0 {
		return megaAttrs{}, fmt.Errorf("attributes are %d bytes, not a whole number of blocks", len(raw))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return megaAttrs{}, err
	}
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(raw, raw)

	raw = bytes.TrimRight(raw, "\x00")
	if !bytes.HasPrefix(raw, megaAttrMagic) {
		return megaAttrs{}, errors.New("attributes did not decrypt: wrong key for this node")
	}
	var attrs megaAttrs
	if err := json.Unmarshal(raw[len(megaAttrMagic):], &attrs); err != nil {
		return megaAttrs{}, fmt.Errorf("read attributes: %w", err)
	}
	return attrs, nil
}
