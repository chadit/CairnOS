package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Error-workflow fixtures. A failing parent names an error_workflow; the fake
// workflow source resolves the handler by name without touching the disk.
// handlerName is the error_workflow the fixtures name and the fake source
// resolves by; it matches the `name` inside the handler TOML above.
const handlerName = "on_failure"

// secondLevelName is the error_workflow the failing handler itself names, which
// must never fire because its caller is an error-triggered run.
const secondLevelName = "second_level"

const (
	failingParentTOML = `
[workflow]
name = "sync_crm"
error_workflow = "on_failure"

[action.boom]
runner = "mock"
params = { fail_class = "permanent" }
`

	succeedingParentTOML = `
[workflow]
name = "sync_crm"
error_workflow = "on_failure"

[action.work]
runner = "mock"
`

	// handlerTOML is a plain handler that succeeds, so a fired run lands
	// succeeded and the parent's failure is the only failure on record.
	handlerTOML = `
[workflow]
name = "on_failure"

[action.notify]
runner = "mock"
`

	// handlerDeclaringSubsetTOML declares three of the five failure-context
	// keys, so the fired run must receive exactly those and nothing else.
	handlerDeclaringSubsetTOML = `
[workflow]
name = "on_failure"

[workflow.inputs.run_id]
type = "string"
required = true

[workflow.inputs.failed_action]
type = "string"
required = true

[workflow.inputs.error_code]
type = "string"
required = true

[action.notify]
runner = "mock"
`

	// failingHandlerTOML fails, so the fire logs but the parent's terminal
	// state must stay its own.
	failingHandlerTOML = `
[workflow]
name = "on_failure"

[action.notify]
runner = "mock"
params = { fail_class = "permanent" }
`

	// handlerWithOwnHandlerTOML is itself an error handler that fails and names
	// its own error_workflow, proving an error-triggered run never fires one.
	handlerWithOwnHandlerTOML = `
[workflow]
name = "on_failure"
error_workflow = "second_level"

[action.notify]
runner = "mock"
params = { fail_class = "permanent" }
`

	secondLevelHandlerTOML = `
[workflow]
name = "second_level"

[action.notify]
runner = "mock"
`

	// slowFailWithHandlerTOML runs a slow action that a done parent context
	// interrupts, failing the run so the WithoutCancel fire can be observed.
	slowFailWithHandlerTOML = `
[workflow]
name = "sync_crm"
error_workflow = "on_failure"

[action.slow]
runner = "mock"
params = { delay_ms = 10000 }
`

	cancelWithHandlerTOML = `
[workflow]
name = "cancel_me"
error_workflow = "on_failure"

[action.stopper]
runner = "local_fn"
params = { fn = "cancel_run" }
`
)

// errorSourceConfig wires the fake workflow source with the named handlers so a
// failed run can resolve its error_workflow.
func errorSourceConfig(t *testing.T, handlers map[string]string) *engine.Config {
	t.Helper()

	cfg := baseConfig()
	cfg.Workflows = &fakeWorkflowSource{flows: parseFlows(t, handlers)}

	return cfg
}

func TestErrorWorkflowFiresOnceSynchronouslyOnFailure(t *testing.T) {
	t.Parallel()

	cfg := errorSourceConfig(t, map[string]string{handlerName: handlerTOML})
	eng, db := newTestEngine(t, cfg, nil)

	parent := mustRun(t, eng, parseFlow(t, failingParentTOML), nil)

	wantRunStatus(t, parent, store.RunFailed)

	// childRunOf demands exactly one run for the handler name, so it both proves
	// the fire happened (synchronously, since RunWorkflow has already returned
	// with no sleep here) and that it fired exactly once.
	handler := childRunOf(t, db, handlerName)

	wantRunStatus(t, handler, store.RunSucceeded)

	if handler.Trigger != "error" {
		t.Fatalf("handler trigger = %q, want %q", handler.Trigger, "error")
	}

	if handler.ParentRunID != parent.ID {
		t.Fatalf("handler parent_run_id = %q, want the failed run id %q", handler.ParentRunID, parent.ID)
	}
}

func TestErrorWorkflowReceivesOnlyDeclaredFailureInputs(t *testing.T) {
	t.Parallel()

	cfg := errorSourceConfig(t, map[string]string{handlerName: handlerDeclaringSubsetTOML})
	eng, db := newTestEngine(t, cfg, nil)

	parent := mustRun(t, eng, parseFlow(t, failingParentTOML), nil)

	wantRunStatus(t, parent, store.RunFailed)

	handler := childRunOf(t, db, handlerName)

	if handler.Inputs["run_id"] != parent.ID {
		t.Fatalf("run_id input = %v, want the failed run id %q", handler.Inputs["run_id"], parent.ID)
	}

	if handler.Inputs["failed_action"] != fnBoom {
		t.Fatalf("failed_action input = %v, want boom", handler.Inputs["failed_action"])
	}

	if handler.Inputs["error_code"] != parent.Error.Code {
		t.Fatalf("error_code input = %v, want the parent code %q", handler.Inputs["error_code"], parent.Error.Code)
	}

	// The undeclared keys never reach the child, because validateInputs would
	// reject them; the filter drops them before the run is created.
	if _, present := handler.Inputs["workflow"]; present {
		t.Fatalf("handler received undeclared workflow input: %v", handler.Inputs)
	}

	if _, present := handler.Inputs["error_message"]; present {
		t.Fatalf("handler received undeclared error_message input: %v", handler.Inputs)
	}
}

