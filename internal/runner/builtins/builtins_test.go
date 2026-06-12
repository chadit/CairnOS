package builtins_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Shared fixture names; goconst applies to tests in this repo's lint policy.
const (
	testRunID      = "run1"
	classTransient = "transient"
	actionFlaky    = "flaky"
	actionCalc     = "calc"
	paramFn        = "fn"
)

func TestMockReturnsConfiguredOutputAndCost(t *testing.T) {
	t.Parallel()

	mock := builtins.NewMock()
	task := &runner.Task{
		RunID:      testRunID,
		ActionName: "a",
		Params:     map[string]any{"output": map[string]any{"count": int64(3)}, "cost_usd": 0.25},
		Resolved:   true,
	}

	result, err := mock.Execute(t.Context(), task)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	output, ok := result.Output.(map[string]any)
	if !ok || output["count"] != int64(3) {
		t.Fatalf("output = %#v", result.Output)
	}

	if result.Usage.CostUSD != 0.25 {
		t.Fatalf("cost = %v, want 0.25", result.Usage.CostUSD)
	}
}

func TestMockFailsWithConfiguredClass(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		failClass string
		wantClass runner.ErrorClass
	}{
		{name: "transient failure", failClass: classTransient, wantClass: runner.ClassTransient},
		{name: "permanent", failClass: "permanent", wantClass: runner.ClassPermanent},
		{name: "rate limited", failClass: "rate_limited", wantClass: runner.ClassRateLimited},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mock := builtins.NewMock()
			task := &runner.Task{
				RunID:      testRunID,
				ActionName: tt.name,
				Params:     map[string]any{"fail_class": tt.failClass},
			}

			_, err := mock.Execute(t.Context(), task)

			classified, ok := errors.AsType[*runner.Error](err)
			if !ok {
				t.Fatalf("error %v is not a runner.Error", err)
			}

			if classified.Class != tt.wantClass {
				t.Fatalf("class = %v, want %v", classified.Class, tt.wantClass)
			}
		})
	}
}

func TestMockFailTimesRecoversAfterN(t *testing.T) {
	t.Parallel()

	mock := builtins.NewMock()
	task := &runner.Task{
		RunID:      testRunID,
		ActionName: actionFlaky,
		Params:     map[string]any{"fail_class": classTransient, "fail_times": int64(2)},
	}

	for attempt := range 2 {
		if _, err := mock.Execute(t.Context(), task); err == nil {
			t.Fatalf("attempt %d succeeded, want failure", attempt)
		}
	}

	if _, err := mock.Execute(t.Context(), task); err != nil {
		t.Fatalf("third attempt failed: %v", err)
	}
}

func TestMockDelayHonorsCancellation(t *testing.T) {
	t.Parallel()

	mock := builtins.NewMock()
	task := &runner.Task{
		RunID:      testRunID,
		ActionName: "slow",
		Params:     map[string]any{"delay_ms": int64(10_000)},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()

	_, err := mock.Execute(ctx, task)
	if err == nil {
		t.Fatal("execute survived cancellation")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %v, want prompt return", elapsed)
	}
}

func TestLocalFnCallsRegisteredFunction(t *testing.T) {
	t.Parallel()

	funcs := map[string]builtins.Func{
		"double": func(_ context.Context, task *runner.Task) (any, error) {
			num, _ := task.Params["n"].(int64)

			return num * 2, nil
		},
	}

	localFn := builtins.NewLocalFn(funcs)
	task := &runner.Task{
		RunID:      testRunID,
		ActionName: actionCalc,
		Params:     map[string]any{paramFn: "double", "n": int64(21)},
		Resolved:   true,
	}

	result, err := localFn.Execute(t.Context(), task)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != int64(42) {
		t.Fatalf("output = %v, want 42", result.Output)
	}
}

func TestLocalFnRejectsUnknownFunction(t *testing.T) {
	t.Parallel()

	localFn := builtins.NewLocalFn(nil)
	task := &runner.Task{
		RunID:      testRunID,
		ActionName: actionCalc,
		Params:     map[string]any{paramFn: "ghost"},
		Resolved:   true,
	}

	err := localFn.Validate(task)

	classified, ok := errors.AsType[*runner.Error](err)
	if !ok || classified.Class != runner.ClassPermanent {
		t.Fatalf("error = %v, want permanent runner.Error", err)
	}
}

func TestLocalFnDefersUnresolvedFnReference(t *testing.T) {
	t.Parallel()

	localFn := builtins.NewLocalFn(nil)
	task := &runner.Task{
		RunID:      testRunID,
		ActionName: actionCalc,
		Params:     map[string]any{paramFn: "${{ pick.output }}"},
		Resolved:   false,
	}

	if err := localFn.Validate(task); err != nil {
		t.Fatalf("load-time validate rejected unresolved reference: %v", err)
	}
}

func TestLocalFnKeepsClassifiedErrors(t *testing.T) {
	t.Parallel()

	funcs := map[string]builtins.Func{
		actionFlaky: func(_ context.Context, _ *runner.Task) (any, error) {
			return nil, runner.NewError(runner.ClassTransient, "flaky.blip", "try again")
		},
		"broken": func(_ context.Context, _ *runner.Task) (any, error) {
			return nil, errors.New("plain failure")
		},
	}

	localFn := builtins.NewLocalFn(funcs)

	_, err := localFn.Execute(t.Context(), &runner.Task{
		RunID: "r", ActionName: "a",
		Params: map[string]any{paramFn: actionFlaky}, Resolved: true,
	})

	transientErr, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified || transientErr.Class != runner.ClassTransient {
		t.Fatalf("classified error lost its class: %v", err)
	}

	_, err = localFn.Execute(t.Context(), &runner.Task{
		RunID: "r", ActionName: "b",
		Params: map[string]any{paramFn: "broken"}, Resolved: true,
	})

	permanentErr, isWrapped := errors.AsType[*runner.Error](err)
	if !isWrapped || permanentErr.Class != runner.ClassPermanent {
		t.Fatalf("plain error not marked permanent: %v", err)
	}
}

func TestRegistryLifecycle(t *testing.T) {
	t.Parallel()

	registry := runner.NewRegistry()

	if err := registry.Register(builtins.MockName, builtins.MockFactory); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := registry.Register(builtins.MockName, builtins.MockFactory); !errors.Is(err, runner.ErrDuplicateRunner) {
		t.Fatalf("duplicate register error = %v, want ErrDuplicateRunner", err)
	}

	if err := registry.BuildAll(nil); err != nil {
		t.Fatalf("build all: %v", err)
	}

	if _, ok := registry.Get(builtins.MockName); !ok {
		t.Fatal("built mock not found")
	}

	if _, ok := registry.Get("ghost"); ok {
		t.Fatal("ghost runner found")
	}

	if err := registry.Build("ghost", nil); !errors.Is(err, runner.ErrUnknownRunner) {
		t.Fatalf("build ghost error = %v, want ErrUnknownRunner", err)
	}

	names := registry.List()
	if len(names) != 1 || names[0] != builtins.MockName {
		t.Fatalf("list = %v", names)
	}

	if err := registry.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
