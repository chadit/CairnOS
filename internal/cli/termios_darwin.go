package cli

import "golang.org/x/sys/unix"

// Darwin spells the same pair of ioctl requests differently from Linux; see the
// Linux file beside this one for why they are constants rather than one
// portable name. Both files take their constraint from the filename.
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
