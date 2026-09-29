package download

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/klauspost/compress/zstd"
)

// The queue file is zstd-compressed JSON. It carries a line for every file
// of every job and almost every line repeats the one before it — the same
// folder, the same status, a name differing by a digit — so it packs about
// fifteen to one: a queue of twenty thousand files goes from five megabytes
// to a few hundred kilobytes. JSON underneath, so `zstdcat` then `jq` reads
// it.
//
// Reading goes by content, not by name. A file that does not open with
// zstd's magic number is the plain JSON every earlier build wrote, and is
// read as such: an upgrade keeps its queue, and a file somebody decompressed
// by hand to look at still loads.

// zstdMagic opens every zstd frame.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// stateCodec is built once and shared: both halves are safe for concurrent
// use through EncodeAll and DecodeAll, and building them allocates tables
// worth keeping. The decoder is bounded, so a corrupt or hostile file
// cannot ask for gigabytes on the way to failing to parse.
var stateCodec = sync.OnceValues(func() (*zstd.Encoder, *zstd.Decoder) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		panic("download: zstd encoder: " + err.Error())
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(config.MaxStateBytes))
	if err != nil {
		panic("download: zstd decoder: " + err.Error())
	}
	return enc, dec
})

// decodeState returns the JSON a queue file holds, compressed or not.
func decodeState(body []byte) ([]byte, error) {
	if !bytes.HasPrefix(body, zstdMagic) {
		return body, nil
	}
	_, dec := stateCodec()
	return dec.DecodeAll(body, nil)
}

// legacyStatePath is where the queue was kept before it was compressed:
// the same name without ".zst". Only a default-style path has one; a path
// named explicitly is read and written as named, whatever it holds.
func legacyStatePath(path string) (string, bool) {
	if !strings.HasSuffix(path, ".zst") {
		return "", false
	}
	return strings.TrimSuffix(path, ".zst"), true
}

// stateVersion marks the on-disk shape. A file written by a newer build is
// left alone rather than guessed at: starting with an empty queue costs one
// re-add, while misreading a file could re-queue the wrong things.
const stateVersion = 1

// savedState is the queue as it is written to disk.
//
// What is *not* here is as deliberate as what is. No item URL is stored:
// several hosts hand out links signed for twenty minutes or so, and the ones
// that do carry no URL at all until a resolver mints one at download time —
// a closure, which no file can hold. A stale link would fail every item of
// an otherwise resumable job, so an unfinished job is resolved afresh
// instead, and the part files already on disk carry the bytes.
type savedState struct {
	Version int        `json:"version"`
	Jobs    []savedJob `json:"jobs"`
	Saved   time.Time  `json:"saved"`
}

type savedJob struct {
	ID        string      `json:"id"`
	Source    string      `json:"source"`
	Title     string      `json:"title"`
	Host      string      `json:"host"`
	Password  string      `json:"password,omitempty"`
	Err       string      `json:"error,omitempty"`
	CreatedAt time.Time   `json:"createdAt"`
	Canceled  bool        `json:"canceled,omitempty"`
	Items     []savedItem `json:"items"`
}

type savedItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Dir  string `json:"dir,omitempty"`
	Path string `json:"path,omitempty"`
	// Status is the outcome to restore. Anything that was in flight is
	// written down as queued: a transfer the process did not live to finish
	// had not finished.
	Status  Status `json:"status"`
	Size    int64  `json:"size"`
	Err     string `json:"error,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
}

// loadState reads the queue left by a previous run, and reports which file
// it came from: path, or where the queue was kept before it was compressed
// when path does not exist yet.
//
// Every failure here returns an empty queue and an error to log rather than
// stopping the program: a state file is a convenience, and refusing to start
// because one is corrupt would turn a lost queue into a lost service.
func loadState(path string) (st *savedState, from string, err error) {
	empty := &savedState{Version: stateVersion}

	from = path
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if legacy, ok := legacyStatePath(path); ok {
			if old, lerr := os.ReadFile(legacy); lerr == nil || !os.IsNotExist(lerr) {
				body, err, from = old, lerr, legacy
			}
		}
	}
	if err != nil {
		if os.IsNotExist(err) {
			return empty, "", nil
		}
		return empty, "", err
	}

	body, err = decodeState(body)
	if err != nil {
		return empty, "", fmt.Errorf("decompress %s: %w", from, err)
	}
	var saved savedState
	if err := json.Unmarshal(body, &saved); err != nil {
		return empty, "", fmt.Errorf("parse %s: %w", from, err)
	}
	if saved.Version != stateVersion {
		return empty, "",
			fmt.Errorf("%s is version %d, this build writes %d", from, saved.Version, stateVersion)
	}
	return &saved, from, nil
}

// saveState writes the queue, atomically and to the owner alone.
//
// Atomically because a half-written file is worse than none: the next start
// would parse what it could and silently lose the rest. To the owner alone
// because it carries the addresses of everything being downloaded, and the
// passwords for those that need one.
func saveState(path string, st *savedState) error {
	st.Version = stateVersion

	plain, err := json.Marshal(st)
	if err != nil {
		return err
	}
	enc, _ := stateCodec()
	body := enc.EncodeAll(plain, make([]byte, 0, len(plain)/8))

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename below has succeeded

	// Before the content, so the passwords are never briefly world-readable.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	// Flushed before the rename: the rename is what makes the file the
	// queue, and a crash between the two would otherwise publish a name
	// pointing at nothing.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// fingerprint summarises what a save would record, so an idle queue is not
// rewritten every interval. Persisted fields only — a transfer's byte
// counter moves constantly and is never written down, since the part file on
// disk is the authority on how far it got.
func (st *savedState) fingerprint() uint64 {
	h := fnv.New64a()
	// Hash every persisted field, with JSON's unambiguous boundaries. Paths,
	// titles and skip outcomes can change without a status or size changing.
	// Saved is deliberately excluded: time alone is not a queue change.
	_ = json.NewEncoder(h).Encode(st.Jobs)
	return h.Sum64()
}
