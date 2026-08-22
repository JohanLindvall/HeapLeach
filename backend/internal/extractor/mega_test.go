package extractor

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// Every value here is synthetic: handles are runs of one letter and keys are
// counted bytes, so nothing in this file points at anything real. What is
// under test is arithmetic over those bytes, which does not care.

func megaKeyString(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func syntheticFileKey() []byte {
	k := make([]byte, megaFileKeyLen)
	for i := range k {
		k[i] = byte(i * 7)
	}
	return k
}

func syntheticFolderKey() []byte {
	k := make([]byte, megaFolderKeyLen)
	for i := range k {
		k[i] = byte(200 - i)
	}
	return k
}

func TestParseMegaLink(t *testing.T) {
	fileKey := megaKeyString(syntheticFileKey())
	folderKey := megaKeyString(syntheticFolderKey())

	tests := []struct {
		name       string
		raw        string
		wantFolder bool
		wantHandle string
		wantNode   string
	}{
		{
			name:       "file",
			raw:        "https://mega.nz/file/AAAAAAAA#" + fileKey,
			wantHandle: "AAAAAAAA",
		},
		{
			name:       "embed",
			raw:        "https://mega.nz/embed/AAAAAAAA#" + fileKey,
			wantHandle: "AAAAAAAA",
		},
		{
			name:       "legacy file",
			raw:        "https://mega.nz/#!AAAAAAAA!" + fileKey,
			wantHandle: "AAAAAAAA",
		},
		{
			name:       "old domain",
			raw:        "https://mega.co.nz/file/AAAAAAAA#" + fileKey,
			wantHandle: "AAAAAAAA",
		},
		{
			name:       "folder",
			raw:        "https://mega.nz/folder/BBBBBBBB#" + folderKey,
			wantFolder: true,
			wantHandle: "BBBBBBBB",
		},
		{
			name:       "file inside a folder",
			raw:        "https://mega.nz/folder/BBBBBBBB#" + folderKey + "/file/CCCCCCCC",
			wantFolder: true,
			wantHandle: "BBBBBBBB",
			wantNode:   "CCCCCCCC",
		},
		{
			name:       "legacy folder",
			raw:        "https://mega.nz/#F!BBBBBBBB!" + folderKey,
			wantFolder: true,
			wantHandle: "BBBBBBBB",
		},
		{
			name:       "legacy folder with a node",
			raw:        "https://mega.nz/#F!BBBBBBBB!" + folderKey + "!CCCCCCCC",
			wantFolder: true,
			wantHandle: "BBBBBBBB",
			wantNode:   "CCCCCCCC",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := ParseURL(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			link, err := parseMegaLink(u)
			if err != nil {
				t.Fatalf("parseMegaLink: %v", err)
			}
			if link.folder != tc.wantFolder {
				t.Errorf("folder = %v, want %v", link.folder, tc.wantFolder)
			}
			if link.handle != tc.wantHandle {
				t.Errorf("handle = %q, want %q", link.handle, tc.wantHandle)
			}
			if link.node != tc.wantNode {
				t.Errorf("node = %q, want %q", link.node, tc.wantNode)
			}
			want := megaFileKeyLen
			if tc.wantFolder {
				want = megaFolderKeyLen
			}
			if len(link.key) != want {
				t.Errorf("key is %d bytes, want %d", len(link.key), want)
			}
		})
	}
}

func TestParseMegaLinkRejectsUnusableLinks(t *testing.T) {
	fileKey := megaKeyString(syntheticFileKey())

	tests := []struct {
		name string
		raw  string
		// hint is a fragment the message must mention, so the user is told
		// which of these it was.
		hint string
	}{
		{name: "no key at all", raw: "https://mega.nz/file/AAAAAAAA", hint: "key"},
		{name: "folder key on a file link", raw: "https://mega.nz/file/AAAAAAAA#" + megaKeyString(syntheticFolderKey()), hint: "bytes"},
		{name: "not a share link", raw: "https://mega.nz/register", hint: "not a file or folder"},
		{name: "malformed key", raw: "https://mega.nz/file/AAAAAAAA#not valid base64!!", hint: "key"},
		{name: "no handle", raw: "https://mega.nz/file/#" + fileKey, hint: "not a file or folder"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := ParseURL(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			_, err = parseMegaLink(u)
			if err == nil {
				t.Fatal("parseMegaLink succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.hint) {
				t.Errorf("error %q does not mention %q", err, tc.hint)
			}
		})
	}
}

func TestMegaFileKeySplitsIntoItsParts(t *testing.T) {
	raw := syntheticFileKey()
	key, nonce, mac, err := megaFileKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 16 || len(nonce) != 8 || len(mac) != 8 {
		t.Fatalf("parts are %d/%d/%d bytes, want 16/8/8", len(key), len(nonce), len(mac))
	}
	for i := range key {
		if want := raw[i] ^ raw[i+16]; key[i] != want {
			t.Fatalf("key[%d] = %d, want %d", i, key[i], want)
		}
	}
	if !bytes.Equal(nonce, raw[16:24]) || !bytes.Equal(mac, raw[24:32]) {
		t.Error("the nonce and mac do not come from the second half of the key")
	}

	if _, _, _, err := megaFileKey(raw[:20]); err == nil {
		t.Error("a short key was accepted")
	}
}

func TestMegaAttrsRoundTrip(t *testing.T) {
	key, _, _, err := megaFileKey(syntheticFileKey())
	if err != nil {
		t.Fatal(err)
	}
	const name = "a synthetic clip.mp4"

	attrs, err := megaDecryptAttrs(encryptMegaAttrs(t, name, key), key)
	if err != nil {
		t.Fatalf("megaDecryptAttrs: %v", err)
	}
	if attrs.Name != name {
		t.Errorf("name = %q, want %q", attrs.Name, name)
	}
}

// TestMegaAttrsRejectsWrongKey covers what the "MEGA" prefix is there for:
// with the wrong key the plaintext is noise, and noise must be reported
// rather than passed off as a filename.
func TestMegaAttrsRejectsWrongKey(t *testing.T) {
	key, _, _, err := megaFileKey(syntheticFileKey())
	if err != nil {
		t.Fatal(err)
	}
	encoded := encryptMegaAttrs(t, "a synthetic clip.mp4", key)

	wrong := append([]byte(nil), key...)
	wrong[0]++
	if _, err := megaDecryptAttrs(encoded, wrong); err == nil {
		t.Fatal("attributes decrypted under the wrong key")
	}
}

func TestMegaUnwrapKeyRoundTrip(t *testing.T) {
	share := syntheticFolderKey()
	node := syntheticFileKey()

	block, err := aes.NewCipher(share)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := make([]byte, len(node))
	for i := 0; i < len(node); i += aes.BlockSize {
		block.Encrypt(wrapped[i:i+aes.BlockSize], node[i:i+aes.BlockSize])
	}

	got, err := megaUnwrapKey(wrapped, share)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, node) {
		t.Error("the unwrapped key does not match the one that was wrapped")
	}

	if _, err := megaUnwrapKey(wrapped[:17], share); err == nil {
		t.Error("a partial block was accepted")
	}
	if _, err := megaUnwrapKey(wrapped, share[:8]); err == nil {
		t.Error("a short share key was accepted")
	}
}

// TestMegaErrorCode covers mega's habit of answering with a bare number
// where a result belongs, which must not be read as one.
func TestMegaErrorCode(t *testing.T) {
	tests := []struct {
		raw      string
		wantCode int
		wantBad  bool
	}{
		{raw: `-9`, wantCode: -9, wantBad: true},
		{raw: `-17`, wantCode: -17, wantBad: true},
		{raw: `0`, wantBad: false},
		{raw: `{"g":"x"}`, wantBad: false},
		{raw: `[{"g":"x"}]`, wantBad: false},
	}
	for _, tc := range tests {
		code, bad := megaErrorCode(json.RawMessage(tc.raw))
		if bad != tc.wantBad || (bad && code != tc.wantCode) {
			t.Errorf("megaErrorCode(%s) = %d, %v; want %d, %v", tc.raw, code, bad, tc.wantCode, tc.wantBad)
		}
	}
}

func TestMegaAPIErrorExplainsQuota(t *testing.T) {
	if msg := megaAPIError(-17).Error(); !strings.Contains(msg, "quota") {
		t.Errorf("the quota code reads %q, which does not say so", msg)
	}
	if msg := megaAPIError(-99).Error(); !strings.Contains(msg, "-99") {
		t.Errorf("an unknown code reads %q, which does not name it", msg)
	}
}

// TestMegaFieldToleratesNonStrings covers listings where mega sends a number
// in place of a string for an entry it cannot describe.
func TestMegaFieldToleratesNonStrings(t *testing.T) {
	var node struct {
		Attrs megaField `json:"a"`
	}
	if err := json.Unmarshal([]byte(`{"a":-1}`), &node); err != nil {
		t.Fatalf("a numeric attribute broke the whole listing: %v", err)
	}
	if node.Attrs != "" {
		t.Errorf("attrs = %q, want it empty", node.Attrs)
	}
	if err := json.Unmarshal([]byte(`{"a":"abc"}`), &node); err != nil || node.Attrs != "abc" {
		t.Errorf("a string attribute did not survive: %q, %v", node.Attrs, err)
	}
}

func TestMegaMatch(t *testing.T) {
	m := NewMega(nil)
	for _, host := range []string{"mega.nz", "www.mega.nz", "mega.co.nz"} {
		u := &url.URL{Scheme: "https", Host: host, Path: "/file/AAAAAAAA"}
		if !m.Match(u) {
			t.Errorf("%s was not matched", host)
		}
	}
	if m.Match(&url.URL{Scheme: "https", Host: "notmega.nz"}) {
		t.Error("a lookalike host was matched")
	}
}

// encryptMegaAttrs builds an attribute blob the way mega does: the magic
// prefix, the JSON, zero padding to a block boundary, CBC under a zero IV.
func encryptMegaAttrs(t *testing.T, name string, key []byte) string {
	t.Helper()
	body, err := json.Marshal(megaAttrs{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	plain := append(append([]byte(nil), megaAttrMagic...), body...)
	if pad := len(plain) % aes.BlockSize; pad != 0 {
		plain = append(plain, make([]byte, aes.BlockSize-pad)...)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCEncrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(plain, plain)
	return base64.RawURLEncoding.EncodeToString(plain)
}
