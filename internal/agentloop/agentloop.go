// Package agentloop is the engine-owned loop behind kind = "agent" actions
// (.agents/spec/15-agent-loop.md): a driver model plans inside hard walls
// (max_iterations, budget_usd, the action timeout), calling only
// allowlisted tool runners, with every step persisted as an agent_steps
// row.
//
// # Driver convention
//
// A driver is a registered runner implementing runner.ToolCaller. Each
// iteration the loop calls ExecuteWithTools with the action's tool
// descriptors and a task whose params carry "system" (the system framing)
// and "prompt" (the goal, memory entries for memory_scope, and the
// transcript so far, rendered as text). The driver answers through Result:
//
//   - Tool call: Output is map[string]any{"tool": <name>, "arguments":
//     map[string]any{...}} and Meta["stop_reason"] is "tool_use".
//   - Final answer: Output is any other value and Meta["stop_reason"] is
//     "end_turn".
//
// When the Output shape alone is ambiguous (a final answer that is a map
// with a "tool" key), the stop reason decides. Per D8 in
// .agents/spec/14-open-decisions.md there is no JSON-in-text fallback:
// a driver without native tool calling fails validation with
// agent.driver_incapable.
//
// # Tool calls
//
// A requested tool runs once: Validate, then a single Execute attempt, no
// retry and no circuit breaker. A failure is classified and written into
// the transcript instead of failing the action, so the driver can adapt
// (re-issue the call, switch tools, or finish). Retrying inside the loop
// would guess at an idempotence the tool call never declared; the driver
// re-issuing a call is the agent-world retry.
package agentloop

import (
	"context"
	"time"

	"github.com/chadit/CairnOS/internal/cairnerr"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/workflow"
	"github.com/chadit/CairnOS/pkg/runner"
)

// StepWriter persists transcript steps; store.Store satisfies it.
type StepWriter interface {
	AppendAgentStep(ctx context.Context, step *store.AgentStep) error
}

// Instrumentation observes loop iterations and tool calls so the telemetry layer
// can hang per-iteration child spans under the action's span and feed the two
// agent metrics (.agents/spec/15-agent-loop.md, "Transcript persistence") without
// the loop importing a telemetry SDK. The engine threads the action scope through
// ctx. Implementations must be concurrency-safe and return promptly; each finish
// func is called exactly once.
type Instrumentation interface {
	// DriverCall opens one driver turn's scope under the action scope in
	// ctx; iteration is 1-based. The finish func receives the turn's usage
	// and error.
	DriverCall(ctx context.Context, actionName string, iteration int) (context.Context, func(usage runner.Usage, err error))

	// ToolCall opens one tool execution's scope under the action scope in
	// ctx. The finish func receives the tool's usage and outcome.
	ToolCall(ctx context.Context, actionName, tool string) (context.Context, func(usage runner.Usage, err error))
}

// nopInstrumentation is the default when New receives nil instrumentation.
type nopInstrumentation struct{}

// DriverCall implements Instrumentation.
func (nopInstrumentation) DriverCall(ctx context.Context, _ string, _ int) (context.Context, func(runner.Usage, error)) {
	return ctx, func(runner.Usage, error) {}
}

// ToolCall implements Instrumentation.
func (nopInstrumentation) ToolCall(ctx context.Context, _, _ string) (context.Context, func(runner.Usage, error)) {
	return ctx, func(runner.Usage, error) {}
}

// Loop executes agent actions against a runner registry, an optional
// memory store, and a step persister. It implements engine.AgentExecutor.
// Safe for concurrent use: per-execution state lives in a loopRun.
type Loop struct {
	registry *runner.Registry
	memory   memory.Store
	steps    StepWriter
	instr    Instrumentation
}

// New builds a Loop. memoryStore may be nil: no memory section loads into
// prompts, and wiring leaves the memory tool unregistered. steps may be
// nil in embedders that do not persist transcripts, and instr may be nil
// when nothing observes the loop.
func New(registry *runner.Registry, memoryStore memory.Store, steps StepWriter, instr Instrumentation) *Loop {
	if instr == nil {
		instr = nopInstrumentation{}
	}

	return &Loop{registry: registry, memory: memoryStore, steps: steps, instr: instr}
}

