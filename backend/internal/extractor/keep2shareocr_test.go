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

func keep2ShareSyntheticCaptchaPNG(t *testing.T) []byte {
	t.Helper()
	var raw bytes.Buffer
	if err := png.Encode(&raw, keep2ShareSyntheticCaptcha()); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

// closeKeep2ShareOCRReaders stops every idle reader and forgets which helpers
// could not serve, so that one test's readers are not another's.
func closeKeep2ShareOCRReaders() {
	pool := &keep2ShareOCRReaders
	pool.Lock()
	idle := pool.idle
	pool.idle, pool.oneShot = nil, nil
	for _, r := range idle {
		r.idle.Stop()
	}
	pool.Unlock()
	for _, r := range idle {
		r.shutdown()
	}
}

// ocrFake writes a shell-script helper, whose readers are stopped when the
// test ends.
func ocrFake(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture helper is a shell script")
	}
	program := filepath.Join(t.TempDir(), "ocr")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeKeep2ShareOCRReaders)
	return program
}

// ocrServes is a helper's --serve exchange, recording each start in
// $HEAPLEACH_TEST_STARTS when that is set, and answering every image with
// answer, a printf format, and the empty line that ends an answer.
func ocrServes(answer string) string {
	return "[ \"$1\" = --serve ] || exit 2\n" +
		"[ -z \"$HEAPLEACH_TEST_STARTS\" ] || printf 'started\\n' >> \"$HEAPLEACH_TEST_STARTS\"\n" +
		"printf 'ready\\n'\n" +
		"while read -r size; do\n\thead -c \"$size\" >/dev/null\n\tprintf '" + answer + "\\n'\ndone\n"
}

func linesIn(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func TestKeep2ShareOCRRequiresAnImageBeforeStartingAHelper(t *testing.T) {
	if _, err := keep2ShareReadCaptcha(context.Background(), "missing-program", []byte("<html>Browser verification required</html>")); err == nil {
		t.Fatal("a browser challenge was passed to the image solver")
	}
}

func TestKeep2ShareOCRKeepsWellFormedReadingsInOrder(t *testing.T) {
	raw := keep2ShareSyntheticCaptchaPNG(t)
	for _, serving := range []bool{true, false} {
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
			name := "one image per helper/" + tc.name
			if serving {
				name = "serving/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				var readings []string
				var err error
				if serving {
					program := ocrFake(t, ocrServes(tc.output))
					readings, err = keep2ShareReadCaptcha(context.Background(), program, raw)
				} else {
					program := ocrFake(t, "cat >/dev/null\nprintf '"+tc.output+"'\n")
					readings, err = keep2ShareReadOnce(context.Background(), program, raw)
				}
				if tc.want != nil && (err != nil || !slices.Equal(readings, tc.want)) {
					t.Fatalf("readings=%q error=%v; want %q, case and order kept", readings, err, tc.want)
				}
				if tc.want == nil && !errors.Is(err, errKeep2ShareOCR) {
					t.Fatalf("readings=%q error=%v; want an unreadable CAPTCHA", readings, err)
				}
			})
		}
	}
}

// Starting the reader is nearly all of what a reading costs, so one reader
// reads image after image.
func TestKeep2ShareOCRReaderServesOneImageAfterAnother(t *testing.T) {
	starts := filepath.Join(t.TempDir(), "starts")
	t.Setenv("HEAPLEACH_TEST_STARTS", starts)
	program := ocrFake(t, ocrServes("aB3dE7\\nxY4zW9\\n"))
	raw := keep2ShareSyntheticCaptchaPNG(t)
	for range 3 {
		readings, err := keep2ShareReadCaptcha(context.Background(), program, raw)
		if err != nil || !slices.Equal(readings, []string{"aB3dE7", "xY4zW9"}) {
			t.Fatalf("readings=%q error=%v", readings, err)
		}
	}
	if got := linesIn(t, starts); len(got) != 1 {
		t.Fatalf("the reader was started %d times for three images, want once", len(got))
	}
}

