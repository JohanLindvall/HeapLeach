// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// The test executable doubles as a portable helper; no installed media
// programs or shell are needed to exercise output limits and cancellation.
func TestMetadataHelperProcess(t *testing.T) {
	if os.Getenv("HEAPLEACH_TEST_HELPER") != "1" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "success":
		fmt.Print(`{"title":"Synthetic clip"}`)
	case "failure":
		fmt.Fprint(os.Stderr, "synthetic helper error\n"+strings.Repeat("x", config.ErrorBodySample*2))
		os.Exit(7)
	case "oversize":
		chunk := strings.Repeat("x", 4096)
		for n := 0; n <= config.MaxResponseBytes; n += len(chunk) {
			fmt.Print(chunk)
		}
	case "wait":
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestProbeBoundsOutputAndReportsFailures(t *testing.T) {
	t.Setenv("HEAPLEACH_TEST_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ mode, want string }{
		{"success", ""}, {"failure", "synthetic helper error"}, {"oversize", "helper output exceeds"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			out, err := Probe(t.Context(), exe, "-test.run=^TestMetadataHelperProcess$", "--", tc.mode)
			if tc.want == "" {
				if err != nil || string(out) != `{"title":"Synthetic clip"}` {
					t.Fatalf("Probe = %q, %v", out, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || len(err.Error()) > 400 || out != nil {
				t.Fatalf("Probe = %d bytes, %v", len(out), err)
			}
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := Probe(ctx, exe, "-test.run=^TestMetadataHelperProcess$", "--", "wait"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled probe = %v", err)
	}
}
