//go:build !windows

package builtins

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// groupProcess gives the probe its own process group and signals that group on
// cancellation. A launcher is usually a wrapper script, so signaling the
// wrapper alone would leave the CLI it started running past the deadline.
func groupProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Cancel = func() error {
		return killProcessGroup(cmd)
	}
}

// killProcessGroup signals the negative group id, which Setpgid made equal to
// the child's pid. A group that is already gone reads as a finished process,
// which is what os/exec expects and what keeps Wait's error meaningful.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}

		return fmt.Errorf("kill launcher process group: %w", err)
	}

	return nil
}
