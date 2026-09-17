//go:build linux || darwin

package cli

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// echoOff stops the terminal behind file from printing what is typed, and
// returns the call that puts it back. A master password is otherwise left on
// screen for whoever looks next.
//
// ICANON goes off with it, so a read returns per keystroke and the prompt can
// star it. ISIG stays on, so Ctrl+C still works.
func echoOff(file *os.File) (func(), error) {
	descriptor := int(file.Fd())

	before, err := unix.IoctlGetTermios(descriptor, ioctlReadTermios)
	if err != nil {
		// A pipe or a file answers ENOTTY here, which is how this tells a real
		// terminal from a redirected stdin without a second probe.
		if errors.Is(err, unix.ENOTTY) {
			return nil, errNotATerminal
		}

		return nil, fmt.Errorf("read the terminal settings: %w", err)
	}

	quiet := *before
	quiet.Lflag &^= unix.ECHO | unix.ICANON

	// Out of canonical mode these two decide when a read returns: on the first
	// byte, with no timer.
	quiet.Cc[unix.VMIN] = 1
	quiet.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(descriptor, ioctlWriteTermios, &quiet); err != nil {
		return nil, fmt.Errorf("turn terminal echo off: %w", err)
	}

	return func() { _ = unix.IoctlSetTermios(descriptor, ioctlWriteTermios, before) }, nil
}