// A helper built before --serve never says it is ready. It is still used,
// one image per helper as before, and not asked to serve again for a while.
func TestKeep2ShareOCRHelperTooOldToServeReadsOneImageEach(t *testing.T) {
	defer func(was time.Duration) { keep2ShareOCRStart = was }(keep2ShareOCRStart)
	keep2ShareOCRStart = 200 * time.Millisecond
	log := filepath.Join(t.TempDir(), "log")
	t.Setenv("HEAPLEACH_TEST_LOG", log)
	program := ocrFake(t, "printf '%s\\n' \"${1:-once}\" >> \"$HEAPLEACH_TEST_LOG\"\ncat >/dev/null\nprintf 'aB3dE7\\n'\n")
	raw := keep2ShareSyntheticCaptchaPNG(t)
	read := func() {
		t.Helper()
		if readings, err := keep2ShareReadCaptcha(context.Background(), program, raw); err != nil || !slices.Equal(readings, []string{"aB3dE7"}) {
			t.Fatalf("readings=%q error=%v; want the image read one helper at a time", readings, err)
		}
	}
	read()
	read()
	if got := linesIn(t, log); !slices.Equal(got, []string{"--serve", "once", "once"}) {
		t.Fatalf("helper runs %q; want one attempt to serve, then one helper per image", got)
	}
	keep2ShareOCRReaders.Lock()
	keep2ShareOCRReaders.oneShot[program] = time.Now().Add(-time.Second) // the retry interval has passed
	keep2ShareOCRReaders.Unlock()
	read()
	if got := linesIn(t, log); !slices.Equal(got, []string{"--serve", "once", "once", "--serve", "once"}) {
		t.Fatalf("helper runs %q; want serving tried again once the interval has passed", got)
	}
}

// A reader that fails on an image is reported the way a helper that failed
// on its own always was, with what it said, and the next image gets a new one.
func TestKeep2ShareOCRReaderThatFailsSaysWhyAndIsReplaced(t *testing.T) {
	dir := t.TempDir()
	starts := filepath.Join(dir, "starts")
	t.Setenv("HEAPLEACH_TEST_STARTS", starts)
	t.Setenv("HEAPLEACH_TEST_FAILED", filepath.Join(dir, "failed"))
	program := ocrFake(t, "if [ ! -e \"$HEAPLEACH_TEST_FAILED\" ]; then\n"+
		"\tprintf 'ready\\n'; read -r size; head -c \"$size\" >/dev/null\n"+
		"\t: > \"$HEAPLEACH_TEST_FAILED\"; echo 'cannot identify image file' >&2; exit 3\nfi\n"+
		ocrServes("aB3dE7\\n"))
	raw := keep2ShareSyntheticCaptchaPNG(t)
	_, err := keep2ShareReadCaptcha(context.Background(), program, raw)
	if err == nil || errors.Is(err, errKeep2ShareOCR) || !strings.Contains(err.Error(), "exit status 3") ||
		!strings.Contains(err.Error(), "cannot identify image file") {
		t.Fatalf("error = %v; want the reader's own failure, with what it said", err)
	}
	if readings, err := keep2ShareReadCaptcha(context.Background(), program, raw); err != nil || !slices.Equal(readings, []string{"aB3dE7"}) {
		t.Fatalf("after a failed reader: readings=%q error=%v", readings, err)
	}
	if got := linesIn(t, starts); len(got) != 1 {
		t.Fatalf("%d serving starts recorded after the failed one, want 1", len(got))
	}
}

// Each reader holds a model in memory, so one left idle is stopped, and the
// runtime it unpacked goes with it.
func TestKeep2ShareOCRIdleReaderIsStopped(t *testing.T) {
	defer func(was time.Duration) { keep2ShareOCRIdle = was }(keep2ShareOCRIdle)
	keep2ShareOCRIdle = 100 * time.Millisecond
	where := filepath.Join(t.TempDir(), "tmpdir")
	t.Setenv("HEAPLEACH_TEST_WHERE", where)
	program := ocrFake(t, "printf '%s' \"$TMPDIR\" > \"$HEAPLEACH_TEST_WHERE\"\n"+ocrServes("aB3dE7\\n"))
	if _, err := keep2ShareReadCaptcha(context.Background(), program, keep2ShareSyntheticCaptchaPNG(t)); err != nil {
		t.Fatal(err)
	}
	scratch, err := os.ReadFile(where)
	if err != nil || len(scratch) == 0 {
		t.Fatalf("the reader was given no TMPDIR of its own: %q, %v", scratch, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		keep2ShareOCRReaders.Lock()
		idle := len(keep2ShareOCRReaders.idle)
		keep2ShareOCRReaders.Unlock()
		_, err := os.Stat(string(scratch))
		if idle == 0 && errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("an idle reader is still kept (%d idle, runtime dir: %v)", idle, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestKeep2ShareOCRCanBeCancelled(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"starting", "cat >/dev/null\nsleep 30\n"},
		{"reading", "printf 'ready\\n'\nread -r size\nsleep 30\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			program := ocrFake(t, tc.body)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, err := keep2ShareReadCaptcha(ctx, program, keep2ShareSyntheticCaptchaPNG(t)); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v; want the OCR process cancelled", err)
			}
			if !keep2ShareOCRServes(program) {
				t.Fatal("a cancelled start was taken for a helper that cannot serve")
			}
		})
	}
}

