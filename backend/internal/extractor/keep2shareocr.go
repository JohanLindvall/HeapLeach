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
func keep2ShareOCR() (func(context.Context, []byte) (string, error), error) {
	program, ok := tools.Find(tools.CaptchaOCR)
	if !ok {
		return nil, errors.New("keep2share: free downloads need local CAPTCHA recognition; " + tools.NotInstalled(tools.CaptchaOCR))
	}
	return func(ctx context.Context, raw []byte) (string, error) {
		return keep2ShareReadCaptcha(ctx, program, raw)
	}, nil
}

func keep2ShareReadCaptcha(ctx context.Context, program string, raw []byte) (string, error) {
	info, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("keep2share: expected an image CAPTCHA: %w", err)
	}
	if info.Width <= 0 || info.Height <= 0 || info.Width > config.Keep2ShareCaptchaPixels/info.Height {
		return "", errors.New("keep2share: CAPTCHA dimensions are too large")
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
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("keep2share: CAPTCHA reader: %w: %s", err, util.Truncate(strings.TrimSpace(stderr.String()), 200))
	}
	answer := strings.Join(strings.Fields(output.String()), "")
	if output.Truncated || len(answer) != 6 {
		return "", errKeep2ShareOCR
	}
	for _, c := range answer {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return "", errKeep2ShareOCR
		}
	}
	return answer, nil
}
