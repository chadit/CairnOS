package builtins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared CLI config keys, the prompt param, and the argv placeholder.
const (
	cfgKeyBinary            = "binary"
	cfgKeyArgs              = "args"
	cfgKeyStdin             = "stdin"
	cfgKeyAllowDashedPrompt = "allow_dashed_prompt"
	paramPrompt             = "prompt"
	promptToken             = "{prompt}"

	// argTerminator is the POSIX end-of-options marker. A {prompt} placed after
	// it in the argv template cannot be read as a flag, so the dashed-prompt
	// guard treats that template as safe.
	argTerminator = "--"
)

// Meta keys every CLI runner reports on success.
const (
	metaExitCode   = "exit_code"
	metaStderrTail = "stderr_tail"
)

const (
	// maxCapturedOutput caps captured stdout (and webhook response bodies) so
	// a chatty tool cannot balloon run storage. 1 << 20 is shift form for
	// 1 MiB per the house power-of-two rule.
	maxCapturedOutput = 1 << 20

	// initialScanBuffer seeds the stdout line scanner. 64 << 10 is shift form
	// of 64 KiB, large enough that typical tool output never reallocates.
	initialScanBuffer = 64 << 10

	// stderrTailKeep is how much trailing stderr survives into Meta and error
	// messages. The tail, not the head, because the cause of a CLI failure is
	// usually the last thing it printed.
	stderrTailKeep = 4096
)

// cliExec runs one configured command line per task. The named CLI runners
// are thin wrappers over this helper, per the spec's build-the-helper-once
// guidance (.agents/spec/03-runner-interface.md, "Built-in runners").
//
// Known limit: a wrapped tool that hands its stdout pipe to a long-lived
// child can delay EOF past the tool's own exit. Process-group cleanup is
// Phase 1b material; the wrapped CLIs today are single-process.
type cliExec struct {
	name   string
	binary string
	args   []string
	stdin  bool

	// allowDashedPrompt opts out of the dashed-prompt guard, for operators who
	// have vetted the wrapped binary and want a prompt that may begin with a
	// dash to ride a bare positional anyway (spec M3).
	allowDashedPrompt bool
}

// newCLIExec decodes the shared CLI config keys (binary, args, stdin) with
// per-runner defaults from the tool's launch template.
func newCLIExec(name string, tool CLITool, cfg map[string]any) (cliExec, error) {
	binary, err := configValue(cfg, cfgKeyBinary, tool.Binary)
	if err != nil {
		return cliExec{}, err
	}

	useStdin, err := configValue(cfg, cfgKeyStdin, false)
	if err != nil {
		return cliExec{}, err
	}

	args, err := configStrings(cfg, cfgKeyArgs)
	if err != nil {
		return cliExec{}, err
	}

	allowDashed, err := configValue(cfg, cfgKeyAllowDashedPrompt, false)
	if err != nil {
		return cliExec{}, err
	}

	if args == nil && !useStdin {
		args = tool.Args
	}

	if args == nil && !useStdin {
		// Without a tool template the prompt travels as the single
		// positional argument, the fallback shape for wrapped CLIs.
		args = []string{promptToken}
	}

	return cliExec{name: name, binary: binary, args: args, stdin: useStdin, allowDashedPrompt: allowDashed}, nil
}

// run launches the configured binary for one task, optionally forwarding
// stdout lines to a chunk stream. A nil stream means plain Execute.
func (c cliExec) run(ctx context.Context, task *runner.Task, stream chan<- runner.Chunk) (runner.Result, error) {
	if err := c.validate(task); err != nil {
		return runner.Result{}, err
	}

	prompt, _ := task.Params[paramPrompt].(string)

	runCtx := ctx

	if !task.Deadline.IsZero() {
		var cancel context.CancelFunc

		runCtx, cancel = context.WithDeadline(ctx, task.Deadline)
		defer cancel()
	}

	// #nosec G204 -- the binary and argv template come from the operator's
	// own runner config block, not from workflow params or network input.
	cmd := exec.CommandContext(runCtx, c.binary, substitutePrompt(c.args, prompt)...)

	if c.stdin {
		cmd.Stdin = strings.NewReader(prompt)
	}

	stderr := &tailBuffer{limit: stderrTailKeep}
	cmd.Stderr = stderr

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return runner.Result{}, &runner.Error{
			Class: runner.ClassPermanent, Code: c.code("start"),
			Message: "open stdout pipe: " + err.Error(), Cause: err,
		}
	}

	start := time.Now()

	if err := cmd.Start(); err != nil {
		return runner.Result{}, c.classifyStart(runCtx, err)
	}

	stdoutText, scanErr := collectStdout(runCtx, stdoutPipe, stream)

	waitErr := cmd.Wait()
	latency := time.Since(start)

	if waitErr != nil {
		return runner.Result{}, c.classifyWait(runCtx, waitErr, cmd.ProcessState.ExitCode(), stderr.Text())
	}

	if scanErr != nil {
		return runner.Result{}, &runner.Error{
			Class: runner.ClassPermanent, Code: c.code("output"),
			Message: "read stdout: " + scanErr.Error(), Cause: scanErr,
		}
	}

	return runner.Result{
		Output: stdoutText,
		Usage:  runner.Usage{Latency: latency},
		Meta: map[string]string{
			metaExitCode:   strconv.Itoa(cmd.ProcessState.ExitCode()),
			metaStderrTail: stderr.Text(),
		},
	}, nil
}

