package agentloop_test

import (
	"errors"
	"testing"

	"github.com/chadit/CairnOS/internal/agentloop"
	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// Delegate tool argument keys and shared fixture values. goconst applies to
// tests in this repo's lint policy, so the reused strings are consts.
const (
	argGoalKey    = "goal"
	argMaxIterKey = "max_iterations"
	argBudgetKey  = "budget_usd"
	delegateGoal  = "dig into the child subsystem"
	childAnswer   = "child finished the sub-goal"
	subActionName = actionBrain + "/sub-1"
)

// delegateTurn scripts a driver turn requesting the delegate tool with the
// given arguments at the standard per-turn cost.
func delegateTurn(arguments map[string]any) runner.Result {
	return toolTurn(agentloop.DelegateToolName, arguments, turnCost)
}

// findDelegateStep returns the first delegate tool_call step in a transcript.
func findDelegateStep(t *testing.T, steps []*store.AgentStep) *store.AgentStep {
	t.Helper()

	for idx := range steps {
		if steps[idx].Kind == store.StepToolCall && steps[idx].Tool == agentloop.DelegateToolName {
			return steps[idx]
		}
	}

	t.Fatalf("no delegate tool_call step among %d steps", len(steps))

	return nil
}

// mustListSteps reads one action's persisted transcript.
func mustListSteps(t *testing.T, db *capturingStore, actionName string) []*store.AgentStep {
	t.Helper()

	steps, err := db.ListAgentSteps(t.Context(), testRunID, actionName)
	if err != nil {
		t.Fatalf("ListAgentSteps(%s): %v", actionName, err)
	}

	return steps
}

// assertClassifiedError checks a delegate step output is the transcript's
// error/class map, the shape a failed child or refused ask records.
func assertClassifiedError(t *testing.T, output any) {
	t.Helper()

	table, isTable := output.(map[string]any)
	if !isTable {
		t.Fatalf("delegate output = %#v, want a classified error map", output)
	}

	if _, hasErr := table["error"]; !hasErr {
		t.Fatalf("delegate output = %#v, want an error key", output)
	}

	if _, hasClass := table["class"]; !hasClass {
		t.Fatalf("delegate output = %#v, want a class key", output)
	}
}

// assertStepKinds checks kinds and 1-based ordering for one action's steps.
func assertStepKinds(t *testing.T, steps []*store.AgentStep, kinds []string) {
	t.Helper()

	if len(steps) != len(kinds) {
		t.Fatalf("recorded %d steps, want %d (%v)", len(steps), len(kinds), kinds)
	}

	for idx := range steps {
		if steps[idx].Kind != kinds[idx] {
			t.Errorf("step %d kind = %q, want %q", idx+1, steps[idx].Kind, kinds[idx])
		}

		if steps[idx].Step != idx+1 {
			t.Errorf("step %d numbered %d, want in-order numbering", idx+1, steps[idx].Step)
		}
	}
}

// An action listing "delegate" spawns one child whose answer lands in the
// parent's delegate tool_call, and the child's transcript persists under
// <action>/sub-1 with no schema change.
func TestDelegateSpawnsChildAndCarriesAnswer(t *testing.T) {
	t.Parallel()

	driver := newPerActionDriver(map[string][]runner.Result{
		actionBrain: {
			delegateTurn(map[string]any{argGoalKey: delegateGoal}),
			finalTurn(),
		},
		subActionName: {
			toolTurn(builtins.MockName, map[string]any{keyOutput: mockEcho}, turnCost),
			finalTurnAnswer(childAnswer, turnCost),
		},
	})
	db := newCapturingStore()
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, db, nil)

	action := agentAction([]string{agentloop.DelegateToolName, builtins.MockName})

	result, err := loop.Execute(t.Context(), action, agentTask())
	if err != nil {
		t.Fatalf("Execute = %v, want the parent to finish after delegating", err)
	}

	if result.Output != finalAnswer {
		t.Fatalf("parent output = %#v, want %q", result.Output, finalAnswer)
	}

	parentSteps := mustListSteps(t, db, actionBrain)
	assertStepKinds(t, parentSteps, []string{store.StepDriverCall, store.StepToolCall, store.StepFinal})

	delegateStep := findDelegateStep(t, parentSteps)
	if delegateStep.Output != childAnswer {
		t.Fatalf("delegate step output = %#v, want the child's answer %q", delegateStep.Output, childAnswer)
	}

	childSteps := mustListSteps(t, db, subActionName)
	assertStepKinds(t, childSteps, []string{store.StepDriverCall, store.StepToolCall, store.StepFinal})

	if got := driver.callsFor(subActionName); got != 2 {
		t.Fatalf("child served %d turns, want 2", got)
	}

	if got := driver.callsFor(actionBrain); got != 2 {
		t.Fatalf("parent served %d turns, want 2 (delegate then final)", got)
	}
}

