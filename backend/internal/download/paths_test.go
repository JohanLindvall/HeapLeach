package download

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "video.mp4", "video.mp4"},
		{"strips directory", "../../etc/passwd", "passwd"},
		{"strips windows directory", `..\..\windows\system32`, "system32"},
		{"replaces separators", "a/b/c.txt", "c.txt"},
		{"replaces reserved chars", `a:b*c?d"e<f>g|h.txt`, "a_b_c_d_e_f_g_h.txt"},
		{"drops control chars", "video\x00\x1b.mp4", "video.mp4"},
		{"collapses whitespace", "  a   b  .mp4 ", "a b .mp4"},
		{"trims trailing dots", "name...", "name"},
		{"trims leading dots", "...hidden", "hidden"},
		{"empty becomes download", "   ", "download"},
		{"dot only", ".", "download"},
		{"dotdot only", "..", "download"},
		{"windows device", "CON", "download"},
		{"windows device with ext", "nul.txt", "download"},
		{"com port", "COM1", "download"},
		{"unicode kept", "ドラマ 第1話.mkv", "ドラマ 第1話.mkv"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SafeName(tc.in); got != tc.want {
				t.Errorf("SafeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSafeNameNeverEscapes(t *testing.T) {
	// Whatever a remote host sends, the result must stay a single component
	// that cannot climb out of the download root.
	inputs := []string{
		"../secret", "/etc/shadow", `C:\Windows\win.ini`, "..%2f..%2fetc",
		"....//....//x", strings.Repeat("../", 40) + "boom",
	}
	for _, in := range inputs {
		got := SafeName(in)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("SafeName(%q) = %q contains a separator", in, got)
		}
		if got == ".." || got == "." {
			t.Errorf("SafeName(%q) = %q is a traversal component", in, got)
		}
		joined := filepath.Join("/downloads", got)
		if !strings.HasPrefix(joined, "/downloads/") {
			t.Errorf("SafeName(%q) escaped the root: %q", in, joined)
		}
	}
}

func TestSafeNameLength(t *testing.T) {
	long := strings.Repeat("a", 400) + ".mp4"
	got := SafeName(long)
	if len(got) > maxNameBytes {
		t.Errorf("length %d exceeds %d", len(got), maxNameBytes)
	}
	if !strings.HasSuffix(got, ".mp4") {
		t.Errorf("extension not preserved: %q", got[len(got)-10:])
	}
}

func TestSafeRelPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"a/b", filepath.Join("a", "b")},
		{"../../a", "a"},
		{"a/../../b", filepath.Join("a", "b")},
		{"", ""},
		{"./x/./y", filepath.Join("x", "y")},
		{`a\b`, filepath.Join("a", "b")},
	}
	for _, tc := range tests {
		if got := SafeRelPath(tc.in); got != tc.want {
			t.Errorf("SafeRelPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()

	first, err := UniquePath(dir, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(first) != "a.txt" {
		t.Fatalf("first = %q, want a.txt", filepath.Base(first))
	}

	mustWrite(t, first)
	second, err := UniquePath(dir, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(second) != "a (2).txt" {
		t.Errorf("second = %q, want %q", filepath.Base(second), "a (2).txt")
	}

	mustWrite(t, second)
	third, err := UniquePath(dir, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(third) != "a (3).txt" {
		t.Errorf("third = %q, want %q", filepath.Base(third), "a (3).txt")
	}
}

func TestNewIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		id := newID()
		if len(id) != 16 {
			t.Fatalf("id %q has length %d, want 16", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := writeFile(path); err != nil {
		t.Fatal(err)
	}
}

func writeFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// The part-file name has to be a pure function of the source, or an
// interrupted transfer would never be found again by the next run.
func TestPartSuffixIsStableAcrossRuns(t *testing.T) {
	const source = "https://example.com/d/abc123"
	first := partSuffix(source)
	if second := partSuffix(source); first != second {
		t.Errorf("partSuffix is not deterministic: %q then %q", first, second)
	}
	if other := partSuffix(source + "?x=1"); other == first {
		t.Error("different sources must not share a part file")
	}
	if len(first) != 16 {
		t.Errorf("suffix length = %d, want 16 hex characters", len(first))
	}
}

func TestClaimPartIsExclusive(t *testing.T) {
	m := &Manager{parts: make(map[string]struct{})}

	release, ok := m.claimPart("/tmp/a.part")
	if !ok {
		t.Fatal("the first claim should succeed")
	}
	if _, ok := m.claimPart("/tmp/a.part"); ok {
		t.Error("a claimed path must not be handed out twice")
	}
	if _, ok := m.claimPart("/tmp/b.part"); !ok {
		t.Error("an unrelated path should still be claimable")
	}

	release()
	if _, ok := m.claimPart("/tmp/a.part"); !ok {
		t.Error("releasing should make the path available again")
	}
}
