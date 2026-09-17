package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"unicode/utf8"
)

// echoOnNotice is printed once per run, and only when a terminal refuses to
// turn its echo off. An operator about to type a master password in front of
// other people has to know before the first keystroke.
const echoOnNotice = "note: terminal echo could not be turned off here, so what you type will be visible"

// readLine writes one prompt and reads its answer. A secret is read with the
// terminal's echo off, so it leaves nothing on screen.
//
// The prompt is written here, not by the caller, because echo has to be off
// first: between a printed prompt and a later toggle sits a window where the
// opening characters are echoed.
func (a *App) readLine(ctx context.Context, reader *bufio.Reader, prompt string, secret bool) (string, error) {
	if !secret {
		a.printOutf("%s", prompt)

		return readString(reader)
	}

	restore, err := a.hideEcho()
	if err != nil {
		a.noticeEchoOn(err)
		a.printOutf("%s", prompt)

		return readString(reader)
	}

	// The terminal keeps whatever state it was left in, so echo has to come back
	// on every exit: a normal return, an interrupt, or a canceled context. The
	// watcher covers the two the defer cannot.
	stop := watchForRestore(ctx, restore)

	defer stop()
	defer a.printOutf("\n")
	defer restore()

	a.printOutf("%s", prompt)

	return a.readMasked(reader)
}

// The two bytes a terminal sends for its erase key.
const (
	asciiBackspace = 0x08
	asciiDelete    = 0x7f
)

// What one typed rune shows as, and the sequence that takes one star back.
const (
	maskCharacter  = "*"
	eraseCharacter = "\b \b"
)

// readMasked reads a hidden answer keystroke by keystroke, printing a star per
// rune so the operator sees how much arrived. The newline is not in the value.
func (a *App) readMasked(reader *bufio.Reader) (string, error) {
	var typed maskedInput

	for {
		next, err := reader.ReadByte()
		if err != nil {
			return typed.value(), fmt.Errorf("read a line: %w", err)
		}

		switch next {
		case '\n', '\r':
			dropPairedNewline(reader)

			return typed.value(), nil
		case asciiBackspace, asciiDelete:
			if typed.eraseRune() {
				a.printOutf("%s", eraseCharacter)
			}
		default:
			if typed.addByte(next) {
				a.printOutf("%s", maskCharacter)
			}
		}
	}
}

// dropPairedNewline eats the second half of a pasted CRLF, which would otherwise
// answer the next prompt with an empty line. Waiting for a byte would hang a
// plain newline, so only a buffered one is taken.
func dropPairedNewline(reader *bufio.Reader) {
	if reader.Buffered() == 0 {
		return
	}

	paired, err := reader.Peek(1)
	if err != nil || (paired[0] != '\n' && paired[0] != '\r') {
		return
	}

	_, _ = reader.ReadByte()
}

// maskedInput tracks the bytes behind each star, so one typed rune is one star
// and one backspace whatever its encoding costs.
type maskedInput struct {
	typed []byte

	// pending holds a multi-byte rune that has not arrived whole yet.
	pending []byte

	// widths is the byte count behind each printed star, newest last.
	widths []int
}

// value returns what was typed, half-finished rune included: a truncated paste
// is the operator's to see.
func (m *maskedInput) value() string {
	return string(m.typed) + string(m.pending)
}

// addByte reports whether the byte completed a rune, which is when a star is
// due. An invalid byte counts at once, the way utf8 decoding treats it.
func (m *maskedInput) addByte(next byte) bool {
	m.pending = append(m.pending, next)

	if !utf8.FullRune(m.pending) {
		return false
	}

	m.typed = append(m.typed, m.pending...)
	m.widths = append(m.widths, len(m.pending))
	m.pending = m.pending[:0]

	return true
}

// eraseRune drops the last rune and reports whether a star goes with it. A
// half-typed rune goes first and was never shown.
func (m *maskedInput) eraseRune() bool {
	if len(m.pending) > 0 {
		m.pending = m.pending[:0]

		return false
	}

	if len(m.widths) == 0 {
		return false
	}

	last := len(m.widths) - 1
	m.typed = m.typed[:len(m.typed)-m.widths[last]]
	m.widths = m.widths[:last]

	return true
}

// readString reads through the newline, leaving an EOF for the caller to decide
// about: a closed stdin is a missing answer, not a read failure.
func readString(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return line, fmt.Errorf("read a line: %w", err)
	}

	return line, nil
}

// hideEcho turns echo off for the process stdin, reporting why it could not
// rather than silently reading a secret in the clear.
func (a *App) hideEcho() (func(), error) {
	file, isFile := a.stdin.(*os.File)
	if !isFile {
		return nil, errNotATerminal
	}

	return echoOff(file)
}

// noticeEchoOn explains a visible prompt, once, and only when there was a
// terminal that refused to hide it. A redirected stdin shows nobody anything,
// so saying it there would be noise on every scripted run.
func (a *App) noticeEchoOn(cause error) {
	if errors.Is(cause, errNotATerminal) {
		return
	}

	if a.echoNoticed {
		return
	}

	a.echoNoticed = true

	a.printOutf("  %s\n", echoOnNotice)
	a.logger.Debug("terminal echo stays on", "error", cause)
}

// watchForRestore puts echo back when the run ends some way the defer will
// never see. Ctrl+C during a hidden read would otherwise leave the operator's
// shell with echo off and no sign of why. It returns the call that ends the
// watch.
func watchForRestore(ctx context.Context, restore func()) func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)

	done := make(chan struct{})

	go func() {
		select {
		case <-signals:
			restore()
			reraiseInterrupt(signals)
		case <-ctx.Done():
			restore()
		case <-done:
		}
	}()

	return func() {
		signal.Stop(signals)
		close(done)
	}
}

// reraiseInterrupt hands the interrupt back to the default handler now that the
// terminal is readable again. Swallowing it would leave the run blocked on a
// prompt the operator has already walked away from.
func reraiseInterrupt(signals chan<- os.Signal) {
	signal.Stop(signals)
	signal.Reset(os.Interrupt)

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		return
	}

	_ = self.Signal(os.Interrupt)
}
