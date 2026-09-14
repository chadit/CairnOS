package agentloop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/chadit/CairnOS/internal/runner/builtins"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// maxStrikes is the consecutive bad-driver-output limit from the spec's
// termination table: "twice in a row".
const maxStrikes = 2

// memoryTokenBudget caps the tokens of memory entries loaded into each
// driver prompt; a config surface for it can come later if scopes outgrow
// the default.
const memoryTokenBudget = 2048

// Driver task params. They mirror the LLM runners' shared prompt/system
// contract (internal/runner/builtins), and paramScope is the argument the
// loop injects into memory-tool calls.
const (
	paramPrompt = "prompt"
	paramSystem = "system"
	paramScope  = "scope"
)

// Tool failure classes as transcript words, so the driver knows whether
// re-issuing the same call can help.
const (
	classTransient   = "transient"
	classRateLimited = "rate_limited"
	classPermanent   = "permanent"
	classNeedsReauth = "needs_reauth"
)

// defaultSystemPrompt frames the loop for drivers when the action declares
// no system_prompt of its own.
const defaultSystemPrompt = "You are an agent action inside a CairnOS workflow. " +
	"Work toward the goal step by step: call at most one of the offered tools per turn, " +
	"or reply with the final answer once the goal is met. Only the listed tools exist."

// loopRun is the per-execution state of one agent action. A single
// goroutine owns it for the whole execution, so no lock guards the fields.
type loopRun struct {
	started    time.Time
	driver     runner.ToolCaller
	loop       *Loop
	action     *workflow.Action
	task       *runner.Task
	tools      map[string]runner.Runner
	transcript []string
	specs      []runner.ToolSpec
	usage      runner.Usage
	stepCount  int
	strikes    int

	// depth is 0 for an engine-started action and r.depth+1 for a delegated
	// child. maxDelegateDepth caps it, so a child never delegates again.
	depth int

	// delegations counts children spawned this execution, walled at
	// maxDelegations.
	delegations int
}

// run drives iterations until a final answer or a wall
// (.agents/spec/15-agent-loop.md, "Loop mechanics"). Steps persist as they
// happen, so by the time any wall fails the action the partial transcript
// is already durable.
func (r *loopRun) run(ctx context.Context) (runner.Result, error) {
	for iteration := 1; ; iteration++ {
		driverResult, err := r.callDriver(ctx, iteration)
		if err != nil {
			return r.partial(), err
		}

		final, done, err := r.applyDecision(ctx, interpret(&driverResult))
		if err != nil {
			return r.partial(), err
		}

		if done {
			return final, nil
		}

		if err := r.checkWalls(ctx, iteration); err != nil {
			return r.partial(), err
		}
	}
}

// callDriver runs one driver turn over the assembled prompt. Memory reloads
// every iteration per the spec's prompt-assembly order, so notes the agent
// wrote are visible next turn. Usage counts toward the budget even when the
// turn errors, since the provider may have billed it. A driver failure ends the
// action unretried, classified by the engine like any runner failure.
func (r *loopRun) callDriver(ctx context.Context, iteration int) (runner.Result, error) {
	memorySection, err := r.memorySection(ctx)
	if err != nil {
		return runner.Result{}, err
	}

	driverTask := &runner.Task{
		RunID:      r.task.RunID,
		ActionName: r.task.ActionName,
		Params: map[string]any{
			paramSystem: r.systemPrompt(),
			paramPrompt: r.prompt(memorySection),
		},
		Inputs:   r.task.Inputs,
		Resolved: true,
		Deadline: r.task.Deadline,
	}

	callCtx, finishCall := r.loop.instr.DriverCall(ctx, r.task.ActionName, iteration)

	result, err := r.driver.ExecuteWithTools(callCtx, driverTask, r.specs)
	r.usage = addUsage(r.usage, result.Usage)

	finishCall(result.Usage, err)

	if err != nil {
		return result, fmt.Errorf("agent driver %q: %w", r.action.Driver, err)
	}

	return result, nil
}

// applyDecision executes one interpreted turn. done reports a final
// answer; a non-nil error is terminal (the strike limit).
func (r *loopRun) applyDecision(ctx context.Context, dec *decision) (runner.Result, bool, error) {
	switch dec.kind {
	case decisionFinal:
		return r.finish(ctx, dec), true, nil
	case decisionBad:
		return runner.Result{}, false, r.strike(ctx, dec,
			"the driver signaled a tool call that does not parse as one")
	case decisionToolCall:
		if _, allowed := r.tools[dec.tool]; !allowed {
			return runner.Result{}, false, r.strike(ctx, dec,
				fmt.Sprintf("tool %q is not in this action's allowlist", dec.tool))
		}

		if dec.tool == DelegateToolName {
			return runner.Result{}, false, r.runDelegate(ctx, dec)
		}

		r.strikes = 0
		r.runTool(ctx, dec)

		return runner.Result{}, false, nil
	default:
		return runner.Result{}, false, errf(CodeBadToolCall,
			"agent action %q: unknown driver decision", r.action.Name)
	}
}

