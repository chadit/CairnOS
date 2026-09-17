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

	before, err := currentTermios(descriptor)
	if err != nil {
		return nil, err
	}

	quiet := quietTermios(*before)

	if err := unix.IoctlSetTermios(descriptor, ioctlWriteTermios, &quiet); err != nil {
		return nil, fmt.Errorf("turn terminal echo off: %w", err)
	}

	return func() { _ = unix.IoctlSetTermios(descriptor, ioctlWriteTermios, before) }, nil
}

// currentTermios reads the terminal settings, answering errNotATerminal for the
// ENOTTY a pipe or a redirected stdin gives back. That answer is how a real
// terminal is told apart without a second probe.
func currentTermios(descriptor int) (*unix.Termios, error) {
	before, err := unix.IoctlGetTermios(descriptor, ioctlReadTermios)
	if err != nil {
		if errors.Is(err, unix.ENOTTY) {
			return nil, errNotATerminal
		}

		return nil, fmt.Errorf("read the terminal settings: %w", err)
	}

	return before, nil
}

// quietTermios turns off echo and canonical mode. Out of canonical mode VMIN and
// VTIME decide when a read returns, and these values return it on the first
// byte with no timer.
func quietTermios(before unix.Termios) unix.Termios {
	quiet := before
	quiet.Lflag &^= unix.ECHO | unix.ICANON
	quiet.Cc[unix.VMIN] = 1
	quiet.Cc[unix.VTIME] = 0

	return quiet
}
