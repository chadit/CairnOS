// Package agentloop is the engine-owned loop behind kind = "agent" actions: a
// driver model plans inside hard walls (max_iterations, budget_usd, the action
// timeout), calling only allowlisted tool runners, with every step persisted as
// an agent_steps row.
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
// with a "tool" key), the stop reason decides. There is no JSON-in-text
// fallback: a driver without native tool calling fails validation with
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
// agent metrics without the loop importing a telemetry SDK. The engine threads
// the action scope through ctx. Implementations must be concurrency-safe and
// return promptly; each finish func is called exactly once.
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
	bound := l.bind(action)
	if len(bound.problems) > 0 {
		return runner.Result{}, bound.problems[0]
	}

	execution := &loopRun{
		loop:    l,
		action:  action,
		task:    task,
		driver:  bound.driver,
		tools:   bound.tools,
		specs:   bound.specs(),
		started: time.Now(),
	}

	return execution.run(ctx)
}

// Delegation is the no-recursion boundary. An action name is not a runner name,
// and no registry runner starts a workflow or another loop. Delegation is bounded
// by its own walls (opt-in via tools, depth capped at 1, one delegation per action
// run, iteration and budget clamped to the parent's remaining), so it adds exactly
// one child level and never a third.

// ValidateAction applies the run-start checks:
// the driver must be a registered runner.ToolCaller (agent.driver_incapable
// otherwise) and every tools entry a registered runner.Describer
// (workflow.invalid otherwise). Tools resolve through the runner registry, except
// "delegate", which bindTool supplies a descriptor for.
func (l *Loop) ValidateAction(action *workflow.Action) error {
	if bound := l.bind(action); len(bound.problems) > 0 {
		return bound.problems
	}

	return nil
}

// binding is what an agent action's driver and tools entries resolve to in the
// runner registry. problems holds one error per entry that did not resolve,
// driver first. ValidateAction reports them all; Execute re-checks what
// validation proved and fails on the first, so a runner vanishing mid-run
// still fails cleanly instead of panicking.
type binding struct {
	driver     runner.ToolCaller
	tools      map[string]runner.Runner
	describers []runner.Describer
	problems   cairnerr.Errors
}

// bind resolves the action against the registry. It leaves descriptors unread
// until specs, because an MCP tool's Describe fetches its schema over the
// network and validation should not.
func (l *Loop) bind(action *workflow.Action) *binding {
	bound := &binding{tools: make(map[string]runner.Runner, len(action.Tools))}
	bound.bindDriver(l.registry, action)

	for _, name := range action.Tools {
		bound.bindTool(l.registry, action, name)
	}

	return bound
}

// bindDriver resolves the action's driver to its ToolCaller capability.
func (b *binding) bindDriver(registry *runner.Registry, action *workflow.Action) {
	rnr, registered := registry.Get(action.Driver)
	if !registered {
		b.problems = append(b.problems, errf(CodeDriverIncapable,
			"agent action %q driver %q is not a registered runner", action.Name, action.Driver))

		return
	}

	caller, capable := rnr.(runner.ToolCaller)
	if !capable {
		b.problems = append(b.problems, errf(CodeDriverIncapable,
			"agent action %q driver %q lacks native tool calling; drivers must implement runner.ToolCaller",
			action.Name, action.Driver))

		return
	}

	b.driver = caller
}

// bindTool resolves one tools entry, enforcing registration and the Describer
// capability. The one exception is "delegate", the loop-owned delegation tool:
// it is accepted without a registry lookup and supplies its own descriptor
// (internal/agentloop/delegate.go).
func (b *binding) bindTool(registry *runner.Registry, action *workflow.Action, name string) {
	if name == DelegateToolName {
		tool := &DelegateTool{}
		b.tools[name] = tool
		b.describers = append(b.describers, tool)

		return
	}

	rnr, registered := registry.Get(name)
	if !registered {
		b.problems = append(b.problems, errf(workflow.CodeInvalid,
			"agent action %q tool %q is not a registered runner", action.Name, name))

		return
	}

	describer, describable := rnr.(runner.Describer)
	if !describable {
		b.problems = append(b.problems, errf(workflow.CodeInvalid,
			"agent action %q tool %q does not implement runner.Describer, so the driver cannot see it",
			action.Name, name))

		return
	}

	b.tools[name] = rnr
	b.describers = append(b.describers, describer)
}

// specs reads the descriptors the driver sees, in the action's declared tool
// order.
func (b *binding) specs() []runner.ToolSpec {
	specs := make([]runner.ToolSpec, 0, len(b.describers))

	for _, describer := range b.describers {
		specs = append(specs, describer.Describe())
	}

	return specs
}
