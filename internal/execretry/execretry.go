// Package execretry launches external commands that may name a file this
// process, or a workflow step just before it, has only now finished writing.
//
// The kernel refuses to exec a file any process holds open for writing. In a
// multithreaded program a fork inherits that write descriptor and keeps it
// until its own execve, so an unrelated goroutine spawning a child is enough to
// make the exec report ETXTBSY (Go issue 22315). The window is microseconds
// wide, so a short bounded retry closes it and a real failure still surfaces
// on the first attempt.
package execretry

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// Build makes the command for one attempt. An exec.Cmd cannot be restarted, so
// every retry needs a fresh one together with the pipes and buffers it owns.
type Build func() (*exec.Cmd, error)

// attempts and backoff bound the wait for a binary the kernel calls busy. Five
// tries doubling from 2ms cover a racing fork's descriptor without leaving a
// genuinely busy file stalling a caller for long.
const (
	attempts = 5
	backoff  = 2 * time.Millisecond
)

// Start starts a command and hands it back running. The caller owns Wait and
// everything the build closure wired onto the returned command, which belongs
// to the attempt that succeeded.
func Start(ctx context.Context, build Build) (*exec.Cmd, error) {
	return retry(ctx, func() (*exec.Cmd, error) {
		cmd, err := build()
		if err != nil {
			return nil, err
		}

		//nolint:wrapcheck // callers classify this error themselves, and retry matches on it
		if startErr := cmd.Start(); startErr != nil {
			return nil, startErr
		}

		return cmd, nil
	})
}

// Output runs a command to completion and returns its stdout. Errors come back
// unwrapped so a caller can still match on exec.ExitError.
func Output(ctx context.Context, build Build) ([]byte, error) {
	return retry(ctx, func() ([]byte, error) {
		cmd, err := build()
		if err != nil {
			return nil, err
		}

		return cmd.Output()
	})
}

// CombinedOutput runs a command to completion and returns its stdout and
// stderr interleaved. A failing attempt still returns whatever the child wrote,
// which is what makes a broken build report what broke.
func CombinedOutput(ctx context.Context, build Build) ([]byte, error) {
	return retry(ctx, func() ([]byte, error) {
		cmd, err := build()
		if err != nil {
			return nil, err
		}

		return cmd.CombinedOutput()
	})
}

// retry runs try until it stops reporting a busy executable, the budget is
// spent, or ctx ends. Every other error, and the first success, returns
// straight away, and the last busy error is what a spent budget reports.
func retry[T any](ctx context.Context, try func() (T, error)) (T, error) {
	var (
		zero T
		out  T
		err  error
	)

	for attempt := range attempts {
		out, err = try()
		if !errors.Is(err, syscall.ETXTBSY) {
			return out, err
		}

		if !wait(ctx, backoff<<attempt) {
			break
		}
	}

	return zero, err
}

// wait sleeps for delay, reporting false when ctx ended first.
func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