// finish persists the final step and assembles the action result.
func (r *loopRun) finish(ctx context.Context, dec *decision) runner.Result {
	r.persistStep(ctx, &store.AgentStep{
		Kind:   store.StepFinal,
		Output: dec.output,
		Usage:  dec.usage,
	})

	return runner.Result{Output: dec.output, Usage: r.totalUsage()}
}

// strike records one rejected driver turn: the bad turn persists as a
// driver_call step, the rejection lands in the transcript so the driver
// can correct course, and the second consecutive strike ends the action
// per the spec's termination table.
func (r *loopRun) strike(ctx context.Context, dec *decision, reason string) error {
	r.strikes++

	r.persistStep(ctx, &store.AgentStep{
		Kind:   store.StepDriverCall,
		Output: dec.output,
		Usage:  dec.usage,
	})

	r.appendNote("[loop] rejected the previous turn: " + reason + "; call one listed tool or answer")

	if r.strikes >= maxStrikes {
		return errf(CodeBadToolCall,
			"agent action %q: %d consecutive driver turns were unparseable or named disallowed tools (last: %s)",
			r.action.Name, r.strikes, reason)
	}

	return nil
}

// runTool executes one allowed tool call: the requesting driver turn and
// the tool outcome each persist as a step, and both render into the
// transcript. The outcome never fails the action; an error becomes
// transcript text the driver can react to (see the package doc).
func (r *loopRun) runTool(ctx context.Context, dec *decision) {
	r.persistStep(ctx, &store.AgentStep{
		Kind:   store.StepDriverCall,
		Output: dec.output,
		Usage:  dec.usage,
	})

	arguments := r.scopedArguments(dec)
	r.appendNote(fmt.Sprintf("[driver] called tool %q with arguments %s", dec.tool, renderValue(arguments)))

	output, usage, callErr := r.executeTool(ctx, dec.tool, arguments)
	r.usage = addUsage(r.usage, usage)

	step := &store.AgentStep{
		Kind:   store.StepToolCall,
		Tool:   dec.tool,
		Input:  arguments,
		Output: output,
		Usage:  usage,
	}

	if callErr != nil {
		step.Output = classifiedFailure(callErr)
		r.appendNote(fmt.Sprintf("[tool %s] failed (%s): %v", dec.tool, failureClass(callErr), callErr))
		r.persistStep(ctx, step)

		return
	}

	r.appendNote(fmt.Sprintf("[tool %s] returned: %s", dec.tool, renderValue(output)))
	r.persistStep(ctx, step)
}

// executeTool runs one tool task through the runner's own contract:
// Validate, then a single Execute attempt. The whole exchange, validation
// included, sits inside one instrumented tool-call scope.
func (r *loopRun) executeTool(ctx context.Context, name string, arguments map[string]any) (any, runner.Usage, error) {
	tool := r.tools[name]

	toolTask := &runner.Task{
		RunID:      r.task.RunID,
		ActionName: r.task.ActionName,
		Params:     arguments,
		Inputs:     r.task.Inputs,
		Resolved:   true,
		Deadline:   r.task.Deadline,
	}

	callCtx, finishCall := r.loop.instr.ToolCall(ctx, r.task.ActionName, name)

	if err := tool.Validate(toolTask); err != nil {
		wrapped := fmt.Errorf("tool %q rejected the arguments: %w", name, err)
		finishCall(runner.Usage{}, wrapped)

		return nil, runner.Usage{}, wrapped
	}

	result, err := tool.Execute(callCtx, toolTask)
	if err != nil {
		wrapped := fmt.Errorf("tool %q: %w", name, err)
		finishCall(result.Usage, wrapped)

		return nil, result.Usage, wrapped
	}

	finishCall(result.Usage, nil)

	return result.Output, result.Usage, nil
}

// scopedArguments clones the driver's arguments, forcing the action's
// memory_scope onto memory-tool calls: the scope is a wall the agent
// cannot argue its way out of, so a driver-supplied scope is overwritten.
func (r *loopRun) scopedArguments(dec *decision) map[string]any {
	arguments := maps.Clone(dec.arguments)
	if arguments == nil {
		arguments = map[string]any{}
	}

	if dec.tool == builtins.MemoryName && r.action.MemoryScope != "" {
		arguments[paramScope] = r.action.MemoryScope
	}

	return arguments
}

// checkWalls applies the loop's hard limits after every iteration: the
// caller context first (the engine maps it to the timeout or cancel
// codes), then budget, then iterations. Budget uses >= because a spend
// that reached the cap leaves nothing for another driver call.
func (r *loopRun) checkWalls(ctx context.Context, iteration int) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("agent action %q stopped: %w", r.action.Name, err)
	}

	if r.usage.CostUSD >= r.action.BudgetUSD {
		return errf(CodeBudgetExhausted,
			"agent action %q spent an estimated $%.4f of its $%.2f budget after %d iteration(s)",
			r.action.Name, r.usage.CostUSD, r.action.BudgetUSD, iteration)
	}

	if iteration >= r.action.MaxIterations {
		return errf(CodeMaxIterations,
			"agent action %q used all %d iterations without a final answer",
			r.action.Name, r.action.MaxIterations)
	}

	return nil
}

