package builtins_test

import (
	"errors"
	"maps"
	"reflect"
	"sync"
	"testing"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Mirrors of the runner's params key and stable error codes. The production
// copies are unexported, so the external package restates them here; matching
// them locks the codes callers depend on.
const (
	scriptKey  = "script"
	codeAction = "transform"

	keyName   = "name"
	keyActive = "active"

	wantFailed        = "code.failed"
	wantBadOutput     = "code.bad_output"
	wantBadParams     = "code.bad_params"
	wantSyntax        = "code.syntax"
	wantMissingScript = "code.missing_script"
	wantBadScript     = "code.bad_script"

	scriptNoop = "output = None"
)

// codeTask builds a resolved task for the code runner, folding script and any
// extra params into one table.
func codeTask(script string, params, inputs map[string]any) *runner.Task {
	full := map[string]any{scriptKey: script}
	maps.Copy(full, params)

	return &runner.Task{
		RunID:      testRunID,
		ActionName: codeAction,
		Params:     full,
		Inputs:     inputs,
		Resolved:   true,
	}
}

// assertCodeError fails unless err is a permanent runner.Error carrying code.
func assertCodeError(t *testing.T, err error, code string) {
	t.Helper()

	classified, ok := errors.AsType[*runner.Error](err)
	if !ok {
		t.Fatalf("error %v is not a runner.Error", err)
	}

	if classified.Code != code {
		t.Fatalf("code = %q, want %q", classified.Code, code)
	}

	if classified.Class != runner.ClassPermanent {
		t.Fatalf("class = %v, want permanent", classified.Class)
	}
}

func TestCodeFiltersAListLikeTheNodeExample(t *testing.T) {
	t.Parallel()

	script := `output = [item["name"] for item in params["items"] if item["active"]]`
	items := []any{
		map[string]any{keyName: "alice", keyActive: true},
		map[string]any{keyName: "bob", keyActive: false},
		map[string]any{keyName: "carol", keyActive: true},
	}

	result, err := builtins.NewCode().Execute(t.Context(), codeTask(script, map[string]any{"items": items}, nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	want := []any{"alice", "carol"}
	if !reflect.DeepEqual(result.Output, want) {
		t.Fatalf("output = %#v, want %#v", result.Output, want)
	}
}

func TestCodeSeesParamsAndInputs(t *testing.T) {
	t.Parallel()

	script := `output = {"p": params["x"], "i": inputs["y"]}`

	result, err := builtins.NewCode().Execute(
		t.Context(),
		codeTask(script, map[string]any{"x": "from-params"}, map[string]any{"y": "from-inputs"}),
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	want := map[string]any{"p": "from-params", "i": "from-inputs"}
	if !reflect.DeepEqual(result.Output, want) {
		t.Fatalf("output = %#v, want %#v", result.Output, want)
	}
}

func TestCodeCannotReadItsOwnScript(t *testing.T) {
	t.Parallel()

	// script is dropped from the params dict the runner exposes, so a lookup
	// returns None rather than the source text.
	result, err := builtins.NewCode().Execute(t.Context(), codeTask(`output = params.get("script")`, nil, nil))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.Output != nil {
		t.Fatalf("output = %#v, want nil", result.Output)
	}
}

func TestCodeFrozenDictsRejectMutation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		script string
	}{
		{name: "params frozen", script: "params[\"new\"] = 1\n" + scriptNoop},
		{name: "inputs frozen", script: "inputs[\"new\"] = 1\n" + scriptNoop},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := builtins.NewCode().Execute(t.Context(), codeTask(tt.script, nil, nil))
			assertCodeError(t, err, wantFailed)
		})
	}
}

func TestCodeValidateStructural(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{name: "missing script", params: map[string]any{}, want: wantMissingScript},
		{name: "non-string script", params: map[string]any{scriptKey: 42}, want: wantBadScript},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			task := &runner.Task{RunID: testRunID, ActionName: codeAction, Params: tt.params, Resolved: true}

			err := builtins.NewCode().Validate(task)
			assertCodeError(t, err, tt.want)
		})
	}
}

func TestCodeValidateSyntaxIsTwoPhase(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		script   string
		resolved bool
		wantErr  bool
	}{
		{name: "resolved syntax error flags", script: "output = (", resolved: true, wantErr: true},
		{name: "literal syntax error flags at load", script: "output = (", resolved: false, wantErr: true},
		{name: "unresolved reference defers", script: `output = ${{ fetch.output }}`, resolved: false, wantErr: false},
		{name: "valid resolved passes", script: "output = 1", resolved: true, wantErr: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			task := &runner.Task{
				RunID:      testRunID,
				ActionName: codeAction,
				Params:     map[string]any{scriptKey: tt.script},
				Resolved:   tt.resolved,
			}

			err := builtins.NewCode().Validate(task)
			if tt.wantErr {
				assertCodeError(t, err, wantSyntax)

				return
			}

			if err != nil {
				t.Fatalf("validate = %v, want nil", err)
			}
		})
	}
}

func TestCodeRuntimeErrorsArePermanentFailed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		script string
	}{
		{name: "undefined name", script: "output = missing_name"},
		{name: "explicit fail", script: `fail("boom")`},
		{name: "type error", script: `output = 1 + "x"`},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := builtins.NewCode().Execute(t.Context(), codeTask(tt.script, nil, nil))
			assertCodeError(t, err, wantFailed)
		})
	}
}

func TestCodeSandboxNamesAreUndefined(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		script string
	}{
		{name: "load is disabled", script: `load("mod.star", "helper")`},
		{name: "open is undefined", script: `output = open("secret.txt")`},
		{name: "os is undefined", script: `output = os.getcwd()`},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := builtins.NewCode().Execute(t.Context(), codeTask(tt.script, nil, nil))
			assertCodeError(t, err, wantFailed)
		})
	}
}

func TestCodeMissingOutputGlobalFails(t *testing.T) {
	t.Parallel()

	_, err := builtins.NewCode().Execute(t.Context(), codeTask("result = 1", nil, nil))
	assertCodeError(t, err, wantBadOutput)
}

func TestCodeConcurrentExecuteIsRaceFree(t *testing.T) {
	t.Parallel()

	code := builtins.NewCode()

	const workers = 8

	var wg sync.WaitGroup

	wg.Add(workers)

	outs := make([]any, workers)
	errs := make([]error, workers)

	for worker := range workers {
		go func() {
			defer wg.Done()

			task := codeTask(`output = params["n"] * 2`, map[string]any{"n": int64(worker)}, nil)

			result, err := code.Execute(t.Context(), task)
			outs[worker] = result.Output
			errs[worker] = err
		}()
	}

	wg.Wait()

	for worker := range workers {
		if errs[worker] != nil {
			t.Fatalf("worker %d: %v", worker, errs[worker])
		}

		want := int64(worker * 2)
		if outs[worker] != want {
			t.Fatalf("worker %d output = %#v, want %d", worker, outs[worker], want)
		}
	}
}
