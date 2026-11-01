package cli_test

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// termiosReadRequest is darwin's ioctl for reading terminal settings.
const termiosReadRequest = unix.TIOCGETA

// openPTY opens a pseudo-terminal pair. Grant and unlock are the ioctls
// darwin's grantpt and unlockpt wrap. The kernel names the terminal side after
// the master's minor device number, which is what ptsname reports, so the name
// comes from Fstat rather than an ioctl x/sys/unix has no wrapper for. Neither
// side becomes this process's controlling terminal.
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}

	descriptor := int(master.Fd())

	for _, request := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if grantErr := unix.IoctlSetInt(descriptor, request, 0); grantErr != nil {
			t.Fatalf("unlock the pseudo-terminal: %v", grantErr)
		}
	}

	var device unix.Stat_t
	if statErr := unix.Fstat(descriptor, &device); statErr != nil {
		t.Fatalf("name the pseudo-terminal: %v", statErr)
	}

	path := fmt.Sprintf("/dev/ttys%03d", unix.Minor(uint64(device.Rdev)))

	slave, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open the terminal side %s: %v", path, err)
	}

	t.Cleanup(func() { _ = slave.Close() })

	return master, slave
}
