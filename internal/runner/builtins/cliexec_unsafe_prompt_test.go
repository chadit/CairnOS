package builtins_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// dashedPromptKey and the dashed prompt value the M3 arg-injection tests drive.
const (
	allowDashedKey   = "allow_dashed_prompt"
	dashedPromptText = "--output=/etc/passwd"
)

// dashedPromptTask builds a CLI task whose prompt begins with a dash, the
// arg-injection vector M3 guards against.
func dashedPromptTask() *runner.Task {
	return &runner.Task{
		RunID:      testRunID,
		ActionName: "cli",
		Params:     map[string]any{promptKey: dashedPromptText},
		Resolved:   true,
	}
}

// assertUnsafePrompt fails unless err is the permanent unsafe_prompt the guard
// returns for a dashed prompt on a bare positional template.
func assertUnsafePrompt(t *testing.T, err error) {
	t.Helper()

	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		t.Fatalf("dashed-prompt error = %v, want a classified *runner.Error", err)
	}

	if classified.Class != runner.ClassPermanent {
		t.Fatalf("unsafe prompt class = %v, want permanent", classified.Class)
	}

	if !strings.HasSuffix(classified.Code, ".unsafe_prompt") {
		t.Fatalf("unsafe prompt code = %q, want a *.unsafe_prompt code", classified.Code)
	}
}

// TestCLIRefusesDashedPromptOnBarePositional proves the M3 guard: a prompt that
// begins with a dash is refused by default when the argv template feeds it as a
// bare positional, since the wrapped binary would read it as a flag.
func TestCLIRefusesDashedPromptOnBarePositional(t *testing.T) {
	t.Parallel()

	cli, err := builtins.NewCLI(builtins.CodexName, "codex", nil)
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	assertUnsafePrompt(t, cli.Validate(dashedPromptTask()))
}

// TestCLIAllowsDashedPromptWhenOptedIn proves the opt-out: with
// allow_dashed_prompt = true the guard is off and the dashed prompt validates.
func TestCLIAllowsDashedPromptWhenOptedIn(t *testing.T) {
	t.Parallel()

	cli, err := builtins.NewCLI(builtins.CodexName, "codex", map[string]any{allowDashedKey: true})
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	if err := cli.Validate(dashedPromptTask()); err != nil {
		t.Fatalf("opted-in dashed prompt validate: %v", err)
	}
}

// TestCLIAllowsDashedPromptAfterTerminator proves the recommended template: a
// {prompt} placed after a -- terminator stops flag parsing, so the dashed
// prompt validates without the opt-out.
func TestCLIAllowsDashedPromptAfterTerminator(t *testing.T) {
	t.Parallel()

	cfg := map[string]any{argsKey: []any{"--flag", "--", "{prompt}"}}

	cli, err := builtins.NewCLI(builtins.CodexName, "codex", cfg)
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	if err := cli.Validate(dashedPromptTask()); err != nil {
		t.Fatalf("terminator-template dashed prompt validate: %v", err)
	}
}

// TestCLIDashedPromptOnStdinIsSafe proves a stdin prompt is never argv, so the
// guard does not apply: a dashed prompt on stdin validates by default.
func TestCLIDashedPromptOnStdinIsSafe(t *testing.T) {
	t.Parallel()

	cli, err := builtins.NewCLI(builtins.CodexName, "codex", map[string]any{stdinKey: true})
	if err != nil {
		t.Fatalf("new cli: %v", err)
	}

	if err := cli.Validate(dashedPromptTask()); err != nil {
		t.Fatalf("stdin dashed prompt validate: %v", err)
	}
}
