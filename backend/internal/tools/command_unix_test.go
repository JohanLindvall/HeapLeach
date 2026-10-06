// SPDX-License-Identifier: MIT

//go:build unix

package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCommandWaitDeadlineKillsSurvivingChildren(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	cmd := CommandContext(t.Context(), "sh", "-c", `(sleep 0.3; echo survived > "$1") & exit 0`, "sh", marker)
	cmd.WaitDelay = 20 * time.Millisecond
	cmd.Stdout = &bytes.Buffer{}
	if err := cmd.Run(); !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("helper with inherited pipe = %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child survived helper wait deadline: %v", err)
	}
}

func TestCommandCancellationKillsChildren(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	marker := filepath.Join(t.TempDir(), "child-survived")
	cmd := CommandContext(ctx, "sh", "-c", `(sleep 0.3; echo survived > "$1") & echo ready; wait`, "sh", marker)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		cancel()
		_ = cmd.Wait()
		t.Fatalf("helper startup = %q, %v", line, err)
	}
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Fatal("canceled helper succeeded")
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child survived group cancellation: %v", err)
	}
}
