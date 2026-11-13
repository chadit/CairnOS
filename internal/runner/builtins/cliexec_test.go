package builtins_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/shellquote"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture names for the runner tests.
const (
	promptKey  = "prompt"
	binaryKey  = "binary"
	argsKey    = "args"
	stdinKey   = "stdin"
	testPrompt = "hello cairn"

	// promptArg is the {prompt} placeholder and codexBinary the codex binary
	// literal, hoisted so the template assertions share one spelling.
	promptArg   = "{prompt}"
	codexBinary = "codex"

	// flagArg is a flag-shaped argv element and argTerminator the POSIX
	// end-of-options marker, for the argv and template cases.
	flagArg       = "--flag"
	argTerminator = "--"

	// actionCLI is the action the CLI tasks run, and shimStderr the line the
	// echo shims print to stderr for stderr_tail to carry back.
	actionCLI  = "cli"
	shimStderr = "warn"

	// qwenTimeoutCode is the code every deadline case expects from the qwen
	// runner the timeout tests drive.
	qwenTimeoutCode = builtins.QwenName + ".timeout"
)

// writeShim drops an executable script into dir and returns its path. Tests
// point a runner's binary at it instead of any real CLI.
//
// Tests call it before t.Parallel: a fork racing an open write fd fails with ETXTBSY
// (Go issue 22315), and top-level tests run serially until they go parallel,
// so writing first closes it before any fork.
func writeShim(t *testing.T, dir, name, script string) string {
	t.Helper()

	path := filepath.Join(dir, name)

	// #nosec G306 -- the shim must be executable; it lives in t.TempDir.
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatalf("write shim: %v", err)
	}

	return path
}

// executeCanceledOnFirstLine cancels task once the tool prints a line, so the
// kill path always meets a running process: on a loaded machine a short
// deadline can expire before Start, which then launches nothing. The duration
// it returns starts at the cancel, so a slow start cannot eat the caller's
// bound.
func executeCanceledOnFirstLine(t *testing.T, streamer runner.Streamer, task *runner.Task) (time.Duration, error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream := make(chan runner.Chunk)
	done := make(chan error, oneShotBufferSize)

	go func() {
		_, err := streamer.ExecuteStream(ctx, task, stream)
		done <- err
	}()

	select {
	case <-stream:
	case err := <-done:
		t.Fatalf("run ended before printing a line: %v", err)
	}

	canceled := time.Now()

	cancel()

	err := <-done

	return time.Since(canceled), err
}

func cliTask() *runner.Task {
	return &runner.Task{
		RunID:      testRunID,
		ActionName: actionCLI,
		Params:     map[string]any{promptKey: testPrompt},
		Resolved:   true,
	}
}

