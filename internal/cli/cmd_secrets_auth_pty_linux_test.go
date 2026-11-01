package cli_test

import (
	"os"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// termiosReadRequest is Linux's ioctl for reading terminal settings.
const termiosReadRequest = unix.TCGETS

// openPTY opens a pseudo-terminal pair. The unlock and the name lookup are the
// two ioctls the C library's grantpt and ptsname wrap, spelled out because the
// repo carries no pty helper. Neither side becomes this process's controlling
// terminal.
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}

	descriptor := int(master.Fd())

	if unlockErr := unix.IoctlSetPointerInt(descriptor, unix.TIOCSPTLCK, 0); unlockErr != nil {
		t.Fatalf("unlock the pseudo-terminal: %v", unlockErr)
	}

	number, err := unix.IoctlGetInt(descriptor, unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("name the pseudo-terminal: %v", err)
	}

	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open the terminal side: %v", err)
	}

	t.Cleanup(func() { _ = slave.Close() })

	return master, slave
}