// Execute runs the loop for one agent action; the engine resolves the goal
// and derives the deadline before calling. The returned Result carries the
// final answer as Output and the summed driver-plus-tool Usage. Failures
// still return the usage spent so far, and every step is already persisted
// by the time any wall fails the action.
func (l *Loop) Execute(ctx context.Context, action *workflow.Action, task *runner.Task) (runner.Result, error) {
	driver, cerr := l.driverFor(action)
	if cerr != nil {
		return runner.Result{}, cerr
	}

	tools, specs, cerr := l.toolset(action)
	if cerr != nil {
		return runner.Result{}, cerr
	}

	execution := &loopRun{
		loop:    l,
		action:  action,
		task:    task,
		driver:  driver,
		tools:   tools,
		specs:   specs,
		started: time.Now(),
	}

	return execution.run(ctx)
}

// Delegation is the no-recursion boundary. An action name is not a runner name,
// and no registry runner starts a workflow or another loop. Delegation is bounded
// by its own walls (opt-in via tools, depth capped at 1, one delegation per action
// run, iteration and budget clamped to the parent's remaining), so it adds exactly
// one child level and never a third (.agents/spec/15-agent-loop.md, "No recursion
// in v1"; D9 and D15 in .agents/spec/14-open-decisions.md).

// ValidateAction applies the run-start checks from the spec's termination table:
// the driver must be a registered runner.ToolCaller (agent.driver_incapable
// otherwise) and every tools entry a registered runner.Describer
// (workflow.invalid otherwise). Tools resolve through the runner registry, except
// "delegate", which describerFor supplies a descriptor for.
func (l *Loop) ValidateAction(action *workflow.Action) error {
	var errs cairnerr.Errors

	if _, cerr := l.driverFor(action); cerr != nil {
		errs = append(errs, cerr)
	}

	for _, name := range action.Tools {
		if _, _, cerr := l.describerFor(action, name); cerr != nil {
			errs = append(errs, cerr)
		}
	}

	if len(errs) > 0 {
		return errs
	}

	return nil
}

// driverFor resolves the action's driver to its ToolCaller capability.
// Execute re-checks what ValidateAction already proved, so a runner
// vanishing mid-run still fails cleanly instead of panicking.
func (l *Loop) driverFor(action *workflow.Action) (runner.ToolCaller, *cairnerr.Error) {
	rnr, registered := l.registry.Get(action.Driver)
	if !registered {
		return nil, errf(CodeDriverIncapable,
			"agent action %q driver %q is not a registered runner", action.Name, action.Driver)
	}

	caller, capable := rnr.(runner.ToolCaller)
	if !capable {
		return nil, errf(CodeDriverIncapable,
			"agent action %q driver %q lacks native tool calling; drivers must implement runner.ToolCaller",
			action.Name, action.Driver)
	}

	return caller, nil
}

// describerFor resolves one tools entry, enforcing registration and the
// Describer capability (.agents/spec/15-agent-loop.md, "Tools"). The one
// exception is "delegate", the loop-owned delegation tool: it is accepted
// without a registry lookup and supplies its own descriptor, so ValidateAction
// and toolset both admit it (internal/agentloop/delegate.go).
func (l *Loop) describerFor(action *workflow.Action, name string) (runner.Runner, runner.Describer, *cairnerr.Error) {
	if name == DelegateToolName {
		tool := &delegateTool{}

		return tool, tool, nil
	}

	rnr, registered := l.registry.Get(name)
	if !registered {
		return nil, nil, errf(workflow.CodeInvalid,
			"agent action %q tool %q is not a registered runner", action.Name, name)
	}

	describer, describable := rnr.(runner.Describer)
	if !describable {
		return nil, nil, errf(workflow.CodeInvalid,
			"agent action %q tool %q does not implement runner.Describer, so the driver cannot see it",
			action.Name, name)
	}

	return rnr, describer, nil
}

// toolset resolves the allowlist to executable runners plus the
// descriptors the driver sees, in the action's declared tool order.
func (l *Loop) toolset(action *workflow.Action) (map[string]runner.Runner, []runner.ToolSpec, *cairnerr.Error) {
	tools := make(map[string]runner.Runner, len(action.Tools))
	specs := make([]runner.ToolSpec, 0, len(action.Tools))

	for _, name := range action.Tools {
		rnr, describer, cerr := l.describerFor(action, name)
		if cerr != nil {
			return nil, nil, cerr
		}

		tools[name] = rnr

		specs = append(specs, describer.Describe())
	}

	return tools, specs, nil
}