// validate enforces the shared CLI params contract: a string prompt, and the
// dashed-prompt guard. An unresolved reference still reads as a string, so the
// load-time pass lets it through and the launch-time pass sees the final value.
func (c cliExec) validate(task *runner.Task) error {
	if err := requireStringParam(task, paramPrompt, c.name); err != nil {
		return err
	}

	return c.guardDashedPrompt(task)
}

// guardDashedPrompt refuses a resolved prompt that begins with a dash when the
// argv template would feed it as a bare positional, since the wrapped binary
// would read it as a flag (spec M3). An unresolved reference defers to launch.
// The guard is skipped when the operator opts in with allow_dashed_prompt or
// when the template places {prompt} after a -- terminator, which already stops
// flag parsing. A prompt sent on stdin is never argv, so the guard does not
// apply there.
func (c cliExec) guardDashedPrompt(task *runner.Task) error {
	if c.allowDashedPrompt || c.stdin || c.promptAfterTerminator() {
		return nil
	}

	prompt, isString := task.Params[paramPrompt].(string)
	if !isString {
		return nil
	}

	if !task.Resolved && containsRefDelimiter(prompt) {
		return nil
	}

	if strings.HasPrefix(prompt, "-") {
		return runner.NewError(runner.ClassPermanent, c.code("unsafe_prompt"),
			"prompt begins with a dash and would be read as a flag by "+c.binary+
				"; place {prompt} after a -- terminator in the args template, or set allow_dashed_prompt = true")
	}

	return nil
}

// promptAfterTerminator reports whether every {prompt} placeholder in the argv
// template sits after a -- terminator, the position where a dash-leading value
// can no longer be parsed as a flag. A template with no {prompt} (the prompt
// rides elsewhere) is treated as safe.
func (c cliExec) promptAfterTerminator() bool {
	var seenTerminator bool

	for _, arg := range c.args {
		if arg == argTerminator {
			seenTerminator = true

			continue
		}

		if strings.Contains(arg, promptToken) && !seenTerminator {
			return false
		}
	}

	return true
}

// code builds a stable machine code under this runner's name.
func (c cliExec) code(suffix string) string {
	return c.name + "." + suffix
}

// classifyStart maps a launch failure. A missing binary is permanent because
// no retry installs it; so is anything else at start (bad path, bad mode). A
// deadline that expires while the process is still launching is the exception:
// that is the same transient timeout classifyWait reports once the process is
// running, and calling it permanent would deny a retry the fresh deadline could
// clear.
func (c cliExec) classifyStart(runCtx context.Context, err error) error {
	if runCtx.Err() != nil {
		return &runner.Error{
			Class:   runner.ClassTransient,
			Code:    c.code("timeout"),
			Message: c.binary + " canceled or timed out",
			Cause:   err,
		}
	}

	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return &runner.Error{
			Class:   runner.ClassPermanent,
			Code:    c.code("not_found"),
			Message: fmt.Sprintf("binary %q not found", c.binary),
			Cause:   err,
		}
	}

	return &runner.Error{
		Class: runner.ClassPermanent, Code: c.code("start"),
		Message: "start " + c.binary + ": " + err.Error(), Cause: err,
	}
}

// classifyWait maps a finished-but-failed process. A done context wins over
// the exit status because CommandContext kills the child on cancellation, so
// the non-zero exit is an effect of the timeout, not the tool's verdict.
func (c cliExec) classifyWait(runCtx context.Context, waitErr error, exitCode int, stderrTail string) error {
	if runCtx.Err() != nil {
		return &runner.Error{
			Class:   runner.ClassTransient,
			Code:    c.code("timeout"),
			Message: c.binary + " canceled or timed out",
			Cause:   waitErr,
		}
	}

	if _, isExit := errors.AsType[*exec.ExitError](waitErr); isExit {
		message := fmt.Sprintf("%s exited with code %d", c.binary, exitCode)
		if stderrTail != "" {
			message += ": " + stderrTail
		}

		return c.classifyExitOutput(stderrTail, message, waitErr)
	}

	return &runner.Error{
		Class: runner.ClassPermanent, Code: c.code("wait"),
		Message: "wait for " + c.binary + ": " + waitErr.Error(), Cause: waitErr,
	}
}

