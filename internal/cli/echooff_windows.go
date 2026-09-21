//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// echoOff stops the console behind file from printing what is typed, and
// returns the call that puts it back. A master password is otherwise left on
// screen for whoever looks next.
//
// Line input goes off with it, so a read returns per keystroke and the prompt
// can star it. Processed input stays on, so Ctrl+C still works.
func echoOff(file *os.File) (func(), error) {
	handle := windows.Handle(file.Fd())

	before, err := currentConsoleMode(handle)
	if err != nil {
		return nil, err
	}

	quiet := before &^ uint32(windows.ENABLE_ECHO_INPUT|windows.ENABLE_LINE_INPUT)

	if err := windows.SetConsoleMode(handle, quiet); err != nil {
		return nil, fmt.Errorf("turn terminal echo off: %w", err)
	}

	return func() { _ = windows.SetConsoleMode(handle, before) }, nil
}

// currentConsoleMode reads the console settings, answering errNotATerminal for
// the invalid-handle error a pipe or a redirected stdin gives back. That answer
// is how a real terminal is told apart without a second probe.
func currentConsoleMode(handle windows.Handle) (uint32, error) {
	var mode uint32

	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		if errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			return 0, errNotATerminal
		}

		return 0, fmt.Errorf("read the terminal settings: %w", err)
	}

	return mode, nil
}
