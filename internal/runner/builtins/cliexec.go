package builtins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chadit/CairnOS/internal/execretry"
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
	// messages, and how much trailing stdout the exit classifier reads. The
	// tail, not the head, because the cause of a CLI failure is usually the
	// last thing it printed.
	stderrTailKeep = 4096

	// cliWaitDelay bounds the wait on the pipes after the deadline or the
	// tool's exit. A helper that left the process group through setsid survives
	// the group kill, and on Windows every child of the tool does, so either can
	// hold stdout open until os/exec closes the pipes.
	cliWaitDelay = 2 * time.Second
)

// Refusal markers for a failed CLI exit, matched on lowercased output inside
// non-alphanumeric boundaries so "quotation" is not "quota" and a "rate
// limiter" log line is not a rate limit.
var (
	// cliLimitPattern joins two sets: the 429, rate-limit, too-many-requests,
	// usage-limit, and quota wording rate-limited APIs and CLIs share, and the
	// cap and credit phrases Claude Code 2.1.284 and codex 0.157 print that
	// don't contain "usage limit": "You've hit your session limit", "out of
	// credits", "out of extra usage", "credit balance is too low".
	cliLimitPattern = regexp.MustCompile(`(^|[^a-z0-9])(429|rate.?limit(ed|s)?|too many requests|usage limits?|hit your [a-z' -]{0,30}(limit|budget|spend cap)|quota|out of credits|out of extra usage|credit balance (is )?too low)([^a-z0-9]|$)`)

	// cliOverloadPattern is kept apart from the limit set because a 529 is
	// the provider shedding load, not this account running out, the same split
	// the Anthropic runner makes for overloaded_error.
	cliOverloadPattern = regexp.MustCompile(`(^|[^a-z0-9])(529|overloaded)([^a-z0-9]|$)`)
)

// decodeCLI decodes the shared CLI config keys (binary, args, stdin) with
// per-runner defaults from the tool's launch template.
func decodeCLI(name string, tool CLITool, cfg map[string]any) (*CLI, error) {
	binary, err := configValue(cfg, cfgKeyBinary, tool.Binary)
	if err != nil {
		return nil, err
	}

	useStdin, err := configValue(cfg, cfgKeyStdin, false)
	if err != nil {
		return nil, err
	}

	args, err := configStrings(cfg, cfgKeyArgs)
	if err != nil {
		return nil, err
	}

	allowDashed, err := configValue(cfg, cfgKeyAllowDashedPrompt, false)
	if err != nil {
		return nil, err
	}

	if args == nil && !useStdin {
		args = tool.Args
	}

	if args == nil && !useStdin {
		// Without a tool template the prompt travels as the single
		// positional argument, the fallback shape for wrapped CLIs.
		args = []string{promptToken}
	}

	return &CLI{
		name: name, binary: binary, args: args, stdin: useStdin, allowDashedPrompt: allowDashed,
		reauthProvider: templateOwner(name, tool),
	}, nil
}

// templateOwner names the built-in whose launch template tool is, which is the
// type wiring built a declared wrapper block from. A template no built-in owns,
// such as a test shim, falls back to the runner's own name.
func templateOwner(name string, tool CLITool) string {
	for builtin, template := range CLIDefaults() {
		if template.Binary == tool.Binary && slices.Equal(template.Args, tool.Args) {
			return builtin
		}
	}

	return name
}

