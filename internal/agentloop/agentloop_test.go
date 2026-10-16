package agentloop_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chadit/CairnOS/internal/agentloop"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/engine"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/reliability"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// agentFlowTOML is the engine-level acceptance workflow: an agent action
// with the memory tool and a mock-backed tool feeding a downstream task
// action through ${{ }}.
const agentFlowTOML = `
[workflow]
name = "agentic"

[action.investigate]
kind = "agent"
driver = "scripted"
goal = "find the failing workflow"
tools = ["memory", "mock"]
max_iterations = 5
budget_usd = 2.0
memory_scope = "project:test"

[action.report]
runner = "mock"
depends_on = ["investigate"]
params = { output = "${{ investigate.output }}" }
`

// happyPathTurns scripts the spec's canned goal walk: memory search, then
// the mock tool, then the final answer.
func happyPathTurns() []runner.Result {
	return []runner.Result{
		toolTurn(builtins.MemoryName, map[string]any{keyOp: opSearch, keyQuery: "failures"}, turnCost),
		toolTurn(builtins.MockName, map[string]any{keyOutput: mockEcho}, turnCost),
		finalTurn(),
	}
}

// Spec 15 acceptance: an agent action with memory and one mock-backed tool
// achieves a canned goal in under max_iterations, and its output flows
// downstream through ${{ }} references.
func TestAgentActionAchievesGoalAndOutputFlowsDownstream(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: happyPathTurns()}
	mem := &fakeMemory{entries: []memory.Entry{{ID: "m1", Scope: scopeProject, Content: memoryNote}}}
	registry := newTestRegistry(t, driver, mem)
	db := newCapturingStore()

	loop := agentloop.New(registry, mem, db, nil)

	cfg := engine.Config{
		MaxConcurrentActions: 2,
		DefaultRetry:         reliability.Policy{MaxAttempts: 1},
		Breaker:              reliability.BreakerConfig{FailureThreshold: 1, MinRequests: 1000},
		NewID:                sequentialIDs(),
		AgentExecutor:        loop,
	}

	eng := engine.New(cfg, registry, db)

	run, err := eng.RunWorkflow(t.Context(), parseFlow(t, agentFlowTOML), nil, "cli")
	if err != nil {
		t.Fatalf("RunWorkflow: %v", err)
	}

	if run.Status != store.RunSucceeded {
		t.Fatalf("run status = %q (error %v), want succeeded", run.Status, run.Error)
	}

	brain := db.actionRow(run.ID, "investigate")
	if brain == nil || brain.Output != finalAnswer || brain.Runner != driverName {
		t.Fatalf("agent row = %+v, want the final answer under the driver name", brain)
	}

	// Budget math: three driver turns at 0.1 each, tools free.
	if wantCost := 0.1 * 3; brain.Usage.CostUSD < wantCost-1e-9 || brain.Usage.CostUSD > wantCost+1e-9 {
		t.Fatalf("agent usage = %+v, want driver and tool usage summed to %.1f", brain.Usage, wantCost)
	}

	report := db.actionRow(run.ID, "report")
	if report == nil || report.Output != finalAnswer {
		t.Fatalf("downstream row = %+v, want the agent output flowing through ${{ investigate.output }}", report)
	}

	// The loop forces memory_scope onto memory-tool calls.
	if scopes := mem.searchScopes(); len(scopes) != 1 || scopes[0] != scopeProject {
		t.Fatalf("memory search scopes = %v, want the loop-injected %q", scopes, scopeProject)
	}

	steps := db.recordedSteps()
	wantKinds := []string{
		store.StepDriverCall, store.StepToolCall,
		store.StepDriverCall, store.StepToolCall,
		store.StepFinal,
	}

	if len(steps) != len(wantKinds) {
		t.Fatalf("persisted %d steps, want %d", len(steps), len(wantKinds))
	}

	for idx := range steps {
		if steps[idx].Kind != wantKinds[idx] || steps[idx].Step != idx+1 {
			t.Fatalf("step %d = kind %q number %d, want %q in order", idx+1, steps[idx].Kind, steps[idx].Step, wantKinds[idx])
		}
	}

	assertHappyPathPrompts(t, driver)
}

// assertHappyPathPrompts checks the driver saw the goal, the memory
// section, and the accumulating transcript.
func assertHappyPathPrompts(t *testing.T, driver *scriptedDriver) {
	t.Helper()

	if driver.callCount() != 3 {
		t.Fatalf("driver served %d turns, want 3", driver.callCount())
	}

	first := driver.promptAt(t, 0)
	if !strings.Contains(first, goalText) || !strings.Contains(first, memoryNote) {
		t.Fatalf("first prompt = %q, want the goal and the memory note", first)
	}

	second := driver.promptAt(t, 1)
	if !strings.Contains(second, builtins.MemoryName) || !strings.Contains(second, memoryNote) {
		t.Fatalf("second prompt = %q, want the memory tool result in the transcript", second)
	}

	third := driver.promptAt(t, 2)
	if !strings.Contains(third, mockEcho) {
		t.Fatalf("third prompt = %q, want the mock result in the transcript", third)
	}
}

