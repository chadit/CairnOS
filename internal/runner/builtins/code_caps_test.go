package builtins_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Mirrors of the cap-related stable codes and config keys. The production
// copies are unexported, so the external package restates them; matching them
// locks the codes and keys callers depend on.
const (
	keyMaxSteps = "max_steps"
	keyTimeout  = "timeout"

	metaSteps      = "steps"
	metaPrint      = "print"
	truncateMarker = "[print truncated]"

	wantTimeout        = "code.timeout"
	wantTooManySteps   = "code.too_many_steps"
	wantScriptTooLarge = "code.script_too_large"
	wantOutputTooLarge = "code.output_too_large"
)

// buildCode builds a code runner through its factory, the same path the CLI
// wiring takes, so the tests exercise the real cap plumbing.
func buildCode(t *testing.T, cfg map[string]any) *builtins.Code {
	t.Helper()

	instance, err := builtins.CodeFactory()(cfg)
	if err != nil {
		t.Fatalf("build code runner: %v", err)
	}

	code, isCode := instance.(*builtins.Code)
	if !isCode {
		t.Fatalf("code factory built %T, want *builtins.Code", instance)
	}

	return code
}

func TestCodeWallClockCapCancelsInfiniteLoop(t *testing.T) {
	t.Parallel()

	// A step budget far above anything this loop can reach, so only the ctx
	// deadline stops it. range() is lazy, so the loop spins without a while.
	code := buildCode(t, map[string]any{keyMaxSteps: int64(1) << 40})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	// A for loop is legal inside a function, and range() is lazy, so this spins
	// long enough for the deadline to fire without allocating the range.
	script := "def spin():\n    total = 0\n    for i in range(10000000000):\n        total += i\n    return total\noutput = spin()"

	done := make(chan error, 1)

	go func() {
		_, err := code.Execute(ctx, codeTask(script, nil, nil))
		done <- err
	}()

	select {
	case err := <-done:
		assertCodeError(t, err, wantTimeout)
	case <-time.After(5 * time.Second):
		t.Fatal("execute did not return after the wall-clock cap fired")
	}
}

func TestCodeStepCapClassifiesByCount(t *testing.T) {
	t.Parallel()

	// A tiny step budget with a generous wall clock, so the loop trips the
	// step cap long before any deadline, proving classification is by count.
	code := buildCode(t, map[string]any{keyMaxSteps: int64(1000), keyTimeout: int64(30)})

	script := "def spin():\n    total = 0\n    for i in range(1000000):\n        total += i\n    return total\noutput = spin()"

	_, err := code.Execute(t.Context(), codeTask(script, nil, nil))
	assertCodeError(t, err, wantTooManySteps)
}

func TestCodeDefaultStepBudgetBoundary(t *testing.T) {
	t.Parallel()

	// Each iteration costs 30 weighted steps (the back edge plus step()'s entry
	// and return) and the run adds 40, so 16,500 iterations spend 99% of the
	// 500,000 default and 16,850 spend 101%. Per-instruction counting before
	// starlark v0.0.0-20261005163335 charged both runs about 270,000 and passed
	// them, so a release that reweights steps fails here instead of silently
	// moving every script's headroom.
	script := "def step(i):\n    return i % 7\n\ndef spin(n):\n    total = 0\n" +
		"    for i in range(n):\n        total += step(i)\n    return total\n\noutput = spin(params[\"n\"])"

	code := builtins.NewCode()

	if _, err := code.Execute(t.Context(), codeTask(script, map[string]any{"n": int64(16_500)}, nil)); err != nil {
		t.Fatalf("run at 99%% of the default budget: %v, want success", err)
	}

	_, err := code.Execute(t.Context(), codeTask(script, map[string]any{"n": int64(16_850)}, nil))
	assertCodeError(t, err, wantTooManySteps)
}

func TestCodeRejectsOversizedScript(t *testing.T) {
	t.Parallel()

	// A comment line past the 64 KiB cap; the body itself is valid Starlark.
	script := strings.Repeat("#", 70000) + "\noutput = 0"

	_, err := builtins.NewCode().Execute(t.Context(), codeTask(script, nil, nil))
	assertCodeError(t, err, wantScriptTooLarge)
}

func TestCodeRejectsOversizedOutput(t *testing.T) {
	t.Parallel()

	// String repetition builds a value whose JSON form clears 1 MiB in one
	// cheap step, so the size cap, not the step cap, is what rejects it.
	_, err := builtins.NewCode().Execute(t.Context(), codeTask(`output = "a" * 1100000`, nil, nil))
	assertCodeError(t, err, wantOutputTooLarge)
}

func TestCodePrintCaptureCapsAndMarks(t *testing.T) {
	t.Parallel()

	// Each print writes ~30 bytes; 2000 of them clear the 8 KiB capture cap.
	// The loop lives in a function because top-level for loops are disallowed.
	script := `def emit():
    for i in range(2000):
        print("print-capture-padding-line")
emit()
output = 1`

	result, err := builtins.NewCode().Execute(t.Context(), codeTask(script, nil, nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	captured := result.Meta[metaPrint]
	if !strings.Contains(captured, truncateMarker) {
		t.Fatalf("capture %q missing truncation marker", captured)
	}

	// Raw output was ~60 KiB; a capped capture stays far under that.
	if len(captured) > 9000 {
		t.Fatalf("capture is %d bytes, want it bounded near the 8 KiB cap", len(captured))
	}
}

func TestCodeMetaReportsStepCount(t *testing.T) {
	t.Parallel()

	result, err := builtins.NewCode().Execute(t.Context(), codeTask("output = 1 + 1", nil, nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	steps := result.Meta[metaSteps]
	if steps == "" || steps == "0" {
		t.Fatalf("steps meta = %q, want a positive count", steps)
	}
}

func TestCodeFactoryBuildsAndRejectsBadConfig(t *testing.T) {
	t.Parallel()

	if _, err := builtins.CodeFactory()(nil); err != nil {
		t.Fatalf("empty config build: %v", err)
	}

	cases := []struct {
		cfg  map[string]any
		name string
	}{
		{name: "max_steps wrong type", cfg: map[string]any{keyMaxSteps: "lots"}},
		{name: "timeout wrong type", cfg: map[string]any{keyTimeout: "soon"}},
		{name: "max_steps not positive", cfg: map[string]any{keyMaxSteps: int64(0)}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			built, err := builtins.CodeFactory()(tt.cfg)
			if !errors.Is(err, builtins.ErrConfigValue) {
				t.Fatalf("err = %v, want ErrConfigValue", err)
			}

			// A nil *Code boxed in the interface would compare non-nil here.
			if built != nil {
				t.Fatalf("failed build returned runner %#v, want nil", built)
			}
		})
	}
}

func TestCodeRegistersInBuiltRegistry(t *testing.T) {
	t.Parallel()

	registry := runner.NewRegistry()

	if err := registry.Register(builtins.CodeName, builtins.CodeFactory()); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := registry.BuildAll(nil); err != nil {
		t.Fatalf("build all: %v", err)
	}

	if !slices.Contains(registry.List(), builtins.CodeName) {
		t.Fatalf("List() = %v, want it to include %q", registry.List(), builtins.CodeName)
	}
}
