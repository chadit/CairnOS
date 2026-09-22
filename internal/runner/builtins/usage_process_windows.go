//go:build windows

package builtins

import "os/exec"

// groupProcess leaves the default cancellation in place. Windows has no process
// group to signal, and a job object is the plugin host's business rather than
// this runner's, so cancellation reaches the launcher alone.
func groupProcess(_ *exec.Cmd) {}
