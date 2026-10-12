// SPDX-License-Identifier: MIT

package captcha

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"slices"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// ErrUnreadable means another image may succeed, rather than a broken OCR
// installation. Caller cancellation is returned unchanged.
var ErrUnreadable = errors.New("captcha: could not read the CAPTCHA")

type engine interface {
	infer(context.Context, []float32, int) ([]float32, error)
	close()
}

type kept struct {
	engine
	timer *time.Timer
}

type pool struct {
	create  func() (engine, error)
	slots   chan struct{}
	timeout time.Duration
	idleFor time.Duration
	mu      sync.Mutex
	idle    []*kept
}

var readers = pool{
	create: newEngine, slots: make(chan struct{}, config.Keep2ShareOCRConcurrency),
	timeout: config.Keep2ShareOCRTimeout, idleFor: config.Keep2ShareOCRIdle,
}

// Read returns ranked readings. There are at most two model sessions, reused
// between images and released after an idle interval. note runs on the caller's
// goroutine and is never called after Read returns.
func Read(ctx context.Context, raw []byte, note func(string)) ([]string, error) {
	return readers.read(ctx, raw, note)
}

func (p *pool) read(ctx context.Context, raw []byte, note func(string)) ([]string, error) {
	if len(raw) > config.Keep2ShareCaptchaBytes {
		return nil, errors.New("captcha: image is too large")
	}
	info, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("captcha: expected an image CAPTCHA: %w", err)
	}
	if info.Width <= 0 || info.Height <= 0 || info.Width > config.Keep2ShareCaptchaPixels/info.Height {
		return nil, errors.New("captcha: CAPTCHA dimensions are too large")
	}
	if note != nil {
		note("waiting for a local CAPTCHA reader")
	}
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	run, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	type result struct {
		readings []string
		err      error
	}
	done := make(chan result, 1)
	stages := make(chan string)
	// Inference can be terminated; native session construction cannot. The
	// worker owns its slot until it actually finishes even when its caller
	// cancels, so cancellations cannot start unbounded sessions or leak one.
	go func() {
		defer func() { <-p.slots }()
		emit := func(stage string) {
			select {
			case stages <- stage:
			case <-run.Done():
			}
		}
		found, err := p.work(run, raw, emit)
		done <- result{found, err}
	}()
	for {
		select {
		case stage := <-stages:
			if note != nil {
				note(stage)
			}
		case result := <-done:
			if run.Err() == nil {
				return result.readings, result.err
			}
		case <-run.Done():
		}
		if run.Err() != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%w: the reader took longer than %s", ErrUnreadable, p.timeout)
		}
	}
}

func (p *pool) work(ctx context.Context, raw []byte, note func(string)) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("captcha: decode image: %w", err)
	}
	vs := views(img)
	if len(vs) == 0 {
		return nil, ErrUnreadable
	}
	r := p.take()
	if r == nil {
		note("loading local CAPTCHA model")
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e, err := p.create()
		if err != nil {
			return nil, fmt.Errorf("captcha: load model: %w", err)
		}
		r = &kept{engine: e}
	}
	usable := false
	defer func() {
		if usable && ctx.Err() == nil {
			p.put(r)
		} else {
			r.close()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	note("reading CAPTCHA")
	var tables [][]probabilities
	for _, view := range vs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		input, width, err := view.tensor()
		if err != nil {
			return nil, fmt.Errorf("captcha: %w", err)
		}
		logits, err := r.infer(ctx, input, width)
		if err != nil {
			return nil, fmt.Errorf("captcha: inference: %w", err)
		}
		table, err := logProbabilities(logits)
		if err != nil {
			return nil, fmt.Errorf("captcha: %w", err)
		}
		tables = append(tables, table)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	found := rank(tables)
	usable = true
	if len(found) == 0 {
		return nil, ErrUnreadable
	}
	return found, nil
}

func (p *pool) take() *kept {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.idle) == 0 {
		return nil
	}
	i := len(p.idle) - 1
	r := p.idle[i]
	p.idle = p.idle[:i]
	r.timer.Stop()
	return r
}

func (p *pool) put(r *kept) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var timer *time.Timer
	timer = time.AfterFunc(p.idleFor, func() {
		p.mu.Lock()
		i := -1
		if r.timer == timer {
			i = slices.Index(p.idle, r)
		}
		if i >= 0 {
			p.idle = slices.Delete(p.idle, i, i+1)
		}
		p.mu.Unlock()
		if i >= 0 {
			r.close()
		}
	})
	r.timer = timer
	p.idle = append(p.idle, r)
}