// Spec 15 acceptance: exhausting max_iterations fails the action with
// agent.max_iterations and a persisted partial transcript.
func TestLoopMaxIterationsWallPersistsPartialTranscript(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{
		toolTurn(builtins.MockName, map[string]any{keyOutput: mockEcho}, turnCost),
	}}
	steps := &stepRecorder{}
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, steps, nil)

	action := agentAction([]string{builtins.MockName})
	action.MaxIterations = 2

	result, err := loop.Execute(t.Context(), action, agentTask())

	wantLoopErrCode(t, err, agentloop.CodeMaxIterations)

	if driver.callCount() != 2 {
		t.Fatalf("driver served %d turns, want exactly max_iterations", driver.callCount())
	}

	assertStepSequence(t, steps.recorded(), []string{
		store.StepDriverCall, store.StepToolCall,
		store.StepDriverCall, store.StepToolCall,
	})

	if result.Usage.CostUSD < 0.2-1e-9 {
		t.Fatalf("partial usage = %+v, want the spend so far reported with the failure", result.Usage)
	}
}

// Spec 15 acceptance: exhausting budget_usd fails the action with
// agent.budget_exhausted and a persisted partial transcript.
func TestLoopBudgetWallPersistsPartialTranscript(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{
		toolTurn(builtins.MockName, map[string]any{keyOutput: mockEcho}, 0.4),
	}}
	steps := &stepRecorder{}
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, steps, nil)

	action := agentAction([]string{builtins.MockName})
	action.BudgetUSD = 0.75

	result, err := loop.Execute(t.Context(), action, agentTask())

	wantLoopErrCode(t, err, agentloop.CodeBudgetExhausted)

	// 0.4 after one turn stays under 0.75; 0.8 after the second crosses it.
	if driver.callCount() != 2 {
		t.Fatalf("driver served %d turns, want the wall to fall after the second", driver.callCount())
	}

	assertStepSequence(t, steps.recorded(), []string{
		store.StepDriverCall, store.StepToolCall,
		store.StepDriverCall, store.StepToolCall,
	})

	if result.Usage.CostUSD < 0.8-1e-9 {
		t.Fatalf("partial usage = %+v, want the blown spend reported", result.Usage)
	}
}

// Spec 15 acceptance: a tools entry without Describer and a driver without
// tool calling each fail validation before anything runs. Tools resolve
// only through runner names, which is the no-recursion rule: an action name
// is not a registered runner.
func TestLoopValidateActionEnforcesCapabilities(t *testing.T) {
	t.Parallel()

	loop := agentloop.New(newTestRegistry(t, &scriptedDriver{}, &fakeMemory{}), nil, nil, nil)

	tests := []struct {
		name     string
		mutate   func(action *workflow.Action)
		wantCode string
	}{
		{
			name:   "valid driver and tools",
			mutate: func(_ *workflow.Action) {},
		},
		{
			name:     "unregistered driver",
			mutate:   func(action *workflow.Action) { action.Driver = unregisteredName },
			wantCode: agentloop.CodeDriverIncapable,
		},
		{
			name:     "driver without tool calling",
			mutate:   func(action *workflow.Action) { action.Driver = builtins.MockName },
			wantCode: agentloop.CodeDriverIncapable,
		},
		{
			name:     "tool that is not a registered runner",
			mutate:   func(action *workflow.Action) { action.Tools = []string{unknownToolName} },
			wantCode: workflow.CodeInvalid,
		},
		{
			name:     "tool without describer",
			mutate:   func(action *workflow.Action) { action.Tools = []string{plainName} },
			wantCode: workflow.CodeInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			action := agentAction([]string{builtins.MemoryName, builtins.MockName})
			tt.mutate(action)

			err := loop.ValidateAction(action)

			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("ValidateAction = %v, want nil", err)
				}

				return
			}

			errs, isErrs := errors.AsType[cairnerr.Errors](err)
			if !isErrs || len(errs) == 0 {
				t.Fatalf("ValidateAction = %v (%T), want cairnerr.Errors", err, err)
			}

			if errs[0].Code != tt.wantCode {
				t.Fatalf("code = %s, want %s (message: %s)", errs[0].Code, tt.wantCode, errs[0].Message)
			}
		})
	}
}

