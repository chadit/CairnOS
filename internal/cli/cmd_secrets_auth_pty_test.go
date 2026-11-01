//go:build linux || darwin

package cli_test

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/chadit/CairnOS/internal/bitwarden"
	"github.com/chadit/CairnOS/internal/cli"
	"github.com/chadit/CairnOS/internal/environ"
)

// promptWait bounds how long a case waits for one prompt to reach the terminal.
const promptWait = 10 * time.Second

// promptCount is how many answers one run of `secrets auth` needs.
const promptCount = 4

// authPrompts names the prompts in the order the command asks them.
func authPrompts() [promptCount]string {
	return [promptCount]string{"item name", "client id", "client secret", "master password"}
}

// What a hidden prompt writes per typed rune, and what it writes to take one back.
const (
	promptStar  = "*"
	promptErase = "\b \b"
)

// The bytes a terminal sends for its erase key, for Ctrl+C, for Ctrl+U, for a
// lone Esc, and an arrow key's escape sequence.
const (
	keyBackspace = 0x08
	keyDelete    = 0x7f
	keyInterrupt = 0x03
	keyKillLine  = 0x15
	keyEscape    = 0x1b
	keyLeftArrow = "\x1b[D"
)

// terminalTranscriptSize preallocates the transcript, which the reader appends
// to in a loop.
const terminalTranscriptSize = 4096

// terminalReadSize is one read off the master side. Any size works, since the
// reader loops until the case ends.
const terminalReadSize = 512

// wakeChanSize holds one pending wake: a waiting case only needs to know that
// something arrived since it last looked at the transcript.
const wakeChanSize = 1

// TestSecretsAuthHidesTypingAndRestoresTheTerminal watches the terminal itself.
// A terminal echoes what is typed back, so the master side sees the item name,
// typed with echo on, and must not see the three secrets after it.
func TestSecretsAuthHidesTypingAndRestoresTheTerminal(t *testing.T) {
	t.Parallel()

	master, slave := openPTY(t)
	dir := t.TempDir()

	before := readTermios(t, slave)
	if before.Lflag&unix.ECHO == 0 {
		t.Fatal("the fresh terminal already has echo off, so this proves nothing")
	}

	seen := answerAuthPrompts(t, master, slave, dir, [promptCount]string{
		authTypedItem + "\n",
		authTypedID + "\n",
		authTypedSecret + "\n",
		authTypedKey + "\n",
	})

	assertTerminalTranscript(t, seen.text())

	if after := readTermios(t, slave); *after != *before {
		t.Fatalf("terminal settings after = %+v, want the original %+v", *after, *before)
	}

	if written := readCredentials(t, dir); !strings.Contains(written, authTypedKey) {
		t.Fatalf("the answers typed on the terminal did not reach the file:\n%s", written)
	}
}

// assertTerminalTranscript is the assertion the whole case exists for: the
// answer typed with echo on came back, and the three typed with echo off did
// not.
func assertTerminalTranscript(t *testing.T, transcript string) {
	t.Helper()

	if !strings.Contains(transcript, authTypedItem) {
		t.Fatalf("the terminal never echoed the item name, so it echoes nothing "+
			"and this case cannot tell hidden typing from visible:\n%s", transcript)
	}

	for _, secret := range []string{authTypedID, authTypedSecret, authTypedKey} {
		if strings.Contains(transcript, secret) {
			t.Fatalf("the terminal echoed a secret back:\n%s", transcript)
		}
	}
}

// TestSecretsAuthShowsOneStarPerTypedCharacter proves a hidden prompt shows how
// much arrived, which is what a mis-paste is otherwise invisible to. The item
// name is not a secret, so the terminal echoes that one instead.
func TestSecretsAuthShowsOneStarPerTypedCharacter(t *testing.T) {
	t.Parallel()

	master, slave := openPTY(t)
	dir := t.TempDir()

	seen := answerAuthPrompts(t, master, slave, dir, [promptCount]string{
		authTypedItem + "\n",
		authTypedID + "\n",
		authTypedSecret + "\n",
		authTypedKey + "\n",
	})

	transcript := seen.text()

	for _, want := range []struct{ prompt, next, typed string }{
		{authPrompts()[1], authPrompts()[2], authTypedID},
		{authPrompts()[2], authPrompts()[3], authTypedSecret},
		{authPrompts()[3], "", authTypedKey},
	} {
		if stars := visibleStars(t, transcript, want.prompt, want.next); stars != len(want.typed) {
			t.Fatalf("the prompt for %q showed %d stars, want %d", want.prompt, stars, len(want.typed))
		}
	}

	if stars := visibleStars(t, transcript, authPrompts()[0], authPrompts()[1]); stars != 0 {
		t.Fatalf("the item name prompt showed %d stars, want none", stars)
	}

	for _, field := range []struct{ variable, typed string }{
		{bitwarden.EnvClientID, authTypedID},
		{bitwarden.EnvClientSecret, authTypedSecret},
		{bitwarden.EnvMasterKey, authTypedKey},
	} {
		assertStored(t, dir, field.variable, field.typed)
	}
}

