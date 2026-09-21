//go:build windows

package plugin_test

import (
	"testing"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a process that
// has not exited yet. x/sys/windows carries no constant for it.
const stillActive = 259

// killTestExitCode is what the simulated crash below reports. Nothing reads
// it; the host treats any unexpected exit as a plugin_down.
const killTestExitCode = 1

// killProcessTree terminates the plugin process, standing in for the SIGKILL
// the Unix helper sends to a process group. The echo plugin spawns no children.
func killProcessTree(t *testing.T, pid int) {
	t.Helper()

	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatalf("open plugin process %d: %v", pid, err)
	}

	defer func() { _ = windows.CloseHandle(handle) }()

	if err := windows.TerminateProcess(handle, killTestExitCode); err != nil {
		t.Fatalf("terminate plugin process %d: %v", pid, err)
	}
}

// requireProcessGone fails unless the process has exited. Close waits for the
// reap, so a process still running here means the job object missed it. A pid
// that can no longer be opened is gone.
func requireProcessGone(t *testing.T, pid int) {
	t.Helper()

	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return
	}

	defer func() { _ = windows.CloseHandle(handle) }()

	var code uint32

	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		t.Fatalf("read exit code of plugin process %d: %v", pid, err)
	}

	if code == stillActive {
		t.Fatalf("plugin process %d still running after Close", pid)
	}
}