// The /sub-<n> separator is safe because no schema-valid action name carries a
// slash: the workflow validator rejects it. Pinned against the validator so a
// future grammar change that admitted slashes would break here first.
func TestDelegateChildNameSeparatorRejectedByValidator(t *testing.T) {
	t.Parallel()

	const toml = "[workflow]\nname = \"w\"\n[action.\"brain/sub-1\"]\nrunner = \"mock\""

	_, err := workflow.Parse([]byte(toml))

	errs, isErrs := errors.AsType[cairnerr.Errors](err)
	if !isErrs || len(errs) == 0 {
		t.Fatalf("Parse(slash action name) = %v, want a validation error", err)
	}

	var found bool

	for idx := range errs {
		if errs[idx].Code == workflow.CodeBadName {
			found = true
		}
	}

	if !found {
		t.Fatalf("Parse errors = %v, want %s for the slash-bearing name", errs, workflow.CodeBadName)
	}
}

// A requested child budget above the parent's remaining clamps down, the
// child's spend sums into the parent, and the parent's own budget wall fires
// once the child's dollars push it over.
func TestDelegateBudgetClampAndParentWall(t *testing.T) {
	t.Parallel()

	driver := newPerActionDriver(map[string][]runner.Result{
		actionBrain: {
			delegateTurn(map[string]any{argGoalKey: delegateGoal, argBudgetKey: 100.0}),
		},
		subActionName: {
			toolTurn(builtins.MockName, map[string]any{keyOutput: mockEcho}, 0.5),
		},
	})
	db := newCapturingStore()
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, db, nil)

	action := agentAction([]string{agentloop.DelegateToolName, builtins.MockName})
	action.BudgetUSD = 1.0
	action.MaxIterations = 8

	_, err := loop.Execute(t.Context(), action, agentTask())

	wantLoopErrCode(t, err, agentloop.CodeBudgetExhausted)

	// Remaining after the parent's first turn is 0.9. Clamped to 0.9, the
	// child's budget wall falls after 2 turns at 0.5 each, far short of the
	// 8-iteration cap it would reach if 100 had stood.
	if got := driver.callsFor(subActionName); got != 2 {
		t.Fatalf("child served %d turns, want 2 from the clamped 0.9 budget", got)
	}

	delegateStep := findDelegateStep(t, mustListSteps(t, db, actionBrain))
	assertClassifiedError(t, delegateStep.Output)
}

// A requested child iteration cap above the parent's clamps down to the
// parent's, and the child's wall failure surfaces as classified transcript
// text without failing the parent.
func TestDelegateIterationClampAndChildWallSurfaces(t *testing.T) {
	t.Parallel()

	driver := newPerActionDriver(map[string][]runner.Result{
		actionBrain: {
			delegateTurn(map[string]any{argGoalKey: delegateGoal, argMaxIterKey: int64(100)}),
			finalTurn(),
		},
		subActionName: {
			toolTurn(builtins.MockName, map[string]any{keyOutput: mockEcho}, 0.01),
		},
	})
	db := newCapturingStore()
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, db, nil)

	action := agentAction([]string{agentloop.DelegateToolName, builtins.MockName})
	action.MaxIterations = 3
	action.BudgetUSD = 100

	result, err := loop.Execute(t.Context(), action, agentTask())
	if err != nil {
		t.Fatalf("Execute = %v, want the parent to finish after the child's iteration wall", err)
	}

	if result.Output != finalAnswer {
		t.Fatalf("parent output = %#v, want %q", result.Output, finalAnswer)
	}

	if got := driver.callsFor(subActionName); got != 3 {
		t.Fatalf("child served %d turns, want 3 from the clamped cap", got)
	}

	delegateStep := findDelegateStep(t, mustListSteps(t, db, actionBrain))
	assertClassifiedError(t, delegateStep.Output)
}

// The count wall allows one delegation per action run: a second ask strikes,
// and two consecutive strikes fail the action with agent.bad_tool_call.
func TestDelegateSecondCallStrikesOut(t *testing.T) {
	t.Parallel()

	driver := newPerActionDriver(map[string][]runner.Result{
		actionBrain: {
			delegateTurn(map[string]any{argGoalKey: delegateGoal}),
			delegateTurn(map[string]any{argGoalKey: delegateGoal}),
			delegateTurn(map[string]any{argGoalKey: delegateGoal}),
		},
		subActionName: {finalTurnAnswer(childAnswer, turnCost)},
	})
	db := newCapturingStore()
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, db, nil)

	action := agentAction([]string{agentloop.DelegateToolName})

	_, err := loop.Execute(t.Context(), action, agentTask())

	wantLoopErrCode(t, err, agentloop.CodeBadToolCall)

	if got := driver.callsFor(subActionName); got != 1 {
		t.Fatalf("child served %d turns, want exactly 1: only the first delegation spawns", got)
	}
}

