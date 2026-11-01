package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/chadit/CairnOS/pkg/runner"
)

// CodeName is the registry name of the inline Starlark runner: an action with
// runner = "code" runs an operator-authored script for transform and glue logic
// without standing up a service.
const CodeName = "code"

// paramScript is the required params key holding the script text. It is dropped
// from the params dict the script sees, so a script cannot read its own source.
const paramScript = "script"

// Predeclared global names the script reads, and the global it must assign.
const (
	predeclaredParams = "params"
	predeclaredInputs = "inputs"
	outputGlobal      = "output"
)

// Config keys for the [runner.code] block. cfgKeyTimeout is shared with http
// (wall-clock backstop, seconds); cfgKeyMaxSteps bounds Starlark computation in
// the interpreter's weighted steps, described at defaultCodeMaxSteps.
const cfgKeyMaxSteps = "max_steps"

// Caps. Every one is a backstop; the engine's action and workflow deadlines are
// normally tighter than the wall clock, and a well-behaved script never nears
// the others.
const (
	// defaultCodeTimeout backstops a run when ctx carries no tighter deadline.
	defaultCodeTimeout = 10 * time.Second

	// defaultCodeMaxSteps bounds Starlark computation per run in weighted
	// steps. Since go.starlark.net v0.0.0-20261005163335 the interpreter
	// charges 10 per loop back edge, function entry, and call return, not one
	// per instruction. Over six loop-heavy scripts the weighted count averaged
	// 1.02 times the old count, so the value stayed put. A release that
	// reweights steps again fails TestCodeDefaultStepBudgetBoundary.
	defaultCodeMaxSteps = 500000

	// maxScriptBytes caps the script text at 64 KiB, checked in both Validate
	// phases because interpolation can grow a script between load and launch.
	maxScriptBytes = 64 << 10

	// maxPrintCapture caps print() output at 8 KiB; overflow ends with a
	// one-time truncation marker instead of unbounded growth.
	maxPrintCapture = 8 << 10
)

// printTruncatedMarker terminates a print capture that hit maxPrintCapture.
const printTruncatedMarker = "[print truncated]"

// Meta keys: the print capture (when non-empty) and the weighted step count the
// run spent against max_steps.
const (
	metaPrint = "print"
	metaSteps = "steps"
)

// Stable error codes. requireStringParam emits code.missing_script and
// code.bad_script; the rest are raised here. Every code is ClassPermanent:
// same script, same inputs, same result, so a retry cannot help.
const (
	codeScriptTooLarge = CodeName + ".script_too_large"
	codeSyntax         = CodeName + ".syntax"
	codeBadParams      = CodeName + ".bad_params"
	codeFailed         = CodeName + ".failed"
	codeTimeout        = CodeName + ".timeout"
	codeTooManySteps   = CodeName + ".too_many_steps"
	codeBadOutput      = CodeName + ".bad_output"
	codeOutputTooLarge = CodeName + ".output_too_large"
)

// codeFileOptions returns the parser and resolver options shared by the
// Validate syntax check and the Execute run, so a script that parses clean at
// load runs under the same rules at launch. The zero value is Starlark's
// default: no set builtin, no top-level control flow, no global reassignment.
func codeFileOptions() *syntax.FileOptions {
	return &syntax.FileOptions{}
}

// Code runs a Starlark script inline. The sandbox is the language itself:
// Thread.Load stays nil so load() fails, and nothing outside the Starlark
// universe is predeclared, so open, os, and http are undefined names.
//
// Every Execute builds a fresh thread and fresh frozen globals, so the type is
// safe for concurrent use.
type Code struct {
	timeout  time.Duration
	maxSteps uint64
}

// NewCode builds the runner with the default caps. The factory overrides them
// from the [runner.code] config block.
func NewCode() *Code {
	return &Code{
		timeout:  defaultCodeTimeout,
		maxSteps: defaultCodeMaxSteps,
	}
}

// CodeFactory adapts codeFromConfig to the registry contract.
func CodeFactory() runner.Factory {
	return asFactory(codeFromConfig)
}

