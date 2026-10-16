package agentloop

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/pkg/runner"
)

// DelegateToolName is the loop-owned delegation tool. It is not a registry
// runner: bindTool supplies its descriptor and the loop intercepts its
// calls the way chat intercepts run_workflow, so its Execute never spawns a
// child in normal operation.
const DelegateToolName = "delegate"

// maxDelegations caps how many children one action execution may spawn, the
// same named-wall style as maxStrikes. One per action run in v1; raising it
// later is a one-const change plus a decision-entry update.
const maxDelegations = 1

// maxDelegateDepth caps nesting. A child never sees the delegate tool, so
// depth 1 is structural; this const backs the defensive re-check in
// runDelegate.
const maxDelegateDepth = 1

// delegate tool argument keys.
const (
	argGoal          = "goal"
	argMaxIterations = "max_iterations"
	argBudgetUSD     = "budget_usd"
)

// JSON Schema keywords the delegate descriptor repeats.
const (
	schemaType    = "type"
	schemaObject  = "object"
	schemaString  = "string"
	schemaInteger = "integer"
	schemaNumber  = "number"
	schemaDesc    = "description"
)

// DelegateTool is the descriptor side of delegation. It implements
// runner.Describer so the driver can see and call it, but its Execute never
// spawns a child: the loop intercepts a delegate call before any execution
// (runDelegate). Execute is wired to fail loudly so the walls cannot be
// bypassed by a code path that runs the tool directly.
type DelegateTool struct{}

var (
	_ runner.Runner    = (*DelegateTool)(nil)
	_ runner.Describer = (*DelegateTool)(nil)
)

// NewDelegateTool exposes the delegate descriptor as a runner so the bypass
// guarantee (Execute refuses outside the loop's interception) is testable and
// the descriptor is reachable for tooling.
func NewDelegateTool() *DelegateTool {
	return &DelegateTool{}
}

// Name implements runner.Runner.
func (*DelegateTool) Name() string {
	return DelegateToolName
}

// Validate implements runner.Runner: a delegate proposal must name a goal.
func (*DelegateTool) Validate(task *runner.Task) error {
	if !task.Resolved {
		return nil
	}

	goal, _ := task.Params[argGoal].(string)
	if goal == "" {
		return runner.NewError(runner.ClassPermanent, DelegateToolName+".missing_goal",
			"delegate requires a non-empty goal")
	}

	return nil
}

// Execute implements runner.Runner and must never run in normal operation: the
// loop intercepts a delegate call and runs the child itself. Reaching here
// means the interception was bypassed, so it refuses rather than spawning an
// unwalled child.
func (*DelegateTool) Execute(ctx context.Context, _ *runner.Task) (runner.Result, error) {
	if err := ctx.Err(); err != nil {
		return runner.Result{}, runner.NewError(runner.ClassTransient, DelegateToolName+".canceled", err.Error())
	}

	return runner.Result{}, runner.NewError(runner.ClassPermanent, CodeDelegateBypassed,
		"delegate must go through the agent loop's interception; direct execution is refused")
}

// Describe implements runner.Describer. The description tells the driver the
// child shares its tools except delegate, runs synchronously, and can happen
// at most once per action run.
func (*DelegateTool) Describe() runner.ToolSpec {
	return runner.ToolSpec{
		Name: DelegateToolName,
		Description: "Delegate a sub-goal to one synchronous child agent loop that shares your tools except delegate itself. " +
			"The child runs to completion and returns its final answer before your next turn, and you may delegate at most once per action run. " +
			"Name the goal; optionally set max_iterations or budget_usd, each clamped down to your own remaining caps.",
		InputSchema: map[string]any{
			schemaType: schemaObject,
			"properties": map[string]any{
				argGoal: map[string]any{
					schemaType: schemaString,
					schemaDesc: "the sub-goal the child agent should pursue",
				},
				argMaxIterations: map[string]any{
					schemaType: schemaInteger,
					schemaDesc: "optional cap on the child's iterations, clamped down to your remaining cap",
				},
				argBudgetUSD: map[string]any{
					schemaType: schemaNumber,
					schemaDesc: "optional cap on the child's budget in USD, clamped down to your remaining budget",
				},
			},
			"required": []string{argGoal},
		},
	}
}

// runDelegate intercepts one delegate tool call. Depth and count walls take
// the strike path; a malformed or unaffordable ask records a transcript
// failure without burning the one delegation; otherwise it builds and runs one
// synchronous child whose caps clamp to the parent's remaining walls.
func (r *loopRun) runDelegate(ctx context.Context, dec *decision) error {
	if r.depth >= maxDelegateDepth {
		return r.strike(ctx, dec, "delegation is not available to a delegated child")
	}

	if r.delegations >= maxDelegations {
		return r.strike(ctx, dec,
			fmt.Sprintf("the delegation wall (%d per action run) is already spent", maxDelegations))
	}

	// A valid delegate attempt clears the strike counter, like any accepted
	// tool call in applyDecision.
	r.strikes = 0

	goal, _ := dec.arguments[argGoal].(string)
	if goal == "" {
		r.recordDelegateFailure(ctx, dec, "delegate requires a non-empty goal")

		return nil
	}

	remaining := r.action.BudgetUSD - r.usage.CostUSD
	if remaining <= 0 {
		r.recordDelegateFailure(ctx, dec,
			fmt.Sprintf("no budget headroom remains ($%.4f of $%.2f already spent)", r.usage.CostUSD, r.action.BudgetUSD))

		return nil
	}

	r.spawnChild(ctx, dec, goal, remaining)

	return nil
}

