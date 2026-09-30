//go:build windows

package cli

import "os/exec"

// killGroupOnCancel leaves Windows to exec's own cancel, which ends the
// process it started.
func killGroupOnCancel(*exec.Cmd) {}
