//go:build linux || darwin

package cli

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel starts the command in a process group of its own and
// signals the whole group on cancel. Otherwise canceling kills make alone, and
// the go processes it started keep building after serve has let go.
func killGroupOnCancel(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	}
}
