// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

func keep2ShareSyntheticCaptcha() *image.RGBA {
	return image.NewRGBA(image.Rect(0, 0, 100, 50))
}

func TestKeep2ShareOCRRequiresAnImageBeforeStartingAHelper(t *testing.T) {
	if _, err := keep2ShareReadCaptcha(context.Background(), "missing-program", []byte("<html>Browser verification required</html>")); err == nil {
		t.Fatal("a browser challenge was passed to the image solver")
	}
}

func TestKeep2ShareOCRKeepsWellFormedReadingsInOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture helper is a shell script")
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, keep2ShareSyntheticCaptcha()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, output string
		want         []string // nil: unreadable
	}{
		{"complete", "aB3dE7\\n", []string{"aB3dE7"}},
		{"whitespace", "a B3dE7\\n", []string{"aB3dE7"}},
		{"ranked", "aB3dE7\\nxY4zW9\\n", []string{"aB3dE7", "xY4zW9"}},
		{"skips malformed", "aB3dE\\naB3dE!\\nxY4zW9\\n", []string{"xY4zW9"}},
		{"short", "aB3dE\\n", nil},
		{"punctuation", "aB3dE!\\n", nil},
		{"silent", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			program := filepath.Join(t.TempDir(), "ocr")
			body := "#!/bin/sh\ncat >/dev/null\nprintf '" + tc.output + "'\n"
			if err := os.WriteFile(program, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			readings, err := keep2ShareReadCaptcha(context.Background(), program, raw.Bytes())
			if tc.want != nil && (err != nil || !slices.Equal(readings, tc.want)) {
				t.Fatalf("readings=%q error=%v; want %q, case and order kept", readings, err, tc.want)
			}
			if tc.want == nil && !errors.Is(err, errKeep2ShareOCR) {
				t.Fatalf("readings=%q error=%v; want an unreadable CAPTCHA", readings, err)
			}
		})
	}
}

func TestKeep2ShareOCRCanBeCancelled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture helper is a shell script")
	}
	program := filepath.Join(t.TempDir(), "ocr")
	if err := os.WriteFile(program, []byte("#!/bin/sh\ncat >/dev/null\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, keep2ShareSyntheticCaptcha()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := keep2ShareReadCaptcha(ctx, program, raw.Bytes()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v; want the OCR process cancelled", err)
	}
}

// Many free downloads reach a CAPTCHA at once through proxy routes, and the
// readers run only a few at a time: side by side they overloaded the host.
func TestKeep2ShareOCRRunsOnlyAFewAtATime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture helper is a shell script")
	}
	dir := t.TempDir()
	slots, log := filepath.Join(dir, "slots"), filepath.Join(dir, "log")
	if err := os.Mkdir(slots, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEAPLEACH_TEST_SLOTS", slots)
	t.Setenv("HEAPLEACH_TEST_LOG", log)
	program := filepath.Join(dir, "ocr")
	body := "#!/bin/sh\ncat >/dev/null\nmkdir \"$HEAPLEACH_TEST_SLOTS/$$\"\nls \"$HEAPLEACH_TEST_SLOTS\" | wc -l >> \"$HEAPLEACH_TEST_LOG\"\n" +
		"sleep 0.2\nrmdir \"$HEAPLEACH_TEST_SLOTS/$$\"\nprintf 'aB3dE7\\n'\n"
	if err := os.WriteFile(program, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, keep2ShareSyntheticCaptcha()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Go(func() {
			_, err := keep2ShareReadCaptcha(context.Background(), program, raw.Bytes())
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	counts, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	most := 0
	for _, field := range strings.Fields(string(counts)) {
		if n, _ := strconv.Atoi(field); n > most {
			most = n
		}
	}
	if most > config.Keep2ShareOCRConcurrency {
		t.Fatalf("%d readers ran at once, want at most %d", most, config.Keep2ShareOCRConcurrency)
	}
}

// A reader out of time has not read the image, which the next one may: the
// file goes on. And the runtime it unpacked goes with it, though a killed
// helper never gets to remove it itself.
func TestKeep2ShareOCRTimeoutIsAnImageNotReadAndLeavesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture helper is a shell script")
	}
	defer func(was time.Duration) { keep2ShareOCRTimeout = was }(keep2ShareOCRTimeout)
	keep2ShareOCRTimeout = 200 * time.Millisecond
	dir := t.TempDir()
	where := filepath.Join(dir, "tmpdir")
	t.Setenv("HEAPLEACH_TEST_WHERE", where)
	program := filepath.Join(dir, "ocr")
	body := "#!/bin/sh\ncat >/dev/null\nprintf '%s' \"$TMPDIR\" > \"$HEAPLEACH_TEST_WHERE\"\n" +
		"mkdir \"$TMPDIR/_MEI0001\" && printf runtime > \"$TMPDIR/_MEI0001/lib\"\nsleep 30\n"
	if err := os.WriteFile(program, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, keep2ShareSyntheticCaptcha()); err != nil {
		t.Fatal(err)
	}
	_, err := keep2ShareReadCaptcha(context.Background(), program, raw.Bytes())
	if !errors.Is(err, errKeep2ShareOCR) {
		t.Fatalf("error = %v; want an image not read, so another can be tried", err)
	}
	scratch, err := os.ReadFile(where)
	if err != nil || len(scratch) == 0 {
		t.Fatalf("the helper was given no TMPDIR of its own: %q, %v", scratch, err)
	}
	if _, err := os.Stat(string(scratch)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the killed reader's runtime is still in %s", scratch)
	}
}
