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
	"testing"
	"time"
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
