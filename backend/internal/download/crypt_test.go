package download

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	mrand "math/rand/v2"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// encryptCTR produces what a host serving this payload would send: AES-CTR
// from a counter of nonce followed by the block index, which is mega's
// scheme and the one writerFor undoes.
func encryptCTR(t *testing.T, plain, key, nonce []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, aes.BlockSize)
	copy(iv, nonce)
	out := make([]byte, len(plain))
	cipher.NewCTR(block, iv).XORKeyStream(out, plain)
	return out
}

func testCipher(t *testing.T) (*extractor.StreamCipher, []byte, []byte) {
	t.Helper()
	key := make([]byte, 16)
	nonce := make([]byte, 8)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return &extractor.StreamCipher{Key: key, Nonce: nonce}, key, nonce
}

// TestDecryptingWriterIsOrderIndependent is the property the whole design
// rests on: eight connections write their ranges whenever they happen to
// arrive, in pieces of whatever size the network hands over, and the file
// has to come out identical to a single sequential pass.
func TestDecryptingWriterIsOrderIndependent(t *testing.T) {
	sc, key, nonce := testCipher(t)
	plain := make([]byte, 512<<10)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	encrypted := encryptCTR(t, plain, key, nonce)

	// Chop the payload into pieces of deliberately awkward sizes, so writes
	// land mid-block far more often than a real transfer would manage.
	type piece struct {
		off  int64
		data []byte
	}
	var pieces []piece
	for off := 0; off < len(encrypted); {
		n := 1 + mrand.IntN(9999)
		if off+n > len(encrypted) {
			n = len(encrypted) - off
		}
		pieces = append(pieces, piece{off: int64(off), data: encrypted[off : off+n]})
		off += n
	}
	mrand.Shuffle(len(pieces), func(i, j int) { pieces[i], pieces[j] = pieces[j], pieces[i] })

	path := filepath.Join(t.TempDir(), "out.part")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	dst, err := writerFor(f, sc)
	if err != nil {
		t.Fatal(err)
	}

	// Concurrently, too: the scratch buffers are pooled and the keystream
	// is derived per call, so nothing may be shared between writers.
	var wg sync.WaitGroup
	for _, p := range pieces {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := dst.WriteAt(p.data, p.off); err != nil || n != len(p.data) {
				t.Errorf("WriteAt(%d bytes at %d) = %d, %v", len(p.data), p.off, n, err)
			}
		}()
	}
	wg.Wait()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("decrypted file differs from the plaintext")
	}
}

// TestDecryptingWriterLeavesCallerBufferAlone guards the io.WriterAt
// contract. Both callers reuse one read buffer for a whole transfer, so
// decrypting in place would corrupt the next read rather than merely being
// impolite.
func TestDecryptingWriterLeavesCallerBufferAlone(t *testing.T) {
	sc, key, nonce := testCipher(t)
	plain := []byte("the quick brown fox jumps over the lazy dog, twice over")
	encrypted := encryptCTR(t, plain, key, nonce)

	f, err := os.CreateTemp(t.TempDir(), "part")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dst, err := writerFor(f, sc)
	if err != nil {
		t.Fatal(err)
	}

	buf := append([]byte(nil), encrypted...)
	if _, err := dst.WriteAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, encrypted) {
		t.Error("WriteAt modified the caller's slice")
	}
}

// TestWriterForPassesPlainFilesThrough keeps the common case free: without a
// cipher the part file is written to directly, with nothing in between.
func TestWriterForPassesPlainFilesThrough(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "part")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	dst, err := writerFor(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dst != any(f) {
		t.Error("a plain transfer was given a wrapper it does not need")
	}
}

func TestWriterForRejectsUnusableCipher(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "part")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for name, sc := range map[string]*extractor.StreamCipher{
		"short key":  {Key: make([]byte, 7), Nonce: make([]byte, 8)},
		"long nonce": {Key: make([]byte, 16), Nonce: make([]byte, 24)},
	} {
		if _, err := writerFor(f, sc); err == nil {
			t.Errorf("%s: writerFor succeeded, want an error", name)
		}
	}
}

// TestEncryptedTransferOverManyConnections runs the real transfer path over
// an encrypted payload: ranges, splitting and the part file all as usual,
// with only the writer differing. What lands on disk must be plaintext at
// the length the host stated.
func TestEncryptedTransferOverManyConnections(t *testing.T) {
	sc, key, nonce := testCipher(t)
	// Comfortably over MinSplitFileSize, and paced slowly enough that the
	// supervisor has time to decide another connection would help.
	plain := make([]byte, 12<<20)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	server := &rangeServer{payload: encryptCTR(t, plain, key, nonce), perChunk: 2 * time.Millisecond}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	manager, item := testTransferDeps(t, srv.URL)
	item.cipher = sc
	manager.timings = downloadTimings{
		poll:     5 * time.Millisecond,
		probe:    25 * time.Millisecond,
		cooldown: 25 * time.Millisecond,
		save:     20 * time.Millisecond,
	}

	part := filepath.Join(manager.cfg.DownloadDir, "clip.bin.part")
	if _, err := manager.transferOnce(context.Background(), item, part, "clip.bin"); err != nil {
		t.Fatalf("encrypted transfer failed: %v", err)
	}

	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("part file holds %d bytes that do not match the %d-byte plaintext", len(got), len(plain))
	}
	if server.peak.Load() < 2 {
		t.Error("the transfer never split, so the out-of-order decryption path went untested")
	}
}

// TestEncryptedTransferResumes covers the property that makes decrypting on
// the way in worth doing at all: the part file holds plaintext, so a second
// run picks up from its length with a Range request and the keystream lines
// up with the offset by itself.
func TestEncryptedTransferResumes(t *testing.T) {
	sc, key, nonce := testCipher(t)
	plain := make([]byte, 1<<20)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	encrypted := encryptCTR(t, plain, key, nonce)
	server := &rangeServer{payload: encrypted}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	manager, item := testTransferDeps(t, srv.URL)
	item.cipher = sc

	// Stand in for an interrupted run: the first third, already decrypted.
	const done = 1 << 18
	part := filepath.Join(manager.cfg.DownloadDir, "clip.bin.part")
	if err := os.WriteFile(part, plain[:done], 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.transferOnce(context.Background(), item, part, "clip.bin"); err != nil {
		t.Fatalf("resumed encrypted transfer failed: %v", err)
	}
	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("the resumed file does not match the plaintext")
	}
}
