//go:build linux || darwin

package cli

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// echoOff hides what is typed, so a master password is not left on screen, and
// returns the call that puts it back. ICANON goes off too, so each keystroke
// can be starred. Ctrl+C still signals, but the quit and suspend keys are
// switched off: either would stop the process with echo still off.
func echoOff(file *os.File) (func() error, error) {
	descriptor := int(file.Fd())

	before, err := currentTermios(descriptor)
	if err != nil {
		return nil, err
	}

	quiet := quietTermios(*before)

	if err := unix.IoctlSetTermios(descriptor, ioctlWriteTermios, &quiet); err != nil {
		return nil, fmt.Errorf("turn terminal echo off: %w", err)
	}

	return func() error {
		if err := unix.IoctlSetTermios(descriptor, ioctlWriteTermios, before); err != nil {
			return fmt.Errorf("turn terminal echo back on: %w", err)
		}

		return nil
	}, nil
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
// byte with no timer. With Ctrl+\ and Ctrl+Z switched off they arrive as
// plain control bytes, which the masked read drops.
func quietTermios(before unix.Termios) unix.Termios {
	quiet := before
	quiet.Lflag &^= unix.ECHO | unix.ICANON
	quiet.Cc[unix.VMIN] = 1
	quiet.Cc[unix.VTIME] = 0
	quiet.Cc[unix.VQUIT] = disabledControlChar
	quiet.Cc[unix.VSUSP] = disabledControlChar

	return quiet
}