// TestSecretsAuthBackspaceTakesBackACharacter types both erase bytes, since a
// terminal sends one or the other.
func TestSecretsAuthBackspaceTakesBackACharacter(t *testing.T) {
	t.Parallel()

	master, slave := openPTY(t)
	dir := t.TempDir()

	seen := answerAuthPrompts(t, master, slave, dir, [promptCount]string{
		authTypedItem + "\n",
		authTypedID + "X" + string(rune(keyDelete)) + "\n",
		authTypedSecret + "Y" + string(rune(keyBackspace)) + "\n",
		authTypedKey + "\n",
	})

	transcript := seen.text()

	for _, want := range []struct{ prompt, next, typed string }{
		{authPrompts()[1], authPrompts()[2], authTypedID},
		{authPrompts()[2], authPrompts()[3], authTypedSecret},
	} {
		if stars := visibleStars(t, transcript, want.prompt, want.next); stars != len(want.typed) {
			t.Fatalf("the prompt for %q was left showing %d stars, want %d after the backspace",
				want.prompt, stars, len(want.typed))
		}
	}

	assertStored(t, dir, bitwarden.EnvClientID, authTypedID)
	assertStored(t, dir, bitwarden.EnvClientSecret, authTypedSecret)
}

// TestSecretsAuthFiltersKeystrokesInASecret covers what a hidden answer keeps.
// An arrow key's escape sequence and Ctrl+U send bytes the operator cannot see
// land, so neither reaches the value or earns a star. The key after a lone Esc
// is typed text, and so is a tab in a pasted secret.
func TestSecretsAuthFiltersKeystrokesInASecret(t *testing.T) {
	t.Parallel()

	const tabbedSecret = "typed\tsecret"

	master, slave := openPTY(t)
	dir := t.TempDir()

	seen := answerAuthPrompts(t, master, slave, dir, [promptCount]string{
		authTypedItem + "\n",
		"typed" + keyLeftArrow + string(rune(keyEscape)) + "-id" + string(rune(keyKillLine)) + "\n",
		tabbedSecret + "\n",
		authTypedKey + "\n",
	})

	for _, want := range []struct{ prompt, next, typed string }{
		{authPrompts()[1], authPrompts()[2], authTypedID},
		{authPrompts()[2], authPrompts()[3], tabbedSecret},
	} {
		if stars := visibleStars(t, seen.text(), want.prompt, want.next); stars != len(want.typed) {
			t.Fatalf("the prompt for %q showed %d stars, want %d", want.prompt, stars, len(want.typed))
		}
	}

	assertStored(t, dir, bitwarden.EnvClientID, authTypedID)
	assertStored(t, dir, bitwarden.EnvClientSecret, tabbedSecret)
}

// TestSecretsAuthStripsAPastedCarriageReturn checks a CRLF paste, which the
// terminal turns into two newlines. On a hidden prompt the second one is
// dropped; on the visible item prompt it reaches the next prompt, which asks
// again instead of failing.
func TestSecretsAuthStripsAPastedCarriageReturn(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name  string
		typed [promptCount]string
	}{
		{
			name:  "pasted into a hidden prompt",
			typed: [promptCount]string{authTypedItem + "\n", authTypedID + "\r\n", authTypedSecret + "\n", authTypedKey + "\n"},
		},
		{
			name:  "pasted into the visible item prompt",
			typed: [promptCount]string{authTypedItem + "\r\n", authTypedID + "\n", authTypedSecret + "\n", authTypedKey + "\n"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			master, slave := openPTY(t)
			dir := t.TempDir()

			answerAuthPrompts(t, master, slave, dir, testCase.typed)

			assertStored(t, dir, bitwarden.EnvItem, authTypedItem)
			assertStored(t, dir, bitwarden.EnvClientID, authTypedID)
			assertStored(t, dir, bitwarden.EnvClientSecret, authTypedSecret)
			assertStored(t, dir, bitwarden.EnvMasterKey, authTypedKey)
		})
	}
}

