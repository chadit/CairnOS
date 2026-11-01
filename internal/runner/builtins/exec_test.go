package builtins_test

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

const (
	envKey          = "env"
	actionReconcile = "reconcile"
	binTrue         = "/bin/true"

	// wantPositiveTimeout pins the start of the exec runner's complaint about a
	// zero or negative timeout.
	wantPositiveTimeout = "timeout must be a positive"

	// hostEnvName and cfgEnvName are the two variables the environment case
	// reads back out of the child, one inherited and one from config.
	hostEnvName = "CAIRNOS_EXEC_HOST"
	cfgEnvName  = "CAIRNOS_EXEC_CFG"

	codeExecDenied   = builtins.ExecName + ".denied"
	codeExecExit     = builtins.ExecName + ".exit"
	codeExecTimeout  = builtins.ExecName + ".timeout"
	codeExecNotFound = builtins.ExecName + ".not_found"
	codeExecBadArgs  = builtins.ExecName + ".bad_args"
)

// execConfig builds a usable block (one allowed action, a generous timeout)
// and overlays any extra keys.
func execConfig(extra map[string]any) map[string]any {
	cfg := map[string]any{
		cfgAllow:   []any{actionReconcile},
		cfgTimeout: int64(30),
	}

	maps.Copy(cfg, extra)

	return cfg
}

// newExec builds the runner or fails the test, since a config decode error is
// never what these cases are checking.
func newExec(t *testing.T, cfg map[string]any) *builtins.Exec {
	t.Helper()

	instance, err := builtins.NewExec(cfg)
	if err != nil {
		t.Fatalf("new exec: %v", err)
	}

	return instance
}

// execTask builds a resolved task for the allowed action.
func execTask(params map[string]any) *runner.Task {
	return &runner.Task{
		RunID:      testRunID,
		ActionName: actionReconcile,
		Params:     params,
		Resolved:   true,
	}
}

// classifiedError pulls the runner.Error out of err, failing when the runner
// returned something the engine cannot classify.
func classifiedError(t *testing.T, err error) *runner.Error {
	t.Helper()

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	return classified
}

func TestExecDeniesActionsOutsideAllowList(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cfg  map[string]any
		name string
	}{
		{name: "no config block at all", cfg: nil},
		{name: "empty config block", cfg: map[string]any{}},
		{name: "empty allow list", cfg: execConfig(map[string]any{cfgAllow: []any{}})},
		{name: "another action allowed", cfg: execConfig(map[string]any{cfgAllow: []any{"publish"}})},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := newExec(t, tt.cfg).Validate(execTask(map[string]any{cmdKey: binTrue}))

			classified := classifiedError(t, err)
			if classified.Class != runner.ClassPermanent || classified.Code != codeExecDenied {
				t.Fatalf("class/code = %v/%q, want permanent/%q", classified.Class, classified.Code, codeExecDenied)
			}
		})
	}
}

func TestExecAllowsListedAction(t *testing.T) {
	t.Parallel()

	if err := newExec(t, execConfig(nil)).Validate(execTask(map[string]any{cmdKey: binTrue})); err != nil {
		t.Fatalf("validate rejected an allowed action: %v", err)
	}
}

func TestExecValidateParams(t *testing.T) {
	t.Parallel()

	cases := []struct {
		params   map[string]any
		name     string
		wantCode string
		resolved bool
	}{
		{
			name:     "missing command",
			params:   map[string]any{},
			resolved: true,
			wantCode: builtins.ExecName + ".missing_command",
		},
		{
			name:     "non-string command",
			params:   map[string]any{cmdKey: int64(7)},
			resolved: true,
			wantCode: builtins.ExecName + ".bad_command",
		},
		{
			name:     "non-string workdir",
			params:   map[string]any{cmdKey: binTrue, wdKey: int64(7)},
			resolved: true,
			wantCode: builtins.ExecName + ".bad_workdir",
		},
		{
			name:     "args is not a list",
			params:   map[string]any{cmdKey: binTrue, argsKey: flagArg},
			resolved: true,
			wantCode: codeExecBadArgs,
		},
		{
			name:     "args holds a non-string element",
			params:   map[string]any{cmdKey: binTrue, argsKey: []any{"--count", int64(3)}},
			resolved: true,
			wantCode: codeExecBadArgs,
		},
		{
			name:     "non-string stdin",
			params:   map[string]any{cmdKey: binTrue, stdinKey: int64(7)},
			resolved: true,
			wantCode: builtins.ExecName + ".bad_stdin",
		},
		{
			name:     "unresolved args reference defers",
			params:   map[string]any{cmdKey: binTrue, argsKey: unresolved},
			resolved: false,
			wantCode: "",
		},
		{
			name:     "string args and workdir",
			params:   map[string]any{cmdKey: binTrue, argsKey: []any{"--quiet"}, wdKey: "/tmp"},
			resolved: true,
			wantCode: "",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			task := execTask(tt.params)
			task.Resolved = tt.resolved

			err := newExec(t, execConfig(nil)).Validate(task)

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("validate error = %v, want none", err)
				}

				return
			}

			if got := classifiedError(t, err).Code; got != tt.wantCode {
				t.Fatalf("code = %q, want %q", got, tt.wantCode)
			}
		})
	}
}

func TestExecFactoryRejectsBadConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cfg      map[string]any
		name     string
		wantText string
	}{
		{
			name:     "configured block without a timeout",
			cfg:      map[string]any{cfgAllow: []any{actionReconcile}},
			wantText: "timeout is required",
		},
		{
			name:     "zero timeout",
			cfg:      execConfig(map[string]any{cfgTimeout: int64(0)}),
			wantText: wantPositiveTimeout,
		},
		{
			name:     "negative timeout",
			cfg:      execConfig(map[string]any{cfgTimeout: int64(-5)}),
			wantText: wantPositiveTimeout,
		},
		{
			name:     "non-integer timeout",
			cfg:      execConfig(map[string]any{cfgTimeout: "540"}),
			wantText: "timeout must be an integer",
		},
		{
			name:     "env value is not a string",
			cfg:      execConfig(map[string]any{envKey: map[string]any{"REPORT_PROJECT": int64(3)}}),
			wantText: "env.REPORT_PROJECT",
		},
		{
			name:     "env key is empty",
			cfg:      execConfig(map[string]any{envKey: map[string]any{"": "x"}}),
			wantText: "env has an empty key",
		},
		{
			name:     "env key holds an equals sign",
			cfg:      execConfig(map[string]any{envKey: map[string]any{"REPORT=PROJECT": "x"}}),
			wantText: `env key "REPORT=PROJECT" must not contain an equals sign or a NUL byte`,
		},
		{
			name:     "env key holds a NUL byte",
			cfg:      execConfig(map[string]any{envKey: map[string]any{"REPORT\x00PROJECT": "x"}}),
			wantText: "must not contain an equals sign or a NUL byte",
		},
		{
			name:     "env is not a table",
			cfg:      execConfig(map[string]any{envKey: "REPORT_PROJECT=x"}),
			wantText: "env must be a table",
		},
		{
			name:     "allow_actions is not a list",
			cfg:      execConfig(map[string]any{cfgAllow: actionReconcile}),
			wantText: "allow_actions must be an array",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := builtins.ExecFactory()(tt.cfg)
			if !errors.Is(err, builtins.ErrConfigValue) {
				t.Fatalf("error %v does not wrap ErrConfigValue", err)
			}

			// ErrConfigValue is the only sentinel the factory exposes, so the
			// message is what tells the operator which key was wrong.
			if message := err.Error(); !strings.Contains(message, tt.wantText) {
				t.Fatalf("error %v does not mention %q", err, tt.wantText)
			}
		})
	}
}

// TestExecConfigErrorNamesRunner pins the "configure <runner>" prefix every
// factory puts on a config failure, so an operator with several runner blocks
// can tell which one is broken.
func TestExecConfigErrorNamesRunner(t *testing.T) {
	t.Parallel()

	_, err := builtins.ExecFactory()(execConfig(map[string]any{envKey: int64(1)}))
	if err == nil {
		t.Fatal("ExecFactory accepted a non-table env, want a config error")
	}

	if want := "configure " + builtins.ExecName + ": "; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error %q does not start with %q", err, want)
	}
}

