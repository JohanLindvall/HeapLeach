package download

import (
	"strings"
	"testing"
)

func TestParseProgress(t *testing.T) {
	cases := []struct {
		in          string
		done, total int64
		ok          bool
	}{
		{"1024 4096", 1024, 4096, true},
		// yt-dlp writes NA while it does not yet know the total.
		{"1024 NA", 1024, 0, true},
		{"1024", 1024, 0, true},
		{"NA 4096", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tc := range cases {
		done, total, ok := parseProgress(tc.in)
		if done != tc.done || total != tc.total || ok != tc.ok {
			t.Errorf("parseProgress(%q) = %d, %d, %v; want %d, %d, %v",
				tc.in, done, total, ok, tc.done, tc.total, tc.ok)
		}
	}
}

func TestReadExternalProgressFoldsOutputIntoTheItem(t *testing.T) {
	m := &Manager{}
	it := &Item{}

	out := strings.NewReader(strings.Join([]string{
		"PROGRESS 100 1000",
		"some chatter the script passes through",
		"PROGRESS 500 1000",
		"PROGRESS garbage",
		"FILE /downloads/a video.mp4",
		"",
	}, "\n"))

	produced := m.readExternalProgress(it, out)
	if produced != "/downloads/a video.mp4" {
		t.Errorf("produced = %q", produced)
	}
	if got := it.downloaded.Load(); got != 500 {
		t.Errorf("downloaded = %d, want the last well-formed count", got)
	}
	m.mu.Lock()
	size := it.Size
	m.mu.Unlock()
	if size != 1000 {
		t.Errorf("size = %d, want 1000", size)
	}
}

func TestReadExternalProgressWithoutAFileLine(t *testing.T) {
	m := &Manager{}
	it := &Item{}
	if got := m.readExternalProgress(it, strings.NewReader("PROGRESS 1 2\n")); got != "" {
		t.Errorf("produced = %q, want none reported", got)
	}
}