// spawnChild builds the delegated child, runs it synchronously inside one
// delegate tool-call scope, then persists the requesting turn and the child's
// outcome as the parent's delegate tool_call step and absorbs the child's
// spend exactly once so the parent's budget wall sees every dollar.
func (r *loopRun) spawnChild(ctx context.Context, dec *decision, goal string, remaining float64) {
	r.delegations++

	childName := r.task.ActionName + "/sub-" + strconv.Itoa(r.delegations)

	childAction := *r.action
	childAction.Name = childName
	childAction.Goal = goal
	childAction.MaxIterations = clampIterations(requestedInt(dec.arguments, argMaxIterations), r.action.MaxIterations)
	childAction.BudgetUSD = clampBudget(requestedFloat(dec.arguments, argBudgetUSD), remaining)

	childTask := *r.task
	childTask.ActionName = childName

	childTools, childSpecs := r.childToolset()

	child := &loopRun{
		loop:    r.loop,
		action:  &childAction,
		task:    &childTask,
		driver:  r.driver,
		tools:   childTools,
		specs:   childSpecs,
		started: time.Now(),
		depth:   r.depth + 1,
	}

	r.persistStep(ctx, &store.AgentStep{
		Kind:   store.StepDriverCall,
		Output: dec.output,
		Usage:  dec.usage,
	})
	r.appendNote(fmt.Sprintf("[driver] called tool %q with arguments %s", DelegateToolName, renderValue(dec.arguments)))

	callCtx, finishCall := r.loop.instr.ToolCall(ctx, r.task.ActionName, DelegateToolName)
	childResult, childErr := child.run(callCtx)
	finishCall(childResult.Usage, childErr)

	r.usage = addUsage(r.usage, childResult.Usage)

	step := &store.AgentStep{
		Kind:  store.StepToolCall,
		Tool:  DelegateToolName,
		Input: dec.arguments,
	}

	if childErr != nil {
		step.Output = classifiedFailure(childErr)
		r.appendNote(fmt.Sprintf("[tool %s] failed (%s): %v", DelegateToolName, failureClass(childErr), childErr))
		r.persistStep(ctx, step)

		return
	}

	step.Output = childResult.Output
	r.appendNote(fmt.Sprintf("[tool %s] returned: %s", DelegateToolName, renderValue(childResult.Output)))
	r.persistStep(ctx, step)
}

// recordDelegateFailure records a delegate call that failed before any child
// spawned (a malformed ask or exhausted budget) in runTool's failure shape:
// the requesting driver turn, then a delegate tool_call carrying the
// classified error. No child spawns and delegations is untouched, so a bad ask
// never burns the one allowed delegation.
func (r *loopRun) recordDelegateFailure(ctx context.Context, dec *decision, reason string) {
	r.persistStep(ctx, &store.AgentStep{
		Kind:   store.StepDriverCall,
		Output: dec.output,
		Usage:  dec.usage,
	})
	r.appendNote(fmt.Sprintf("[driver] called tool %q with arguments %s", DelegateToolName, renderValue(dec.arguments)))

	err := runner.NewError(runner.ClassPermanent, DelegateToolName+".rejected", reason)
	r.appendNote(fmt.Sprintf("[tool %s] failed (%s): %v", DelegateToolName, failureClass(err), err))
	r.persistStep(ctx, &store.AgentStep{
		Kind:   store.StepToolCall,
		Tool:   DelegateToolName,
		Input:  dec.arguments,
		Output: classifiedFailure(err),
	})
}

// childToolset copies the parent's tools and specs minus delegate, so a
// delegated child can never delegate again. This removal is the primary
// no-recursion guard; the depth re-check in runDelegate is the defensive
// backstop for a child that somehow still holds the tool.
func (r *loopRun) childToolset() (map[string]runner.Runner, []runner.ToolSpec) {
	tools := make(map[string]runner.Runner, len(r.tools))

	for name, rnr := range r.tools {
		if name == DelegateToolName {
			continue
		}

		tools[name] = rnr
	}

	specs := make([]runner.ToolSpec, 0, len(r.specs))

	for idx := range r.specs {
		if r.specs[idx].Name == DelegateToolName {
			continue
		}

		specs = append(specs, r.specs[idx])
	}

	return tools, specs
}

// clampIterations returns the child's iteration cap: the requested value when
// present and below the parent's, otherwise the parent's. Zero or negative
// means unrequested, which inherits the parent's cap.
func clampIterations(requested, parent int) int {
	if requested <= 0 || requested > parent {
		return parent
	}

	return requested
}

// clampBudget returns the child's budget: the requested value when present and
// within the parent's remaining headroom, otherwise the remaining headroom.
// Zero or negative means unrequested, which inherits the remaining headroom.
func clampBudget(requested, remaining float64) float64 {
	if requested <= 0 || requested > remaining {
		return remaining
	}

	return requested
}

// requestedInt reads an optional integer tool argument. Native calls carry
// int/int64; provider tool calls arrive as float64 from JSON. Absent or
// non-numeric returns 0, which the clamp treats as "unrequested".
func requestedInt(args map[string]any, key string) int {
	switch num := args[key].(type) {
	case int:
		return num
	case int64:
		return int(num)
	case float64:
		return int(num)
	default:
		return 0
	}
}

// requestedFloat reads an optional numeric tool argument, with the same type
// coverage and "unrequested" convention as requestedInt.
func requestedFloat(args map[string]any, key string) float64 {
	switch num := args[key].(type) {
	case float64:
		return num
	case int:
		return float64(num)
	case int64:
		return float64(num)
	default:
		return 0
	}
}
