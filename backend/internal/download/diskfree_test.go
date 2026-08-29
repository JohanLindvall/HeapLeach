package download

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

func TestDiskSpaceMeasuresTheFilesystem(t *testing.T) {
	dir := t.TempDir()

	free, total, err := diskSpace(dir)
	if err != nil {
		t.Fatalf("measuring %s: %v", dir, err)
	}
	if total <= 0 {
		t.Errorf("filesystem size reported as %d", total)
	}
	if free < 0 || free > total {
		t.Errorf("free = %d, which is not a share of %d", free, total)
	}
}

// A destination that cannot be read must report nothing rather than zero:
// zero free is what a full disk reports, and the UI draws it as a warning.
func TestDiskSpaceOnAPathThatIsNotThere(t *testing.T) {
	_, total, err := diskSpace(filepath.Join(t.TempDir(), "gone"))
	if err == nil {
		t.Error("measuring a missing directory should fail")
	}
	if total != 0 {
		t.Errorf("total = %d for a path that does not exist, want 0", total)
	}
}

func TestSampleDiskKeepsToItsOwnCadence(t *testing.T) {
	m := &Manager{dir: t.TempDir()}
	start := time.Now()

	m.sampleDisk(start)
	if m.diskFree.Load() <= 0 || m.diskTotal.Load() <= 0 {
		t.Fatalf("first sample read nothing: free=%d total=%d", m.diskFree.Load(), m.diskTotal.Load())
	}
	if !m.dirty.Load() {
		t.Error("a figure arriving where there was none is worth publishing")
	}

	// Inside the interval nothing is re-read, which is the point: Statfs is
	// a syscall and the destination may be a network mount.
	m.diskFree.Store(-1)
	m.dirty.Store(false)
	m.sampleDisk(start.Add(config.DiskSampleInterval / 2))
	if m.diskFree.Load() != -1 {
		t.Error("re-measured inside its own interval")
	}
	if m.dirty.Load() {
		t.Error("marked dirty without having measured anything")
	}

	// Past it, it looks again.
	m.sampleDisk(start.Add(config.DiskSampleInterval + time.Second))
	if m.diskFree.Load() <= 0 {
		t.Errorf("did not re-measure after the interval: free=%d", m.diskFree.Load())
	}
}

// An unreadable destination reports nothing at all, and says so by leaving
// the total at zero — the one thing that distinguishes it from a full disk.
func TestSampleDiskReportsNothingForAnUnreadableDir(t *testing.T) {
	m := &Manager{dir: filepath.Join(t.TempDir(), "gone")}
	m.sampleDisk(time.Now())
	if m.diskFree.Load() != 0 || m.diskTotal.Load() != 0 {
		t.Errorf("free=%d total=%d, want both zero", m.diskFree.Load(), m.diskTotal.Load())
	}
}
