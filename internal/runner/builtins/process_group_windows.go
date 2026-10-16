//go:build windows

package builtins

import "os/exec"

// groupProcess does nothing on Windows: there is no process group to signal,
// and the plugin host's job object covers only plugin processes. Cancellation
// keeps the os/exec default, which kills the process a caller started but not
// a CLI a wrapper script started. That CLI can hold the caller's pipes open, so
// every caller must set WaitDelay to bound the wait.
func groupProcess(_ *exec.Cmd) {}