// persistStep numbers and writes one transcript row. Persistence failures
// log rather than fail the loop, matching the engine's persistAction
// stance: the in-memory transcript keeps the loop correct, history is
// best effort. The write detaches from cancellation so a timed-out loop
// still records its last steps.
func (r *loopRun) persistStep(ctx context.Context, step *store.AgentStep) {
	r.stepCount++
	step.RunID = r.task.RunID
	step.ActionName = r.task.ActionName
	step.Step = r.stepCount
	step.CreatedAt = time.Now()

	if r.loop.steps == nil {
		return
	}

	if err := r.loop.steps.AppendAgentStep(context.WithoutCancel(ctx), step); err != nil {
		slog.Error("persist agent step",
			"run_id", step.RunID,
			"action_name", step.ActionName,
			"step", step.Step,
			"error", err)
	}
}

// partial is the Result returned alongside a wall failure: the usage spent
// so far, so the engine persists real cost for failed agent actions too.
func (r *loopRun) partial() runner.Result {
	return runner.Result{Usage: r.totalUsage()}
}

// totalUsage stamps the loop's wall-clock latency onto the accumulated
// driver-plus-tool usage.
func (r *loopRun) totalUsage() runner.Usage {
	total := r.usage
	total.Latency = time.Since(r.started)

	return total
}

// decisionKind classifies one driver turn.
type decisionKind int

const (
	// decisionFinal ends the loop with the turn's output as the answer.
	decisionFinal decisionKind = iota
	// decisionToolCall requests one tool execution.
	decisionToolCall
	// decisionBad is an unparseable tool request: the driver signaled
	// tool_use but the output does not follow the convention.
	decisionBad
)

// decision is one interpreted driver turn.
type decision struct {
	output    any
	arguments map[string]any
	tool      string
	usage     runner.Usage
	kind      decisionKind
}

// interpret applies the driver convention to one turn: the stop reason
// settles ambiguity, the output shape decides when no stop reason came
// back (pkg/runner.ToolCaller).
func interpret(result *runner.Result) *decision {
	dec := &decision{output: result.Output, usage: result.Usage}

	name, arguments, parses := parseToolCall(result.Output)

	switch result.Meta[runner.MetaStopReason] {
	case runner.StopReasonEndTurn:
		dec.kind = decisionFinal
	case runner.StopReasonToolUse:
		dec.kind = decisionBad
		if parses {
			dec.kind = decisionToolCall
		}
	default:
		dec.kind = decisionFinal
		if parses {
			dec.kind = decisionToolCall
		}
	}

	if dec.kind == decisionToolCall {
		dec.tool = name
		dec.arguments = arguments
	}

	return dec
}

// parseToolCall reads the convention's tool-call shape out of a driver
// output: a map with a non-empty "tool" string and an optional map of
// "arguments".
func parseToolCall(output any) (string, map[string]any, bool) {
	table, isTable := output.(map[string]any)
	if !isTable {
		return "", nil, false
	}

	name, isText := table[runner.ToolCallName].(string)
	if !isText || name == "" {
		return "", nil, false
	}

	raw, present := table[runner.ToolCallArguments]
	if !present || raw == nil {
		return name, map[string]any{}, true
	}

	arguments, isArgs := raw.(map[string]any)
	if !isArgs {
		return "", nil, false
	}

	return name, arguments, true
}

// classifiedFailure renders a failed tool or delegate call as the transcript's
// error map: the message and its retry class, so the driver can judge whether
// re-issuing the call could help.
func classifiedFailure(err error) map[string]any {
	return map[string]any{"error": err.Error(), "class": failureClass(err)}
}

// failureClass names a tool failure's retry class for the transcript.
func failureClass(err error) string {
	classified, isClassified := errors.AsType[*runner.Error](err)
	if !isClassified {
		return classPermanent
	}

	switch classified.Class {
	case runner.ClassTransient:
		return classTransient
	case runner.ClassRateLimited:
		return classRateLimited
	case runner.ClassNeedsReauth:
		return classNeedsReauth
	case runner.ClassPermanent:
		return classPermanent
	default:
		return classPermanent
	}
}

// addUsage sums two usage reports field-wise. Latency is left alone: the
// loop reports its own wall-clock latency at the end instead of a sum of
// overlapping per-call latencies.
func addUsage(total, delta runner.Usage) runner.Usage {
	total.InputTokens += delta.InputTokens
	total.OutputTokens += delta.OutputTokens
	total.CachedTokens += delta.CachedTokens
	total.CostUSD += delta.CostUSD

	return total
}