// TestSecretsAuthInterruptRestoresTheTerminal presses Ctrl+C at a hidden prompt
// of the command, run as a process controlling this terminal so the keystroke
// becomes a real SIGINT. The run must end with echo back on and nothing
// written. Settings are read through the master side, since darwin revokes
// the terminal side when its controlling process exits.
func TestSecretsAuthInterruptRestoresTheTerminal(t *testing.T) {
	t.Parallel()

	master, slave := openPTY(t)
	dir := t.TempDir()
	before := readTermios(t, master)
	seen := watchTerminal(t, master, slave)

	command := exec.CommandContext(t.Context(), linkedCairnos(t), secretsCmd, authSubcmd, flagDirArg, dir)
	command.Env = []string{cli.HomeEnvName() + "=" + t.TempDir()}
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	// Ctty 0 is the child's stdin, the terminal's slave side.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if err := command.Start(); err != nil {
		t.Fatalf("start the command: %v", err)
	}

	finished := make(chan error, oneShotChanSize)

	go func() { finished <- command.Wait() }()

	seen.await(t, authPrompts()[0])
	typeOn(t, master, authTypedItem+"\n")
	seen.await(t, authPrompts()[1])
	typeOn(t, master, string(rune(keyInterrupt)))

	err := awaitFinish(t, finished)
	if exitErr, exited := errors.AsType[*exec.ExitError](err); !exited || exitErr.ExitCode() != cli.ExitFailure {
		t.Fatalf("the interrupted command finished with %v, want exit %d", err, cli.ExitFailure)
	}

	if after := readTermios(t, master); *after != *before {
		t.Fatalf("terminal settings after = %+v, want the original %+v", *after, *before)
	}

	// The output side turns each newline into CRLF; the check is about lines.
	want := "\nsecrets auth: read " + bitwarden.EnvClientID + ": interrupted\n"
	if transcript := strings.ReplaceAll(seen.text(), "\r", ""); !strings.Contains(transcript, want) {
		t.Fatalf("the interrupt was not reported as %q on a line of its own:\n%s", want, transcript)
	}

	assertAbsent(t, filepath.Join(dir, credentialsFileName), "a credentials file")
}

// linkedCairnos links this test binary under the name TestMain runs as the
// real command, and returns the link.
func linkedCairnos(t *testing.T) string {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	link := filepath.Join(t.TempDir(), cairnosName)
	if err := os.Symlink(self, link); err != nil {
		t.Fatalf("link the test binary as %s: %v", cairnosName, err)
	}

	return link
}

// typeOn writes keystrokes to the master side, as an operator's keyboard would.
func typeOn(t *testing.T, master *os.File, keys string) {
	t.Helper()

	if _, err := master.WriteString(keys); err != nil {
		t.Fatalf("type %q: %v", keys, err)
	}
}

// assertStored reports lengths rather than values, so no password reaches a log.
func assertStored(t *testing.T, dir, variable, typed string) {
	t.Helper()

	if !strings.Contains(readCredentials(t, dir), assignment(variable, typed)) {
		t.Fatalf("%s does not hold the %d characters typed for it", variable, len(typed))
	}
}

// visibleStars counts the stars left on screen between two prompts, erased ones
// taken back off. An empty next marker reads to the end of the transcript.
func visibleStars(t *testing.T, transcript, prompt, next string) int {
	t.Helper()

	start := strings.Index(transcript, prompt)
	if start < 0 {
		t.Fatalf("the transcript never carried the prompt for %q", prompt)
	}

	segment := transcript[start+len(prompt):]

	if next != "" {
		end := strings.Index(segment, next)
		if end < 0 {
			t.Fatalf("the transcript never carried the prompt for %q", next)
		}

		segment = segment[:end]
	}

	return strings.Count(segment, promptStar) - strings.Count(segment, promptErase)
}

