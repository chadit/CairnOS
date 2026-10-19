//go:build windows

package cli

import "os/exec"

func killGroupOnCancel(*exec.Cmd) {
	// Nothing to do on Windows. The Unix version exists to take down make's
	// children with it, and here exec's own cancel already ends the process it
	// started.
}
