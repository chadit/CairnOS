//go:build !windows

package plugin_test

import (
	"errors"
	"syscall"
	"testing"
)

// killProcessTree SIGKILLs the whole group the plugin leads. New starts the
// child with Setpgid, so the group id equals the pid the caller passes.
func killProcessTree(t *testing.T, pid int) {
	t.Helper()

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill plugin process group %d: %v", pid, err)
	}
}

// requireProcessGone fails unless the process has been reaped. Close waits for
// the reap before returning, so signal 0 must report ESRCH with no polling.
func requireProcessGone(t *testing.T, pid int) {
	t.Helper()

	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("plugin process %d still findable after Close (kill err = %v)", pid, err)
	}
}