// codeFromConfig builds the runner with the two caps from the [runner.code]
// block. A present key of the wrong type, or a non-positive max_steps, is a
// build error so a config typo cannot silently disable a cap.
func codeFromConfig(cfg map[string]any) (*Code, error) {
	code := NewCode()

	timeout, hasTimeout, err := configInt(cfg, cfgKeyTimeout)
	if err != nil {
		return nil, &configError{runner: CodeName, err: err}
	}

	if hasTimeout && timeout > 0 {
		code.timeout = time.Duration(timeout) * time.Second
	}

	maxSteps, hasSteps, err := configInt(cfg, cfgKeyMaxSteps)
	if err != nil {
		return nil, &configError{runner: CodeName, err: err}
	}

	if hasSteps {
		if maxSteps <= 0 {
			return nil, &configError{runner: CodeName, err: fmt.Errorf("%w: %s must be positive", ErrConfigValue, cfgKeyMaxSteps)}
		}

		code.maxSteps = uint64(maxSteps)
	}

	return code, nil
}

// Name implements runner.Runner.
func (*Code) Name() string {
	return CodeName
}

// Validate implements runner.Runner in two phases. The structural check runs in
// both. The syntax check runs once params resolve, or when the literal script
// carries no interpolation delimiter, so an author hears about a typo'd script
// at workflow load; a script still holding an unresolved reference defers the
// parse to launch (pkg/runner's two-phase contract).
func (*Code) Validate(task *runner.Task) error {
	if err := requireStringParam(task, paramScript, CodeName); err != nil {
		return err
	}

	script, _ := task.Params[paramScript].(string)

	// The size cap runs in both phases: interpolation can grow a script between
	// load and launch, so a launch-time check catches a reference that expands
	// past the cap even when the literal passed at load.
	if len(script) > maxScriptBytes {
		return newCodeError(codeScriptTooLarge,
			fmt.Sprintf("script is %d bytes, over the %d cap", len(script), maxScriptBytes), nil)
	}

	if !task.Resolved && containsRefDelimiter(script) {
		return nil
	}

	if _, err := codeFileOptions().Parse(scriptName(task), script, 0); err != nil {
		return newCodeError(codeSyntax, "script has a syntax error: "+err.Error(), err)
	}

	return nil
}

// Execute implements runner.Runner. It builds two frozen predeclared dicts,
// runs the script on a fresh thread, and converts the output global back to
// JSON-shaped Go values so downstream references consume it unchanged.
func (c *Code) Execute(ctx context.Context, task *runner.Task) (runner.Result, error) {
	if err := c.Validate(task); err != nil {
		return runner.Result{}, err
	}

	// Validate guarantees a string; the launch pass has already resolved it.
	script, _ := task.Params[paramScript].(string)

	predeclared, err := buildPredeclared(task)
	if err != nil {
		return runner.Result{}, err
	}

	// The backstop deadline catches a script that the engine's ctx left
	// unbounded; a tighter action or workflow deadline still wins.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var printed printCapture

	thread := &starlark.Thread{
		Name:  task.RunID + "/" + task.ActionName,
		Print: func(_ *starlark.Thread, msg string) { printed.add(msg) },
	}
	thread.SetMaxExecutionSteps(c.maxSteps)

	// Starlark evaluation is not ctx-aware, so bridge a canceled ctx to
	// thread.Cancel across goroutines. Either deadline fires this without a
	// polling watchdog.
	stop := context.AfterFunc(ctx, func() { thread.Cancel("wall clock cap") })
	defer stop()

	start := time.Now()

	globals, execErr := starlark.ExecFileOptions(codeFileOptions(), thread, scriptName(task), script, predeclared)

	elapsed := time.Since(start)

	if execErr != nil {
		return runner.Result{}, c.classifyExecError(ctx, thread, execErr)
	}

	output, err := readOutput(globals)
	if err != nil {
		return runner.Result{}, err
	}

	if err := checkOutputSize(output); err != nil {
		return runner.Result{}, err
	}

	return runner.Result{
		Output: output,
		Usage:  runner.Usage{Latency: elapsed},
		Meta:   buildMeta(thread, &printed),
	}, nil
}

// classifyExecError maps a Starlark eval error onto a stable code. It compares
// the step counter against the cap first and the ctx state second, per the
// order in the spec, rather than matching Starlark's cancellation text: both
// cap paths surface the same cancellation message from the library, and that
// string is not ours to depend on across upgrades.
func (c *Code) classifyExecError(ctx context.Context, thread *starlark.Thread, execErr error) *runner.Error {
	if thread.ExecutionSteps() >= c.maxSteps {
		return newCodeError(codeTooManySteps, "script exceeded the step budget: "+execErr.Error(), execErr)
	}

	if ctx.Err() != nil {
		return newCodeError(codeTimeout, "script hit the wall-clock cap: "+execErr.Error(), execErr)
	}

	return newCodeError(codeFailed, "script failed: "+execErr.Error(), execErr)
}