func TestKeep2ShareOCRWaitingForASlotCanBeCancelled(t *testing.T) {
	for range cap(keep2ShareOCRSlots) {
		keep2ShareOCRSlots <- struct{}{}
	}
	defer func() {
		for range cap(keep2ShareOCRSlots) {
			<-keep2ShareOCRSlots
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var notes []string
	ctx = WithResolveNote(ctx, func(note string) { notes = append(notes, note) })
	_, err := keep2ShareReadCaptcha(ctx, "must-not-start", keep2ShareSyntheticCaptchaPNG(t))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for a reader: %v", err)
	}
	if !slices.Equal(notes, []string{"waiting for a local CAPTCHA reader"}) {
		t.Fatalf("notes=%v; a queued reader must not report that OCR is running", notes)
	}
	if len(keep2ShareOCRSlots) != cap(keep2ShareOCRSlots) {
		t.Fatal("a cancelled waiter released another reader's slot")
	}
}

// Many free downloads reach a CAPTCHA at once through proxy routes, and the
// readers run only a few at a time: side by side they overloaded the host.
// Kept between images, no more of them are started than that either.
func TestKeep2ShareOCRRunsOnlyAFewAtATime(t *testing.T) {
	dir := t.TempDir()
	slots, log, starts := filepath.Join(dir, "slots"), filepath.Join(dir, "log"), filepath.Join(dir, "starts")
	if err := os.Mkdir(slots, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEAPLEACH_TEST_SLOTS", slots)
	t.Setenv("HEAPLEACH_TEST_LOG", log)
	t.Setenv("HEAPLEACH_TEST_STARTS", starts)
	program := ocrFake(t, "[ \"$1\" = --serve ] || exit 2\nprintf 'started\\n' >> \"$HEAPLEACH_TEST_STARTS\"\nprintf 'ready\\n'\n"+
		"while read -r size; do\n\thead -c \"$size\" >/dev/null\n"+
		"\tmkdir \"$HEAPLEACH_TEST_SLOTS/$$\"\n\tls \"$HEAPLEACH_TEST_SLOTS\" | wc -l >> \"$HEAPLEACH_TEST_LOG\"\n"+
		"\tsleep 0.2\n\trmdir \"$HEAPLEACH_TEST_SLOTS/$$\"\n\tprintf 'aB3dE7\\n\\n'\ndone\n")
	raw := keep2ShareSyntheticCaptchaPNG(t)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Go(func() {
			_, err := keep2ShareReadCaptcha(context.Background(), program, raw)
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
	most := 0
	for _, field := range linesIn(t, log) {
		if n, _ := strconv.Atoi(field); n > most {
			most = n
		}
	}
	if most > config.Keep2ShareOCRConcurrency {
		t.Fatalf("%d readers read at once, want at most %d", most, config.Keep2ShareOCRConcurrency)
	}
	if n := len(linesIn(t, starts)); n > config.Keep2ShareOCRConcurrency {
		t.Fatalf("%d readers started for six images, want at most %d", n, config.Keep2ShareOCRConcurrency)
	}
}

// A reader out of time has not read the image, which the next one may: the
// file goes on. And the runtime it unpacked goes with it, though a killed
// helper never gets to remove it itself.
func TestKeep2ShareOCRTimeoutIsAnImageNotReadAndLeavesNothing(t *testing.T) {
	defer func(was time.Duration) { keep2ShareOCRTimeout = was }(keep2ShareOCRTimeout)
	keep2ShareOCRTimeout = 200 * time.Millisecond
	const unpack = "printf '%s' \"$TMPDIR\" > \"$HEAPLEACH_TEST_WHERE\"\n" +
		"mkdir \"$TMPDIR/_MEI0001\" && printf runtime > \"$TMPDIR/_MEI0001/lib\"\n"
	for _, tc := range []struct {
		name, body string
		read       func(context.Context, string, []byte) ([]string, error)
	}{
		{"serving", unpack + "printf 'ready\\n'\nread -r size\nsleep 30\n", keep2ShareReadCaptcha},
		{"one image per helper", "cat >/dev/null\n" + unpack + "sleep 30\n", keep2ShareReadOnce},
	} {
		t.Run(tc.name, func(t *testing.T) {
			where := filepath.Join(t.TempDir(), "tmpdir")
			t.Setenv("HEAPLEACH_TEST_WHERE", where)
			program := ocrFake(t, tc.body)
			start := time.Now()
			_, err := tc.read(context.Background(), program, keep2ShareSyntheticCaptchaPNG(t))
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("OCR took %s despite its %s budget", elapsed, keep2ShareOCRTimeout)
			}
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
		})
	}
}