// A delegated child never receives the delegate tool, so a forced call strikes
// via the missing-tool path; the child's failure is classified transcript text
// in the parent, which still finishes.
func TestDelegatedChildCannotDelegate(t *testing.T) {
	t.Parallel()

	driver := newPerActionDriver(map[string][]runner.Result{
		actionBrain: {
			delegateTurn(map[string]any{argGoalKey: delegateGoal}),
			finalTurn(),
		},
		subActionName: {
			delegateTurn(map[string]any{argGoalKey: delegateGoal}),
			delegateTurn(map[string]any{argGoalKey: delegateGoal}),
		},
	})
	db := newCapturingStore()
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, db, nil)

	action := agentAction([]string{agentloop.DelegateToolName, builtins.MockName})

	result, err := loop.Execute(t.Context(), action, agentTask())
	if err != nil {
		t.Fatalf("Execute = %v, want the parent to finish despite the child's failure", err)
	}

	if result.Output != finalAnswer {
		t.Fatalf("parent output = %#v, want %q", result.Output, finalAnswer)
	}

	delegateStep := findDelegateStep(t, mustListSteps(t, db, actionBrain))
	assertClassifiedError(t, delegateStep.Output)

	for _, step := range mustListSteps(t, db, subActionName) {
		if step.Kind == store.StepToolCall {
			t.Fatalf("child ran a tool_call %q: a struck delegate call must never execute", step.Tool)
		}
	}
}

// Zero remaining budget refuses the delegation as a transcript failure without
// spawning a child or burning the one delegation; the parent's own budget wall
// then fires.
func TestDelegateZeroBudgetRefusesWithoutSpawning(t *testing.T) {
	t.Parallel()

	driver := newPerActionDriver(map[string][]runner.Result{
		actionBrain:   {delegateTurn(map[string]any{argGoalKey: delegateGoal})},
		subActionName: {finalTurn()},
	})
	db := newCapturingStore()
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, db, nil)

	action := agentAction([]string{agentloop.DelegateToolName})
	action.BudgetUSD = turnCost // exactly the cost of the one driver turn

	_, err := loop.Execute(t.Context(), action, agentTask())

	wantLoopErrCode(t, err, agentloop.CodeBudgetExhausted)

	if got := driver.callsFor(subActionName); got != 0 {
		t.Fatalf("child served %d turns, want 0: a no-budget delegate must not spawn", got)
	}

	delegateStep := findDelegateStep(t, mustListSteps(t, db, actionBrain))
	assertClassifiedError(t, delegateStep.Output)
}

// A goal-less delegate ask fails as a transcript entry without burning the one
// delegation, so a later valid ask still spawns the child.
func TestDelegateMissingGoalDoesNotBurnDelegation(t *testing.T) {
	t.Parallel()

	driver := newPerActionDriver(map[string][]runner.Result{
		actionBrain: {
			delegateTurn(map[string]any{}),
			delegateTurn(map[string]any{argGoalKey: delegateGoal}),
			finalTurn(),
		},
		subActionName: {finalTurnAnswer(childAnswer, turnCost)},
	})
	db := newCapturingStore()
	loop := agentloop.New(newTestRegistry(t, driver, nil), nil, db, nil)

	action := agentAction([]string{agentloop.DelegateToolName})

	result, err := loop.Execute(t.Context(), action, agentTask())
	if err != nil {
		t.Fatalf("Execute = %v, want the parent to finish", err)
	}

	if result.Output != finalAnswer {
		t.Fatalf("parent output = %#v, want %q", result.Output, finalAnswer)
	}

	if got := driver.callsFor(subActionName); got != 1 {
		t.Fatalf("child served %d turns, want 1: the valid delegation must still spawn", got)
	}
}

// Bypass protection: DelegateTool.Execute reached outside the loop's
// interception refuses with agent.delegate_bypassed, permanent.
func TestDelegateToolExecuteRefusesBypass(t *testing.T) {
	t.Parallel()

	tool := agentloop.NewDelegateTool()

	_, err := tool.Execute(t.Context(), &runner.Task{Resolved: true, Params: map[string]any{argGoalKey: delegateGoal}})

	rerr, isRunnerErr := errors.AsType[*runner.Error](err)
	if !isRunnerErr {
		t.Fatalf("Execute err = %v (%T), want a *runner.Error", err, err)
	}

	if rerr.Code != agentloop.CodeDelegateBypassed {
		t.Fatalf("Execute code = %s, want %s", rerr.Code, agentloop.CodeDelegateBypassed)
	}

	if rerr.Class != runner.ClassPermanent {
		t.Fatalf("Execute class = %v, want permanent", rerr.Class)
	}
}

// ValidateAction accepts "delegate" without a registry entry (the loop owns it)
// and still rejects an unknown tool name.
func TestDelegateValidateActionAcceptsDelegate(t *testing.T) {
	t.Parallel()

	loop := agentloop.New(newTestRegistry(t, &scriptedDriver{}, nil), nil, nil, nil)

	valid := agentAction([]string{agentloop.DelegateToolName, builtins.MockName})
	if err := loop.ValidateAction(valid); err != nil {
		t.Fatalf("ValidateAction(delegate + mock) = %v, want nil", err)
	}

	unknown := agentAction([]string{"ghost_tool"})

	err := loop.ValidateAction(unknown)

	errs, isErrs := errors.AsType[cairnerr.Errors](err)
	if !isErrs || len(errs) == 0 {
		t.Fatalf("ValidateAction(unknown tool) = %v, want a validation error", err)
	}

	if errs[0].Code != workflow.CodeInvalid {
		t.Fatalf("code = %s, want %s", errs[0].Code, workflow.CodeInvalid)
	}
}
