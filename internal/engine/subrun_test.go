package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
)

// childName and regionLax are shared literals across the workflow-call fixtures
// and assertions, named so the reused strings stay in one place.
const (
	childName = "child"
	regionLax = "lax"
)

// Workflow-call fixtures. A parent calls a child by name; the fake workflow
// source resolves the names without touching the filesystem.
const (
	parentCallTOML = `
[workflow]
name = "parent"

[action.call]
kind = "workflow"
workflow = "child"

[action.call.inputs]
region = "lax"
`

	childEchoTOML = `
[workflow]
name = "child"

[workflow.inputs.region]
type = "string"
required = true

[action.work]
runner = "mock"
params = { output = { done = true } }
`

	// parentNoInputTOML calls the child but passes no inputs, so the child's
	// required region input fails validation inside the recursive RunWorkflow.
	parentNoInputTOML = `
[workflow]
name = "parent"

[action.call]
kind = "workflow"
workflow = "child"
`

	// wfATOML and wfBTOML form a call cycle: a calls b, b calls a. The depth
	// cap refuses it and prints the ancestry chain.
	wfATOML = `
[workflow]
name = "a"

[action.call]
kind = "workflow"
workflow = "b"
`

	wfBTOML = `
[workflow]
name = "b"

[action.call]
kind = "workflow"
workflow = "a"
`

	parentCallFailerTOML = `
[workflow]
name = "parent"

[action.call]
kind = "workflow"
workflow = "failer"
`

	failerTOML = `
[workflow]
name = "failer"

[action.boom]
runner = "mock"
params = { fail_class = "permanent" }
`
)

// pausedWorkflows is a standalone engine.PauseChecker so the passthrough test
// pauses a child workflow without wiring the whole store fake as the checker.
type pausedWorkflows map[string]bool

var _ engine.PauseChecker = pausedWorkflows(nil)

func (p pausedWorkflows) GetWorkflowPause(_ context.Context, name string) (*store.WorkflowPause, bool, error) {
	if p[name] {
		return &store.WorkflowPause{WorkflowName: name}, true, nil
	}

	return nil, false, nil
}

func TestWorkflowCallRunsChildAsRealRun(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Workflows = &fakeWorkflowSource{flows: map[string]*workflow.Workflow{
		childName: parseFlow(t, childEchoTOML),
	}}

	eng, db := newTestEngine(t, cfg, nil)

	parent := mustRun(t, eng, parseFlow(t, parentCallTOML), nil)

	wantRunStatus(t, parent, store.RunSucceeded)

	child := childRunOf(t, db, childName)
	if child.ParentRunID != parent.ID {
		t.Fatalf("child parent_run_id = %q, want the parent run id %q", child.ParentRunID, parent.ID)
	}

	if child.Trigger != "workflow" {
		t.Fatalf("child trigger = %q, want %q", child.Trigger, "workflow")
	}

	if child.Inputs["region"] != regionLax {
		t.Fatalf("child inputs = %v, want the interpolated region", child.Inputs)
	}

	call := wantActionStatus(t, db, parent.ID, "call", store.ActionSucceeded)

	output, isMap := call.Output.(map[string]any)
	if !isMap {
		t.Fatalf("call output = %T, want the {run_id, status, outputs} map", call.Output)
	}

	if output["run_id"] != child.ID || output["status"] != string(store.RunSucceeded) {
		t.Fatalf("call output = %v, want the child run id and succeeded status", output)
	}

	outputs, isOutputs := output["outputs"].(map[string]any)
	if !isOutputs {
		t.Fatalf("call output.outputs = %T, want the child per-action output map", output["outputs"])
	}

	work, isWork := outputs["work"].(map[string]any)
	if !isWork || work["done"] != true {
		t.Fatalf("child work output = %v, want {done: true}", outputs["work"])
	}
}

func TestWorkflowCallNilSourceFailsAction(t *testing.T) {
	t.Parallel()

	// baseConfig leaves Workflows nil, so the workflow-call action has no source.
	eng, db := newTestEngine(t, baseConfig(), nil)

	run := mustRun(t, eng, parseFlow(t, parentCallTOML), nil)

	wantRunStatus(t, run, store.RunFailed)
	wantErrCode(t, run.Error, engine.CodeWorkflowSourceMissing)

	call := wantActionStatus(t, db, run.ID, "call", store.ActionFailed)
	wantErrCode(t, call.Error, engine.CodeWorkflowSourceMissing)

	if db.runCount() != 1 {
		t.Fatalf("runs created = %d, want 1: a missing source creates no child run", db.runCount())
	}
}