// TestExecBuildsWithoutAConfigBlock pins the startup contract: the registry
// constructs every built-in whether or not the operator configured it, so an
// absent block must build a runner that denies everything rather than failing
// the whole daemon over a timeout nobody needed.
func TestExecBuildsWithoutAConfigBlock(t *testing.T) {
	t.Parallel()

	for _, cfg := range []map[string]any{nil, {}} {
		instance, err := builtins.ExecFactory()(cfg)
		if err != nil {
			t.Fatalf("factory rejected an unconfigured block: %v", err)
		}

		if instance.Name() != builtins.ExecName {
			t.Fatalf("name = %q, want %q", instance.Name(), builtins.ExecName)
		}
	}
}

func TestExecCapturesOutputExitCodeAndStderrTail(t *testing.T) {
	shim := writeShim(t, t.TempDir(), "reporter", `echo "ran: $1"
echo "warn" >&2`)

	t.Parallel()

	result, err := newExec(t, execConfig(nil)).Execute(t.Context(),
		execTask(map[string]any{cmdKey: shim, argsKey: []any{"nightly"}}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != "ran: nightly" {
		t.Fatalf("output = %#v, want the echoed argv", result.Output)
	}

	if result.Meta["exit_code"] != "0" {
		t.Fatalf("exit_code = %q, want 0", result.Meta["exit_code"])
	}

	if result.Meta["stderr_tail"] != shimStderr {
		t.Fatalf("stderr_tail = %q, want warn", result.Meta["stderr_tail"])
	}

	if result.Usage.Latency <= 0 {
		t.Fatalf("latency = %v, want > 0", result.Usage.Latency)
	}
}

// TestExecNonZeroExitIsPermanent pins the retry contract: the runner reports
// the exit code and stays permanent, leaving the retry decision to the engine.
func TestExecNonZeroExitIsPermanent(t *testing.T) {
	shim := writeShim(t, t.TempDir(), "failer", `echo "reconcile refused" >&2
exit 3`)

	t.Parallel()

	_, err := newExec(t, execConfig(nil)).Execute(t.Context(), execTask(map[string]any{cmdKey: shim}))

	classified := classifiedError(t, err)
	if classified.Class != runner.ClassPermanent || classified.Code != codeExecExit {
		t.Fatalf("class/code = %v/%q, want permanent/%q", classified.Class, classified.Code, codeExecExit)
	}

	if !strings.Contains(classified.Message, "3") {
		t.Fatalf("message %q does not carry the exit code", classified.Message)
	}

	if !strings.Contains(classified.Message, "reconcile refused") {
		t.Fatalf("message %q does not carry the stderr tail", classified.Message)
	}

	if exitErr, isExit := errors.AsType[*exec.ExitError](err); !isExit || exitErr.ExitCode() != 3 {
		t.Fatalf("errors.As found no *exec.ExitError with code 3 behind %v", err)
	}
}

func TestExecMissingCommandIsPermanent(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "not-installed")

	_, err := newExec(t, execConfig(nil)).Execute(t.Context(), execTask(map[string]any{cmdKey: missing}))

	classified := classifiedError(t, err)
	if classified.Class != runner.ClassPermanent || classified.Code != codeExecNotFound {
		t.Fatalf("class/code = %v/%q, want permanent/%q", classified.Class, classified.Code, codeExecNotFound)
	}

	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("errors.Is(%v, fs.ErrNotExist) = false; the launch failure is not in the chain", err)
	}
}

// TestExecTimeoutKillsTheProcess proves the config timeout bounds a command
// that would otherwise outlive its schedule, and that the kill surfaces as a
// transient error rather than a silent success.
func TestExecTimeoutKillsTheProcess(t *testing.T) {
	// The exec makes sleep the direct child, so this case covers a single
	// process. TestExecCancelKillsAForkedChild covers a script that forks.
	sleeper := writeShim(t, t.TempDir(), "sleeper", "exec sleep 30")

	t.Parallel()

	start := time.Now()

	_, err := newExec(t, execConfig(map[string]any{cfgTimeout: int64(1)})).
		Execute(t.Context(), execTask(map[string]any{cmdKey: sleeper}))

	classified := classifiedError(t, err)
	if classified.Class != runner.ClassTransient || classified.Code != codeExecTimeout {
		t.Fatalf("class/code = %v/%q, want transient/%q", classified.Class, classified.Code, codeExecTimeout)
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(%v, context.DeadlineExceeded) = false", err)
	}

	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("kill took %v, want the 1s timeout to bound it", elapsed)
	}
}