// TestLoopReportsEveryUnresolvedEntryDriverFirst covers an action whose driver
// and one tool are both unregistered: ValidateAction reports both problems,
// driver first, and Execute fails on that first one before anything runs.
func TestLoopReportsEveryUnresolvedEntryDriverFirst(t *testing.T) {
	t.Parallel()

	loop := agentloop.New(newTestRegistry(t, &scriptedDriver{}, &fakeMemory{}), nil, nil, nil)

	action := agentAction([]string{builtins.MockName, unknownToolName})
	action.Driver = unregisteredName

	validateErr := loop.ValidateAction(action)

	errs, isErrs := errors.AsType[cairnerr.Errors](validateErr)
	if !isErrs || len(errs) != 2 {
		t.Fatalf("ValidateAction = %v (%T), want two problems", validateErr, validateErr)
	}

	if errs[0].Code != agentloop.CodeDriverIncapable || errs[1].Code != workflow.CodeInvalid {
		t.Fatalf("codes = %s, %s; want %s, %s",
			errs[0].Code, errs[1].Code, agentloop.CodeDriverIncapable, workflow.CodeInvalid)
	}

	_, executeErr := loop.Execute(t.Context(), action, &runner.Task{})

	first, isCanonical := errors.AsType[*cairnerr.Error](executeErr)
	if !isCanonical || first.Code != errs[0].Code || first.Message != errs[0].Message {
		t.Fatalf("Execute = %v, want the first validation problem %v", executeErr, errs[0])
	}
}

// Spec 15 acceptance: a disallowed tool call is rejected and never
// executed; two consecutive bad turns end the action with
// agent.bad_tool_call, while one bad turn followed by a good one recovers.
func TestLoopBadDriverTurnsStrikeOutOrRecover(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantCode string
		turns    []runner.Result
	}{
		{
			name: "two disallowed tool calls",
			turns: []runner.Result{
				toolTurn(toolForbidden, map[string]any{}, turnCost),
				toolTurn(toolForbidden, map[string]any{}, turnCost),
			},
			wantCode: agentloop.CodeBadToolCall,
		},
		{
			name: "two unparseable tool_use turns",
			turns: []runner.Result{
				rawTurn("not a tool call", runner.StopReasonToolUse, turnCost),
				rawTurn("still not one", runner.StopReasonToolUse, turnCost),
			},
			wantCode: agentloop.CodeBadToolCall,
		},
		{
			name: "disallowed then unparseable still strikes out",
			turns: []runner.Result{
				toolTurn(toolForbidden, map[string]any{}, turnCost),
				rawTurn("garbage", runner.StopReasonToolUse, turnCost),
			},
			wantCode: agentloop.CodeBadToolCall,
		},
		{
			name: "one strike then a final answer recovers",
			turns: []runner.Result{
				toolTurn(toolForbidden, map[string]any{}, turnCost),
				finalTurn(),
			},
		},
		{
			name: "strikes reset after a valid tool call",
			turns: []runner.Result{
				toolTurn(toolForbidden, map[string]any{}, turnCost),
				toolTurn(builtins.MockName, map[string]any{keyOutput: mockEcho}, turnCost),
				toolTurn(toolForbidden, map[string]any{}, turnCost),
				finalTurn(),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			driver := &scriptedDriver{turns: tt.turns}
			steps := &stepRecorder{}
			loop := agentloop.New(newTestRegistry(t, driver, nil), nil, steps, nil)

			result, err := loop.Execute(t.Context(), agentAction([]string{builtins.MockName}), agentTask())

			if tt.wantCode != "" {
				wantLoopErrCode(t, err, tt.wantCode)
				assertNoForbiddenExecution(t, steps.recorded())

				return
			}

			if err != nil {
				t.Fatalf("Execute = %v, want recovery to the final answer", err)
			}

			if result.Output != finalAnswer {
				t.Fatalf("output = %#v, want %q", result.Output, finalAnswer)
			}

			// The rejection surfaced to the driver as transcript text.
			rejected := driver.promptAt(t, 1)
			if !strings.Contains(rejected, "rejected") {
				t.Fatalf("post-strike prompt = %q, want the rejection note", rejected)
			}
		})
	}
}

// assertNoForbiddenExecution proves rejected calls never run: bad turns
// persist as driver_call rows only, with no tool_call row at all.
func assertNoForbiddenExecution(t *testing.T, steps []store.AgentStep) {
	t.Helper()

	for idx := range steps {
		if steps[idx].Kind == store.StepToolCall {
			t.Fatalf("step %d is a tool_call: a rejected tool call must never execute", idx+1)
		}
	}
}

