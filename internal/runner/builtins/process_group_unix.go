//go:build !windows

package builtins

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// groupProcess gives a child its own process group and kills that group on
// cancel, since every caller may run a script that forks its real work.
// Neither launchd's job-exit sweep nor a terminal hangup reaches the new group,
// so cmd/cairnos turns SIGTERM and SIGHUP into a cancel. A SIGKILL to the
// parent still leaves the group running.
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

		return fmt.Errorf("kill process group: %w", err)
	}

	return nil
}
