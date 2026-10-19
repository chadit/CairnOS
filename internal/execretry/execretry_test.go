package execretry_test

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/execretry"
)

// shimScript is what every staged binary here does, and shimPerm the mode that
// makes it runnable. A script stands in for a real tool because these cases
// only care whether execve was allowed to happen.
const (
	shimScript = "#!/bin/sh\necho ran\n"
	shimPerm   = 0o700
)

// busyBudget is the floor a retrying call cannot beat: 2 + 4 + 8 + 16ms across
// the package's five attempts. It mirrors the unexported pair, pinned here as
// the one observable a caller has.
const busyBudget = 30 * time.Millisecond

// holdOpen keeps a write handle on path, which is what makes execve report
// ETXTBSY. It stands in for the racing fork of Go issue 22315, whose inherited
// descriptor a test cannot arrange on purpose.
func holdOpen(t *testing.T, path string) {
	t.Helper()

	busy, err := os.OpenFile(path, os.O_WRONLY, shimPerm) // #nosec G304 -- the path is built by the test
	if err != nil {
		t.Fatalf("hold the staged shim open for writing: %v", err)
	}

	t.Cleanup(func() { _ = busy.Close() })
}

// stageShim writes a runnable shim into a fresh directory. Call it before
// t.Parallel so its own write handle is closed before any fork.
func stageShim(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "shim")
	if err := os.WriteFile(path, []byte(shimScript), shimPerm); err != nil {
		t.Fatalf("stage a shim at %s: %v", path, err)
	}

	return path
}

// builder returns a builder for path and a counter of how often it ran, which
// is how a case tells one attempt from the full budget.
func builder(ctx context.Context, path string) (execretry.Build, *atomic.Int64) {
	var calls atomic.Int64

	return func() (*exec.Cmd, error) {
		calls.Add(1)

		return exec.CommandContext(ctx, path), nil // #nosec G204 -- the test names the binary
	}, &calls
}

// holdOpenInChild has a helper shell hold a write handle on path and returns
// the function that releases it. The handle lives only in that child, so no
// fork this test binary makes can inherit it and keep the file busy after the
// release returns. Cleanup releases it too, so a test that fails early still
// reaps the shell.
func holdOpenInChild(t *testing.T, path string) func() {
	t.Helper()

	// The shell exits when its stdin closes, so it needs no context kill, and a
	// kill from t.Context, canceled before cleanup runs, would fail the reap.
	holder := exec.CommandContext(context.WithoutCancel(t.Context()), "sh", "-c", `exec 3>>"$1"; echo held; read -r _; exit 0`, "sh", path)

	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatalf("holder stdin: %v", err)
	}

	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatalf("holder stdout: %v", err)
	}

	if err := holder.Start(); err != nil {
		t.Fatalf("start the holder: %v", err)
	}

	var once sync.Once

	release := func() {
		once.Do(func() {
			_ = stdin.Close()

			if err := holder.Wait(); err != nil {
				t.Errorf("holder exit: %v", err)
			}
		})
	}

	t.Cleanup(release)

	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("holder reported %q (%v), want held", line, err)
	}

	return release
}

// TestOutputSucceedsOnceTheWriteHandleCloses is the case the package exists
// for: the exec is refused while a write descriptor is open and goes through
// once it closes. Releasing the handle from the second build ties success to
// the retry, not a timer.
func TestOutputSucceedsOnceTheWriteHandleCloses(t *testing.T) {
	path := stageShim(t)

	t.Parallel()

	release := holdOpenInChild(t, path)

	var calls atomic.Int64

	out, err := execretry.Output(t.Context(), func() (*exec.Cmd, error) {
		if calls.Add(1) == 2 {
			release()
		}

		return exec.CommandContext(t.Context(), path), nil // #nosec G204 -- the test names the binary
	})
	if err != nil {
		t.Fatalf("output: %v", err)
	}

	if string(out) != "ran\n" {
		t.Fatalf("output = %q, want the shim's own line", out)
	}

	if calls.Load() != 2 {
		t.Fatalf("builder ran %d times, want the one retry the released handle needed", calls.Load())
	}
}

// TestStartGivesUpWithTheBusyErrorStillVisible proves a file that never frees
// reports what actually stopped it. A caller classifying the failure sees the
// kernel's own answer, not a retry wrapper's.
func TestStartGivesUpWithTheBusyErrorStillVisible(t *testing.T) {
	path := stageShim(t)
	holdOpen(t, path)

	t.Parallel()

	build, calls := builder(t.Context(), path)

	started := time.Now()

	if _, err := execretry.Start(t.Context(), build); !errors.Is(err, syscall.ETXTBSY) {
		t.Fatalf("start error = %v, want a busy executable", err)
	}

	if elapsed := time.Since(started); elapsed < busyBudget {
		t.Fatalf("gave up after %s, want at least %s of retries", elapsed, busyBudget)
	}

	if calls.Load() != 5 {
		t.Fatalf("builder ran %d times, want the full budget of 5", calls.Load())
	}
}

// TestStartDoesNotRetryOtherFailures pins the other half of the contract: a
// command that is simply missing fails on the first try, or every typo in an
// operator's config would cost a runner the whole retry budget.
func TestStartDoesNotRetryOtherFailures(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "nothing-here")

	build, calls := builder(t.Context(), missing)

	if _, err := execretry.Start(t.Context(), build); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("start error = %v, want the missing binary", err)
	}

	if calls.Load() != 1 {
		t.Fatalf("builder ran %d times, want one attempt for a non-busy failure", calls.Load())
	}
}

// TestCombinedOutputStopsOnContextCancel proves a canceled caller stops waiting
// on a busy file. A shutdown that has to sit through the backoff is a shutdown
// that looks hung.
func TestCombinedOutputStopsOnContextCancel(t *testing.T) {
	path := stageShim(t)
	holdOpen(t, path)

	t.Parallel()

	// The command keeps a live context so its own exec still reports busy; only
	// the retry budget is canceled.
	build, calls := builder(t.Context(), path)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := execretry.CombinedOutput(canceled, build); !errors.Is(err, syscall.ETXTBSY) {
		t.Fatalf("combined output error = %v, want the busy executable that stopped it", err)
	}

	if calls.Load() != 1 {
		t.Fatalf("builder ran %d times, want the canceled context to stop after one", calls.Load())
	}
}

// TestStartReportsABuilderFailure covers the path where nothing is ever
// launched: the builder's own error reaches the caller unchanged, so a wiring
// mistake does not read as a process failure.
func TestStartReportsABuilderFailure(t *testing.T) {
	t.Parallel()

	wanted := errors.New("no command to build")

	_, err := execretry.Start(t.Context(), func() (*exec.Cmd, error) {
		return nil, wanted
	})

	if !errors.Is(err, wanted) {
		t.Fatalf("start error = %v, want the builder's own error", err)
	}
}
