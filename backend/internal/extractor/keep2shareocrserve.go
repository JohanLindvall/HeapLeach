// SPDX-License-Identifier: MIT

package extractor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/tools"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// A CAPTCHA reader is started once and handed one image after another.
// Starting it was nearly all of what reading a CAPTCHA cost: the frozen
// helper unpacks a Python runtime of 150 MB, starts it and loads the model,
// some five seconds of the five and a quarter a reading took, for a tenth of
// a second of reading. A reader left idle for keep2ShareOCRIdle is stopped,
// since each holds a model in memory.
//
// The exchange is the helper's --serve mode (helpers/captcha): "ready" once
// the model is loaded, then for each image a line giving its length and the
// image, answered with its readings, one per line, and an empty line. A
// reader that fails, outruns its budget or is cancelled partway is stopped,
// never reused, since what it would say next is no longer known.
//
// A helper built before --serve ignores the argument and waits for its input
// to end, so it never says it is ready. Its start runs out of time, the image
// is read the old way, a helper per image, and that helper is not asked to
// serve again for config.Keep2ShareOCRServeRetry.

// Variables so that tests need not sit the real ones out.
var (
	keep2ShareOCRStart = config.Keep2ShareOCRStart
	keep2ShareOCRIdle  = config.Keep2ShareOCRIdle
)

// keep2ShareOCRLines bounds an answer: a reader saying more than this many
// lines, or a line longer than the buffer, is not giving readings.
const keep2ShareOCRLines = 64

var errOCRCannotServe = errors.New("captcha: the CAPTCHA reader did not start")

type ocrReader struct {
	program string
	stop    context.CancelFunc // kills the process group
	in      io.WriteCloser
	pipe    *os.File // the read end of its stdout
	out     *bufio.Reader
	stderr  *util.BoundedBuffer // read only once exited is closed
	exited  chan struct{}
	err     error       // how it ended, once exited is closed
	idle    *time.Timer // while in the pool; guarded by its lock
}

var keep2ShareOCRReaders struct {
	sync.Mutex
	idle    []*ocrReader
	oneShot map[string]time.Time // program -> until when it reads one image per helper
}

// keep2ShareOCRServes reports whether program is to be asked to serve, which
// it is unless a recent start of it never said it was ready.
func keep2ShareOCRServes(program string) bool {
	pool := &keep2ShareOCRReaders
	pool.Lock()
	defer pool.Unlock()
	return !time.Now().Before(pool.oneShot[program])
}

func keep2ShareOCRCannotServe(program string) {
	pool := &keep2ShareOCRReaders
	pool.Lock()
	defer pool.Unlock()
	if pool.oneShot == nil {
		pool.oneShot = make(map[string]time.Time)
	}
	pool.oneShot[program] = time.Now().Add(config.Keep2ShareOCRServeRetry)
}

// takeOCRReader hands out an idle reader for program, if one is running.
func takeOCRReader(program string) *ocrReader {
	pool := &keep2ShareOCRReaders
	pool.Lock()
	defer pool.Unlock()
	for i := len(pool.idle) - 1; i >= 0; i-- {
		r := pool.idle[i]
		if r.program != program {
			continue
		}
		pool.idle = slices.Delete(pool.idle, i, i+1)
		r.idle.Stop()
		select {
		case <-r.exited: // ended while idle
			r.close()
		default:
			return r
		}
	}
	return nil
}

// putOCRReader keeps a reader that has just answered for the next image.
func putOCRReader(r *ocrReader) {
	pool := &keep2ShareOCRReaders
	pool.Lock()
	defer pool.Unlock()
	r.idle = time.AfterFunc(keep2ShareOCRIdle, func() {
		pool.Lock()
		i := slices.Index(pool.idle, r)
		if i >= 0 {
			pool.idle = slices.Delete(pool.idle, i, i+1)
		}
		pool.Unlock()
		if i >= 0 { // not taken again in the meantime
			r.close()
		}
	})
	pool.idle = append(pool.idle, r)
}