func TestErrorTriggeredRunNeverFiresItsOwn(t *testing.T) {
	t.Parallel()

	cfg := errorSourceConfig(t, map[string]string{
		handlerName:     handlerWithOwnHandlerTOML,
		secondLevelName: secondLevelHandlerTOML,
	})
	eng, db := newTestEngine(t, cfg, nil)

	parent := mustRun(t, eng, parseFlow(t, failingParentTOML), nil)

	wantRunStatus(t, parent, store.RunFailed)

	// The first-level handler ran (and failed), but its own error_workflow must
	// not fire because it was triggered as an error run.
	handler := childRunOf(t, db, handlerName)

	wantRunStatus(t, handler, store.RunFailed)

	second, err := db.ListRuns(t.Context(), store.ListFilter{WorkflowName: secondLevelName})
	if err != nil {
		t.Fatalf("list second-level runs: %v", err)
	}

	if len(second) != 0 {
		t.Fatalf("second-level handler fired %d times, want 0: an error run never fires its own", len(second))
	}
}

func TestSucceededRunFiresNoErrorWorkflow(t *testing.T) {
	t.Parallel()

	cfg := errorSourceConfig(t, map[string]string{handlerName: handlerTOML})
	eng, db := newTestEngine(t, cfg, nil)

	parent := mustRun(t, eng, parseFlow(t, succeedingParentTOML), nil)

	wantRunStatus(t, parent, store.RunSucceeded)
	wantNoHandlerRun(t, db)
}

func TestCanceledRunFiresNoErrorWorkflow(t *testing.T) {
	t.Parallel()

	cfg := errorSourceConfig(t, map[string]string{handlerName: handlerTOML})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stop := func(runCtx context.Context, _ *runner.Task) (any, error) {
		cancel()
		<-runCtx.Done()

		return nil, runCtx.Err()
	}

	eng, db := newTestEngine(t, cfg, map[string]builtins.Func{fnCancel: stop})

	run, err := eng.RunWorkflow(ctx, parseFlow(t, cancelWithHandlerTOML), nil, triggerCLI)
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	wantRunStatus(t, run, store.RunCanceled)
	wantNoHandlerRun(t, db)
}

func TestFailingErrorWorkflowLeavesParentTerminalStateUntouched(t *testing.T) {
	t.Parallel()

	cfg := errorSourceConfig(t, map[string]string{handlerName: failingHandlerTOML})
	eng, db := newTestEngine(t, cfg, nil)

	parent := mustRun(t, eng, parseFlow(t, failingParentTOML), nil)

	// The handler failed, but the parent keeps its own failure code and status.
	wantRunStatus(t, parent, store.RunFailed)
	wantErrCode(t, parent.Error, engine.CodeRunnerPermanent)

	stored := db.storedRun(parent.ID)
	if stored.Status != store.RunFailed || stored.Error.Code != engine.CodeRunnerPermanent {
		t.Fatalf("persisted parent = %q/%v, want failed with the parent's own code", stored.Status, stored.Error)
	}

	handler := childRunOf(t, db, handlerName)
	wantRunStatus(t, handler, store.RunFailed)
}

func TestErrorWorkflowFiresOnTimedOutParentContext(t *testing.T) {
	t.Parallel()

	cfg := errorSourceConfig(t, map[string]string{handlerName: handlerTOML})
	eng, db := newTestEngine(t, cfg, nil)

	// The parent context expires while the slow action still runs, so the run
	// fails with a run timeout rather than a cancel. The fire then runs on
	// context.WithoutCancel: the handler must succeed even though the parent
	// context is already done.
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()

	run, err := eng.RunWorkflow(ctx, parseFlow(t, slowFailWithHandlerTOML), nil, triggerCLI)
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	wantRunStatus(t, run, store.RunFailed)

	handler := childRunOf(t, db, handlerName)
	wantRunStatus(t, handler, store.RunSucceeded)
}

func TestNilWorkflowSourceLeavesFailedParentUntouched(t *testing.T) {
	t.Parallel()

	// baseConfig leaves Workflows nil, so a failed run with an error_workflow
	// logs and moves on without creating a handler run or panicking.
	eng, db := newTestEngine(t, baseConfig(), nil)

	parent := mustRun(t, eng, parseFlow(t, failingParentTOML), nil)

	wantRunStatus(t, parent, store.RunFailed)

	if db.runCount() != 1 {
		t.Fatalf("runs created = %d, want 1: a nil source fires no handler run", db.runCount())
	}
}

// wantNoHandlerRun asserts the on_failure handler workflow produced no run.
func wantNoHandlerRun(t *testing.T, db *fakeStore) {
	t.Helper()

	runs, err := db.ListRuns(t.Context(), store.ListFilter{WorkflowName: handlerName})
	if err != nil {
		t.Fatalf("list handler runs: %v", err)
	}

	if len(runs) != 0 {
		t.Fatalf("error workflow fired %d times, want 0", len(runs))
	}
}

// _ keeps the workflow import used even if a future refactor drops the only
// reference; the fixtures above parse through workflow.Parse via parseFlow.
var _ = workflow.KindWorkflow
