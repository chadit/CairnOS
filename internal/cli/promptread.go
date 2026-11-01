package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"unicode/utf8"
)

// echoOnNotice is printed once per run, and only when a terminal refuses to
// turn its echo off. An operator about to type a master password in front of
// other people has to know before the first keystroke.
const echoOnNotice = "note: terminal echo could not be turned off here, so what you type will be visible"

// readLine writes one prompt to stderr and reads its answer, so a redirected
// stdout still shows the prompts and records nothing about the answers. A
// secret is read with echo off. The prompt is written here because echo has to
// be off first, or the opening characters are echoed.
func (a *App) readLine(ctx context.Context, reader *bufio.Reader, prompt string, secret bool) (string, error) {
	if !secret {
		a.printErrf("%s", prompt)

		return awaitRead(ctx, nil, func() (string, error) { return readString(reader) })
	}

	restore, err := a.hideEcho()
	if err != nil {
		return a.readSecretVisibly(ctx, reader, prompt, err)
	}

	// Ctrl+C has to reach the read even when nothing else traps it, or the
	// process dies with the operator's terminal still silent.
	interrupts := make(chan os.Signal, signalChanSize)
	signal.Notify(interrupts, os.Interrupt)

	defer signal.Stop(interrupts)
	defer a.restoreEcho(restore)

	return askAgainOnEmpty(ctx, func() (string, error) {
		a.printErrf("%s", prompt)

		return a.readMasked(ctx, interrupts, reader)
	})
}

// readSecretVisibly reads a secret the terminal could not hide. A pipe gets one
// read, since nobody is there to answer again.
func (a *App) readSecretVisibly(
	ctx context.Context, reader *bufio.Reader, prompt string, cause error,
) (string, error) {
	a.noticeEchoOn(cause)

	read := func() (string, error) {
		a.printErrf("%s", prompt)

		return awaitRead(ctx, nil, func() (string, error) { return readString(reader) })
	}

	if errors.Is(cause, errNotATerminal) {
		return read()
	}

	return askAgainOnEmpty(ctx, read)
}

// askAgainOnEmpty repeats a terminal prompt until it gets an answer. An empty
// line there is the second half of a pasted line break or an Enter pressed
// early, and failing on it would tell an operator at a terminal to go find
// one.
func askAgainOnEmpty(ctx context.Context, read func() (string, error)) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", errInterrupted
		}

		line, err := read()
		if err != nil || trimLineEnd(line) != "" {
			return line, err
		}
	}
}

// readResult carries one read's outcome out of the goroutine awaitRead runs it
// in.
type readResult[T any] struct {
	err   error
	value T
}

// awaitRead waits for read, an interrupt, or the end of the run, whichever
// comes first. A terminal read cannot itself be interrupted, so read runs in
// its own goroutine; one that loses stays blocked until the process exits,
// which is harmless because the run is ending and nothing reads after it.
func awaitRead[T any](ctx context.Context, interrupts <-chan os.Signal, read func() (T, error)) (T, error) {
	arrived := make(chan readResult[T], oneShotChanSize)

	go func() {
		value, err := read()
		arrived <- readResult[T]{value: value, err: err}
	}()

	var zero T

	select {
	case got := <-arrived:
		return got.value, got.err
	case <-interrupts:
		return zero, errInterrupted
	case <-ctx.Done():
		return zero, errInterrupted
	}
}

// trimLineEnd drops the line terminator and nothing else, so a secret keeps
// any spaces at its edges.
func trimLineEnd(line string) string {
	return strings.TrimRight(line, "\r\n")
}

// The bytes a terminal sends for its erase key, and the one that opens the
// escape sequence an arrow or function key sends.
const (
	asciiBackspace = 0x08
	asciiDelete    = 0x7f
	asciiEscape    = 0x1b
)

// firstPrintable is the lowest byte that is typed text rather than a control
// character such as Ctrl+U or Ctrl+D. A tab is the one control byte kept, since
// a pasted secret can hold one.
const firstPrintable = 0x20

// escapeFinalFirst and escapeFinalLast bound the byte that ends a CSI or SS3
// escape sequence; every byte before it is a parameter.
const (
	escapeFinalFirst = 0x40
	escapeFinalLast  = 0x7e
)

