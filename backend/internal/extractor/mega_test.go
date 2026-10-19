// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
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

// megaTestAPI serves mega's API from answer, which is given each command and
// returns its result.
func megaTestAPI(t *testing.T, answer func(megaCommand) any) *Mega {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []megaCommand
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil || len(batch) != 1 {
			t.Errorf("not a batch of one command: %v", err)
			http.Error(w, "bad batch", http.StatusBadRequest)
			return
		}
		w.Header().Set(httpx.HeaderContentType, httpx.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode([]any{answer(batch[0])})
	}))
	t.Cleanup(srv.Close)
	m := NewMega(httpx.New("test-agent", "en-US", 0, 5*time.Second))
	m.endpoint = srv.URL
	return m
}

func megaRoute(m *Mega, id string) context.Context {
	return httpx.WithRoute(context.Background(), id, m.links, nil)
}

// An address whose quota is used up is told so, and how long is left,
// where a link would be. That is the address's wait, kept so that it is not
// asked again before then, and no other address's: a route has a quota of
// its own. Nor does it keep a file from being added, since what the file is
// can still be asked.
func TestMegaQuotaIsAWaitForTheAddressAlone(t *testing.T) {
	key, _, _, err := megaFileKey(syntheticFileKey())
	if err != nil {
		t.Fatal(err)
	}
	attrs := encryptMegaAttrs(t, "First Clip.mp4", key)
	var links, infos atomic.Int32
	m := megaTestAPI(t, func(cmd megaCommand) any {
		if cmd.G == 0 {
			infos.Add(1)
			return map[string]any{"s": 4, "at": attrs}
		}
		if links.Add(1) == 1 {
			return map[string]any{"e": -17, "tl": 600}
		}
		return map[string]any{"g": "http://storage.example.test/dl/token", "s": 4, "at": attrs}
	})

	u, err := ParseURL("https://mega.nz/file/AAAAAAAA#" + megaKeyString(syntheticFileKey()))
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Extract(context.Background(), u, Options{})
	if err != nil {
		t.Fatalf("an address without quota could not add the file: %v", err)
	}
	f := res.Files[0]
	if f.Name != "First Clip.mp4" || f.Size != 4 || f.URL != "" || infos.Load() != 1 {
		t.Fatalf("added %q of %d bytes at %q after %d descriptions; want the file described once, with no link",
			f.Name, f.Size, f.URL, infos.Load())
	}
	if f.Pace == nil || !f.Pace.DirectFirst || !f.Pace.PerRoute || f.Pace.Group != "mega" || f.Refused == nil {
		t.Fatalf("pace %+v: the file would not move onto mega's routes", f.Pace)
	}

	_, err = f.Resolve(context.Background())
	wait, ok := errors.AsType[*WaitError](err)
	if !ok || time.Until(wait.Until) < 9*time.Minute || links.Load() != 1 {
		t.Fatalf("the normal connection resolved to %v after %d link requests; want its wait, kept from the first",
			err, links.Load())
	}
	if _, err := f.Resolve(megaRoute(m, httpx.DirectRoute)); !isWait(err) || links.Load() != 1 {
		t.Fatalf("the normal connection leased as a route asked again (%v)", err)
	}
	target, err := f.Resolve(megaRoute(m, "another"))
	if err != nil || target.URL != "https://storage.example.test/dl/token" || links.Load() != 2 {
		t.Fatalf("another address resolved to %+v, %v; want a link of its own", target, err)
	}
}

// A storage server refuses a transfer once the address has used up its
// quota, with the seconds left in a header of its own, and that route is not
// asked for a link again until then.
func TestMegaStorageRefusalIsTheRoutesWait(t *testing.T) {
	var links atomic.Int32
	m := megaTestAPI(t, func(megaCommand) any {
		links.Add(1)
		return map[string]any{"g": "https://storage.example.test/dl/token", "s": 4}
	})
	if err := m.refused(megaRoute(m, "a"), http.StatusNotFound, nil); err != nil {
		t.Errorf("a 404 was read as %v", err)
	}

	header := http.Header{}
	header.Set(megaHeaderTimeLeft, "5400")
	wait, ok := errors.AsType[*WaitError](m.refused(megaRoute(m, "a"), megaStatusOverQuota, header))
	if !ok || !strings.Contains(wait.Reason, "quota") {
		t.Fatalf("509 was read as %v; want the route's wait for its quota", wait)
	}
	if left := time.Until(wait.Until); left < 89*time.Minute || left > 91*time.Minute {
		t.Errorf("the route waits %s; want the 90 minutes mega named", left.Round(time.Minute))
	}
	wait, ok = errors.AsType[*WaitError](m.refused(megaRoute(m, "b"), megaStatusOverQuota, nil))
	if !ok || time.Until(wait.Until) < config.MegaQuotaWait-time.Minute {
		t.Errorf("509 naming no time left was read as %v; want MegaQuotaWait", wait)
	}

	resolve := m.resolver("", megaCommand{A: "g", G: 1, SSL: 2, P: "AAAAAAAA"}, "First Clip.mp4")
	if _, err := resolve(megaRoute(m, "a")); !isWait(err) || links.Load() != 0 {
		t.Fatalf("the refused route resolved to %v after %d link requests; want its wait, unasked", err, links.Load())
	}
	if _, err := resolve(megaRoute(m, "c")); err != nil || links.Load() != 1 {
		t.Fatalf("another route resolved to %v after %d link requests", err, links.Load())
	}
}

// The code alone, in place of an answer, says the quota is used up without
// saying for how long, which waits the default.
func TestMegaQuotaCodeAloneWaitsTheDefault(t *testing.T) {
	m := megaTestAPI(t, func(megaCommand) any { return -17 })
	_, err := m.link(megaRoute(m, "a"), "", megaCommand{A: "g", G: 1, SSL: 2, P: "AAAAAAAA"})
	wait, ok := errors.AsType[*WaitError](err)
	if !ok || time.Until(wait.Until) < config.MegaQuotaWait-time.Minute {
		t.Fatalf("a bare -17 was read as %v; want MegaQuotaWait", err)
	}
}

// Congestion and a limit on an address's requests may well not be met by
// the next request, through another route, so neither fails the file there.
func TestMegaCongestionIsTransient(t *testing.T) {
	for _, answer := range []any{map[string]any{"e": -3}, -4} {
		m := megaTestAPI(t, func(megaCommand) any { return answer })
		_, err := m.link(megaRoute(m, "a"), "", megaCommand{A: "g", G: 1, SSL: 2, P: "AAAAAAAA"})
		if _, ok := errors.AsType[*TransientError](err); !ok {
			t.Errorf("%v was read as %v; want a transient failure", answer, err)
		}
	}
	m := megaTestAPI(t, func(megaCommand) any { return map[string]any{"e": -9} })
	_, err := m.link(megaRoute(m, "a"), "", megaCommand{A: "g", G: 1, SSL: 2, P: "AAAAAAAA"})
	if _, ok := errors.AsType[*TransientError](err); ok || err == nil {
		t.Errorf("a missing file was read as %v; want it to fail", err)
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
