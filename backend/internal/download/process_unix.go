//go:build unix

package download

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the helper in its own process group, so cancelling
// can reach the tools it spawns rather than only the script itself.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup terminates the helper and everything it started.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