// What one typed rune shows as, and the sequence that takes one star back.
const (
	maskCharacter  = "*"
	eraseCharacter = "\b \b"
)

// readMasked reads a hidden answer keystroke by keystroke, printing a star per
// rune so the operator sees how much arrived. The newline is not in the value,
// and it is echoed here because the terminal no longer echoes the Enter.
func (a *App) readMasked(ctx context.Context, interrupts <-chan os.Signal, reader *bufio.Reader) (string, error) {
	var typed maskedInput

	for {
		next, err := awaitRead(ctx, interrupts, reader.ReadByte)
		if errors.Is(err, errInterrupted) {
			return "", err
		}

		if err != nil {
			return typed.value(), fmt.Errorf("read a line: %w", err)
		}

		if next == '\n' || next == '\r' {
			a.printErrf("\n")

			return typed.value(), dropPairedNewline(reader)
		}

		a.maskKeystroke(&typed, next)
	}
}

// maskKeystroke applies one non-newline byte to typed and draws or erases the
// star it earns, so the screen always shows one star per rune held.
func (a *App) maskKeystroke(typed *maskedInput, next byte) {
	if next == asciiBackspace || next == asciiDelete {
		if typed.eraseRune() {
			a.printErrf("%s", eraseCharacter)
		}

		return
	}

	if typed.isText(next) && typed.addByte(next) {
		a.printErrf("%s", maskCharacter)
	}
}

// dropPairedNewline eats the second half of a pasted CRLF, which would otherwise
// answer the next prompt with an empty line. Waiting for a byte would hang a
// plain newline, so only a buffered one is taken.
func dropPairedNewline(reader *bufio.Reader) error {
	if reader.Buffered() == 0 {
		return nil
	}

	paired, err := reader.Peek(1)
	if err != nil {
		return fmt.Errorf("read past the newline: %w", err)
	}

	if paired[0] != '\n' && paired[0] != '\r' {
		return nil
	}

	if _, err := reader.ReadByte(); err != nil {
		return fmt.Errorf("read past the newline: %w", err)
	}

	return nil
}

// escapeState tracks how far into a key's escape sequence the input is.
type escapeState int

const (
	escapeNone escapeState = iota
	escapeStarted
	escapeSequence
)

// maskedInput tracks the bytes behind each star, so one typed rune is one star
// and one backspace whatever its encoding costs.
type maskedInput struct {
	typed []byte

	// pending holds a multi-byte rune that has not arrived whole yet.
	pending []byte

	// widths is the byte count behind each printed star, newest last.
	widths []int

	escape escapeState
}

// value returns what was typed, half-finished rune included: a truncated paste
// is the operator's to see.
func (m *maskedInput) value() string {
	return string(m.typed) + string(m.pending)
}

// isText reports whether next belongs in the answer. Control bytes and the
// escape sequences arrow and function keys send are dropped, since the
// operator cannot see them land in a hidden secret. Only [ or O after Esc opens
// a sequence, so the key after a lone Esc, or an Alt chord's key, is kept.
func (m *maskedInput) isText(next byte) bool {
	if m.escape == escapeSequence {
		if next >= escapeFinalFirst && next <= escapeFinalLast {
			m.escape = escapeNone
		}

		return false
	}

	if m.escape == escapeStarted {
		m.escape = escapeNone
		if next == '[' || next == 'O' {
			m.escape = escapeSequence

			return false
		}
	}

	if next == asciiEscape {
		m.escape = escapeStarted

		return false
	}

	return next >= firstPrintable || next == '\t'
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
func (a *App) hideEcho() (func() error, error) {
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

	a.printErrf("  %s\n", echoOnNotice)
	a.logger.Debug("terminal echo stays on", "error", cause)
}

// restoreEcho puts the terminal back, logging a failure: the answer is already
// read, and an operator left with a silent shell needs to see why.
func (a *App) restoreEcho(restore func() error) {
	if err := restore(); err != nil {
		a.logger.Warn("terminal echo may still be off", "error", err)
	}
}
