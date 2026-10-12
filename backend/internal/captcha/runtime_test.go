// SPDX-License-Identifier: MIT

//go:build cgo

package captcha

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func requireModel(t *testing.T) {
	t.Helper()
	if os.Getenv("HEAPLEACH_OCR_DIR") == "" {
		t.Skip("make test-captcha installs and tests the real OCR model")
	}
	if err := Available(); err != nil {
		t.Fatal(err)
	}
}

func TestModelMatchesPythonReadings(t *testing.T) {
	requireModel(t)
	for name, ref := range references(t) {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Read(context.Background(), raw, nil)
			if err != nil || !slices.Equal(got, ref.Readings) {
				t.Fatalf("readings %q, error %v; want %q", got, err, ref.Readings)
			}
		})
	}
}

func TestModelInferenceCanBeCancelledAndReused(t *testing.T) {
	requireModel(t)
	r, err := newEngine()
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		_, err := r.infer(ctx, make([]float32, 64*1024), 1024)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("native inference was not cancelled: %v", err)
		}
	}
	// A per-run termination must not poison the next inference in this session.
	if _, err := r.infer(context.Background(), make([]float32, 64*100), 100); err != nil {
		t.Fatalf("inference after cancellation: %v", err)
	}
}
