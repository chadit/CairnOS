//go:build !windows

package cli_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/cli"
)

// The signal tests run their subject in a child copy of this test binary, so a
// real signal never reaches this process, where every parallel test's signal
// watch would see it. Each child test skips unless its variable is set.
const (
	// childWait bounds a child's startup and how long it may run after the
	// signal that should end it.
	childWait = 5 * time.Second

	// resendEvery paces repeated signals while StopContext's release, which
	// runs in a goroutine just after the first cancel, catches up.
	resendEvery = 100 * time.Millisecond

	chatChildAddrEnv    = "CAIRNOS_TEST_CHAT_CHILD_ADDR"
	blockedReadChildEnv = "CAIRNOS_TEST_BLOCKED_READ_CHILD"

	// chatBanner is the part of the session banner the chat child prints once
	// cmdChat watches for stop signals.
	chatBanner = "type a message"

	blockedReadReady    = "blocked read child ready"
	blockedReadCanceled = "blocked read child context canceled"
)

// TestChatStopSignalEndsSession sends SIGTERM or SIGHUP to a chat session and
// expects a clean exit. The child traps what main traps, so only cmdChat's own
// watch can end the session.
func TestChatStopSignalEndsSession(t *testing.T) {
	t.Parallel()

	for _, stop := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(stop.String(), func(t *testing.T) {
			t.Parallel()

			addr := startScriptedChat(t, newScriptedChat())

			child, lines := startChild(t, "TestChatStopSignalChild", chatChildAddrEnv+"="+addr)
			waitForLine(t, child, lines, chatBanner)

			if err := child.Process.Signal(stop); err != nil {
				t.Fatalf("send %v: %v", stop, err)
			}

			select {
			case err := <-exitOf(child):
				if err != nil {
					t.Fatalf("child exit = %v, want a clean exit after %v; stderr: %s", err, stop, child.Stderr)
				}
			case <-time.After(childWait):
				t.Fatalf("chat still running %v after %v", childWait, stop)
			}
		})
	}
}

// TestChatStopSignalChild is the chat session TestChatStopSignalEndsSession
// runs in a child process; run directly, it skips.
func TestChatStopSignalChild(t *testing.T) {
	t.Parallel()

	addr := os.Getenv(chatChildAddrEnv)
	if addr == "" {
		t.Skip("runs only as the child of TestChatStopSignalEndsSession")
	}

	ctx, stop := cli.StopContext(t.Context())

	code := cli.New(os.Stdout, os.Stderr).Run(ctx, chatArgs(addr))

	stop()

	// Exiting here hands the parent the chat's own exit code and skips the leak
	// sweep, which would flag the REPL goroutine a stop leaves parked on stdin.
	os.Exit(code) //nolint:revive // the child process ends here, as main would
}

// TestSecondStopSignalEndsBlockedRead covers a command blocked in a stdin read,
// which never sees its context. The first SIGTERM only cancels that context,
// and a later one has to end the process by signal.
func TestSecondStopSignalEndsBlockedRead(t *testing.T) {
	t.Parallel()

	child, lines := startChild(t, "TestBlockedReadChild", blockedReadChildEnv+"=1")
	waitForLine(t, child, lines, blockedReadReady)

	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send first SIGTERM: %v", err)
	}

	// The child printing this proves the first signal was trapped, not fatal.
	waitForLine(t, child, lines, blockedReadCanceled)

	err := resendUntilExit(t, child, syscall.SIGTERM)

	exitErr, isExit := errors.AsType[*exec.ExitError](err)
	if !isExit {
		t.Fatalf("child exit = %v, want an exit by SIGTERM", err)
	}

	status, isWaitStatus := exitErr.Sys().(syscall.WaitStatus)
	if !isWaitStatus || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("child exit = %v, want an exit by SIGTERM", exitErr)
	}
}

// TestBlockedReadChild is the process TestSecondStopSignalEndsBlockedRead runs;
// run directly, it skips. It reports its context's cancel and then stays in a
// stdin read nothing writes to, the way a hidden prompt waits for input.
func TestBlockedReadChild(t *testing.T) {
	t.Parallel()

	if os.Getenv(blockedReadChildEnv) == "" {
		t.Skip("runs only as the child of TestSecondStopSignalEndsBlockedRead")
	}

	ctx, stop := cli.StopContext(t.Context())
	defer stop()

	context.AfterFunc(ctx, func() { fmt.Println(blockedReadCanceled) })
	fmt.Println(blockedReadReady)

	if _, err := io.ReadAll(os.Stdin); err != nil {
		t.Fatalf("read stdin: %v", err)
	}

	t.Fatal("stdin closed before a second signal ended the process")
}

// startChild re-runs this test binary limited to childTest, with env added,
// and returns it with a scanner over its stdout. The child's stdin is a pipe
// nothing writes to, so a read on it blocks until the test ends.
func startChild(t *testing.T, childTest string, env ...string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	// #nosec G204 -- re-runs this test binary, limited to one child test.
	child := exec.CommandContext(t.Context(), self, "-test.run=^"+childTest+"$")

	child.Env = append(os.Environ(), env...)
	child.Stderr = &syncBuffer{}

	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatalf("child stdin: %v", err)
	}

	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout: %v", err)
	}

	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	t.Cleanup(func() { _ = stdin.Close() })

	return child, bufio.NewScanner(stdout)
}

// waitForLine reads the child's stdout until a line contains want. A child
// that never prints it would block the scan forever, so a kill after
// childWait turns that hang into EOF and a failure.
func waitForLine(t *testing.T, child *exec.Cmd, lines *bufio.Scanner, want string) {
	t.Helper()

	startupKill := time.AfterFunc(childWait, func() {
		if err := child.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill the hung child: %v", err)
		}
	})
	defer startupKill.Stop()

	for lines.Scan() {
		if strings.Contains(lines.Text(), want) {
			return
		}
	}

	t.Fatalf("child never printed %q (read error %v); stderr: %s", want, lines.Err(), child.Stderr)
}

// exitOf waits for the child in the background so a caller can bound the wait.
func exitOf(child *exec.Cmd) <-chan error {
	exited := make(chan error, 1)

	go func() {
		exited <- child.Wait()
	}()

	return exited
}

// signalChild sends sig to a child that may already have exited. A child that
// is gone is what the caller waits for, so only another failure stops the test.
func signalChild(t *testing.T, child *exec.Cmd, sig os.Signal) {
	t.Helper()

	if err := child.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("send %v to the child: %v", sig, err)
	}
}

// resendUntilExit sends stop every resendEvery until the child exits, and
// fails if it is still running after childWait.
func resendUntilExit(t *testing.T, child *exec.Cmd, stop os.Signal) error {
	t.Helper()

	exited := exitOf(child)

	resend := time.NewTicker(resendEvery)
	defer resend.Stop()

	deadline := time.After(childWait)

	for {
		select {
		case err := <-exited:
			return err
		case <-resend.C:
			signalChild(t, child, stop)
		case <-deadline:
			t.Fatalf("child still running %v into repeated %v", childWait, stop)

			return nil
		}
	}
}
