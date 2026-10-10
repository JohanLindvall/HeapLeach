// SPDX-License-Identifier: MIT

package extractor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

var errKeep2ShareOCR = errors.New("captcha: could not read the CAPTCHA")

// Reading a CAPTCHA starts a frozen Python runtime and an ONNX model, and
// with proxy routes a dozen free downloads reach one at once. Run side by
// side, they took the load past fifty on sixteen cores, ran past their
// timeout and failed their files, and each one killed left the runtime it
// had unpacked behind in /tmp, which is memory in the container. So a few
// run at a time, the rest wait for a turn, and the timeout starts with it.
var keep2ShareOCRSlots = make(chan struct{}, config.Keep2ShareOCRConcurrency)

// keep2ShareOCRTimeout is a reader's budget once it has its turn. A
// variable so that tests need not sit the real one out.
var keep2ShareOCRTimeout = config.Keep2ShareOCRTimeout

// Check the helper before requesting a challenge: an installation without
// OCR must not leave a succession of unanswered CAPTCHAs on the host.
func keep2ShareOCR() (func(context.Context, []byte) ([]string, error), error) {
	program, ok := tools.Find(tools.CaptchaOCR)
	if !ok {
		return nil, errors.New("captcha: free downloads need local CAPTCHA recognition; " + tools.NotInstalled(tools.CaptchaOCR))
	}
	return func(ctx context.Context, raw []byte) ([]string, error) {
		return keep2ShareReadCaptcha(ctx, program, raw)
	}, nil
}

// The helper prints its readings one per line, most likely first. Each line
// is judged on its own, so a malformed one costs that reading rather than
// the image; an image with no well-formed reading at all is unreadable.
func keep2ShareReadCaptcha(ctx context.Context, program string, raw []byte) ([]string, error) {
	info, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("captcha: expected an image CAPTCHA: %w", err)
	}
	if info.Width <= 0 || info.Height <= 0 || info.Width > config.Keep2ShareCaptchaPixels/info.Height {
		return nil, errors.New("captcha: CAPTCHA dimensions are too large")
	}
	ResolveNote(ctx, "waiting for a local CAPTCHA reader")
	select {
	case keep2ShareOCRSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-keep2ShareOCRSlots }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The frozen helper unpacks its runtime under TMPDIR and removes it on
	// the way out, which a killed one never reaches. A directory of its own,
	// removed here, cannot be left behind however the helper ends.
	scratch, err := os.MkdirTemp("", "heapleach-ocr-")
	if err != nil {
		return nil, fmt.Errorf("captcha: %w", err)
	}
	defer os.RemoveAll(scratch)

	run, cancel := context.WithTimeout(ctx, keep2ShareOCRTimeout)
	defer cancel()
	cmd := tools.CommandContext(run, program)
	cmd.Env = append(os.Environ(), "TMPDIR="+scratch)
	cmd.Stdin = bytes.NewReader(raw)
	output := &util.BoundedBuffer{Limit: config.ErrorBodySample}
	stderr := &util.BoundedBuffer{Limit: config.ErrorBodySample}
	cmd.Stdout, cmd.Stderr = output, stderr
	ResolveNote(ctx, "reading CAPTCHA")
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if run.Err() != nil {
		// Out of time is an image not read, which the next one may fix:
		// the file has not failed.
		return nil, fmt.Errorf("%w: the reader took longer than %s", errKeep2ShareOCR, keep2ShareOCRTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("captcha: CAPTCHA reader: %w: %s", err, util.Truncate(strings.TrimSpace(stderr.String()), 200))
	}
	if output.Truncated {
		return nil, errKeep2ShareOCR
	}
	var readings []string
	for _, line := range strings.Split(output.String(), "\n") {
		if answer := strings.Join(strings.Fields(line), ""); keep2ShareAnswer(answer) {
			readings = append(readings, answer)
		}
	}
	if len(readings) == 0 {
		return nil, errKeep2ShareOCR
	}
	return readings, nil
}

func keep2ShareAnswer(answer string) bool {
	if len(answer) != 6 {
		return false
	}
	for _, c := range answer {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return false
		}
	}
	return true
}