// answerAuthPrompts runs `secrets auth` against the terminal, typing each answer
// once its prompt lands: typing ahead would buffer it while echo was still on.
// The prompts go to stderr, so that is the stream pointed at the terminal.
func answerAuthPrompts(t *testing.T, master, slave *os.File, dir string, typed [promptCount]string) *terminalWatch {
	t.Helper()

	seen := watchTerminal(t, master, slave)

	finished := make(chan int, oneShotChanSize)

	go func() {
		finished <- cli.New(io.Discard, slave).WithStdin(slave).WithEnv(environ.Map(emptyHome(t))).
			Run(t.Context(), []string{secretsCmd, authSubcmd, flagDirArg, dir})
	}()

	for index, marker := range authPrompts() {
		seen.await(t, marker)
		typeOn(t, master, typed[index])
	}

	if code := awaitFinish(t, finished); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok", code)
	}

	return seen
}

// terminalWatch collects everything the terminal sends back, which is the
// prompts the command wrote plus whatever the line discipline echoed.
type terminalWatch struct {
	// arrived carries one wake per read, so a waiting case is woken by the
	// terminal rather than by a timer it polls.
	arrived chan struct{}

	builder strings.Builder
	mutex   sync.Mutex

	// stopping tells the reader the case is over. Closing the master does not
	// reliably wake a read already blocked in the kernel on a pty, so the
	// cleanup writes one byte to wake it and this says what that byte means.
	stopping atomic.Bool
}

// text copies the transcript under the lock, since the reader goroutine may
// still be appending to it.
func (w *terminalWatch) text() string {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	return w.builder.String()
}

// await blocks until the transcript carries the marker, so each answer is typed
// only after the prompt asking for it has landed. Typing ahead would put every
// answer in the terminal's buffer while echo was still on.
func (w *terminalWatch) await(t *testing.T, marker string) {
	t.Helper()

	expired := time.NewTimer(promptWait)
	defer expired.Stop()

	for {
		if strings.Contains(w.text(), marker) {
			return
		}

		select {
		case <-w.arrived:
		case <-expired.C:
			t.Fatalf("the prompt for %q never reached the terminal:\n%s", marker, w.text())
		}
	}
}

// record appends what the terminal sent and wakes a waiting case. The send is
// non-blocking because a wake nobody is waiting for is not worth holding the
// reader for.
func (w *terminalWatch) record(chunk []byte) {
	w.mutex.Lock()
	w.builder.Write(chunk)
	w.mutex.Unlock()

	select {
	case w.arrived <- struct{}{}:
	default:
	}
}

// watchTerminal reads the master side for the life of the case. The read has to
// run while the command is blocked on a prompt, which is also what keeps the
// terminal's buffer from filling. Cleanup wakes the reader with one byte, then
// waits, so the goroutine cannot outlive the case.
func watchTerminal(t *testing.T, master, slave *os.File) *terminalWatch {
	t.Helper()

	watch := &terminalWatch{arrived: make(chan struct{}, wakeChanSize)}
	watch.builder.Grow(terminalTranscriptSize)

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		buffer := make([]byte, terminalReadSize)

		for {
			read, err := master.Read(buffer)
			if read > 0 && !watch.stopping.Load() {
				watch.record(buffer[:read])
			}

			if err != nil || watch.stopping.Load() {
				return
			}
		}
	}()

	t.Cleanup(func() {
		watch.stopping.Store(true)

		// A reader that already stopped needs no wake, and the terminal side
		// may be gone by then: darwin revokes it when a child that controlled
		// it exits.
		if !isDone(stopped) {
			wakeReader(t, slave)
		}

		<-stopped

		_ = master.Close()
	})

	return watch
}

// isDone checks done without blocking, so cleanup can tell a reader that
// already stopped from one still waiting for a byte.
func isDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// wakeReader writes one byte out of the terminal side, which the master's
// blocked read then returns with.
func wakeReader(t *testing.T, slave *os.File) {
	t.Helper()

	if _, err := slave.WriteString("\n"); err != nil {
		t.Errorf("wake the reader: %v", err)
	}
}

// awaitFinish waits for the command's result, an exit code in process or the
// child's wait error, rather than leaving it running past the case.
func awaitFinish[T any](t *testing.T, finished <-chan T) T {
	t.Helper()

	select {
	case result := <-finished:
		return result
	case <-time.After(promptWait):
		t.Fatal("the command never finished after its last keystroke")

		var zero T

		return zero
	}
}

// readTermios reads the settings through the request each platform names
// differently, which openPTY's file supplies beside it.
func readTermios(t *testing.T, file *os.File) *unix.Termios {
	t.Helper()

	settings, err := unix.IoctlGetTermios(int(file.Fd()), termiosReadRequest)
	if err != nil {
		t.Fatalf("read the terminal settings: %v", err)
	}

	return settings
}