// Timing for the cases where a child outlives the command. execWaitDelay is the
// runner's 2-second WaitDelay, the longest a run may outlast its exit or its
// cancel while something holds its pipes. pipeHolderSlack absorbs a loaded
// machine and stays well under the 30 seconds a surviving child holds a pipe.
const (
	execWaitDelay   = 2 * time.Second
	pipeHolderSlack = 5 * time.Second
)

// TestExecCancelKillsAForkedChild covers a script that runs its work as a child
// instead of exec-ing it. The child inherits stdout, so a kill that reached
// only the script would leave the run reading until the child exited, and the
// child would outlive the action. A deadline and a cancel share this kill path.
func TestExecCancelKillsAForkedChild(t *testing.T) {
	// The pid lands before the running marker, so the pid file is whole by the
	// time waitForFile sees the marker.
	forker := writeShim(t, t.TempDir(), "forker",
		"sleep 30 &\necho $! > \"$0.pid\"\necho forked > \"$0.running\"\nwait")

	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	execRunner := newExec(t, execConfig(nil))
	runErr := make(chan error, oneShotBufferSize)

	go func() {
		_, err := execRunner.Execute(ctx, execTask(map[string]any{cmdKey: forker}))
		runErr <- err
	}()

	// Canceling only once the child exists means the kill always meets it,
	// where a short deadline on a loaded machine can pass before the fork.
	waitForFile(t, forker+".running")
	childPID := readChildPID(t, forker+".pid")

	canceledAt := time.Now()

	cancel()

	err := <-runErr
	elapsed := time.Since(canceledAt)

	classified := classifiedError(t, err)
	if classified.Class != runner.ClassTransient || classified.Code != codeExecTimeout {
		t.Fatalf("class/code = %v/%q, want transient/%q", classified.Class, classified.Code, codeExecTimeout)
	}

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(%v, context.Canceled) = false", err)
	}

	if bound := execWaitDelay + pipeHolderSlack; elapsed > bound {
		t.Fatalf("run returned %v after the cancel, want under %v: the forked child held stdout", elapsed, bound)
	}

	requireProcessGone(t, childPID)
}

// readChildPID reads the pid a script recorded for the child it forked.
func readChildPID(t *testing.T, pidFile string) int {
	t.Helper()

	raw, err := os.ReadFile(pidFile) // #nosec G304 -- the path is under t.TempDir.
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse child pid %q: %v", raw, err)
	}

	return pid
}

// requireProcessGone polls signal 0 until pid no longer exists. The kill leaves
// an orphan for its new parent to reap, so a dead child can still answer for a
// moment. One that outlasts the window is killed so it cannot outlive the test.
func requireProcessGone(t *testing.T, pid int) {
	t.Helper()

	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find child %d: %v", pid, err)
	}

	t.Cleanup(func() {
		if err := proc.Release(); err != nil {
			t.Errorf("release child %d: %v", pid, err)
		}
	})

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	deadline := time.After(2 * time.Second)

	for !errors.Is(proc.Signal(syscall.Signal(0)), os.ErrProcessDone) {
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("child %d outlived the kill; killing it now: %v", pid, proc.Kill())
		}
	}
}

// TestExecCleanExitIgnoresHelperHoldingStderr covers a command that exits 0
// while a background helper keeps stderr open. stdout is complete, so the run
// succeeds once WaitDelay passes rather than failing or waiting out the helper.
func TestExecCleanExitIgnoresHelperHoldingStderr(t *testing.T) {
	const output = "stdout complete"

	shim := writeShim(t, t.TempDir(), "stderr-holder", "sleep 30 >/dev/null &\necho "+output)

	t.Parallel()

	start := time.Now()

	result, err := newExec(t, execConfig(nil)).Execute(t.Context(), execTask(map[string]any{cmdKey: shim}))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if bound := execWaitDelay + pipeHolderSlack; elapsed > bound {
		t.Fatalf("run returned after %v, want under %v: the helper held stderr", elapsed, bound)
	}

	if result.Output != output {
		t.Fatalf("output = %#v, want %q", result.Output, output)
	}
}

