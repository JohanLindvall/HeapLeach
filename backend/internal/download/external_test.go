package download

import (
	"strings"
	"testing"
)

func TestParseProgress(t *testing.T) {
	cases := []struct {
		in          string
		done, total int64
		stream      string
		ok          bool
	}{
		{"1024 4096", 1024, 4096, "", true},
		// yt-dlp writes NA while it does not yet know the total.
		{"1024 NA", 1024, 0, "", true},
		{"1024", 1024, 0, "", true},
		{"NA 4096", 0, 0, "", false},
		{"", 0, 0, "", false},
		// A fragmented download has no length to state, so yt-dlp averages
		// what it has and reports a float. Read as an integer these are not
		// numbers at all, and every one of them was being dropped — which is
		// how a 158MB download came to display a total of 712 B.
		{"3072 69632.0", 3072, 69632, "", true},
		{"18182519 129456458.22222222", 18182519, 129456458, "", true},
		// Truncated, not rounded: the fraction is an averaging artefact.
		{"1 4096.99", 1, 4096, "", true},
		// Nothing usable is refused rather than folded into zero, which
		// here means "no total yet" and would hide a real answer.
		{"1024 inf", 1024, 0, "", true},
		{"1024 NaN", 1024, 0, "", true},
		{"1024 -5", 1024, 0, "", true},
		{"-5 4096", 0, 0, "", false},
		// The helper names the stream last, which is what separates a
		// merged download's video from its audio.
		{"1024 4096 616", 1024, 4096, "616", true},
		{"1024 4096 NA", 1024, 4096, "", true},
	}
	for _, tc := range cases {
		done, total, stream, ok := parseProgress(tc.in)
		if done != tc.done || total != tc.total || stream != tc.stream || ok != tc.ok {
			t.Errorf("parseProgress(%q) = %d, %d, %q, %v; want %d, %d, %q, %v",
				tc.in, done, total, stream, ok, tc.done, tc.total, tc.stream, tc.ok)
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

// A merged download is two files, and the item has to describe both.
//
// yt-dlp fetches the video stream and then the audio, reporting each from
// zero with a total of its own. Passed through as they arrive, the item
// finished a 154 MB download reading "5 MB / 5 MB" — the audio stream alone
// — and showed the bar collapsing to nothing when the second stream began.
// The figures below are the shape of a real run: 148 MB of video, then
// 5.4 MB of audio.
func TestReadExternalProgressAddsUpMergedStreams(t *testing.T) {
	m := &Manager{}
	it := &Item{}

	lines := strings.Join([]string{
		"PROGRESS 712 712 616",
		"PROGRESS 74000000 148762022.0 616",
		"PROGRESS 148762022 148762022.0 616",
		// The audio stream starts over, with a total of its own.
		"PROGRESS 1024 5449599.0 251",
		"PROGRESS 5449599 5449599.0 251",
		"FILE /tmp/video.mp4",
	}, "\n")

	produced := m.readExternalProgress(it, strings.NewReader(lines))
	if produced != "/tmp/video.mp4" {
		t.Errorf("produced = %q", produced)
	}
	const wantBytes = 148762022 + 5449599
	if got := it.downloaded.Load(); got != wantBytes {
		t.Errorf("downloaded = %d, want %d — the streams are one file", got, wantBytes)
	}
	if it.Size != wantBytes {
		t.Errorf("size = %d, want %d — the audio total alone is not the file", it.Size, wantBytes)
	}
}

// Fetching fragments several at a time makes the counter wobble backwards by
// a fraction of a percent. That is one stream still, and must not be counted
// as a second one — which would add the whole of it in again.
func TestReadExternalProgressIgnoresAFragmentWobble(t *testing.T) {
	m := &Manager{}
	it := &Item{}

	// No format id, so the fallback is what decides: this is the shape an
	// overridden helper script would produce.
	lines := strings.Join([]string{
		"PROGRESS 79884658 148762022.0",
		"PROGRESS 79618418 148762022.0",
		"PROGRESS 80100000 148762022.0",
	}, "\n")

	m.readExternalProgress(it, strings.NewReader(lines))
	if got := it.downloaded.Load(); got != 80100000 {
		t.Errorf("downloaded = %d, want 80100000 — a 0.3%% wobble is not a new stream", got)
	}
}

// And without a format id, a genuine restart still has to be recognised.
func TestReadExternalProgressCatchesARestartWithoutAFormatID(t *testing.T) {
	m := &Manager{}
	it := &Item{}

	lines := strings.Join([]string{
		"PROGRESS 148762022 148762022.0",
		"PROGRESS 1024 5449599.0",
		"PROGRESS 5449599 5449599.0",
	}, "\n")

	m.readExternalProgress(it, strings.NewReader(lines))
	const wantBytes = 148762022 + 5449599
	if got := it.downloaded.Load(); got != wantBytes {
		t.Errorf("downloaded = %d, want %d", got, wantBytes)
	}
}