// ExecuteStream implements runner.Streamer, launching the configured binary for
// one task and forwarding stdout lines as chunks while the transcript still
// accumulates into Result.Output. Execute passes a nil stream.
func (c *CLI) ExecuteStream(ctx context.Context, task *runner.Task, stream chan<- runner.Chunk) (runner.Result, error) {
	if err := c.Validate(task); err != nil {
		return runner.Result{}, err
	}

	prompt, _ := task.Params[paramPrompt].(string)

	runCtx := ctx

	if !task.Deadline.IsZero() {
		var cancel context.CancelFunc

		runCtx, cancel = context.WithDeadline(ctx, task.Deadline)
		defer cancel()
	}

	start := time.Now()

	piped, err := startPiped(runCtx, func() *exec.Cmd {
		// #nosec G204 -- the binary and argv template come from the operator's
		// own runner config block, not from workflow params or network input.
		cmd := exec.CommandContext(runCtx, c.binary, substitutePrompt(c.args, prompt)...)
		cmd.WaitDelay = cliWaitDelay
		groupProcess(cmd)

		// A nil Env inherits serve's environment, which is what these runners have
		// always done and what the wrapped tools' own logins depend on. Setting it is
		// reserved for the case where a caller supplied one.
		if c.inherited != nil || len(c.env) > 0 {
			cmd.Env = append(slices.Clone(c.inherited), c.env...)
		}

		if c.stdin {
			cmd.Stdin = strings.NewReader(prompt)
		}

		return cmd
	})
	if err != nil {
		if errors.Is(err, errStdoutPipe) {
			return runner.Result{}, &runner.Error{
				Class: runner.ClassPermanent, Code: c.code("start"),
				Message: err.Error(), Cause: err,
			}
		}

		return runner.Result{}, c.classifyStart(runCtx, err)
	}

	cmd, stderr := piped.cmd, piped.stderr

	// Claude Code prints its limit and login refusals on stdout, and the
	// transcript stops growing at maxCapturedOutput, so the classifier gets its
	// own tail rather than the transcript's head.
	stdoutTail := &tailBuffer{limit: stderrTailKeep}

	stdoutText, scanErr := collectStdout(runCtx, io.TeeReader(piped.stdout, stdoutTail), stream)

	waitErr := cmd.Wait()
	latency := time.Since(start)

	// WaitDelay also cuts the wait short when the tool exits cleanly but leaves
	// a helper holding stderr. stdout already reached EOF, so the result is
	// whole and only the stderr tail can come up short.
	if errors.Is(waitErr, exec.ErrWaitDelay) && runCtx.Err() == nil {
		waitErr = nil
	}

	if waitErr != nil {
		return runner.Result{}, c.classifyWait(runCtx, waitErr, stderr.Text(), stdoutTail.Text())
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

// Validate implements runner.Runner, enforcing the CLI params contract: a
// string prompt, and the dashed-prompt guard. An unresolved reference still
// reads as a string, so the load-time pass lets it through and the launch-time
// pass sees the final value.
func (c *CLI) Validate(task *runner.Task) error {
	if err := requireStringParam(task, paramPrompt, c.name); err != nil {
		return err
	}

	return c.guardDashedPrompt(task)
}

// guardDashedPrompt refuses a resolved prompt starting with a dash when the
// argv template feeds it as a bare positional, since the wrapped binary would
// read it as a flag (spec M3). An unresolved reference defers to launch. The
// guard steps aside for allow_dashed_prompt, a template placing {prompt} after
// a -- terminator, or a stdin prompt, which never reaches argv.
func (c *CLI) guardDashedPrompt(task *runner.Task) error {
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
func (c *CLI) promptAfterTerminator() bool {
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

// code builds a stable machine code under this runner's name. The re-auth code
// is the one exception and uses reauthProvider.
func (c *CLI) code(suffix string) string {
	return c.name + "." + suffix
}

// classifyStart maps a launch failure. A missing binary is permanent because no
// retry installs it, and so is anything else at start (bad path, bad mode). An
// expired deadline is the exception: it is the transient timeout classifyWait
// reports once the process runs, and calling it permanent would deny a retry a
// fresh deadline could clear.
func (c *CLI) classifyStart(runCtx context.Context, err error) error {
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
func (c *CLI) classifyWait(runCtx context.Context, waitErr error, stderrTail, stdoutTail string) error {
	if runCtx.Err() != nil {
		return &runner.Error{
			Class:   runner.ClassTransient,
			Code:    c.code("timeout"),
			Message: c.binary + " canceled or timed out",
			Cause:   waitErr,
		}
	}

	if exitErr, isExit := errors.AsType[*exec.ExitError](waitErr); isExit {
		message := fmt.Sprintf("%s exited with code %d", c.binary, exitErr.ExitCode())
		if stderrTail != "" {
			message += ": " + stderrTail
		}

		return c.classifyExitOutput(stderrTail+"\n"+stdoutTail, message, waitErr)
	}

	return &runner.Error{
		Class: runner.ClassPermanent, Code: c.code("wait"),
		Message: "wait for " + c.binary + ": " + waitErr.Error(), Cause: waitErr,
	}
}

// classifyExitOutput reads a non-zero exit's output tails to pick the error
// class. A limit wins over re-auth, so a capped call retries (the wrapped CLI
// picks its account per launch) instead of holding the job for a human (D12,
// .agents/spec/14-open-decisions.md). An unexplained non-zero exit is
// permanent: it is not safe to repeat.
func (c *CLI) classifyExitOutput(output, message string, cause error) error {
	lowered := strings.ToLower(output)

	switch {
	case cliLimitPattern.MatchString(lowered):
		return &runner.Error{Class: runner.ClassRateLimited, Code: c.code("rate_limited"), Message: message, Cause: cause}
	case cliOverloadPattern.MatchString(lowered):
		return &runner.Error{Class: runner.ClassTransient, Code: c.code("overloaded"), Message: message, Cause: cause}
	case looksReauth(lowered):
		return &runner.Error{
			Class:   runner.ClassNeedsReauth,
			Code:    c.reauthProvider + ".needs_reauth",
			Message: message + " (re-authenticate the " + c.binary + " CLI)",
			Cause:   cause,
		}
	default:
		return &runner.Error{Class: runner.ClassPermanent, Code: c.code("exit"), Message: message, Cause: cause}
	}
}

// looksReauth spots an expired-credential signal in a CLI tool's lowercased
// output. The strongest cross-tool marker is invalid_grant (a dead OAuth
// refresh token); codex also surfaces 401 Unauthorized and "exceeded retry
// limit", and Claude Code ends each login refusal with "Please run /login".
// Callers rule out limit text first, so this never sees a capped call.
func looksReauth(lowered string) bool {
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
		"not logged in",
		"please run /login",
		"invalid api key",
		"session has expired",
		"authentication failed",
		"authentication error",
		"failed to authenticate",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}

	return false
}

// pipedCmd is a child with the pipes its caller drains: stdout to read, and a
// bounded stderr tail the failure message quotes. path is the resolved
// executable of the last attempt, which a failed start still needs to name.
type pipedCmd struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *tailBuffer
	path   string
}

// startPiped wires the pipes onto a fresh command and starts it, retrying a
// busy executable: a workflow can write a script and run it in the next breath,
// and execve refuses while another fork holds the write descriptor. A pipe
// failure wraps errStdoutPipe, which is how a caller tells it apart from a
// failed start. The result is never nil.
func startPiped(ctx context.Context, build func() *exec.Cmd) (*pipedCmd, error) {
	piped := &pipedCmd{}

	started, err := execretry.Start(ctx, func() (*exec.Cmd, error) {
		cmd := build()
		piped.path = cmd.Path
		piped.stderr = &tailBuffer{limit: stderrTailKeep}
		cmd.Stderr = piped.stderr

		stdout, pipeErr := cmd.StdoutPipe()
		if pipeErr != nil {
			return nil, fmt.Errorf("%w: %w", errStdoutPipe, pipeErr)
		}

		piped.stdout = stdout

		return cmd, nil
	})

	piped.cmd = started

	//nolint:wrapcheck // each runner classifies this failure into its own code
	return piped, err
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
	data  []byte
	limit int
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