// Spec 15 acceptance (as resolved for 1c): a transient tool failure
// surfaces in the transcript without consuming a driver iteration beyond
// the turn that requested it; the driver decides whether to re-issue.
func TestLoopTransientToolFailureSurfacesInTranscript(t *testing.T) {
	t.Parallel()

	const transientClass = "transient"

	driver := &scriptedDriver{turns: []runner.Result{
		toolTurn(builtins.MockName, map[string]any{"fail_class": transientClass}, turnCost),
		finalTurn(),
	}}
	steps := &stepRecorder{}
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, steps, nil)

	result, err := loop.Execute(t.Context(), agentAction([]string{builtins.MockName}), agentTask())
	if err != nil {
		t.Fatalf("Execute = %v, want the loop to survive the tool failure", err)
	}

	if result.Output != finalAnswer {
		t.Fatalf("output = %#v, want %q", result.Output, finalAnswer)
	}

	if driver.callCount() != 2 {
		t.Fatalf("driver served %d turns, want 2: the failure must not burn extra iterations", driver.callCount())
	}

	failureNote := driver.promptAt(t, 1)
	if !strings.Contains(failureNote, "failed (transient)") {
		t.Fatalf("post-failure prompt = %q, want the classified failure in the transcript", failureNote)
	}

	recorded := steps.recorded()
	assertStepSequence(t, recorded, []string{store.StepDriverCall, store.StepToolCall, store.StepFinal})

	failed, isMap := recorded[1].Output.(map[string]any)
	if !isMap || failed["class"] != transientClass {
		t.Fatalf("tool step output = %#v, want the classified error persisted", recorded[1].Output)
	}
}

// Walls include the caller context: cancellation surfaces after the
// in-flight iteration so the engine can map it to its cancel/timeout codes.
func TestLoopStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{
		toolTurn(builtins.MockName, map[string]any{keyOutput: mockEcho}, turnCost),
	}}
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, &stepRecorder{}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := loop.Execute(ctx, agentAction([]string{builtins.MockName}), agentTask())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute = %v, want the context cancellation surfaced", err)
	}

	if driver.callCount() != 1 {
		t.Fatalf("driver served %d turns after cancellation, want 1", driver.callCount())
	}
}

// system_prompt replaces the default framing; without it the loop default
// applies.
func TestLoopSystemPromptOverride(t *testing.T) {
	t.Parallel()

	const customPrompt = "custom framing"

	driver := &scriptedDriver{turns: []runner.Result{finalTurn()}}
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, nil, nil)

	action := agentAction(nil)
	action.Tools = []string{}
	action.SystemPrompt = customPrompt

	if _, err := loop.Execute(t.Context(), action, agentTask()); err != nil {
		t.Fatalf("Execute with custom system prompt: %v", err)
	}

	if got := driver.systemAt(t, 0); got != customPrompt {
		t.Fatalf("system = %q, want the action's system_prompt verbatim", got)
	}

	plain := &scriptedDriver{turns: []runner.Result{finalTurn()}}
	defaultLoop := agentloop.New(newTestRegistry(t, plain, nil), nil, nil, nil)

	if _, err := defaultLoop.Execute(t.Context(), agentAction(nil), agentTask()); err != nil {
		t.Fatalf("Execute with default system prompt: %v", err)
	}

	if got := plain.systemAt(t, 0); !strings.Contains(got, "CairnOS workflow") {
		t.Fatalf("system = %q, want the loop default framing", got)
	}
}

// A memory load failure fails the action: the workflow asked for that
// scope, so dropping it silently is not an option.
func TestLoopMemoryLoadFailureFailsAction(t *testing.T) {
	t.Parallel()

	driver := &scriptedDriver{turns: []runner.Result{finalTurn()}}
	mem := &fakeMemory{failWith: errMemoryDown}
	loop := agentloop.New(newTestRegistry(t, driver, mem), mem, nil, nil)

	action := agentAction(nil)
	action.MemoryScope = scopeProject

	_, err := loop.Execute(t.Context(), action, agentTask())
	if !errors.Is(err, errMemoryDown) {
		t.Fatalf("Execute = %v, want the memory failure surfaced", err)
	}

	if driver.callCount() != 0 {
		t.Fatalf("driver served %d turns, want 0: prompt assembly failed first", driver.callCount())
	}
}

// parseFlow parses a workflow fixture.
func parseFlow(t *testing.T, text string) *workflow.Workflow {
	t.Helper()

	flow, err := workflow.Parse([]byte(text))
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}

	return flow
}

// sequentialIDs mints repeatable run IDs.
func sequentialIDs() func() string {
	var counter atomic.Int64

	return func() string {
		return "run-" + string(rune('0'+counter.Add(1)))
	}
}
