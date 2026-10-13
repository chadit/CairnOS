//go:build windows

package builtins

import "os/exec"

// groupProcess leaves the default cancellation in place. Windows has no process
// group to signal, and the plugin host's job object covers only plugin
// processes, so cancellation kills the process a caller started but not a CLI
// that a wrapper script started. That CLI can keep the caller's pipes open, so
// every caller must set WaitDelay to bound the wait.
func groupProcess(_ *exec.Cmd) {}
