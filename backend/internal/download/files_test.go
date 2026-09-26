package download

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTransfersCannotWriteThroughEscapingSymlinks(t *testing.T) {
	for _, location := range []string{"directory", "part", "checkpoint"} {
		t.Run(location, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write([]byte("downloaded data"))
			}))
			defer server.Close()
			m, dir := newTestManager(t)
			outside := t.TempDir()
			victim := filepath.Join(outside, "untouched.bin")
			if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			item := &Item{ID: "item", Name: "payload.bin", URL: server.URL + "/payload.bin"}
			part := filepath.Join(dir, item.Name+"."+partSuffix(item.URL)+".part")
			link, target := part, victim
			switch location {
			case "directory":
				item.Dir = "album"
				link, target = filepath.Join(dir, item.Dir), outside
			case "checkpoint":
				link = statePath(part) + ".tmp"
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			err := m.transfer(context.Background(), item)
			if location != "checkpoint" && err == nil {
				t.Fatal("escaping symlink was accepted")
			}
			body, err := os.ReadFile(victim)
			if err != nil || string(body) != "original" {
				t.Fatalf("outside file changed: %q, %v", body, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 {
				t.Fatalf("download escaped: %v, %v", entries, err)
			}
		})
	}
}

func TestConcurrentCompletionsReserveDistinctNames(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files := transferFiles{root: root}
	var workers sync.WaitGroup
	paths := make(chan string, 40)
	for range cap(paths) {
		workers.Go(func() {
			path, err := files.reserve(dir, "payload.bin")
			if err != nil {
				t.Error(err)
				return
			}
			paths <- path
		})
	}
	workers.Wait()
	close(paths)
	seen := make(map[string]bool)
	for path := range paths {
		if seen[path] {
			t.Fatalf("two completions claimed %s", path)
		}
		seen[path] = true
	}
	if len(seen) != cap(paths) {
		t.Fatalf("reserved %d paths, want %d", len(seen), cap(paths))
	}
}

func TestCheckpointFailureStopsBeforeWritingFileData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("downloaded data"))
	}))
	defer server.Close()
	m, dir := newTestManager(t)
	item := &Item{ID: "item", Name: "payload.bin", URL: server.URL + "/payload.bin"}
	part := filepath.Join(dir, item.Name+"."+partSuffix(item.URL)+".part")
	// A nonempty directory cannot be cleared or replaced as a checkpoint.
	blocked := statePath(part) + ".tmp"
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "marker"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.transfer(context.Background(), item); err == nil {
		t.Fatal("continued without a usable checkpoint")
	}
	info, err := os.Stat(part)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 || item.downloaded.Load() != 0 {
		t.Fatal("wrote data without a checkpoint")
	}
}
