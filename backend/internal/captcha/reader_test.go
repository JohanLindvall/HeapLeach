// SPDX-License-Identifier: MIT

package captcha

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

type fakeEngine struct {
	run  func(context.Context) error
	stop func()
}

func (f *fakeEngine) close() {
	if f.stop != nil {
		f.stop()
	}
}

func (f *fakeEngine) infer(ctx context.Context, _ []float32, _ int) ([]float32, error) {
	if f.run != nil {
		if err := f.run(ctx); err != nil {
			return nil, err
		}
	}
	data := make([]float32, config.CaptchaModelAlphabet*12)
	for i := range data {
		data[i] = -30
	}
	for i, c := range "ab3de7" {
		k := bytes.IndexByte([]byte(symbols), byte(c))
		data[2*i*config.CaptchaModelAlphabet+columns[k+1][0]] = 0
		data[(2*i+1)*config.CaptchaModelAlphabet] = 0
	}
	return data, nil
}

func testImage(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 100, 50))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testPool(t *testing.T, create func() (engine, error)) *pool {
	t.Helper()
	p := &pool{create: create, slots: make(chan struct{}, 2), timeout: time.Second, idleFor: time.Minute}
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for len(p.slots) > 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if len(p.slots) > 0 {
			t.Error("OCR worker did not release its slot")
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, r := range p.idle {
			r.timer.Stop()
			r.close()
		}
		p.idle = nil
	})
	return p
}

func TestReaderRejectsInvalidImagesBeforeLoading(t *testing.T) {
	p := testPool(t, func() (engine, error) {
		t.Error("loaded a model for an invalid image")
		return &fakeEngine{}, nil
	})
	for _, raw := range [][]byte{[]byte("<html>challenge</html>"), make([]byte, config.Keep2ShareCaptchaBytes+1)} {
		if _, err := p.read(context.Background(), raw, nil); err == nil {
			t.Fatal("accepted a non-image or oversized input")
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 1024, 1024))); err != nil {
		t.Fatal(err)
	}
	if _, err := p.read(context.Background(), b.Bytes(), nil); err == nil {
		t.Fatal("accepted excessive pixel dimensions")
	}
}

func TestReaderReusesModelAndReleasesItWhenIdle(t *testing.T) {
	var starts atomic.Int32
	closed := make(chan struct{}, 1)
	p := testPool(t, func() (engine, error) {
		starts.Add(1)
		return &fakeEngine{stop: func() { closed <- struct{}{} }}, nil
	})
	p.idleFor = 100 * time.Millisecond
	for range 3 {
		got, err := p.read(context.Background(), testImage(t), nil)
		if err != nil || !slices.Equal(got, []string{"ab3de7"}) {
			t.Fatalf("readings=%q error=%v", got, err)
		}
	}
	if starts.Load() != 1 {
		t.Fatalf("loaded %d models for three consecutive images", starts.Load())
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("idle model was not released")
	}
}

func TestReaderConcurrencyIsBounded(t *testing.T) {
	var starts, active, peak atomic.Int32
	p := testPool(t, func() (engine, error) {
		starts.Add(1)
		var busy atomic.Bool
		return &fakeEngine{run: func(ctx context.Context) error {
			if busy.Swap(true) {
				t.Error("one session was used concurrently")
			}
			defer busy.Store(false)
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			select {
			case <-time.After(10 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}, nil
	})
	raw := testImage(t)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if _, err := p.read(context.Background(), raw, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if starts.Load() > 2 || peak.Load() > 2 {
		t.Fatalf("started=%d simultaneous=%d; want at most two", starts.Load(), peak.Load())
	}
}

func TestReaderCancelledDuringLoadRetainsSlotUntilConstructionEnds(t *testing.T) {
	started, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	p := testPool(t, func() (engine, error) {
		close(started)
		<-release
		return &fakeEngine{stop: func() { close(closed) }}, nil
	})
	p.slots = make(chan struct{}, 1)
	raw := testImage(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.read(ctx, raw, nil); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled load: %v", err)
	}
	if len(p.slots) != 1 {
		t.Fatal("session construction lost its concurrency reservation")
	}
	wait, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	var notes []string
	_, err := p.read(wait, raw, func(s string) { notes = append(notes, s) })
	if !errors.Is(err, context.DeadlineExceeded) || !slices.Equal(notes, []string{"waiting for a local CAPTCHA reader"}) {
		t.Fatalf("cancelled waiter: error=%v notes=%v", err, notes)
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("session created after cancellation was leaked")
	}
}

func TestReaderCancellationAndTimeoutHaveDifferentMeanings(t *testing.T) {
	for _, callerCancels := range []bool{false, true} {
		closed := make(chan struct{}, 1)
		p := testPool(t, func() (engine, error) {
			return &fakeEngine{run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
				stop: func() { closed <- struct{}{} }}, nil
		})
		ctx := context.Background()
		if callerCancels {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
			defer cancel()
		} else {
			p.timeout = 30 * time.Millisecond
		}
		_, err := p.read(ctx, testImage(t), nil)
		want := ErrUnreadable
		if callerCancels {
			want = context.DeadlineExceeded
		}
		if !errors.Is(err, want) {
			t.Fatalf("callerCancels=%v: %v; want %v", callerCancels, err, want)
		}
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("cancelled inference did not release the session")
		}
	}
}
