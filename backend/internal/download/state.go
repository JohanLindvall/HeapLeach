package download

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

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

// loadState reads the queue left by a previous run.
//
// Every failure here returns an empty queue and an error to log rather than
// stopping the program: a state file is a convenience, and refusing to start
// because one is corrupt would turn a lost queue into a lost service.
func loadState(path string) (*savedState, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &savedState{Version: stateVersion}, nil
		}
		return &savedState{Version: stateVersion}, err
	}

	var st savedState
	if err := json.Unmarshal(body, &st); err != nil {
		return &savedState{Version: stateVersion}, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Version != stateVersion {
		return &savedState{Version: stateVersion},
			fmt.Errorf("%s is version %d, this build writes %d", path, st.Version, stateVersion)
	}
	return &st, nil
}

// saveState writes the queue, atomically and to the owner alone.
//
// Atomically because a half-written file is worse than none: the next start
// would parse what it could and silently lose the rest. To the owner alone
// because it carries the addresses of everything being downloaded, and the
// passwords for those that need one.
func saveState(path string, st *savedState) error {
	st.Version = stateVersion

	body, err := json.Marshal(st)
	if err != nil {
		return err
	}

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
// rewritten every interval. Statuses and identities only — a transfer's byte
// counter moves constantly and is never written down, since the part file on
// disk is the authority on how far it got.
func (st *savedState) fingerprint() uint64 {
	h := fnv.New64a()
	for _, job := range st.Jobs {
		h.Write([]byte(job.ID))
		h.Write([]byte(job.Err))
		h.Write([]byte(strconv.FormatBool(job.Canceled)))
		for _, it := range job.Items {
			h.Write([]byte(it.ID))
			h.Write([]byte(it.Status))
			h.Write([]byte(it.Err))
			h.Write([]byte(strconv.FormatInt(it.Size, 10)))
		}
	}
	return h.Sum64()
}
