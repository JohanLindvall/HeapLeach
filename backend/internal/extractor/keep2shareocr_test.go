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

func TestKeep2ShareOCRPreservesCaseAndRejectsPartialAnswers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture helper is a shell script")
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, keep2ShareSyntheticCaptcha()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, output string
		ok           bool
	}{
		{"complete", "aB3dE7\\n", true},
		{"whitespace", "a B3dE7\\n", true},
		{"short", "aB3dE\\n", false},
		{"punctuation", "aB3dE!\\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			program := filepath.Join(t.TempDir(), "ocr")
			body := "#!/bin/sh\ncat >/dev/null\nprintf '" + tc.output + "'\n"
			if err := os.WriteFile(program, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			answer, err := keep2ShareReadCaptcha(context.Background(), program, raw.Bytes())
			if tc.ok && (err != nil || answer != "aB3dE7") {
				t.Fatalf("answer=%q error=%v; want the case-sensitive answer", answer, err)
			}
			if !tc.ok && !errors.Is(err, errKeep2ShareOCR) {
				t.Fatalf("error=%v; want an unreadable CAPTCHA", err)
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
