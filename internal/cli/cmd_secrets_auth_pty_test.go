//go:build linux

package cli_test

import (
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/chadit/CairnOS/internal/cli"
)

// promptWait bounds how long a case waits for one prompt to reach the terminal.
const promptWait = 10 * time.Second

// terminalTranscriptSize preallocates the transcript, which the reader appends
// to in a loop.
const terminalTranscriptSize = 4096

// TestSecretsAuthHidesTypingAndRestoresTheTerminal watches the terminal itself.
// A terminal echoes what is typed back, so the master side sees the item name,
// typed with echo on, and must not see the three secrets after it.
//
// The repo carries no pty module, so the pair is opened by hand, which makes
// this case Linux-only.
//
//nolint:paralleltest // t.Setenv rewrites HOME for the whole process
func TestSecretsAuthHidesTypingAndRestoresTheTerminal(t *testing.T) {
	emptyHome(t)

	master, slave := openPTY(t)
	dir := t.TempDir()

	before := readTermios(t, slave)
	if before.Lflag&unix.ECHO == 0 {
		t.Fatal("the fresh terminal already has echo off, so this proves nothing")
	}

	seen := watchTerminal(t, master, slave)

	// The command owns both ends of the conversation, writing its prompts to the
	// terminal the way it would for an operator sitting at one.
	finished := make(chan int, 1)

	go func() {
		finished <- cli.New(slave, io.Discard).WithStdin(slave).
			Run(t.Context(), []string{secretsCmd, authSubcmd, flagDirArg, dir})
	}()

	for _, step := range []struct{ prompt, answer string }{
		{"item name", authTypedItem},
		{"client id", authTypedID},
		{"client secret", authTypedSecret},
		{"master password", authTypedKey},
	} {
		seen.await(t, step.prompt)

		if _, err := master.WriteString(step.answer + "\n"); err != nil {
			t.Fatalf("type the answer to %q: %v", step.prompt, err)
		}
	}

	if code := awaitExit(t, finished); code != cli.ExitOK {
		t.Fatalf("exit = %d, want ok", code)
	}

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

// text returns the transcript so far.
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
// terminal's buffer from filling.
func watchTerminal(t *testing.T, master, slave *os.File) *terminalWatch {
	t.Helper()

	watch := &terminalWatch{arrived: make(chan struct{}, 1)}
	watch.builder.Grow(terminalTranscriptSize)

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		buffer := make([]byte, 512)

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

	// The command has finished by now, so one byte through the terminal wakes
	// the reader and it sees the flag. The wait keeps the goroutine from
	// outliving the case, which the package's leak sweep would report.
	t.Cleanup(func() {
		watch.stopping.Store(true)

		_, _ = slave.WriteString("\n")

		<-stopped

		_ = master.Close()
	})

	return watch
}

// awaitExit waits for the command rather than leaving it running past the case.
func awaitExit(t *testing.T, finished <-chan int) int {
	t.Helper()

	select {
	case code := <-finished:
		return code
	case <-time.After(promptWait):
		t.Fatal("the command never finished after every prompt was answered")

		return cli.ExitFailure
	}
}

// openPTY opens a pseudo-terminal pair. The unlock and the name lookup are the
// two ioctls the C library's grantpt and ptsname wrap, spelled out because the
// repo carries no pty helper.
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
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

	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open the terminal side: %v", err)
	}

	t.Cleanup(func() { _ = slave.Close() })

	return master, slave
}

// readTermios returns the terminal's current settings.
func readTermios(t *testing.T, file *os.File) *unix.Termios {
	t.Helper()

	settings, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("read the terminal settings: %v", err)
	}

	return settings
}