func TestWorkflowCallDepthCapRefusesWithAncestry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		wantChain string
		maxDepth  int
	}{
		{name: "overridden depth of one", maxDepth: 1, wantChain: "a -> b -> a"},
		{name: "engine default depth of three", maxDepth: 0, wantChain: "a -> b -> a -> b -> a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := baseConfig()
			cfg.MaxCallDepth = tt.maxDepth
			cfg.Workflows = &fakeWorkflowSource{flows: map[string]*workflow.Workflow{
				"a": parseFlow(t, wfATOML),
				"b": parseFlow(t, wfBTOML),
			}}

			eng, _ := newTestEngine(t, cfg, nil)

			run := mustRun(t, eng, parseFlow(t, wfATOML), nil)

			wantRunStatus(t, run, store.RunFailed)
			wantErrCode(t, run.Error, engine.CodeCallDepthExceeded)

			if !strings.Contains(run.Error.Message, tt.wantChain) {
				t.Fatalf("error message = %q, want the ancestry chain %q", run.Error.Message, tt.wantChain)
			}
		})
	}
}

func TestWorkflowCallRejectionPropagates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		flows    map[string]string
		pause    pausedWorkflows
		wantCode string
	}{
		{
			name:     "paused child fails the parent action with the governance code",
			flows:    map[string]string{childName: childEchoTOML},
			pause:    pausedWorkflows{"child": true},
			wantCode: engine.CodeWorkflowPaused,
		},
		{
			name:     "missing required child input fails the parent action at validation",
			flows:    map[string]string{childName: childEchoTOML},
			wantCode: workflow.CodeInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := baseConfig()
			cfg.Workflows = &fakeWorkflowSource{flows: parseFlows(t, tt.flows)}

			if tt.pause != nil {
				cfg.Pause = tt.pause
			}

			eng, db := newTestEngine(t, cfg, nil)

			// The paused case supplies region so validation passes and the run
			// reaches the governance gate; the validation case supplies nothing.
			parentTOML := parentCallTOML
			if tt.pause == nil {
				parentTOML = parentNoInputTOML
			}

			run := mustRun(t, eng, parseFlow(t, parentTOML), nil)

			wantRunStatus(t, run, store.RunFailed)

			call := wantActionStatus(t, db, run.ID, "call", store.ActionFailed)
			wantErrCode(t, call.Error, tt.wantCode)

			if db.runCount() != 1 {
				t.Fatalf("runs created = %d, want 1: a rejected child creates no run", db.runCount())
			}
		})
	}
}

func TestWorkflowCallFailedChildPropagatesCodeAndChildRunID(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.Workflows = &fakeWorkflowSource{flows: map[string]*workflow.Workflow{
		"failer": parseFlow(t, failerTOML),
	}}

	eng, db := newTestEngine(t, cfg, nil)

	run := mustRun(t, eng, parseFlow(t, parentCallFailerTOML), nil)

	wantRunStatus(t, run, store.RunFailed)
	wantErrCode(t, run.Error, engine.CodeRunnerPermanent)

	child := childRunOf(t, db, "failer")

	call := wantActionStatus(t, db, run.ID, "call", store.ActionFailed)
	wantErrCode(t, call.Error, engine.CodeRunnerPermanent)

	if call.Error.Details["child_run_id"] != child.ID {
		t.Fatalf("child_run_id detail = %v, want the failed child run id %q", call.Error.Details["child_run_id"], child.ID)
	}
}

// childRunOf returns the single run recorded for a workflow name, failing when
// none or more than one exists.
func childRunOf(t *testing.T, db *fakeStore, workflowName string) *store.Run {
	t.Helper()

	runs, err := db.ListRuns(t.Context(), store.ListFilter{WorkflowName: workflowName})
	if err != nil {
		t.Fatalf("list runs for %q: %v", workflowName, err)
	}

	if len(runs) != 1 {
		t.Fatalf("found %d runs for workflow %q, want exactly 1", len(runs), workflowName)
	}

	return runs[0]
}

// parseFlows parses a name-to-TOML map into resolvable workflows for the fake
// source.
func parseFlows(t *testing.T, sources map[string]string) map[string]*workflow.Workflow {
	t.Helper()

	flows := make(map[string]*workflow.Workflow, len(sources))
	for name, text := range sources {
		flows[name] = parseFlow(t, text)
	}

	return flows
}
