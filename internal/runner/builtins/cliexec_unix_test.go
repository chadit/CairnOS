//go:build !windows

package builtins_test

import (
	"errors"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Timing for the wrapper tests below. Each bound is half of how long a
// surviving child holds a pipe, so a run that returns inside it was ended by
// the mechanism under test rather than by that child exiting. strayChildHold
// is long enough that the CLI runner's 2-second WaitDelay fits under its half.
const (
	forkedChildHold = 3 * time.Second
	strayChildHold  = 10 * time.Second
)

// sleepFor is the sh command that sleeps for d, in whole seconds because POSIX
// sleep takes no fraction.
func sleepFor(d time.Duration) string {
	return "sleep " + strconv.Itoa(int(d/time.Second))
}

// forkingWrapper is a wrapper body whose CLI, played by sleep, runs as a child
// holding stdout. The echo after sleep keeps sh from exec-ing sleep as its last
// command, which would make this a single-process wrapper.
func forkingWrapper() string {
	return "echo started\n" + sleepFor(forkedChildHold) + "\necho done"
}

// executeUntilCanceled runs wrapper as the qwen CLI and cancels the run once
// the wrapper prints its first line, returning how long the run took to end
// after the cancel along with its error.
func executeUntilCanceled(t *testing.T, wrapper string) (time.Duration, error) {
	t.Helper()

	cli, err := builtins.NewCLI(builtins.QwenName, builtins.CLITool{Binary: wrapper}, nil)
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	return executeCanceledOnFirstLine(t, cli, cliTask())
}

// requireKilledByCancel fails unless err is the transient timeout of a wrapper
// the cancellation killed. The exit error in the chain tells that apart from a
// run canceled before the wrapper started, which classifies the same way.
func requireKilledByCancel(t *testing.T, err error) {
	t.Helper()

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified || classified.Class != runner.ClassTransient || classified.Code != qwenTimeoutCode {
		t.Fatalf("error = %v, want transient %s", err, qwenTimeoutCode)
	}

	exitErr, hasExit := errors.AsType[*exec.ExitError](err)
	if !hasExit {
		t.Fatalf("error = %v, want the killed wrapper's exit error in its chain", err)
	}

	// ExitCode is -1 only for a process a signal ended, so a wrapper that exited
	// on its own before the kill landed fails here.
	if code := exitErr.ExitCode(); code != -1 {
		t.Fatalf("wrapper exit code = %d, want -1 for a signal kill", code)
	}
}

// TestCLICancelKillsWrapperForkedChild covers a wrapper script that runs the
// real CLI as a child instead of exec-ing it. The child inherits stdout, so a
// kill that reached only the wrapper would leave run reading until the child
// exited on its own. Windows kills only the wrapper, hence the build tag.
func TestCLICancelKillsWrapperForkedChild(t *testing.T) {
	wrapper := writeShim(t, t.TempDir(), "forking-wrapper", forkingWrapper())

	t.Parallel()

	elapsed, err := executeUntilCanceled(t, wrapper)

	requireKilledByCancel(t, err)

	if bound := forkedChildHold / 2; elapsed > bound {
		t.Fatalf("run returned after %v, want under %v: only a group kill ends the read this soon", elapsed, bound)
	}
}

// TestCLICancelEndsRunHeldByEscapedChild covers a wrapper helper that left
// the process group through setsid and keeps stdout open. The group kill
// misses it, so only WaitDelay closing the pipes ends the read before the
// helper exits.
func TestCLICancelEndsRunHeldByEscapedChild(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}

	script := "setsid " + sleepFor(strayChildHold) + " &\n" + forkingWrapper()
	wrapper := writeShim(t, t.TempDir(), "escaping-wrapper", script)

	t.Parallel()

	elapsed, err := executeUntilCanceled(t, wrapper)

	requireKilledByCancel(t, err)

	if bound := strayChildHold / 2; elapsed > bound {
		t.Fatalf("run returned after %v, want under %v: the escaped child held stdout", elapsed, bound)
	}
}

// TestCLICleanExitIgnoresHelperHoldingStderr covers a tool that exits 0 while a
// background helper keeps stderr open. stdout is complete, so the run succeeds
// once WaitDelay passes rather than failing or waiting out the helper.
func TestCLICleanExitIgnoresHelperHoldingStderr(t *testing.T) {
	script := sleepFor(strayChildHold) + " >/dev/null &\necho ok"
	wrapper := writeShim(t, t.TempDir(), "stderr-holder", script)

	t.Parallel()

	cli, err := builtins.NewCLI(builtins.QwenName, builtins.CLITool{Binary: wrapper}, nil)
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	start := time.Now()

	result, err := cli.Execute(t.Context(), cliTask())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if bound := strayChildHold / 2; elapsed > bound {
		t.Fatalf("run returned after %v, want under %v: the helper held stderr", elapsed, bound)
	}

	if output, isString := result.Output.(string); !isString || output != "ok" {
		t.Fatalf("output = %#v, want ok", result.Output)
	}
}