// TestExecFeedsStdinToTheCommand covers the webhook-receiver shape: the payload
// reaches the script on stdin rather than argv, so a process listing never
// carries it.
func TestExecFeedsStdinToTheCommand(t *testing.T) {
	shim := writeShim(t, t.TempDir(), "reader", "exec cat")

	t.Parallel()

	payload := `{"id":42,"state":"approved"}`

	result, err := newExec(t, execConfig(nil)).Execute(t.Context(),
		execTask(map[string]any{cmdKey: shim, stdinKey: payload}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != payload {
		t.Fatalf("output = %#v, want the stdin payload %q", result.Output, payload)
	}
}

// TestExecWithoutStdinClosesIt proves an absent param leaves a stdin-reading
// command at EOF instead of blocking on whatever the serve process inherited.
func TestExecWithoutStdinClosesIt(t *testing.T) {
	shim := writeShim(t, t.TempDir(), "reader", "exec cat")

	t.Parallel()

	result, err := newExec(t, execConfig(nil)).Execute(t.Context(),
		execTask(map[string]any{cmdKey: shim}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != "" {
		t.Fatalf("output = %#v, want empty", result.Output)
	}
}

func TestExecRunsInTheConfiguredWorkdir(t *testing.T) {
	shim := writeShim(t, t.TempDir(), "wherearewe", "pwd")

	t.Parallel()

	workdir := t.TempDir()

	result, err := newExec(t, execConfig(nil)).Execute(t.Context(),
		execTask(map[string]any{cmdKey: shim, wdKey: workdir}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// pwd reports the physical path and a temp dir is a symlink on some
	// platforms, so the expectation is resolved the same way.
	want, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		t.Fatalf("resolve workdir: %v", err)
	}

	if result.Output != want {
		t.Fatalf("output = %#v, want %q", result.Output, want)
	}
}

func TestExecRejectsAMissingWorkdir(t *testing.T) {
	shim := writeShim(t, t.TempDir(), "noop", "true")

	t.Parallel()

	missing := filepath.Join(t.TempDir(), "gone")

	_, err := newExec(t, execConfig(nil)).Execute(t.Context(),
		execTask(map[string]any{cmdKey: shim, wdKey: missing}))

	if classified := classifiedError(t, err); classified.Class != runner.ClassPermanent {
		t.Fatalf("class = %v, want permanent", classified.Class)
	}
}

// TestExecPassesConfigEnvAndKeepsInheritedEnv pins the whole point of this
// runner: the child sees the serve user's own environment, with the config
// table layered on top.
func TestExecPassesConfigEnvAndKeepsInheritedEnv(t *testing.T) {
	t.Parallel()

	shim := writeShim(t, t.TempDir(), "printenv",
		`printf '%s|%s\n' "$`+hostEnvName+`" "$`+cfgEnvName+`"`)

	cfg := execConfig(map[string]any{envKey: map[string]any{cfgEnvName: "from-config"}})

	built, err := builtins.ExecFactoryWithHostEnv(nil, []string{hostEnvName + "=from-host"}, builtins.ExecTrust{})(cfg)
	if err != nil {
		t.Fatalf("build exec: %v", err)
	}

	result, err := built.Execute(t.Context(), execTask(map[string]any{cmdKey: shim}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != "from-host|from-config" {
		t.Fatalf("output = %#v, want both the inherited and configured values", result.Output)
	}
}

// TestExecCapsCapturedOutput proves a chatty command cannot balloon run
// storage: the transcript stops at 1 MiB plus at most the line that crossed it.
func TestExecCapsCapturedOutput(t *testing.T) {
	shim := writeShim(t, t.TempDir(), "chatty", `i=0
while [ $i -lt 20000 ]; do
  echo "0123456789012345678901234567890123456789012345678901234567890123"
  i=$((i+1))
done`)

	t.Parallel()

	result, err := newExec(t, execConfig(nil)).Execute(t.Context(), execTask(map[string]any{cmdKey: shim}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isString := result.Output.(string)
	if !isString {
		t.Fatalf("output type = %T", result.Output)
	}

	cap1MiB := 1 << 20
	if len(output) > cap1MiB+128 {
		t.Fatalf("output length %d exceeds the 1 MiB cap", len(output))
	}

	if len(output) < cap1MiB/2 {
		t.Fatalf("output length %d suggests the shim under-produced", len(output))
	}
}