func TestCLICapturesOutputAndExitCode(t *testing.T) {
	shim := writeShim(t, t.TempDir(), "echoer", `echo "got: $1"
echo "warn" >&2`)

	t.Parallel()

	cli, err := builtins.NewCLI(builtins.CodexName, builtins.CLITool{Binary: shim}, nil)
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	result, err := cli.Execute(t.Context(), cliTask())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, isString := result.Output.(string)
	if !isString || output != "got: "+testPrompt {
		t.Fatalf("output = %#v", result.Output)
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

func TestCLIClassifiesFailures(t *testing.T) {
	tmpDir := t.TempDir()
	failing := writeShim(t, tmpDir, "failing", `echo "boom" >&2
exit 1`)

	t.Parallel()

	cases := []struct {
		name      string
		binary    string
		wantCode  string
		wantClass runner.ErrorClass
	}{
		{name: "non-zero exit is permanent", binary: failing, wantClass: runner.ClassPermanent, wantCode: codexExit},
		{name: "missing binary is permanent", binary: filepath.Join(tmpDir, "missing"), wantClass: runner.ClassPermanent, wantCode: "codex.not_found"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cli, err := builtins.NewCLI(builtins.CodexName, builtins.CLITool{Binary: tt.binary}, nil)
			if err != nil {
				t.Fatalf("new cli: %v", err)
			}

			_, err = cli.Execute(t.Context(), cliTask())

			classified, isClassified := errors.AsType[*runner.Error](err)
			if !isClassified {
				t.Fatalf("error %v is not a runner.Error", err)
			}

			if classified.Class != tt.wantClass || classified.Code != tt.wantCode {
				t.Fatalf("class/code = %v/%q, want %v/%q", classified.Class, classified.Code, tt.wantClass, tt.wantCode)
			}
		})
	}
}

// Messages the CLIs print when they turn a call away, read out of the Claude
// Code 2.1.284 and codex 0.157 binaries. Codex's apostrophe is U+2019.
const (
	codexUsageLimit    = "You’ve hit your usage limit. Upgrade to Plus to continue using Codex (https://chatgpt.com/explore/plus), or try again at 3:05 PM."
	claudeSessionLimit = "You've hit your session limit · resets 3pm (America/Denver)"
	claudeWeeklyLimit  = "You've hit your weekly limit · resets Oct 6, 1pm (America/Denver)"
	claudeUsageLimit   = "Usage limit reached"
)

// Runner codes the classification tables expect on several rows.
const (
	codexRateLimited  = "codex.rate_limited"
	claudeRateLimited = "claude.rate_limited"
	codexExit         = "codex.exit"
	codexNeedsReauth  = "codex.needs_reauth"
	claudeNeedsReauth = "claude.needs_reauth"
)

// failOnStdout and failOnStderr are shim bodies that print message on one
// stream and exit 1.
func failOnStdout(message string) string {
	return "printf '%s\\n' " + shellquote.Word(message) + "\nexit 1"
}

func failOnStderr(message string) string {
	return "printf '%s\\n' " + shellquote.Word(message) + " >&2\nexit 1"
}

func TestCLIClassifiesExitOutput(t *testing.T) {
	tmpDir := t.TempDir()

	var written int

	// shim writes every case's script before t.Parallel below; see writeShim.
	shim := func(script string) string {
		written++

		return writeShim(t, tmpDir, "shim"+strconv.Itoa(written), script)
	}

	cases := []struct {
		name      string
		tool      string
		binary    string
		wantCode  string
		wantClass runner.ErrorClass
	}{
		{name: "codex usage limit on stderr", tool: builtins.CodexName, binary: shim(failOnStderr(codexUsageLimit)), wantClass: runner.ClassRateLimited, wantCode: codexRateLimited},
		{name: "codex usage limit on stdout", tool: builtins.CodexName, binary: shim(failOnStdout(codexUsageLimit)), wantClass: runner.ClassRateLimited, wantCode: codexRateLimited},
		{name: "codex out of credits", tool: builtins.CodexName, binary: shim(failOnStderr("Your workspace is out of credits. Ask your workspace owner to refill in order to continue.")), wantClass: runner.ClassRateLimited, wantCode: codexRateLimited},
		{name: "insufficient_quota", tool: builtins.CodexName, binary: shim(failOnStderr("stream error: insufficient_quota")), wantClass: runner.ClassRateLimited, wantCode: codexRateLimited},
		{name: "claude session limit on stdout", tool: builtins.ClaudeName, binary: shim(failOnStdout(claudeSessionLimit)), wantClass: runner.ClassRateLimited, wantCode: claudeRateLimited},
		{name: "claude session limit on stderr", tool: builtins.ClaudeName, binary: shim(failOnStderr(claudeSessionLimit)), wantClass: runner.ClassRateLimited, wantCode: claudeRateLimited},
		{name: "claude weekly limit on stdout", tool: builtins.ClaudeName, binary: shim(failOnStdout(claudeWeeklyLimit)), wantClass: runner.ClassRateLimited, wantCode: claudeRateLimited},
		{name: "claude usage limit reached on stdout", tool: builtins.ClaudeName, binary: shim(failOnStdout(claudeUsageLimit)), wantClass: runner.ClassRateLimited, wantCode: claudeRateLimited},
		{name: "claude credit balance on stdout", tool: builtins.ClaudeName, binary: shim(failOnStdout("Your credit balance is too low to access the Anthropic API.")), wantClass: runner.ClassRateLimited, wantCode: claudeRateLimited},
		{
			name: "limit notice after a transcript longer than the captured output",
			tool: builtins.ClaudeName,
			// 250000 lines of "transcript" run past the 1 MiB transcript cap, so
			// only a true stdout tail still holds the final line.
			binary:    shim("yes transcript | head -n 250000\n" + failOnStdout(claudeUsageLimit)),
			wantClass: runner.ClassRateLimited, wantCode: claudeRateLimited,
		},
		{name: "rate-limit text wins over reauth", tool: builtins.CodexName, binary: shim(failOnStderr("429 too many requests; exceeded retry limit")), wantClass: runner.ClassRateLimited, wantCode: codexRateLimited},
		{name: "claude overloaded 529 is transient", tool: builtins.ClaudeName, binary: shim(failOnStdout(`API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)), wantClass: runner.ClassTransient, wantCode: "claude.overloaded"},
		{name: "invalid_grant is needs-reauth", tool: builtins.CodexName, binary: shim(failOnStderr("error: invalid_grant")), wantClass: runner.ClassNeedsReauth, wantCode: codexNeedsReauth},
		{name: "claude not logged in on stdout is needs-reauth", tool: builtins.ClaudeName, binary: shim(failOnStdout("Not logged in · Please run /login")), wantClass: runner.ClassNeedsReauth, wantCode: claudeNeedsReauth},
		{name: "claude invalid api key is needs-reauth", tool: builtins.ClaudeName, binary: shim(failOnStdout("Invalid API key · Please run /login")), wantClass: runner.ClassNeedsReauth, wantCode: claudeNeedsReauth},
		{name: "codex not logged in is needs-reauth", tool: builtins.CodexName, binary: shim(failOnStderr("Not logged in")), wantClass: runner.ClassNeedsReauth, wantCode: codexNeedsReauth},
		{name: "quotation is not quota", tool: builtins.CodexName, binary: shim(failOnStderr("error: unterminated quotation in config.toml")), wantClass: runner.ClassPermanent, wantCode: codexExit},
		{name: "rate limiter log line is not a limit", tool: builtins.CodexName, binary: shim(failOnStderr("client rate limiter delayed a request")), wantClass: runner.ClassPermanent, wantCode: codexExit},
		{name: "unrelated stdout failure is permanent", tool: builtins.ClaudeName, binary: shim(failOnStdout("compilation failed: 3 errors")), wantClass: runner.ClassPermanent, wantCode: "claude.exit"},
	}

	t.Parallel()

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cli, err := builtins.NewCLI(tt.tool, builtins.CLITool{Binary: tt.binary}, nil)
			if err != nil {
				t.Fatalf("new cli: %v", err)
			}

			_, err = cli.Execute(t.Context(), cliTask())

			classified, isClassified := errors.AsType[*runner.Error](err)
			if !isClassified {
				t.Fatalf("error %v is not a runner.Error", err)
			}

			if classified.Class != tt.wantClass || classified.Code != tt.wantCode {
				t.Fatalf("class/code = %v/%q, want %v/%q", classified.Class, classified.Code, tt.wantClass, tt.wantCode)
			}
		})
	}
}

// TestCLIDeclaredWrapperReauthNamesItsType covers a [runner.<name>] block that
// wraps a built-in. The scheduler reads the credential to hold a job on from
// the re-auth code, so that code carries the wrapped built-in's name, while
// every other code keeps the block's own name.
func TestCLIDeclaredWrapperReauthNamesItsType(t *testing.T) {
	const blockName = "summarize"

	tmpDir := t.TempDir()
	notLoggedIn := writeShim(t, tmpDir, "not-logged-in", failOnStdout("Not logged in · Please run /login"))
	failing := writeShim(t, tmpDir, "failing", failOnStderr("compilation failed: 3 errors"))

	t.Parallel()

	cases := []struct {
		name      string
		binary    string
		wantCode  string
		wantClass runner.ErrorClass
	}{
		{name: "re-auth carries the wrapped type", binary: notLoggedIn, wantClass: runner.ClassNeedsReauth, wantCode: builtins.ClaudeName + ".needs_reauth"},
		{name: "other failures keep the block name", binary: failing, wantClass: runner.ClassPermanent, wantCode: blockName + ".exit"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Wiring builds a type = "claude" block from claude's launch
			// template, and the block's own binary key points it at the shim.
			claudeTemplate := builtins.CLIDefaults()[builtins.ClaudeName]

			cli, err := builtins.NewCLI(blockName, claudeTemplate, map[string]any{binaryKey: tt.binary})
			if err != nil {
				t.Fatalf("new cli: %v", err)
			}

			_, err = cli.Execute(t.Context(), cliTask())

			classified, isClassified := errors.AsType[*runner.Error](err)
			if !isClassified {
				t.Fatalf("error %v is not a runner.Error", err)
			}

			if classified.Class != tt.wantClass || classified.Code != tt.wantCode {
				t.Fatalf("class/code = %v/%q, want %v/%q", classified.Class, classified.Code, tt.wantClass, tt.wantCode)
			}
		})
	}
}

func TestCLIDeadlineIsTransientTimeout(t *testing.T) {
	// The exec keeps sleep from surviving as an orphan that holds the
	// stdout pipe open past the kill.
	sleeper := writeShim(t, t.TempDir(), "sleeper", "exec sleep 30")

	t.Parallel()

	cli, err := builtins.NewCLI(builtins.QwenName, builtins.CLITool{Binary: sleeper}, nil)
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	task := cliTask()
	task.Deadline = time.Now().Add(50 * time.Millisecond)

	start := time.Now()

	_, err = cli.Execute(t.Context(), task)

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified || classified.Class != runner.ClassTransient || classified.Code != qwenTimeoutCode {
		t.Fatalf("error = %v, want transient qwen.timeout", err)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("kill took %v, want prompt return", elapsed)
	}
}

// TestCLIDeadlineAtStartIsTransientTimeout proves a deadline that is already
// spent when the process launches classifies as the same transient timeout a
// deadline mid-run does. Calling a spent deadline a permanent start fault would
// deny the retry a fresh deadline could clear, and under load a short deadline
// takes this path rather than the mid-run one.
func TestCLIDeadlineAtStartIsTransientTimeout(t *testing.T) {
	sleeper := writeShim(t, t.TempDir(), "sleeper", "exec sleep 30")

	t.Parallel()

	cli, err := builtins.NewCLI(builtins.QwenName, builtins.CLITool{Binary: sleeper}, nil)
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	task := cliTask()
	task.Deadline = time.Now().Add(-time.Second)

	_, err = cli.Execute(t.Context(), task)

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified || classified.Class != runner.ClassTransient || classified.Code != qwenTimeoutCode {
		t.Fatalf("error = %v, want transient qwen.timeout", err)
	}
}

func TestCLIStreamsChunks(t *testing.T) {
	multi := writeShim(t, t.TempDir(), "multi", `echo "line one"
echo "line two"`)

	t.Parallel()

	cli, err := builtins.NewCLI(builtins.OMPName, builtins.CLITool{Binary: multi}, nil)
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	out := make(chan runner.Chunk, streamChunkBufferSize)

	result, err := cli.ExecuteStream(t.Context(), cliTask(), out)
	if err != nil {
		t.Fatalf("execute stream: %v", err)
	}

	close(out)

	var lines []string

	for chunk := range out {
		lines = append(lines, chunk.Data)
	}

	if len(lines) != 2 || lines[0] != "line one" || lines[1] != "line two" {
		t.Fatalf("chunks = %v", lines)
	}

	if result.Output != "line one\nline two" {
		t.Fatalf("output = %#v", result.Output)
	}
}

func TestCLIStdinMode(t *testing.T) {
	catShim := writeShim(t, t.TempDir(), "catter", "cat")

	t.Parallel()

	cli, err := builtins.NewCLI(builtins.AntigravityName, builtins.CLITool{Binary: catShim}, map[string]any{stdinKey: true})
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	result, err := cli.Execute(t.Context(), cliTask())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != testPrompt {
		t.Fatalf("output = %#v, want prompt echoed from stdin", result.Output)
	}
}

func TestCLIConfigOverridesBinaryAndArgs(t *testing.T) {
	shim := writeShim(t, t.TempDir(), "override", `echo "$1|$2"`)

	t.Parallel()

	cfg := map[string]any{
		binaryKey: shim,
		argsKey:   []any{"--prompt", promptArg},
	}

	cli, err := builtins.NewCLI(builtins.CodexName, builtins.CLITool{Binary: "definitely-not-on-path"}, cfg)
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	result, err := cli.Execute(t.Context(), cliTask())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if want := "--prompt|" + testPrompt; result.Output != want {
		t.Fatalf("output = %#v, want %q", result.Output, want)
	}
}

func TestCLIValidatePromptParam(t *testing.T) {
	t.Parallel()

	cases := []struct {
		params   map[string]any
		name     string
		resolved bool
		wantErr  bool
	}{
		{name: "missing prompt", params: map[string]any{}, resolved: true, wantErr: true},
		{name: "non-string prompt", params: map[string]any{promptKey: int64(7)}, resolved: true, wantErr: true},
		{name: caseUnresolvedDefers, params: map[string]any{promptKey: unresolved}, resolved: false, wantErr: false},
		{name: "literal prompt", params: map[string]any{promptKey: testPrompt}, resolved: true, wantErr: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cli, err := builtins.NewCLI(builtins.CodexName, builtins.CLITool{Binary: codexBinary}, nil)
			if err != nil {
				t.Fatalf("new cli: %v", err)
			}

			err = cli.Validate(&runner.Task{
				RunID:      testRunID,
				ActionName: "validate",
				Params:     tt.params,
				Resolved:   tt.resolved,
			})

			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("validate error = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}
