package builtins

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chadit/CairnOS/pkg/runner"
)

// LocalFnName is the registry name of the in-process function runner.
const LocalFnName = "local_fn"

// Func is one registered Go function callable from a workflow. The bridge for
// in-process logic: scoring, parsing, glue that needs no external service.
type Func func(ctx context.Context, task *runner.Task) (any, error)

// LocalFn calls a registered Go function by name. Functions are supplied at
// wiring time (not from config, which cannot carry code), so the instance
// holds its own table and no package state exists.
type LocalFn struct {
	funcs map[string]Func
}

// NewLocalFn builds the runner over a function table.
func NewLocalFn(funcs map[string]Func) *LocalFn {
	return &LocalFn{funcs: funcs}
}

// LocalFnFactory adapts a function table to the registry contract.
func LocalFnFactory(funcs map[string]Func) runner.Factory {
	return func(_ map[string]any) (runner.Runner, error) {
		return NewLocalFn(funcs), nil
	}
}

// Name implements runner.Runner.
func (*LocalFn) Name() string {
	return LocalFnName
}

// Validate implements runner.Runner: the fn param must name a registered
// function. The check runs even at load time because fn is a literal in
// practice; an unresolved reference in fn defers the check to launch.
func (l *LocalFn) Validate(task *runner.Task) error {
	name, exists := task.Params["fn"]
	if !exists {
		return taskError(runner.ClassPermanent, "local_fn.missing_fn", "params must include fn")
	}

	text, isString := name.(string)
	if !isString {
		return paramError(LocalFnName, "fn", "must be a string")
	}

	if !task.Resolved && containsRefDelimiter(text) {
		return nil
	}

	if _, known := l.funcs[text]; !known {
		return taskError(runner.ClassPermanent, "local_fn.unknown",
			"no registered function %q", text)
	}

	return nil
}

// Execute implements runner.Runner.
func (l *LocalFn) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := l.Validate(task); err != nil {
		return runner.Result{}, err
	}

	name, isString := task.Params["fn"].(string)
	if !isString {
		return runner.Result{}, paramError(LocalFnName, "fn", "must be a string")
	}

	fn := l.funcs[name]

	output, err := fn(ctx, task)
	if err != nil {
		return runner.Result{}, wrapFnError(name, err)
	}

	return runner.Result{Output: output}, nil
}

// wrapFnError returns the runner error already in the chain unchanged and
// marks everything else permanent, matching the contract's safe default:
// never retry a side effect the function did not declare safe.
func wrapFnError(name string, err error) error {
	if classified, isClassified := errors.AsType[*runner.Error](err); isClassified {
		return classified
	}

	return &runner.Error{
		Class:   runner.ClassPermanent,
		Code:    "local_fn.failed",
		Message: fmt.Sprintf("function %q failed", name),
		Cause:   err,
	}
}

func containsRefDelimiter(text string) bool {
	return strings.Contains(text, "${{")
}
