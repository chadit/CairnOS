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
	classPermanent = "permanent"
	actionFlaky    = "flaky"
	actionCalc     = "calc"
	paramFn        = "fn"
	paramFailClass = "fail_class"
	paramFailTimes = "fail_times"
	paramDelayMs   = "delay_ms"
	paramCostUSD   = "cost_usd"

	classRateLimited = "rate_limited"

	// fnDouble and fnBroken are the local functions the local_fn cases register.
	fnDouble = "double"
	fnBroken = "broken"
)

func TestMockReturnsConfiguredOutputAndCost(t *testing.T) {
	t.Parallel()

	mock := builtins.NewMock()
	task := &runner.Task{
		RunID:      testRunID,
		ActionName: "a",
		Params:     map[string]any{"output": map[string]any{"count": int64(3)}, paramCostUSD: 0.25},
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

// TestMockNullFailClassSucceeds proves a null fail_class, which Validate reads
// as absent, does not fail the action as an unnamed permanent class.
func TestMockNullFailClassSucceeds(t *testing.T) {
	t.Parallel()

	task := &runner.Task{RunID: testRunID, ActionName: "a", Params: map[string]any{paramFailClass: nil}, Resolved: true}

	if _, err := builtins.NewMock().Execute(t.Context(), task); err != nil {
		t.Fatalf("execute: %v, want success", err)
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
		{name: "permanent failure", failClass: classPermanent, wantClass: runner.ClassPermanent},
		{name: "rate limited", failClass: classRateLimited, wantClass: runner.ClassRateLimited},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mock := builtins.NewMock()
			task := &runner.Task{
				RunID:      testRunID,
				ActionName: tt.name,
				Params:     map[string]any{paramFailClass: tt.failClass},
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
		Params:     map[string]any{paramFailClass: classTransient, paramFailTimes: int64(2)},
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
		Params:     map[string]any{paramDelayMs: int64(10_000)},
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
		fnDouble: func(_ context.Context, task *runner.Task) (any, error) {
			num, _ := task.Params["n"].(int64)

			return num * 2, nil
		},
	}

	localFn := builtins.NewLocalFn(funcs)
	task := &runner.Task{
		RunID:      testRunID,
		ActionName: actionCalc,
		Params:     map[string]any{paramFn: fnDouble, "n": int64(21)},
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
		Params:     map[string]any{paramFn: unresolved},
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
		fnBroken: func(_ context.Context, _ *runner.Task) (any, error) {
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
		Params: map[string]any{paramFn: fnBroken}, Resolved: true,
	})

	permanentErr, isWrapped := errors.AsType[*runner.Error](err)
	if !isWrapped || permanentErr.Class != runner.ClassPermanent {
		t.Fatalf("plain error not marked permanent: %v", err)
	}
}

func TestRegistryLifecycle(t *testing.T) {
	t.Parallel()

	registry := runner.NewRegistry()

	if err := registry.Register(builtins.MockName, builtins.MockFactory()); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := registry.Register(builtins.MockName, builtins.MockFactory()); !errors.Is(err, runner.ErrDuplicateRunner) {
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

// TestMockValidateChecksKnownParamShapes covers both validation passes: a
// load-time reference string must pass, and a resolved param of the wrong
// shape must fail with the param named in the code.
func TestMockValidateChecksKnownParamShapes(t *testing.T) {
	t.Parallel()

	codeBadFailClass := builtins.MockName + ".bad_fail_class"

	cases := []struct {
		params   map[string]any
		name     string
		wantCode string
		resolved bool
	}{
		{name: "no params", params: map[string]any{}, resolved: true},
		{name: "unknown param is ignored", params: map[string]any{"volume": 1}, resolved: true},
		{
			name:     "every known param well formed",
			params:   map[string]any{paramFailClass: classRateLimited, paramFailTimes: int64(2), paramDelayMs: 5.0, paramCostUSD: 0.25},
			resolved: true,
		},
		{
			name:     "null params read as absent",
			params:   map[string]any{paramFailClass: nil, paramFailTimes: nil, paramDelayMs: nil, paramCostUSD: nil},
			resolved: true,
		},
		{name: "load-time reference in a number param", params: map[string]any{paramDelayMs: "${inputs.delay}"}},
		{name: "load-time reference in fail_class", params: map[string]any{paramFailClass: "${inputs.class}"}},
		{
			name:     "resolved delay is not a number",
			params:   map[string]any{paramDelayMs: "shortly"},
			resolved: true,
			wantCode: builtins.MockName + ".bad_delay_ms",
		},
		{
			name:     "resolved fail_times is not a number",
			params:   map[string]any{paramFailClass: classTransient, paramFailTimes: "twice"},
			resolved: true,
			wantCode: builtins.MockName + ".bad_fail_times",
		},
		{
			name:     "resolved cost is not a number",
			params:   map[string]any{paramCostUSD: true},
			resolved: true,
			wantCode: builtins.MockName + ".bad_cost_usd",
		},
		{
			name:     "fail_class is not a string",
			params:   map[string]any{paramFailClass: 3},
			wantCode: codeBadFailClass,
		},
		{
			name:     "resolved fail_class names no class",
			params:   map[string]any{paramFailClass: "fatal"},
			resolved: true,
			wantCode: codeBadFailClass,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := builtins.NewMock().Validate(&runner.Task{Params: tt.params, Resolved: tt.resolved})
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("Validate: %v, want nil", err)
				}

				return
			}

			assertCodeError(t, err, tt.wantCode)
		})
	}
}
