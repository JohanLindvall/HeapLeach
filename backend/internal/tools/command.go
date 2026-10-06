// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/util"
)

// Command owns a helper and, on Unix, its child processes.
type Command struct{ *exec.Cmd }

// Wait also cleans up children when a helper fails or leaves inherited pipes
// open. os/exec's WaitDelay bounds the wait but only kills the parent process.
func (c *Command) Wait() error {
	err := c.Cmd.Wait()
	if err != nil {
		_ = killGroup(c.Cmd)
	}
	return err
}

// Run starts the helper and waits with the same child cleanup as Wait.
func (c *Command) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// CommandContext cancels a helper's process group on Unix, including the
// children yt-dlp and shell scripts spawn. WaitDelay bounds inherited pipes
// left open by a child after its parent exits.
func CommandContext(ctx context.Context, name string, args ...string) *Command {
	cmd := exec.CommandContext(ctx, name, args...)
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = config.HelperWaitDelay
	return &Command{cmd}
}

// Probe runs a metadata helper with bounded time and output. The returned
// JSON must be whole: truncation is an error, never a partial listing.
func Probe(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, config.ProbeTimeout)
	defer cancel()
	cmd := CommandContext(ctx, name, args...)
	out := &util.BoundedBuffer{Limit: config.MaxResponseBytes}
	errout := &util.BoundedBuffer{Limit: config.ErrorBodySample}
	cmd.Stdout, cmd.Stderr = out, errout
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		if detail := strings.TrimSpace(errout.String()); detail != "" {
			return nil, fmt.Errorf("%w: %s", err, util.Truncate(detail, 300))
		}
		return nil, err
	}
	if out.Truncated {
		return nil, fmt.Errorf("helper output exceeds %d bytes", out.Limit)
	}
	return out.Bytes(), nil
}