// startOCRReader starts program as a reader and waits for it to be ready.
func startOCRReader(ctx context.Context, program string) (*ocrReader, error) {
	// The frozen helper unpacks its runtime under TMPDIR, and a killed one
	// never removes it; a directory of its own is removed once it has ended.
	scratch, err := os.MkdirTemp("", "heapleach-ocr-")
	if err != nil {
		return nil, fmt.Errorf("captcha: %w", err)
	}
	life, stop := context.WithCancel(context.Background())
	cmd := tools.CommandContext(life, program, "--serve")
	cmd.Env = append(os.Environ(), "TMPDIR="+scratch)
	in, err := cmd.StdinPipe()
	if err != nil {
		stop()
		_ = os.RemoveAll(scratch)
		return nil, fmt.Errorf("captcha: %w", err)
	}
	// A pipe of our own rather than StdoutPipe, which Wait closes: a reader
	// is waited for in the background while it is still being read.
	pipe, sink, err := os.Pipe()
	if err != nil {
		stop()
		_ = in.Close()
		_ = os.RemoveAll(scratch)
		return nil, fmt.Errorf("captcha: %w", err)
	}
	stderr := &util.BoundedBuffer{Limit: config.ErrorBodySample}
	cmd.Stdout, cmd.Stderr = sink, stderr
	err = cmd.Start()
	_ = sink.Close()
	if err != nil {
		stop()
		_ = in.Close()
		_ = pipe.Close()
		_ = os.RemoveAll(scratch)
		return nil, fmt.Errorf("captcha: CAPTCHA reader: %w", err)
	}
	r := &ocrReader{program: program, stop: stop, in: in, pipe: pipe, out: bufio.NewReader(pipe),
		stderr: stderr, exited: make(chan struct{})}
	go func() {
		r.err = cmd.Wait()
		_ = os.RemoveAll(scratch)
		close(r.exited)
	}()

	var line []byte
	expired, err := r.within(ctx, keep2ShareOCRStart, func() (err error) {
		line, err = r.out.ReadSlice('\n')
		return err
	})
	switch {
	case expired && ctx.Err() != nil:
		r.shutdown()
		return nil, ctx.Err()
	case expired:
		r.shutdown()
		return nil, errOCRCannotServe
	case err != nil:
		return nil, r.failed(err)
	case string(line) != "ready\n":
		r.shutdown()
		return nil, fmt.Errorf("captcha: CAPTCHA reader: began with %q", util.Truncate(strings.TrimSpace(string(line)), 80))
	}
	return r, nil
}

// read hands the reader one image and returns the lines it answers with.
func (r *ocrReader) read(ctx context.Context, raw []byte) ([]string, error) {
	var lines []string
	tooMuch := errors.New("more than readings")
	expired, err := r.within(ctx, keep2ShareOCRTimeout, func() error {
		if _, err := fmt.Fprintf(r.in, "%d\n", len(raw)); err != nil {
			return err
		}
		if _, err := r.in.Write(raw); err != nil {
			return err
		}
		for {
			line, err := r.out.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) || len(lines) == keep2ShareOCRLines {
				return tooMuch
			}
			if err != nil {
				return err
			}
			if len(line) == 1 { // the empty line ending the answer
				return nil
			}
			lines = append(lines, string(line[:len(line)-1]))
		}
	})
	switch {
	case expired && ctx.Err() != nil:
		r.shutdown()
		return nil, ctx.Err()
	case expired:
		// Out of time is an image not read, which the next one may fix: the
		// file has not failed.
		r.shutdown()
		return nil, fmt.Errorf("%w: the reader took longer than %s", errKeep2ShareOCR, keep2ShareOCRTimeout)
	case errors.Is(err, tooMuch):
		r.shutdown()
		return nil, errKeep2ShareOCR
	case err != nil:
		return nil, r.failed(err)
	}
	return lines, nil
}

// within runs exchange, stopping the reader if it outlasts budget or ctx is
// cancelled first. A reader stopped that way is not to be used again.
func (r *ocrReader) within(ctx context.Context, budget time.Duration, exchange func() error) (expired bool, err error) {
	run, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	stop := context.AfterFunc(run, r.close)
	err = exchange()
	return !stop(), err
}

// close stops the reader without waiting for it to end. Closing the pipe as
// well unblocks a read even where a child the helper spawned keeps the
// other end open.
func (r *ocrReader) close() {
	r.stop()
	_ = r.in.Close()
	_ = r.pipe.Close()
}

// shutdown stops the reader and waits until it has ended and its runtime has
// been removed.
func (r *ocrReader) shutdown() {
	r.close()
	<-r.exited
}

// failed reports a reader that stopped answering, the way a helper that
// failed on its own image is reported: how it ended, and what it said.
func (r *ocrReader) failed(cause error) error {
	r.shutdown()
	if r.err != nil {
		cause = r.err
	}
	return fmt.Errorf("captcha: CAPTCHA reader: %w: %s", cause, util.Truncate(strings.TrimSpace(r.stderr.String()), 200))
}
