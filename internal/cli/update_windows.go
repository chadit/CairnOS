//go:build windows

package cli

import "os/exec"

// killGroupOnCancel does nothing on Windows. The Unix version exists to take
// down make's children with it, and here exec's own cancel already ends the
// process it started.
func killGroupOnCancel(*exec.Cmd) {}
