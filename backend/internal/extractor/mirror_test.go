package extractor

import (
	"context"
	"testing"
)

func TestMirroredRotatesOnEveryAttempt(t *testing.T) {
	urls := []string{
		"https://one.example.test/file.bin",
		"https://two.example.test/file.bin",
		"https://three.example.test/file.bin",
	}
	file := Mirrored(File{Name: "file.bin", Size: 1234}, urls)

	if file.URL != urls[0] {
		t.Errorf("URL = %q, want the first mirror %q", file.URL, urls[0])
	}
	if file.Resolve == nil {
		t.Fatal("a set of three mirrors produced no resolver")
	}

	// Two full cycles: the downloader resolves once before the first
	// attempt and again before each retry, so this is the real sequence.
	for i, want := range append(append([]string{}, urls...), urls...) {
		target, err := file.Resolve(context.Background())
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		if target.URL != want {
			t.Errorf("attempt %d resolved to %q, want %q", i+1, target.URL, want)
		}
		if target.Name != "file.bin" || target.Size != 1234 {
			t.Errorf("attempt %d lost the file's name or size: %+v", i+1, target)
		}
	}
}

func TestMirroredWithOneURLNeedsNoResolver(t *testing.T) {
	const only = "https://one.example.test/file.bin"
	file := Mirrored(File{Name: "file.bin"}, []string{only, only, ""})

	if file.URL != only {
		t.Errorf("URL = %q, want %q", file.URL, only)
	}
	if file.Resolve != nil {
		t.Error("a single mirror was given a resolver with nothing to fail over to")
	}
}

func TestMirroredWithNoURLsLeavesTheFileAlone(t *testing.T) {
	original := File{Name: "file.bin", URL: "https://one.example.test/file.bin"}
	file := Mirrored(original, nil)

	if file.URL != original.URL {
		t.Errorf("URL = %q, want it unchanged as %q", file.URL, original.URL)
	}
}

func TestMirrorHosts(t *testing.T) {
	got := MirrorHosts("https://store1.example.test/download/abc/clip.mp4?token=xyz",
		[]string{"store2.example.test", "store3.example.test", "store1.example.test", ""})

	want := []string{
		"https://store1.example.test/download/abc/clip.mp4?token=xyz",
		"https://store2.example.test/download/abc/clip.mp4?token=xyz",
		"https://store3.example.test/download/abc/clip.mp4?token=xyz",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d mirrors, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mirror %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMirrorHostsKeepsAnUnparseableLink(t *testing.T) {
	got := MirrorHosts("://nonsense", []string{"other.example.test"})
	if len(got) != 1 || got[0] != "://nonsense" {
		t.Errorf("got %q, want the original link alone", got)
	}
}
