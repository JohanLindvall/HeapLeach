package download

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/HeapLeach/internal/extractor"
)

// encryptSegment is what a packager does to one segment: PKCS#7 padding,
// then AES-128-CBC.
func encryptSegment(t *testing.T, plain, key, iv []byte) []byte {
	t.Helper()
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, data)
	return data
}

func TestDecryptSegmentRoundTrips(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("fedcba9876543210")
	for _, size := range []int{0, 1, 15, 16, 17, 188 * 7} {
		plain := bytes.Repeat([]byte{0x47}, size)
		got, err := decryptSegment(encryptSegment(t, plain, key, iv), key, iv)
		if err != nil {
			t.Fatalf("%d bytes: %v", size, err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("%d bytes: decrypted to %d bytes that differ", size, len(got))
		}
	}
}

// CBC decrypts anything; the padding is what says the key was wrong, and a
// wrong key must fail the transfer rather than write noise.
func TestDecryptSegmentRefusesTheWrongKey(t *testing.T) {
	iv := make([]byte, aes.BlockSize)
	data := encryptSegment(t, []byte("a segment of transport stream"), []byte("0123456789abcdef"), iv)
	if _, err := decryptSegment(data, []byte("not-the-real-key"), iv); !errors.Is(err, errSegmentPadding) {
		t.Errorf("wrong key gave %v, want the padding error", err)
	}
	if _, err := decryptSegment([]byte("short"), []byte("0123456789abcdef"), iv); err == nil {
		t.Error("a segment that is not whole blocks was accepted")
	}
}

// With no IV named, each segment's IV is its media sequence number; with
// one named, every segment uses it.
func TestSegmentIV(t *testing.T) {
	key := &extractor.SegmentKey{Sequence: 5}
	want := make([]byte, aes.BlockSize)
	want[15] = 7
	if got := segmentIV(key, 2); !bytes.Equal(got, want) {
		t.Errorf("IV for segment 2 from sequence 5 = %x, want %x", got, want)
	}
	named := &extractor.SegmentKey{IV: []byte("fedcba9876543210"), Sequence: 5}
	if got := segmentIV(named, 2); !bytes.Equal(got, named.IV) {
		t.Errorf("named IV ignored: %x", got)
	}
}

// End to end: an encrypted playlist is fetched, its key once, and joined
// into the plain stream — the sequence-numbered IVs counted from where the
// playlist says it starts.
func TestEncryptedPlaylistJoinsThePlainStream(t *testing.T) {
	const parts = 6
	const firstSequence = 40
	key := []byte("0123456789abcdef")
	var plain, served [][]byte
	for i := range parts {
		p := bytes.Repeat([]byte{byte(0x40 + i)}, 1000+i*37)
		plain = append(plain, p)
		iv := segmentIV(&extractor.SegmentKey{Sequence: firstSequence}, i)
		served = append(served, encryptSegment(t, p, key, iv))
	}

	var keyFetches atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/key", func(w http.ResponseWriter, _ *http.Request) {
		keyFetches.Add(1)
		_, _ = w.Write(key)
	})
	mux.HandleFunc("/seg/", func(w http.ResponseWriter, r *http.Request) {
		i, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/seg/"))
		if err != nil || i < 0 || i >= parts {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(served[i])
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := busyManager(t)
	m.streams = 3
	var segments []string
	for i := range parts {
		segments = append(segments, fmt.Sprintf("%s/seg/%d", srv.URL, i))
	}
	it := &Item{ID: newID(), Name: "clip.ts", URL: srv.URL, Size: -1, Segments: segments,
		SegmentKey: &extractor.SegmentKey{URI: srv.URL + "/key", Sequence: firstSequence}}

	if err := m.transfer(context.Background(), it); err != nil {
		t.Fatalf("encrypted playlist: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(m.cfg.DownloadDir, "clip.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Join(plain, nil); !bytes.Equal(got, want) {
		t.Fatalf("joined file is not the plain stream: %d bytes, want %d", len(got), len(want))
	}
	if n := keyFetches.Load(); n != 1 {
		t.Errorf("the key was fetched %d times, want once", n)
	}
}
