package builtins_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture names for the Phase 1a runner tests.
const (
	promptKey  = "prompt"
	binaryKey  = "binary"
	argsKey    = "args"
	stdinKey   = "stdin"
	testPrompt = "hello cairn"
)

// writeShim drops an executable script into dir and returns its path. Tests
// point a runner's binary at it instead of any real CLI.
func writeShim(t *testing.T, dir, name, script string) string {
	t.Helper()

	path := filepath.Join(dir, name)

	// #nosec G306 -- the shim must be executable; it lives in t.TempDir.
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatalf("write shim: %v", err)
	}

	return path
}

func cliTask() *runner.Task {
	return &runner.Task{
		RunID:      testRunID,
		ActionName: "cli",
		Params:     map[string]any{promptKey: testPrompt},
		Resolved:   true,
	}
}

func TestCLICapturesOutputAndExitCode(t *testing.T) {
	t.Parallel()

	shim := writeShim(t, t.TempDir(), "echoer", `echo "got: $1"
echo "warn" >&2`)

	cli, err := builtins.NewCLI(builtins.CodexName, shim, nil)
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

	if result.Meta["stderr_tail"] != "warn" {
		t.Fatalf("stderr_tail = %q, want warn", result.Meta["stderr_tail"])
	}

	if result.Usage.Latency <= 0 {
		t.Fatalf("latency = %v, want > 0", result.Usage.Latency)
	}
}

func TestCLIClassifiesFailures(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	failing := writeShim(t, tmpDir, "failing", `echo "boom" >&2
exit 1`)

	cases := []struct {
		name      string
		binary    string
		wantClass runner.ErrorClass
		wantCode  string
	}{
		{name: "non-zero exit is permanent", binary: failing, wantClass: runner.ClassPermanent, wantCode: "codex.exit"},
		{name: "missing binary is permanent", binary: filepath.Join(tmpDir, "missing"), wantClass: runner.ClassPermanent, wantCode: "codex.not_found"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cli, err := builtins.NewCLI(builtins.CodexName, tt.binary, nil)
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

func TestCLIClassifiesReauthAndRateLimit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	reauth := writeShim(t, tmpDir, "reauth", `echo "error: invalid_grant" >&2
exit 1`)
	throttled := writeShim(t, tmpDir, "throttled", `echo "429 too many requests; exceeded retry limit" >&2
exit 1`)

	cases := []struct {
		name      string
		binary    string
		wantClass runner.ErrorClass
		wantCode  string
	}{
		{name: "invalid_grant is needs-reauth", binary: reauth, wantClass: runner.ClassNeedsReauth, wantCode: "codex.needs_reauth"},
		{name: "rate-limit text wins over reauth", binary: throttled, wantClass: runner.ClassRateLimited, wantCode: "codex.rate_limited"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cli, err := builtins.NewCLI(builtins.CodexName, tt.binary, nil)
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
	t.Parallel()

	// The exec keeps sleep from surviving as an orphan that holds the
	// stdout pipe open past the kill.
	sleeper := writeShim(t, t.TempDir(), "sleeper", "exec sleep 30")

	cli, err := builtins.NewCLI(builtins.QwenName, sleeper, nil)
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	task := cliTask()
	task.Deadline = time.Now().Add(50 * time.Millisecond)

	start := time.Now()

	_, err = cli.Execute(t.Context(), task)

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified || classified.Class != runner.ClassTransient || classified.Code != "qwen.timeout" {
		t.Fatalf("error = %v, want transient qwen.timeout", err)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("kill took %v, want prompt return", elapsed)
	}
}

func TestCLIStreamsChunks(t *testing.T) {
	t.Parallel()

	multi := writeShim(t, t.TempDir(), "multi", `echo "line one"
echo "line two"`)

	cli, err := builtins.NewCLI(builtins.OMPName, multi, nil)
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	out := make(chan runner.Chunk, 16)

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
	t.Parallel()

	catShim := writeShim(t, t.TempDir(), "catter", "cat")

	cli, err := builtins.NewCLI(builtins.AntigravityName, catShim, map[string]any{stdinKey: true})
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
	t.Parallel()

	shim := writeShim(t, t.TempDir(), "override", `echo "$1|$2"`)

	cfg := map[string]any{
		binaryKey: shim,
		argsKey:   []any{"--prompt", "{prompt}"},
	}

	cli, err := builtins.NewCLI(builtins.CodexName, "definitely-not-on-path", cfg)
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
		name     string
		params   map[string]any
		resolved bool
		wantErr  bool
	}{
		{name: "missing prompt", params: map[string]any{}, resolved: true, wantErr: true},
		{name: "non-string prompt", params: map[string]any{promptKey: int64(7)}, resolved: true, wantErr: true},
		{name: "unresolved reference defers", params: map[string]any{promptKey: "${{ pick.output }}"}, resolved: false, wantErr: false},
		{name: "literal prompt", params: map[string]any{promptKey: testPrompt}, resolved: true, wantErr: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cli, err := builtins.NewCLI(builtins.CodexName, "codex", nil)
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