// classifyExitOutput reads a non-zero exit's stderr to pick the error class.
// Rate-limit signals win first, so a throttled call is not misread as expiry;
// only then does a re-auth signal (a dead OAuth refresh token on a subscription
// CLI tool) classify as NeedsReauth, which holds the job for a human instead of
// failing it (D12, .agents/spec/14-open-decisions.md). Everything else is
// permanent: a non-zero exit the tool did not explain is not safe to repeat.
func (c cliExec) classifyExitOutput(stderrTail, message string, cause error) error {
	switch {
	case looksRateLimited(stderrTail):
		return &runner.Error{Class: runner.ClassRateLimited, Code: c.code("rate_limited"), Message: message, Cause: cause}
	case looksReauth(stderrTail):
		return &runner.Error{
			Class:   runner.ClassNeedsReauth,
			Code:    c.code("needs_reauth"),
			Message: message + " (re-authenticate the " + c.binary + " CLI)",
			Cause:   cause,
		}
	default:
		return &runner.Error{Class: runner.ClassPermanent, Code: c.code("exit"), Message: message, Cause: cause}
	}
}

// looksReauth spots an expired-credential signal in a CLI tool's output. The
// strongest cross-tool marker is invalid_grant (a dead OAuth refresh token);
// codex also surfaces 401 Unauthorized and "exceeded retry limit". Callers rule
// out rate-limit text first, so this never sees a throttled call.
func looksReauth(message string) bool {
	lowered := strings.ToLower(message)

	for _, marker := range []string{
		"invalid_grant",
		"unauthorized",
		"exceeded retry limit",
		"token expired",
		"token has expired",
		"expired credentials",
		"re-authenticate",
		"reauthenticate",
		"not authenticated",
		"log in again",
		"login again",
		"please log in",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}

	return false
}

// collectStdout drains the pipe line by line, forwarding to the stream when
// one is attached and keeping a bounded transcript for Result.Output. The
// bound is checked per line, so the transcript may overshoot by one line.
func collectStdout(ctx context.Context, pipe io.Reader, stream chan<- runner.Chunk) (string, error) {
	var collected strings.Builder

	collected.Grow(initialScanBuffer)

	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, initialScanBuffer), maxCapturedOutput)

	for scanner.Scan() {
		line := scanner.Text()

		if stream != nil && !forwardChunk(ctx, stream, line) {
			stream = nil
		}

		if collected.Len() >= maxCapturedOutput {
			continue
		}

		if collected.Len() > 0 {
			collected.WriteByte('\n')
		}

		collected.WriteString(line)
	}

	if err := scanner.Err(); err != nil {
		return collected.String(), fmt.Errorf("scan stdout: %w", err)
	}

	return collected.String(), nil
}

// forwardChunk sends one line to the stream, reporting false when the context
// died first so a dying run cannot block stdout draining on an abandoned
// consumer.
func forwardChunk(ctx context.Context, stream chan<- runner.Chunk, line string) bool {
	select {
	case stream <- runner.Chunk{Data: line, Time: time.Now()}:
		return true
	case <-ctx.Done():
		return false
	}
}

// substitutePrompt expands the argv template, replacing every occurrence of
// the {prompt} placeholder with the task's prompt value.
func substitutePrompt(args []string, prompt string) []string {
	argv := make([]string, len(args))

	for idx, arg := range args {
		argv[idx] = strings.ReplaceAll(arg, promptToken, prompt)
	}

	return argv
}

// tailBuffer keeps the last limit bytes written to it. Failures usually end
// with the useful line, so the tail beats the head for diagnostics.
type tailBuffer struct {
	limit int
	data  []byte
}

// Write implements io.Writer; it never fails.
func (b *tailBuffer) Write(chunk []byte) (int, error) {
	b.data = append(b.data, chunk...)

	if len(b.data) > b.limit {
		b.data = b.data[len(b.data)-b.limit:]
	}

	return len(chunk), nil
}

// Text returns the retained tail without surrounding whitespace.
func (b *tailBuffer) Text() string {
	return strings.TrimSpace(string(b.data))
}
