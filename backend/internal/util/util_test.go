package util

import (
	"context"
	"net/url"
	"testing"
	"time"
)

func TestFirstNonEmpty(t *testing.T) {
	if got := FirstNonEmpty("", "", "a", "b"); got != "a" {
		t.Errorf("got %q, want %q", got, "a")
	}
	if got := FirstNonEmpty("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := FirstNonEmpty(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDedupe(t *testing.T) {
	got := Dedupe([]string{"a", "", "b", "a", "c", "b", ""})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestRemoveDoesNotMutateInput(t *testing.T) {
	in := []string{"a", "b", "c"}
	got := Remove(in, "b")
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("got %v", got)
	}
	// Remove must not corrupt the caller's slice beyond the removal point.
	if in[0] != "a" {
		t.Errorf("input head changed: %v", in)
	}
	if same := Remove(in, "zz"); len(same) != 3 {
		t.Errorf("removing a missing value changed the slice: %v", same)
	}
}

func TestNameFromURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://x.test/a/b/file.mp4", "file.mp4"},
		{"https://x.test/a/b/file.mp4?token=1&n=other.mp4", "file.mp4"},
		{"https://x.test/a/my%20file.mp4", "my file.mp4"},
		{"https://x.test/", ""},
		{"https://x.test", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := NameFromURL(tc.in); got != tc.want {
			t.Errorf("NameFromURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHostMatches(t *testing.T) {
	tests := []struct {
		host, domain string
		want         bool
	}{
		{"gofile.io", "gofile.io", true},
		{"api.gofile.io", "gofile.io", true},
		{"GoFile.IO", "gofile.io", true},
		{"gofile.io:443", "gofile.io", true},
		{"gofile.io.", "gofile.io", true},
		{"notgofile.io", "gofile.io", false},
		{"gofile.io.evil.test", "gofile.io", false},
		{"evil-gofile.io", "gofile.io", false},
	}
	for _, tc := range tests {
		if got := HostMatches(tc.host, tc.domain); got != tc.want {
			t.Errorf("HostMatches(%q, %q) = %v, want %v", tc.host, tc.domain, got, tc.want)
		}
	}
}

func TestPathSegments(t *testing.T) {
	u, err := url.Parse("https://x.test//a//b/c/")
	if err != nil {
		t.Fatal(err)
	}
	got := PathSegments(u)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestTrimTitleSuffix(t *testing.T) {
	tests := []struct {
		in   string
		seps []string
		want string
	}{
		{"IPCAMERA.mp4 | Bunkr", []string{" | ", " - "}, "IPCAMERA.mp4"},
		{"Shower MILF - Porn Videos & Photos - EroMe", []string{" | ", " - "}, "Shower MILF"},
		// A pipe wins over a dash, so a dash inside the name survives.
		{"My - Clip.mp4 | Bunkr", []string{" | ", " - "}, "My - Clip.mp4"},
		{"No suffix here", []string{" | "}, "No suffix here"},
		{"  padded  ", []string{" | "}, "padded"},
		{"- leading sep", []string{" - "}, "- leading sep"},
	}
	for _, tc := range tests {
		if got := TrimTitleSuffix(tc.in, tc.seps...); got != tc.want {
			t.Errorf("TrimTitleSuffix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTruncateCollapsesAndCutsOnARuneBoundary(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"  runs   of\n\twhitespace  ", 40, "runs of whitespace"},
		{"abcdefgh", 5, "abcde..."},
		// A cut inside "é" would leave half a rune; the character goes whole.
		{"caf\u00e9 au lait", 4, "caf..."},
		{"", 3, ""},
	}
	for _, tc := range cases {
		if got := Truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestOriginDropsEverythingAfterTheHost(t *testing.T) {
	u, err := url.Parse("https://user:pw@example.test:8443/a/b?c=d#e")
	if err != nil {
		t.Fatal(err)
	}
	if got := Origin(u); got != "https://example.test:8443" {
		t.Errorf("Origin = %q", got)
	}
}

func TestDecodeBase64TakesEverySpelling(t *testing.T) {
	// "hi?>" encodes to a string that differs between the two alphabets and
	// needs padding, so the four spellings are genuinely different inputs.
	const want = "hi?>"
	for _, in := range []string{"aGk/Pg==", "aGk/Pg", "aGk_Pg==", "aGk_Pg", " aGk_Pg \n"} {
		got, err := DecodeBase64(in)
		if err != nil {
			t.Errorf("DecodeBase64(%q): %v", in, err)
			continue
		}
		if string(got) != want {
			t.Errorf("DecodeBase64(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "   ", "===", "not base64!"} {
		if _, err := DecodeBase64(in); err == nil {
			t.Errorf("DecodeBase64(%q) decoded without complaint", in)
		}
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	const base, cap = 100 * time.Millisecond, time.Second

	// Jitter is +/-25%, so compare against the jitter-adjusted bounds.
	for attempt := range 4 {
		got := Backoff(attempt, base, cap)
		grown := base * (1 << attempt)
		lo, hi := time.Duration(float64(grown)*0.75), time.Duration(float64(grown)*1.25)
		if hi > cap {
			hi = cap
		}
		if got < lo || got > hi {
			t.Errorf("Backoff(%d) = %v, want within [%v, %v]", attempt, got, lo, hi)
		}
	}
	for attempt := 8; attempt < 20; attempt++ {
		if got := Backoff(attempt, base, cap); got > cap {
			t.Errorf("Backoff(%d) = %v exceeds cap %v", attempt, got, cap)
		}
	}
	if got := Backoff(-5, base, cap); got <= 0 {
		t.Errorf("Backoff(-5) = %v, want positive", got)
	}
}

func TestSleepCtxHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := SleepCtx(ctx, time.Minute); err == nil {
		t.Fatal("want an error from a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("returned after %v, want immediately", elapsed)
	}

	if err := SleepCtx(context.Background(), 10*time.Millisecond); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
