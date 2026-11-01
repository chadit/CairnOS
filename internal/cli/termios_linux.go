package cli

import "golang.org/x/sys/unix"

// The ioctl requests that read and write a terminal's settings. They are split
// per platform because the two families spell the same pair differently, and
// naming the wrong one fails against the operator's terminal rather than at
// compile time.
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)

// disabledControlChar is Linux's _POSIX_VDISABLE: a control character set to
// it is switched off.
const disabledControlChar = 0