// checkOutputSize marshals the converted output and rejects it when the JSON
// form overflows maxCapturedOutput, so a script cannot balloon run storage. The
// bytes are discarded; the persisted value stays the converted Go value.
func checkOutputSize(output any) error {
	encoded, err := json.Marshal(output)
	if err != nil {
		return newCodeError(codeBadOutput, "output is not json-encodable: "+err.Error(), err)
	}

	if len(encoded) > maxCapturedOutput {
		return newCodeError(codeOutputTooLarge,
			fmt.Sprintf("output is %d bytes, over the %d cap", len(encoded), maxCapturedOutput), nil)
	}

	return nil
}

// buildMeta reports the executed step count always, and the print capture only
// when a script printed something.
func buildMeta(thread *starlark.Thread, printed *printCapture) map[string]string {
	meta := map[string]string{
		metaSteps: strconv.FormatUint(thread.ExecutionSteps(), 10),
	}

	if text := printed.buf.String(); text != "" {
		meta[metaPrint] = text
	}

	return meta
}

// printCapture accumulates print() output up to maxPrintCapture bytes, then
// appends a one-time marker and drops the rest. Starlark invokes Print on the
// single evaluating goroutine, so the buffer needs no lock.
type printCapture struct {
	buf       strings.Builder
	truncated bool
}

// add appends one newline-terminated print message until the cap is reached.
func (p *printCapture) add(msg string) {
	if p.truncated {
		return
	}

	line := msg + "\n"

	room := maxPrintCapture - p.buf.Len()
	if len(line) <= room {
		p.buf.WriteString(line)

		return
	}

	if room > 0 {
		p.buf.WriteString(line[:room])
	}

	p.buf.WriteString(printTruncatedMarker)
	p.truncated = true
}

// buildPredeclared converts params (minus script) and inputs into two Starlark
// dicts and freezes them, so a script mutating either fails with a Starlark
// frozen-value error rather than leaking state between runs.
func buildPredeclared(task *runner.Task) (starlark.StringDict, error) {
	paramsDict, err := toStarlarkDict(paramsWithoutScript(task.Params))
	if err != nil {
		return nil, newCodeError(codeBadParams, "params: "+err.Error(), err)
	}

	paramsDict.Freeze()

	inputsDict, err := toStarlarkDict(task.Inputs)
	if err != nil {
		return nil, newCodeError(codeBadParams, "inputs: "+err.Error(), err)
	}

	inputsDict.Freeze()

	return starlark.StringDict{
		predeclaredParams: paramsDict,
		predeclaredInputs: inputsDict,
	}, nil
}

// readOutput pulls the output global and converts it to a Go value. A missing
// global, a non-data value, a non-string dict key, or an int outside int64 all
// fail as code.bad_output.
func readOutput(globals starlark.StringDict) (any, error) {
	value, assigned := globals[outputGlobal]
	if !assigned {
		return nil, newCodeError(codeBadOutput, "script must assign a global named output", nil)
	}

	converted, err := fromStarlarkValue(value)
	if err != nil {
		return nil, newCodeError(codeBadOutput, err.Error(), err)
	}

	return converted, nil
}

// paramsWithoutScript copies the params table minus the script key, so the
// script cannot read its own source through params.
func paramsWithoutScript(params map[string]any) map[string]any {
	copied := make(map[string]any, len(params))

	for key, value := range params {
		if key == paramScript {
			continue
		}

		copied[key] = value
	}

	return copied
}

// scriptName is the filename the parser and evaluator report in error
// positions, tied to the action so a backtrace names its origin.
func scriptName(task *runner.Task) string {
	return task.ActionName + ".star"
}

// newCodeError builds a permanent classified error. All code runner failures
// are permanent: a retry spends the same budget on the same script and inputs
// for the same result.
func newCodeError(code, message string, cause error) *runner.Error {
	return &runner.Error{
		Class:   runner.ClassPermanent,
		Code:    code,
		Message: message,
		Cause:   cause,
	}
}
