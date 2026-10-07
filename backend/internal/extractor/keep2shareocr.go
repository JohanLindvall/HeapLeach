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
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

var errKeep2ShareOCR = errors.New("keep2share: could not read the CAPTCHA")

// Check the helper before requesting a challenge: an installation without
// OCR must not leave a succession of unanswered CAPTCHAs on the host.
func keep2ShareOCR() (func(context.Context, []byte) ([]string, error), error) {
	program, ok := tools.Find(tools.CaptchaOCR)
	if !ok {
		return nil, errors.New("keep2share: free downloads need local CAPTCHA recognition; " + tools.NotInstalled(tools.CaptchaOCR))
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
		return nil, fmt.Errorf("keep2share: expected an image CAPTCHA: %w", err)
	}
	if info.Width <= 0 || info.Height <= 0 || info.Width > config.Keep2ShareCaptchaPixels/info.Height {
		return nil, errors.New("keep2share: CAPTCHA dimensions are too large")
	}
	ctx, cancel := context.WithTimeout(ctx, config.Keep2ShareOCRTimeout)
	defer cancel()
	cmd := tools.CommandContext(ctx, program)
	cmd.Stdin = bytes.NewReader(raw)
	output := &util.BoundedBuffer{Limit: config.ErrorBodySample}
	stderr := &util.BoundedBuffer{Limit: config.ErrorBodySample}
	cmd.Stdout, cmd.Stderr = output, stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("keep2share: CAPTCHA reader: %w: %s", err, util.Truncate(strings.TrimSpace(stderr.String()), 200))
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
